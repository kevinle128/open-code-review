// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package server is the review service orchestrator: it receives webhooks,
// serializes work per pull request, runs the review chain of each PR round
// by round, and drives checkout, review, fix, push and commit status through
// the forge and review interfaces.
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
	"sync"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// Deps are the collaborators of the server. A nil entry in Forges disables
// that platform, and its webhook route answers 404. A nil Fixer makes every
// round report-only, with the review findings as they are. Locales holds
// the text of each enabled platform, and Languages the output language the
// engines get for its PRs; a platform without a language leaves the choice
// to the engines.
type Deps struct {
	Config     *serveconfig.Config
	Forges     map[forge.Platform]forge.Forge
	Reviewer   review.Reviewer
	Fixer      review.Fixer
	Workspaces review.Workspaces
	Locales    map[forge.Platform]state.Locale
	Languages  map[forge.Platform]string
	Logger     *slog.Logger
	Now        func() time.Time
	Version    string
}

// Server is the webhook service.
type Server struct {
	cfg        *serveconfig.Config
	forges     map[forge.Platform]forge.Forge
	reviewer   review.Reviewer
	fixer      review.Fixer
	workspaces review.Workspaces
	locales    map[forge.Platform]state.Locale
	languages  map[forge.Platform]string
	log        *slog.Logger
	now        func() time.Time
	version    string

	seen    *dedupe
	queue   *queue
	workers chan struct{}
	journal *journal

	chainMu sync.Mutex
	chains  map[string]*chainCtl

	// mu orders intake against Shutdown: work is only added while holding
	// the read lock and closing is false, so wg never grows after Shutdown
	// starts waiting on it.
	mu        sync.RWMutex
	closing   bool
	started   bool
	wg        sync.WaitGroup
	runCtx    context.Context
	cancelRun context.CancelFunc
}

const (
	sideCallTimeout = 30 * time.Second
	// failWriteGrace bounds the wait after a drain timeout for the failed
	// status writes of the cut rounds. The drain (60 s) plus this wait stays
	// below the 75 s stop timeout of the PM2 and compose deploys.
	failWriteGrace = 10 * time.Second
	maxReruns      = 3
)

// New checks d.
func New(d Deps) (*Server, error) {
	switch {
	case d.Config == nil:
		return nil, errors.New("server: config is required")
	case d.Reviewer == nil || d.Workspaces == nil:
		return nil, errors.New("server: reviewer and workspaces are required")
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
	for p := range forges {
		if d.Locales[p] == nil {
			return nil, fmt.Errorf("server: no locale for %s", p)
		}
	}
	if d.Logger == nil {
		d.Logger = NewLogger(os.Stdout)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Config.MaxShutdownCuts < 1 {
		d.Config.MaxShutdownCuts = serveconfig.DefaultMaxShutdownCuts
	}
	s := &Server{
		cfg: d.Config, forges: forges, reviewer: d.Reviewer, fixer: d.Fixer, workspaces: d.Workspaces,
		locales: d.Locales, languages: d.Languages, log: d.Logger, now: d.Now, version: d.Version,
		seen:    newDedupe(d.Now, dedupeTTL, dedupeMaxKeys),
		workers: make(chan struct{}, d.Config.Workers),
		chains:  map[string]*chainCtl{},
	}
	jr, err := openJournal(d.Config.DataDir)
	if err != nil {
		s.log.Error("round journal unreadable; starting with an empty one", "error", err.Error())
	}
	s.journal = jr
	s.runCtx, s.cancelRun = context.WithCancel(context.Background())
	s.queue = newQueue(&s.wg, s.process)
	return s, nil
}

// Handler serves the webhook routes and the health check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /github/webhook", s.webhook(forge.GitHub))
	mux.HandleFunc("POST /gitlab/webhook", s.webhook(forge.GitLab))
	mux.HandleFunc("POST /gitlab/ci-trigger", s.ciTrigger)
	mux.HandleFunc("GET /healthz", s.health)
	return mux
}

// Start marks the server started and queues again the rounds that the
// last run did not end: a restart or a crash cut them.
func (s *Server) Start(context.Context) error {
	s.mu.Lock()
	switch {
	case s.closing:
		s.mu.Unlock()
		return errors.New("server: already shut down")
	case s.started:
		s.mu.Unlock()
		return errors.New("server: already started")
	}
	s.started = true
	s.mu.Unlock()
	for _, e := range s.journal.pending() {
		ref := e.ref()
		if s.forges[ref.Platform] == nil {
			continue
		}
		rj := e.roundJob()
		rj.gen = s.startChain(ref, rj.n, rj.scope)
		withPR(s.log, ref).Info("round resumed after a restart", "round", rj.n, "scope", rj.scope)
		s.queue.submit(ref, job{round: &rj, requestID: "resume-" + newRequestID()})
	}
	return nil
}

// Shutdown stops intake and waits for queued and running work until ctx
// ends; then it cancels the remaining work and returns ctx's error.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.cancelRun()
		return nil
	case <-ctx.Done():
	}
	// The cancelled rounds still write their failed status. Give them a
	// short time to finish, so a PR is not left at the pending status of a
	// round that will never run.
	s.cancelRun()
	select {
	case <-done:
	case <-time.After(failWriteGrace):
	}
	return ctx.Err()
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
	if p == forge.GitLab && s.cfg.GitLab != nil && s.cfg.GitLab.WebhookSecret == "" {
		return webhookResult{status: http.StatusNotFound, outcome: "gitlab webhook disabled"}
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
