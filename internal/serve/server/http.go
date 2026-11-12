// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package server is the review service orchestrator: it receives webhooks,
// serializes work per pull request, runs the round state machine, and drives
// checkout, review and posting through the forge and review interfaces.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// Deps are the collaborators of the server. A nil entry in Forges disables
// that platform, and its webhook route answers 404.
type Deps struct {
	Config     *serveconfig.Config
	Forges     map[forge.Platform]forge.Forge
	Reviewer   review.Reviewer
	Workspaces review.Workspaces
	Locale     state.Locale
	Logger     *slog.Logger
	Now        func() time.Time
	Version    string
}

// Server is the webhook service.
type Server struct {
	cfg        *serveconfig.Config
	forges     map[forge.Platform]forge.Forge
	reviewer   review.Reviewer
	workspaces review.Workspaces
	loc        state.Locale
	log        *slog.Logger
	now        func() time.Time
	version    string

	store   *Store
	seen    *dedupe
	queue   *queue
	workers chan struct{}

	// mu orders intake against Shutdown: work is only added while holding
	// the read lock and closing is false, so wg never grows after Shutdown
	// starts waiting on it.
	mu        sync.RWMutex
	closing   bool
	started   bool
	wg        sync.WaitGroup
	runCtx    context.Context
	cancelRun context.CancelFunc
	pollStop  context.CancelFunc
	pollDone  chan struct{}
}

const (
	waitingFile     = "waiting.json"
	sideCallTimeout = 30 * time.Second
	maxReruns       = 3
)

// New checks d and opens the waiting store under Config.DataDir.
func New(d Deps) (*Server, error) {
	switch {
	case d.Config == nil:
		return nil, errors.New("server: config is required")
	case d.Reviewer == nil || d.Workspaces == nil:
		return nil, errors.New("server: reviewer and workspaces are required")
	case d.Locale == nil:
		return nil, errors.New("server: locale is required")
	case d.Config.Workers < 1:
		return nil, fmt.Errorf("server: workers must be at least 1, got %d", d.Config.Workers)
	}
	forges := map[forge.Platform]forge.Forge{}
	for p, f := range d.Forges {
		if f != nil {
			forges[p] = f
		}
	}
	if len(forges) == 0 {
		return nil, errors.New("server: no platform is enabled")
	}
	if d.Logger == nil {
		d.Logger = NewLogger(os.Stdout)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	store, err := OpenStore(filepath.Join(d.Config.DataDir, waitingFile))
	if errors.Is(err, ErrStoreCorrupt) {
		d.Logger.Error("waiting store reset", "error", err.Error())
	} else if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: d.Config, forges: forges, reviewer: d.Reviewer, workspaces: d.Workspaces,
		loc: d.Locale, log: d.Logger, now: d.Now, version: d.Version,
		store: store, seen: newDedupe(d.Now, dedupeTTL, dedupeMaxKeys),
		workers: make(chan struct{}, d.Config.Workers),
	}
	s.runCtx, s.cancelRun = context.WithCancel(context.Background())
	s.queue = newQueue(&s.wg, s.process)
	return s, nil
}

// Handler serves the webhook routes and the health check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /github/webhook", s.webhook(forge.GitHub))
	mux.HandleFunc("POST /gitlab/webhook", s.webhook(forge.GitLab))
	mux.HandleFunc("GET /healthz", s.health)
	return mux
}

// Start starts the GitLab poller. Reviews run on demand, so there is no
// worker pool to start.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closing:
		return errors.New("server: already shut down")
	case s.started:
		return errors.New("server: already started")
	}
	s.started = true
	if s.forges[forge.GitLab] == nil {
		return nil
	}
	pctx, stop := context.WithCancel(ctx)
	s.pollStop, s.pollDone = stop, make(chan struct{})
	go func() {
		defer close(s.pollDone)
		s.poll(pctx, s.cfg.PollInterval)
	}()
	return nil
}

// Shutdown stops intake and waits for queued and running work until ctx
// ends; then it cancels the remaining work and returns ctx's error.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	stop, pollDone := s.pollStop, s.pollDone
	s.mu.Unlock()
	if stop != nil {
		stop()
		<-pollDone
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	defer s.cancelRun()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// intake runs fn only while the server accepts work.
func (s *Server) intake(fn func()) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closing {
		return false
	}
	fn()
	return true
}

func (s *Server) isClosing() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closing
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	if s.isClosing() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

// webhook verifies and parses a delivery and hands it to dispatch. It makes
// no forge API call: a rate-limit retry here could pass GitHub's 10 second
// delivery timeout and cause a redelivery.
func (s *Server) webhook(p forge.Platform) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		res := s.handleWebhook(p, w, r)
		if res.requestID == "" {
			res.requestID = newRequestID()
		}
		w.Header().Set("X-Request-Id", res.requestID)
		writeJSON(w, res.status, map[string]string{"status": http.StatusText(res.status), "request_id": res.requestID})
		withRequest(s.log, res.requestID).Info("webhook",
			"platform", string(p), "event", res.kind, "outcome", res.outcome,
			"status", res.status, "duration_ms", time.Since(start).Milliseconds())
	}
}

type webhookResult struct {
	status    int
	requestID string
	kind      string
	outcome   string
}

func (s *Server) handleWebhook(p forge.Platform, w http.ResponseWriter, r *http.Request) webhookResult {
	f := s.forges[p]
	if f == nil {
		return webhookResult{status: http.StatusNotFound, outcome: "platform disabled"}
	}
	if s.isClosing() {
		return webhookResult{status: http.StatusServiceUnavailable, outcome: "shutting down"}
	}
	limit := s.cfg.MaxBodyBytes
	if r.ContentLength > limit {
		return webhookResult{status: http.StatusRequestEntityTooLarge, outcome: "body too large"}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return webhookResult{status: http.StatusRequestEntityTooLarge, outcome: "body too large"}
		}
		return webhookResult{status: http.StatusBadRequest, outcome: "read body: " + err.Error()}
	}
	if err := f.Verify(r.Header, body); err != nil {
		status := http.StatusUnauthorized
		if p == forge.GitHub {
			status = http.StatusForbidden
		}
		return webhookResult{status: status, outcome: "verify: " + err.Error()}
	}
	ev, err := f.ParseEvent(r.Header, body)
	if err != nil {
		return webhookResult{status: http.StatusBadRequest, outcome: "parse: " + err.Error()}
	}
	res := webhookResult{status: http.StatusAccepted, requestID: ev.DeliveryID, kind: string(ev.Kind)}
	if res.requestID == "" {
		res.requestID = newRequestID()
	}
	ev.Ref.Platform = p
	if !s.intake(func() { res.outcome = s.dispatch(ev, body, res.requestID) }) {
		return webhookResult{status: http.StatusServiceUnavailable, requestID: res.requestID, kind: res.kind, outcome: "shutting down"}
	}
	return res
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
