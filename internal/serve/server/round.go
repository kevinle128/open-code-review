// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// roundJob is one round of a chain. A commit round reviews commit, or the
// PR head when commit is empty; fixCommit marks commit as the fix the
// previous round pushed. gen is the chain the round belongs to; effort is
// the override the command that started the chain gave.
type roundJob struct {
	n         int
	scope     string
	commit    string
	fixCommit bool
	gen       uint64
	effort    string
}

// chainCtl tracks the chain of one PR: gen counts the chains started, and
// cancel stops the round that runs. status serializes the marker and commit
// status writes of a round against a status copy for a new head.
type chainCtl struct {
	gen    uint64
	cancel context.CancelFunc
	status sync.Mutex
}

func (s *Server) chain(ref forge.PRRef) *chainCtl {
	s.chainMu.Lock()
	defer s.chainMu.Unlock()
	c := s.chains[ref.Key()]
	if c == nil {
		c = &chainCtl{}
		s.chains[ref.Key()] = c
	}
	return c
}

// restartChain starts a new chain for the PR: the round that runs is
// cancelled, and every round of the old chain still queued is skipped.
func (s *Server) restartChain(ref forge.PRRef) uint64 {
	c := s.chain(ref)
	s.chainMu.Lock()
	defer s.chainMu.Unlock()
	c.gen++
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	return c.gen
}

// current reports whether gen is still the chain of the PR.
func (s *Server) current(ref forge.PRRef, gen uint64) bool {
	c := s.chain(ref)
	s.chainMu.Lock()
	defer s.chainMu.Unlock()
	return c.gen == gen
}

// begin records cancel as the running round of chain gen; it is false when
// a newer chain replaced gen.
func (s *Server) begin(ref forge.PRRef, gen uint64, cancel context.CancelFunc) bool {
	c := s.chain(ref)
	s.chainMu.Lock()
	defer s.chainMu.Unlock()
	if c.gen != gen {
		return false
	}
	c.cancel = cancel
	return true
}

func (s *Server) end(ref forge.PRRef, gen uint64) {
	c := s.chain(ref)
	s.chainMu.Lock()
	defer s.chainMu.Unlock()
	if c.gen == gen {
		c.cancel = nil
	}
}

// forgetChain stops the chain of a closed PR. The entry stays with a new
// generation, so a round of the old chain still queued cannot pass as a
// round of a chain started after the PR is reopened.
func (s *Server) forgetChain(ref forge.PRRef) {
	s.restartChain(ref)
}

// evaluate handles an opened or ready PR: it starts round 1 on the whole PR
// when no chain ran yet. A PR that already has a chain, for example one
// marked draft and ready again, keeps it and only gets its status copied.
func (s *Server) evaluate(f forge.Forge, ref forge.PRRef, j job, log *slog.Logger) error {
	ctx, cancel := s.sideContext()
	defer cancel()
	pr, m, present, err := s.readChain(ctx, f, ref, log)
	if err != nil {
		log.Error("read pull request state", "error", err.Error())
		return err
	}
	ignored, rule := s.cfg.Ignore.Match(ignoreSubject(ref, pr))
	var reason string
	switch {
	case pr.State != forge.StateOpen:
		reason = "pr not open"
	case pr.Draft:
		reason = "draft"
	case ignored:
		reason = "ignore rule " + rule
	case m != nil || present:
		s.copyStatus(ctx, f, ref, pr, m, log)
		reason = "chain exists"
	}
	if reason == "" {
		repo, err := s.repoConfig(ctx, f, ref, pr, log)
		if err != nil {
			log.Error("read repository config", "error", err.Error())
			return err
		}
		if !repo.Enabled {
			reason = "disabled for repository"
		}
	}
	if reason != "" {
		log.Info("decided", "trigger", string(j.trigger), "action", "none", "reason", reason)
		return nil
	}
	log.Info("decided", "trigger", string(j.trigger), "action", "start chain")
	return s.round(f, ref, roundJob{n: 1, scope: state.ScopeFull, gen: s.restartChain(ref)}, j, log)
}

// readChain reads the PR and its chain marker. present is true when a
// summary exists, even one whose marker cannot be read; m is nil then.
func (s *Server) readChain(ctx context.Context, f forge.Forge, ref forge.PRRef, log *slog.Logger) (forge.PR, *state.Marker, bool, error) {
	pr, err := f.GetPR(ctx, ref)
	if err != nil {
		return pr, nil, false, fmt.Errorf("get pull request: %w", err)
	}
	// The event ref carries ids that only the webhook knows, such as the
	// GitHub installation id that the API calls need for a token.
	pr.Ref = ref
	summary, err := f.GetSummary(ctx, ref)
	if err != nil {
		return pr, nil, false, fmt.Errorf("get summary: %w", err)
	}
	if summary == nil {
		return pr, nil, false, nil
	}
	m, err := state.Decode(summary.Body)
	if err != nil {
		log.Error("summary marker unreadable; the next review rewrites it", "error", err.Error())
	}
	return pr, m, true, nil
}

// copyStatusAsync gives a new head the status of the chain, so a push by a
// person, which starts no round, neither unblocks nor blocks the merge on
// its own. It runs at once, beside a round that may be running.
func (s *Server) copyStatusAsync(ref forge.PRRef, requestID string) {
	f := s.forges[ref.Platform]
	log := withPR(withRequest(s.log, requestID), ref)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := s.sideContext()
		defer cancel()
		c := s.chain(ref)
		c.status.Lock()
		defer c.status.Unlock()
		pr, m, _, err := s.readChain(ctx, f, ref, log)
		if err != nil {
			log.Error("read pull request for a status copy", "error", err.Error())
			return
		}
		if pr.State == forge.StateOpen {
			s.copyStatus(ctx, f, ref, pr, m, log)
		}
	}()
}

// copyStatus sets the chain status on the PR head; without a chain there
// is nothing to copy.
func (s *Server) copyStatus(ctx context.Context, f forge.Forge, ref forge.PRRef, pr forge.PR, m *state.Marker, log *slog.Logger) {
	if m == nil {
		return
	}
	n := 0
	if len(m.Rounds) > 0 {
		n = m.Rounds[len(m.Rounds)-1].N
	}
	s.setStatus(ctx, f, pr, pr.HeadSHA, m.CommitState(), state.StatusText(*m, n, s.loc), log)
	log.Info("status copied", "head", pr.HeadSHA, "status", m.Status)
}

func (s *Server) setStatus(ctx context.Context, f forge.Forge, pr forge.PR, sha string, st forge.CommitState, text string, log *slog.Logger) {
	if err := f.SetStatus(ctx, pr, sha, st, text); err != nil {
		log.Error("set commit status", "sha", sha, "status", string(st), "error", err.Error())
	}
}

// round runs one round of a chain: review, fix run, commit and push, one
// PR comment, and the chain state in the summary and the commit status. A
// round that pushes a fix queues the next round on the fix commit. Time
// spent waiting for a free worker does not count toward the round timeout.
func (s *Server) round(f forge.Forge, ref forge.PRRef, rj roundJob, j job, log *slog.Logger) error {
	log = log.With("round", rj.n, "scope", rj.scope)
	if !s.current(ref, rj.gen) {
		log.Info("round skipped: a newer review replaced its chain")
		return nil
	}
	select {
	case s.workers <- struct{}{}:
	default:
		s.markQueued(f, ref, log)
		select {
		case s.workers <- struct{}{}:
		case <-s.runCtx.Done():
			log.Error("review round failed", "stage", "wait for worker", "error", s.runCtx.Err().Error())
			s.commandFailed(f, ref, j.cmd, s.failureReason("wait for worker", j.requestID), log)
			return s.runCtx.Err()
		}
	}
	defer func() { <-s.workers }()
	// The round context has no deadline of its own: run gives the review
	// and the fix each their limit. cancel stops the round when a newer
	// review replaces its chain.
	ctx, cancel := context.WithCancel(s.runCtx)
	defer cancel()
	if !s.begin(ref, rj.gen, cancel) {
		log.Info("round skipped: a newer review replaced its chain")
		return nil
	}
	defer s.end(ref, rj.gen)
	r := &roundRun{s: s, f: f, ref: ref, rj: rj, j: j, log: log}
	return r.run(ctx)
}

// markQueued sets a pending status on the PR head while the round waits
// for a free worker. Without it a GitLab project whose CI pipeline passes
// first could merge before the review starts, because "Pipelines must
// succeed" sees no ocr-review status yet.
func (s *Server) markQueued(f forge.Forge, ref forge.PRRef, log *slog.Logger) {
	ctx, cancel := s.sideContext()
	defer cancel()
	c := s.chain(ref)
	c.status.Lock()
	defer c.status.Unlock()
	pr, err := f.GetPR(ctx, ref)
	if err != nil {
		log.Warn("read pull request for the queued status", "error", err.Error())
		return
	}
	if pr.State != forge.StateOpen {
		return
	}
	pr.Ref = ref
	s.setStatus(ctx, f, pr, pr.HeadSHA, forge.StatusPending, s.loc.Text("status.queued"), log)
	log.Info("round queued", "head", pr.HeadSHA)
}

// roundRun holds the state of one round while it runs.
type roundRun struct {
	s   *Server
	f   forge.Forge
	ref forge.PRRef
	rj  roundJob
	j   job
	log *slog.Logger

	pr     forge.PR
	marker state.Marker
	limit  int
}

func (r *roundRun) run(round context.Context) error {
	s, f, rj := r.s, r.f, r.rj
	ctx, cancel := context.WithTimeout(round, s.cfg.ReviewTimeout)
	defer cancel()
	pr, m, _, err := s.readChain(ctx, f, r.ref, r.log)
	if err != nil {
		return r.fail("read", err)
	}
	r.pr = pr
	r.marker = state.Marker{Status: state.ChainRunning, Max: s.cfg.MaxRounds}
	if m != nil {
		r.marker.Max = m.Max
		if rj.n > 1 {
			r.marker.Rounds = m.Rounds
		}
	}
	if pr.State != forge.StateOpen {
		r.log.Info("round skipped: pr not open")
		s.commandFailed(f, r.ref, r.j.cmd, "pr not open", r.log)
		return nil
	}
	repo, err := s.repoConfig(ctx, f, r.ref, pr, r.log)
	if err != nil {
		return r.fail("read", err)
	}
	rule, err := s.projectRule(ctx, f, r.ref, pr, r.log)
	if err != nil {
		return r.fail("read", err)
	}
	r.limit = repo.MaxRounds
	r.marker.Max = r.limit
	r.publish(ctx, pr.HeadSHA, rj.n, s.loc.Text("pending.round", "round", state.RoundLabel(rj.n, r.limit, s.loc)), true)

	report := ""
	switch {
	case rj.n > r.limit:
		report = state.ReportLimit
	case s.fixer == nil:
		report = state.ReportEngine
	case pr.Fork:
		report = state.ReportFork
	}
	ws, release, err := s.workspaces.Acquire(ctx)
	if err != nil {
		return r.fail("workspace", err)
	}
	defer release()
	if report == "" {
		report, err = r.checkoutBranch(ctx, ws)
	} else {
		err = s.fetch(ctx, f, r.ref, pr, ws)
	}
	if errors.Is(err, errHeadMoved) {
		return err
	}
	if err != nil {
		return r.fail("checkout", err)
	}

	req := review.Request{RepoDir: ws.Dir(), From: pr.BaseSHA, To: pr.HeadSHA,
		Background: repo.Background, Effort: repo.Effort, Excludes: repo.Exclude, ProjectRule: rule, PR: pr}
	if rj.scope == state.ScopeCommit {
		req.From, req.To, req.Commit, req.FixCommit = "", "", cmpOr(rj.commit, pr.HeadSHA), rj.fixCommit
	}
	if rj.effort != "" {
		req.Effort = rj.effort
	}
	res, err := s.reviewer.Review(ctx, req)
	if len(res.Warnings) > 0 {
		warnings := make([]string, len(res.Warnings))
		for i, w := range res.Warnings {
			warnings[i] = truncate(w, maxLoggedReason)
		}
		r.log.Warn("review warnings", "terminal", res.Terminal, "warnings", warnings)
	}
	logCoverage(r.log, res)
	if err == nil && res.Terminal == review.TerminalFailed {
		err = errors.New("review ended in the failed state")
	}
	if err != nil {
		return r.fail("review", err)
	}

	out := state.RoundReport{N: rj.n, Max: r.limit, From: cmpOr(res.Base, pr.BaseSHA), To: pr.HeadSHA,
		Coverage: res.Coverage, Skipped: res.Terminal == review.TerminalSkipped, Branch: pr.SourceBranch}
	if rj.scope == state.ScopeCommit {
		out.From, out.To = "", req.Commit
	}
	if findings := roundFindings(res.Findings); len(findings) > 0 {
		fctx, fcancel := context.WithTimeout(round, s.cfg.FixTimeout)
		defer fcancel()
		if err := r.fix(fctx, ws, findings, report, &out); err != nil {
			return r.fail("fix", err)
		}
		ctx = fctx
	}
	return r.finish(ctx, out)
}

// checkoutBranch checks out the head branch for a fix round. A fork found
// only now turns the round report-only.
func (r *roundRun) checkoutBranch(ctx context.Context, ws review.Workspace) (string, error) {
	spec, err := r.f.PushSpec(ctx, r.pr)
	if errors.Is(err, forge.ErrFork) {
		return state.ReportFork, r.s.fetch(ctx, r.f, r.ref, r.pr, ws)
	}
	if err != nil {
		return "", err
	}
	if ferr := ws.FetchBranch(ctx, spec, r.pr.HeadSHA, r.pr.BaseSHA); ferr != nil {
		if cur, err := r.f.GetPR(ctx, r.ref); err == nil && cur.HeadSHA != r.pr.HeadSHA {
			return "", fmt.Errorf("%w: %s -> %s", errHeadMoved, r.pr.HeadSHA, cur.HeadSHA)
		}
		return "", ferr
	}
	return "", nil
}

// fix runs the fix run on the findings and, in a fix round, commits and
// pushes what it changed. Without a fixer every finding is reported as the
// review gave it. A fix that cannot be committed or pushed turns into a
// report of the same findings.
func (r *roundRun) fix(ctx context.Context, ws review.Workspace, findings []review.Finding, report string, out *state.RoundReport) error {
	s := r.s
	out.Report = report
	if s.fixer == nil {
		for _, fd := range findings {
			out.Reported = append(out.Reported, review.Reported{ID: fd.ID, Path: fd.Path, Line: fd.Line, Problem: fd.Content})
		}
		return nil
	}
	req := review.FixRequest{RepoDir: ws.Dir(), PR: r.pr, Base: out.From, Head: out.To, Findings: findings, ReportOnly: report != ""}
	if report == "" {
		files, err := ws.ChangedFiles(ctx, r.pr.BaseSHA)
		if err != nil {
			return err
		}
		req.Files = files
	}
	res, err := s.fixer.Fix(ctx, req)
	if len(res.Warnings) > 0 {
		r.log.Warn("fix warnings", "warnings", res.Warnings)
	}
	if err != nil {
		return err
	}
	out.Fixed, out.Reported, out.Tests = res.Fixed, res.Reported, res.Tests
	r.log.Info("fix run", "findings", len(findings), "fixed", len(res.Fixed), "reported", len(res.Reported),
		"tests", res.Tests.Command, "tests_passed", res.Tests.Passed)
	if report != "" || len(res.Fixed) == 0 {
		return nil
	}
	ident, err := r.f.CommitIdentity(ctx, r.ref)
	if err != nil {
		return err
	}
	sha, changed, err := ws.Commit(ctx, ident, res.CommitMessage)
	if err != nil {
		return err
	}
	if !changed {
		r.log.Warn("fix run reported fixes but changed no file")
		unfixed(out, state.ReportNoEdit, nil)
		return nil
	}
	diffs, other := r.fixDiff(ctx, ws, sha, out.Fixed)
	// A fresh token: the one used for the fetch can expire during a long run.
	spec, err := r.f.PushSpec(ctx, r.pr)
	if err != nil {
		return err
	}
	if err := ws.Push(ctx, spec, sha); err != nil {
		r.log.Warn("fix push rejected", "error", err.Error())
		unfixed(out, state.ReportPush, diffs)
		out.PushReason = pushReason(err)
		return nil
	}
	r.log.Info("fix pushed", "commit", sha, "branch", r.pr.SourceBranch)
	out.FixCommit, out.CommitURL = sha, r.f.CommitURL(r.ref, sha)
	out.FixDiffs, out.OtherDiff = diffs, other
	return nil
}

// fixDiff splits the diff of the fix commit among the fixed findings. A
// diff that cannot be read leaves the comment with the fixes as prose.
func (r *roundRun) fixDiff(ctx context.Context, ws review.Workspace, sha string, fixed []review.Fixed) ([]string, string) {
	diff, err := ws.CommitDiff(ctx, sha)
	if err != nil {
		r.log.Warn("read the fix commit diff; the round comment shows the fixes as prose", "commit", sha, "error", err.Error())
		return nil, ""
	}
	diffs, other, unmatched := state.SplitDiff(diff, fixed)
	if len(unmatched) > 0 {
		r.log.Warn("fixed findings match no hunk of the fix commit; the round comment shows them as prose", "commit", sha, "ids", unmatched)
	}
	return diffs, other
}

// unfixed moves the fixes that did not reach the branch to the report, with
// each fix as the proposal; diffs holds the part of the unpushed fix commit
// of each fix, by index, or is nil.
func unfixed(out *state.RoundReport, reason string, diffs []string) {
	for i, f := range out.Fixed {
		rep := review.Reported{ID: f.ID, Path: f.Path, Line: f.Line, Problem: f.Problem, Scenario: f.Scenario, ProposedFix: f.Fix}
		if i < len(diffs) {
			rep.ProposedDiff = diffs[i]
		}
		out.Reported = append(out.Reported, rep)
	}
	out.Fixed, out.Tests, out.Report = nil, review.Tests{}, reason
}

// finish posts the round comment and records the round. The chain goes on
// when the round pushed a fix and the chain is still current; it ends
// otherwise.
func (r *roundRun) finish(ctx context.Context, out state.RoundReport) error {
	s, rj := r.s, r.rj
	next := 0
	stale := !s.current(r.ref, rj.gen)
	if out.FixCommit != "" && !stale {
		next = rj.n + 1
	}
	out.Next = next
	if err := r.f.PostNote(ctx, r.ref, state.RenderRound(out, s.loc)); err != nil {
		r.log.Error("post round comment", "error", err.Error())
	}
	if stale {
		r.log.Info("round done after a newer review replaced its chain; the chain state is left to that review", "fix_commit", out.FixCommit)
		return nil
	}
	r.marker.Rounds = append(r.marker.Rounds, state.Round{N: rj.n, Scope: rj.scope, From: out.From, To: out.To,
		Real: len(out.Fixed) + len(out.Reported), Fixed: len(out.Fixed), Fix: out.FixCommit})
	if next > 0 {
		r.publish(ctx, out.FixCommit, next, "", false)
		s.queue.submit(r.ref, job{round: &roundJob{n: next, scope: state.ScopeCommit, commit: out.FixCommit,
			fixCommit: true, gen: rj.gen, effort: rj.effort}, requestID: r.j.requestID})
	} else {
		r.marker.Status = state.ChainSuccess
		r.publish(ctx, r.pr.HeadSHA, rj.n, "", false)
	}
	r.log.Info("round done", "from", out.From, "to", out.To, "fixed", len(out.Fixed), "reported", len(out.Reported),
		"fix_commit", out.FixCommit, "next", next, "status", r.marker.Status)
	if rj.n == 1 {
		s.commandDone(r.f, r.ref, r.j.cmd, r.log)
	}
	return nil
}

// fail ends the chain with a failed round: merge stays blocked until a new
// review succeeds. A round cancelled because a newer review replaced its
// chain fails quietly, since that review now owns the PR. The forge calls
// use a fresh deadline because ctx may be the one that expired.
func (r *roundRun) fail(stage string, err error) error {
	if !r.s.current(r.ref, r.rj.gen) {
		r.log.Info("round cancelled: a newer review replaced its chain", "stage", stage)
		return nil
	}
	r.log.Error("review round failed", "stage", stage, "error", err.Error())
	side, cancel := r.s.sideContext()
	defer cancel()
	if r.pr.HeadSHA != "" {
		r.marker.Status = state.ChainFailure
		r.publish(side, r.pr.HeadSHA, r.rj.n, r.s.loc.Text("pending.failed"), false)
	}
	r.s.commandFailed(r.f, r.ref, r.j.cmd, r.s.failureReason(stage, r.j.requestID), r.log)
	return err
}

// publish writes the marker to the summary and the chain status to sha.
// At the end of a chain it also sets the status of the current head, which
// a push by a person may have moved while the round ran. A failed summary
// write is logged: the commit status, not the summary, gates the merge.
func (r *roundRun) publish(ctx context.Context, sha string, round int, line string, first bool) {
	s, f := r.s, r.f
	c := s.chain(r.ref)
	c.status.Lock()
	defer c.status.Unlock()
	if err := f.UpsertSummary(ctx, r.ref, state.RenderSummary(r.marker, s.loc, line, s.mention(r.ref.Platform))); err != nil {
		r.log.Error("write summary", "error", err.Error())
	}
	text := state.StatusText(r.marker, round, s.loc)
	st := r.marker.CommitState()
	s.setStatus(ctx, f, r.pr, sha, st, text, r.log)
	if first || r.marker.Status == state.ChainRunning {
		return
	}
	if cur, err := f.GetPR(ctx, r.ref); err != nil {
		r.log.Warn("read the head after the chain ended", "error", err.Error())
	} else if cur.HeadSHA != sha {
		s.setStatus(ctx, f, r.pr, cur.HeadSHA, st, text, r.log)
	}
}

// roundFindings gives each review finding an id for the fix run.
func roundFindings(cs []model.LlmComment) []review.Finding {
	out := make([]review.Finding, len(cs))
	for i, c := range cs {
		out[i] = review.Finding{ID: "F" + strconv.Itoa(i+1), Path: c.Path, Line: c.StartLine, EndLine: c.EndLine,
			Severity: c.Severity, Category: c.Category, Content: c.Content}
	}
	return out
}

const maxPushReason = 300

// pushReason keeps the git text after the exit status, which says why the
// forge refused (protected branch, missing permission, branch moved). The
// push URL in it carries no credential.
func pushReason(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "exit status "); i >= 0 {
		if j := strings.Index(msg[i:], ": "); j >= 0 {
			msg = msg[i+j+2:]
		}
	}
	return truncate(strings.Join(strings.Fields(msg), " "), maxPushReason)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
