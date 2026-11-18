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
		if ignored || strings.HasPrefix(reason, "disabled") {
			s.markNotRequired(ctx, f, pr, log)
		}
		return nil
	}
	log.Info("decided", "trigger", string(j.trigger), "action", "start chain")
	s.clearCheckpoints(ref, log)
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
		if pr.State != forge.StateOpen {
			return
		}
		if m == nil && s.notReviewed(ctx, f, ref, pr, log) {
			s.markNotRequired(ctx, f, pr, log)
			return
		}
		s.copyStatus(ctx, f, ref, pr, m, log)
	}()
}

// notReviewed reports whether the service never reviews pr: an ignore rule
// matches it or its repository turned the review off.
func (s *Server) notReviewed(ctx context.Context, f forge.Forge, ref forge.PRRef, pr forge.PR, log *slog.Logger) bool {
	if ignored, _ := s.cfg.Ignore.Match(ignoreSubject(ref, pr)); ignored {
		return true
	}
	repo, err := s.repoConfig(ctx, f, ref, pr, log)
	return err == nil && !repo.Enabled
}

// markNotRequired gives a PR that the service does not review a success
// status. On GitLab "Pipelines must succeed" is on for every project, so
// without it such a PR could never merge.
func (s *Server) markNotRequired(ctx context.Context, f forge.Forge, pr forge.PR, log *slog.Logger) {
	s.setStatus(ctx, f, pr, pr.HeadSHA, forge.StatusSuccess, s.locales[pr.Ref.Platform].Text("status.not_required"), log)
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
	s.setStatus(ctx, f, pr, pr.HeadSHA, m.CommitState(), state.StatusText(*m, n, s.locales[ref.Platform]), log)
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
	token := s.record(ref, rj, log)
	err := s.runRound(f, ref, rj, j, log)
	if !errors.Is(err, errShutdown) && !errors.Is(err, errHeadMoved) {
		if rerr := s.journal.remove(ref, token); rerr != nil {
			log.Error("update the round journal", "error", rerr.Error())
		}
		s.clearCheckpoints(ref, log)
	}
	return err
}

// record writes rj to the journal, so a restart or a crash before the round
// ends runs it again.
func (s *Server) record(ref forge.PRRef, rj roundJob, log *slog.Logger) string {
	token, err := s.journal.put(ref, rj)
	if err != nil {
		log.Error("update the round journal", "error", err.Error())
	}
	return token
}

func (s *Server) runRound(f forge.Forge, ref forge.PRRef, rj roundJob, j job, log *slog.Logger) error {
	select {
	case s.workers <- struct{}{}:
	default:
		s.markQueued(f, ref, log)
		select {
		case s.workers <- struct{}{}:
		case <-s.runCtx.Done():
			return s.failQueued(f, ref, rj, j, log)
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
	s.setStatus(ctx, f, pr, pr.HeadSHA, forge.StatusPending, s.locales[ref.Platform].Text("status.queued"), log)
	log.Info("round queued", "head", pr.HeadSHA)
}

// failQueued handles a shutdown that stops a round still waiting for a
// worker. With a journal the round runs again after the restart, so the PR
// keeps its pending status. Without one the chain is marked failed, or the
// PR would stay blocked with no message to its author.
func (s *Server) failQueued(f forge.Forge, ref forge.PRRef, rj roundJob, j job, log *slog.Logger) error {
	if s.journal != nil {
		log.Info("round cut by shutdown; it runs again after the restart", "stage", "wait for worker")
		return errShutdown
	}
	r := &roundRun{s: s, f: f, ref: ref, rj: rj, j: j, log: log,
		marker: state.Marker{Max: s.cfg.MaxRounds}}
	ctx, cancel := s.sideContext()
	defer cancel()
	if pr, m, _, err := s.readChain(ctx, f, ref, log); err != nil {
		log.Warn("read pull request for the failed status", "error", err.Error())
	} else if pr.State == forge.StateOpen {
		r.pr = pr
		if m != nil {
			r.marker.Max, r.marker.Rounds = m.Max, m.Rounds
		}
	}
	return r.fail("wait for worker", s.runCtx.Err())
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

// loc is the text of the PR's platform.
func (r *roundRun) loc() state.Locale {
	return r.s.locales[r.ref.Platform]
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
	loc := r.loc()
	r.publish(ctx, pr.HeadSHA, rj.n, loc.Text("pending.round", "round", state.RoundLabel(rj.n, r.limit, loc)), true)

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
		Background: repo.Background, Effort: repo.Effort, Excludes: repo.Exclude, ProjectRule: rule, PR: pr,
		Language: s.languages[r.ref.Platform]}
	if rj.scope == state.ScopeCommit {
		req.From, req.To, req.Commit, req.FixCommit = "", "", cmpOr(rj.commit, pr.HeadSHA), rj.fixCommit
	}
	if rj.effort != "" {
		req.Effort = rj.effort
	}
	req.Checkpoint = s.roundCheckpoints(r.ref, rj.n)
	res, err := r.review(ctx, req)
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
		Coverage: res.Coverage, Skipped: res.Terminal == review.TerminalSkipped, Branch: pr.SourceBranch,
		LineURL: func(sha, path string, line int) string { return f.LineURL(r.ref, sha, path, line) }}
	if rj.scope == state.ScopeCommit {
		out.From, out.To, out.FixReview = "", req.Commit, rj.fixCommit
	}
	if findings := roundFindings(res.Findings); len(findings) > 0 {
		out.Findings = findings
		fctx, fcancel := context.WithTimeout(round, s.cfg.FixTimeout)
		defer fcancel()
		if err := r.fix(fctx, ws, findings, report, &out); err != nil {
			return r.fail("fix", err)
		}
		ctx = fctx
	}
	return r.finish(ctx, out)
}

// review runs the review stage, or returns its result saved before a
// restart cut the round. A finished review is saved for the same reason.
func (r *roundRun) review(ctx context.Context, req review.Request) (review.Result, error) {
	head, scope := r.pr.HeadSHA, r.rj.scope
	if res, ok := loadReview(req.Checkpoint, head, scope, req.Commit); ok {
		r.log.Info("review reused from a checkpoint", "findings", len(res.Findings))
		return res, nil
	}
	res, err := r.s.reviewer.Review(ctx, req)
	if errors.Is(ctx.Err(), context.Canceled) {
		// A shutdown or a newer review stopped the batches still running;
		// what they left is no partial result. The batches that finished
		// are saved, and the round runs again after a restart. A review
		// that hit its time limit keeps its partial result.
		return res, fmt.Errorf("review stopped: %w", ctx.Err())
	}
	if err == nil && res.Terminal != review.TerminalFailed {
		sv := savedReview{Head: head, Scope: scope, Commit: req.Commit, Result: res}
		if serr := saveReview(req.Checkpoint, sv); serr != nil {
			r.log.Warn("save review checkpoint", "error", serr.Error())
		}
	}
	return res, err
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
	req := review.FixRequest{RepoDir: ws.Dir(), PR: r.pr, Base: out.From, Head: out.To, Findings: findings, ReportOnly: report != "",
		Language: s.languages[r.ref.Platform]}
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
	out.Fixed, out.Reported, out.Dropped, out.Tests = res.Fixed, res.Reported, res.Dropped, res.Tests
	r.log.Info("fix run", "findings", len(findings), "fixed", len(res.Fixed), "reported", len(res.Reported), "dropped", len(res.Dropped),
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
	// diffs stay on the fix made on r.pr.HeadSHA for the report; the push
	// shows the diffs of the commit it pushed.
	pushed, pushedOther, fix := diffs, other, sha
	pushErr := ws.Push(ctx, spec, sha)
	for try := 0; pushErr != nil && ctx.Err() == nil && branchMoved(pushErr) && try < maxRebasePushes; try++ {
		r.log.Warn("fix push rejected because the branch moved; rebasing the fix", "error", pushErr.Error(), "try", try+1)
		if spec, err = r.f.PushSpec(ctx, r.pr); err != nil {
			return err
		}
		rb, rerr := ws.Rebase(ctx, ident, spec, sha)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(rerr, review.ErrRebaseConflict) {
			r.log.Warn("fix conflicts with the moved branch", "head", rb.Head, "error", rerr.Error())
			unfixed(out, state.ReportMoved, diffs)
			out.NewHead = rb.Head
			return nil
		}
		if rerr != nil {
			// The branch cannot be read again (deleted, no access): the
			// rejection of the first push is the reason to show.
			r.log.Warn("rebase the fix", "error", rerr.Error())
			break
		}
		sha, out.NewHead = rb.Fix, rb.Head
		pushed, pushedOther = r.fixDiff(ctx, ws, sha, r.rebasedChanges(ctx, ws, fix, sha, out.Fixed))
		pushErr = ws.PushLease(ctx, spec, sha, rb.Head)
	}
	if pushErr != nil && ctx.Err() != nil {
		// A shutdown, a newer review or the fix timeout cut the push: the
		// round fails instead of reporting a refusal by the forge.
		return ctx.Err()
	}
	if pushErr != nil {
		r.log.Warn("fix push rejected", "error", pushErr.Error())
		unfixed(out, state.ReportPush, diffs)
		out.PushReason, out.NewHead = pushReason(pushErr), ""
		return nil
	}
	r.log.Info("fix pushed", "commit", sha, "branch", r.pr.SourceBranch, "rebased_onto", out.NewHead)
	out.FixCommit, out.CommitURL = sha, r.f.CommitURL(r.ref, sha)
	out.FixDiffs, out.OtherDiff = pushed, pushedOther
	if out.NewHead != "" {
		out.OldHead = r.pr.HeadSHA
	}
	return nil
}

// rebasedChanges moves the line ranges of fixed from the fix commit to the
// same fix rebased onto a moved branch, whose earlier lines may have
// shifted. A diff that cannot be read leaves the ranges as they are.
func (r *roundRun) rebasedChanges(ctx context.Context, ws review.Workspace, fix, rebased string, fixed []review.Fixed) []review.Fixed {
	from, err := ws.CommitDiff(ctx, fix)
	if err != nil {
		return fixed
	}
	to, err := ws.CommitDiff(ctx, rebased)
	if err != nil {
		return fixed
	}
	return state.ShiftChanges(from, to, fixed)
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
		rep := review.Reported{ID: f.ID, Path: f.Path, Line: f.Line, Title: f.Title, Problem: f.Problem, Scenario: f.Scenario, ProposedFix: f.Fix}
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
	if err := r.f.PostNote(ctx, r.ref, state.RenderRound(out, r.loc())); err != nil {
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
		nj := roundJob{n: next, scope: state.ScopeCommit, commit: out.FixCommit, fixCommit: true, gen: rj.gen, effort: rj.effort}
		s.record(r.ref, nj, r.log)
		s.queue.submit(r.ref, job{round: &nj, requestID: r.j.requestID})
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
	if r.s.runCtx.Err() != nil && r.s.journal != nil {
		r.log.Info("round cut by shutdown; it runs again after the restart", "stage", stage)
		return errShutdown
	}
	r.log.Error("review round failed", "stage", stage, "error", err.Error())
	side, cancel := r.s.sideContext()
	defer cancel()
	if r.pr.HeadSHA != "" {
		r.marker.Status = state.ChainFailure
		r.publish(side, r.pr.HeadSHA, r.rj.n, r.loc().Text("pending.failed"), false)
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
	if err := f.UpsertSummary(ctx, r.ref, state.RenderSummary(r.marker, r.loc(), line, s.mention(r.ref.Platform))); err != nil {
		r.log.Error("write summary", "error", err.Error())
	}
	text := state.StatusText(r.marker, round, r.loc())
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

// maxRebasePushes bounds how often a fix is rebased and pushed again after
// the branch moved.
const maxRebasePushes = 2

// branchMoved tells a push that git refused because the remote branch is no
// longer where the checkout started (non-fast-forward, fetch first, stale
// lease) from one the forge declined (permissions, protected branch, hooks),
// which git reports as "remote rejected".
func branchMoved(err error) bool {
	msg := err.Error()
	if strings.Contains(msg, "remote rejected") {
		return false
	}
	for _, m := range []string{"fetch first", "non-fast-forward", "stale info"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

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
