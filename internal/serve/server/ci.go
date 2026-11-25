// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// ciJob is a review started by POST /gitlab/ci-trigger. gen is the chain
// the handler started before it queued the job. sha is the commit the
// pipeline built; pipelineID is that pipeline when the job sent one.
type ciJob struct {
	gen        uint64
	sha        string
	pipelineID int64
	force      bool
}

var ciSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type ciTriggerBody struct {
	ProjectID   int64  `json:"project_id"`
	MRIID       int    `json:"mr_iid"`
	SHA         string `json:"sha"`
	PipelineID  int64  `json:"pipeline_id"`
	ProjectPath string `json:"project_path"`
	Force       bool   `json:"force"`
}

// projectPathLookup reads a GitLab project's path_with_namespace.
type projectPathLookup interface {
	ResolveProjectPath(ctx context.Context, projectID int64) (string, error)
}

// directStatus posts a commit status and reports only a failure to post it.
type directStatus interface {
	PostCommitStatus(ctx context.Context, pr forge.PR, sha string, st forge.CommitState, description string) error
}

func (s *Server) ciTrigger(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	res := s.handleCITrigger(w, r)
	if res.requestID == "" {
		res.requestID = newRequestID()
	}
	w.Header().Set("X-Request-Id", res.requestID)
	writeJSON(w, res.status, map[string]string{
		"status":     http.StatusText(res.status),
		"outcome":    res.outcome,
		"request_id": res.requestID,
	})
	withRequest(s.log, res.requestID).Info("ci trigger",
		"outcome", res.outcome, "status", res.status, "duration_ms", time.Since(start).Milliseconds())
}

func (s *Server) gitlabCIEnabled() bool {
	return s.forges[forge.GitLab] != nil && s.cfg.GitLab != nil && s.cfg.GitLab.CIToken != ""
}

func (s *Server) handleCITrigger(w http.ResponseWriter, r *http.Request) webhookResult {
	if !s.gitlabCIEnabled() {
		return webhookResult{status: http.StatusNotFound, outcome: "gitlab ci trigger disabled"}
	}
	if !tokenMatches(presentedCIToken(r), s.cfg.GitLab.CIToken) {
		return webhookResult{status: http.StatusUnauthorized, outcome: "bad token"}
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
	in, reason := decodeCITrigger(body)
	if reason != "" {
		return webhookResult{status: http.StatusBadRequest, outcome: reason}
	}
	f := s.forges[forge.GitLab]
	path := in.ProjectPath
	if path == "" {
		lookup, ok := f.(projectPathLookup)
		if !ok {
			return webhookResult{status: http.StatusBadGateway, outcome: "project path lookup is not available"}
		}
		ctx, cancel := s.sideContext()
		defer cancel()
		path, err = lookup.ResolveProjectPath(ctx, in.ProjectID)
		if err != nil {
			if errors.Is(err, forge.ErrNotFound) {
				return webhookResult{status: http.StatusNotFound, outcome: "project not found"}
			}
			return webhookResult{status: http.StatusBadGateway, outcome: "resolve project: " + err.Error()}
		}
	}
	ref := forge.PRRef{Platform: forge.GitLab, Repo: path, ProjectID: in.ProjectID, Number: in.MRIID}
	if matched, rule := s.cfg.Ignore.Match(serveconfig.IgnoreSubject{Repo: ref.Repo}); matched {
		return webhookResult{status: http.StatusAccepted, outcome: "ignore rule " + rule}
	}
	requestID := newRequestID()
	// The status call stays outside intake. Holding the intake lock across
	// it would stall Shutdown, and marking the chain busy before the post
	// succeeds would make a parallel trigger see a chain that nobody runs.
	pr := withCIPipeline(forge.PR{Ref: ref}, &ciJob{sha: in.SHA, pipelineID: in.PipelineID})
	ctx, cancel := ciStatusContext(r.Context(), s.runCtx)
	defer cancel()
	if err := s.postCommitStatus(ctx, f, pr, in.SHA, forge.StatusPending, s.locales[forge.GitLab].Text("status.waiting"), withPR(withRequest(s.log, requestID), ref)); err != nil {
		return webhookResult{status: http.StatusBadGateway, requestID: requestID, outcome: "set status: " + err.Error()}
	}
	res := webhookResult{status: http.StatusAccepted, requestID: requestID}
	if !s.intake(func() {
		gen, _, started := s.startChainIfIdle(ref, 1, state.ScopeFull)
		if !started {
			res.outcome = "chain running"
			return
		}
		s.queue.submit(ref, job{
			ci:        &ciJob{gen: gen, sha: in.SHA, pipelineID: in.PipelineID, force: in.Force},
			requestID: requestID,
		})
		res.outcome = "queued ci trigger"
	}) {
		return webhookResult{status: http.StatusServiceUnavailable, requestID: requestID, outcome: "shutting down"}
	}
	return res
}

// ciStatusContext stops when the HTTP request ends, the server shuts down,
// or sideCallTimeout passes, whichever comes first.
func ciStatusContext(req, run context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(req, sideCallTimeout)
	stop := context.AfterFunc(run, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func presentedCIToken(r *http.Request) string {
	const prefix = "bearer "
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return strings.TrimSpace(r.Header.Get("X-OCR-Token"))
}

// tokenMatches compares got with want in constant time. The values are
// hashed first so a shorter header does not skip the compare.
func tokenMatches(got, want string) bool {
	if want == "" {
		return false
	}
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

func decodeCITrigger(body []byte) (ciTriggerBody, string) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var in ciTriggerBody
	if err := dec.Decode(&in); err != nil {
		return ciTriggerBody{}, "invalid json"
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return ciTriggerBody{}, "trailing json"
	}
	in.ProjectPath = strings.TrimSpace(in.ProjectPath)
	in.SHA = strings.TrimSpace(in.SHA)
	switch {
	case in.ProjectID <= 0:
		return ciTriggerBody{}, "invalid project_id"
	case in.MRIID <= 0:
		return ciTriggerBody{}, "invalid mr_iid"
	case !ciSHAPattern.MatchString(in.SHA):
		return ciTriggerBody{}, "invalid sha"
	case in.PipelineID < 0:
		return ciTriggerBody{}, "invalid pipeline_id"
	case in.ProjectPath != "" && !forge.ValidRepoPath(in.ProjectPath):
		return ciTriggerBody{}, "invalid project_path"
	}
	return in, ""
}

func withCIPipeline(pr forge.PR, ci *ciJob) forge.PR {
	if ci != nil && ci.pipelineID > 0 {
		pr.PipelineID = ci.pipelineID
		pr.PipelineSHA = ci.sha
	}
	return pr
}

func (s *Server) postCommitStatus(ctx context.Context, f forge.Forge, pr forge.PR, sha string, st forge.CommitState, text string, log *slog.Logger) error {
	var err error
	if p, ok := f.(directStatus); ok {
		err = p.PostCommitStatus(ctx, pr, sha, st, text)
	} else {
		err = f.SetStatus(ctx, pr, sha, st, text)
	}
	if err != nil && log != nil {
		log.Error("set commit status", "sha", sha, "status", string(st), "error", err.Error())
	}
	return err
}

// runCITrigger reads the merge request and either starts round 1 or stops.
// Every return before s.round ends the chain the handler started.
func (s *Server) runCITrigger(f forge.Forge, ref forge.PRRef, j job, log *slog.Logger) error {
	ci := j.ci
	stop := func(reason string, err error) error {
		if reason != "" {
			log.Info("ci trigger stopped", "reason", reason)
		}
		s.roundEnded(ref, ci.gen)
		return err
	}
	ctx, cancel := s.sideContext()
	defer cancel()
	pr, m, _, err := s.readChain(ctx, f, ref, log)
	if err != nil {
		log.Error("read pull request state", "error", err.Error())
		return stop("", err)
	}
	pr = withCIPipeline(pr, ci)
	if !s.current(ref, ci.gen) || pr.State != forge.StateOpen {
		reason := "pr not open"
		if pr.State == forge.StateOpen {
			reason = "chain replaced"
		}
		return stop(reason, nil)
	}
	if !ci.force && m != nil && m.Status == state.ChainSuccess && len(m.Rounds) > 0 && m.Rounds[len(m.Rounds)-1].To == ci.sha {
		n := m.Rounds[len(m.Rounds)-1].N
		s.setStatus(ctx, f, pr, ci.sha, forge.StatusSuccess, state.StatusText(*m, n, s.locales[ref.Platform]), log)
		return stop("already reviewed", nil)
	}
	repo, err := s.repoConfig(ctx, f, ref, pr, log)
	if err != nil {
		log.Error("read repository config", "error", err.Error())
		return stop("", err)
	}
	if !repo.Enabled {
		s.markNotRequired(ctx, f, pr, log)
		return stop("disabled for repository", nil)
	}
	return s.round(f, ref, roundJob{n: 1, scope: state.ScopeFull, gen: ci.gen}, j, log)
}
