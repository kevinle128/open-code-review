// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/alibaba/open-code-review/internal/config/rules"
	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// errHeadMoved asks the queue to evaluate the PR again: HEAD changed while
// the checkout was fetched, so the review would cover the wrong commit.
var errHeadMoved = errors.New("head moved during checkout")

// prState is what one evaluation read from the forge.
type prState struct {
	pr      forge.PR
	marker  *state.Marker
	present bool
	open    int
	openIDs map[string]bool
	repo    serveconfig.RepoConfig
	rule    *rules.ProjectRule
}

// process runs one job and reports whether the queue should run it again.
func (s *Server) process(ref forge.PRRef, j job) bool {
	log := withPR(withRequest(s.log, j.requestID), ref)
	f := s.forges[ref.Platform]
	ctx, cancel := context.WithTimeout(s.runCtx, s.cfg.ReviewTimeout)
	defer cancel()
	if j.cmd != nil && j.cmd.reject != "" {
		log.Info("command rejected", "reason", j.cmd.reject)
		s.commandFailed(f, ref, j.cmd, j.cmd.reject, log)
		return false
	}
	err := s.evaluate(ctx, f, ref, j, log)
	if errors.Is(err, errHeadMoved) {
		log.Info("head moved, evaluating again")
		return true
	}
	return false
}

func (s *Server) evaluate(ctx context.Context, f forge.Forge, ref forge.PRRef, j job, log *slog.Logger) error {
	st, err := s.read(ctx, f, ref, log)
	if err != nil {
		log.Error("read pull request state", "error", err.Error())
		if errors.Is(err, forge.ErrNotFound) {
			s.unwait(ref, log)
		}
		s.commandFailed(f, ref, j.cmd, s.failureReason("read", j.requestID), log)
		return err
	}
	trigger := j.trigger
	if j.cmd != nil {
		trigger = j.cmd.trigger()
	}
	// A command the platform raised (a reviewer assignment) follows the
	// draft rule of automatic reviews; only a typed command runs on a draft.
	if j.cmd != nil && j.cmd.commentID == "" && st.pr.Draft {
		log.Info("decided", "trigger", string(trigger), "action", state.None.String(), "reason", state.ReasonDraft)
		s.unwait(ref, log)
		return nil
	}
	ignored, rule := s.cfg.Ignore.Match(ignoreSubject(ref, st.pr))
	a := state.Decide(state.Input{
		Trigger: trigger, PR: st.pr, SummaryPresent: st.present, Marker: st.marker,
		OpenThreads: st.open, Repo: st.repo, Ignored: ignored,
	})
	log.Info("decided", "trigger", string(trigger), "action", a.Kind.String(), "round", a.Round,
		"reason", a.Reason, "open_threads", st.open, "head", st.pr.HeadSHA, "ignore_rule", rule)

	switch a.Kind {
	case state.None:
		if j.cmd != nil {
			s.commandFailed(f, ref, j.cmd, a.Reason, log)
		}
		if st.open == 0 || st.pr.State != forge.StateOpen || stopsPolling(a.Reason) {
			s.unwait(ref, log)
		}
		return nil
	case state.Wait:
		s.wait(ref, log)
		return nil
	case state.NoteLimit:
		return s.noteLimit(ctx, f, ref, st, a, log)
	default:
		rule, err := s.projectRule(ctx, f, ref, st.pr, log)
		if err != nil {
			log.Error("read project rules", "error", err.Error())
			s.commandFailed(f, ref, j.cmd, s.failureReason("read", j.requestID), log)
			return err
		}
		st.rule = rule
		return s.run(f, ref, st, a, j, log)
	}
}

// stopsPolling reports whether a None reason holds until a webhook says the
// merge request changed (marked ready, new push, new note, manual review), so
// polling it before then only spends API calls.
func stopsPolling(reason string) bool {
	switch reason {
	case state.ReasonDraft, state.ReasonDisabled, state.ReasonIgnored, state.ReasonMarkerUnreadable:
		return true
	}
	return false
}

func (s *Server) read(ctx context.Context, f forge.Forge, ref forge.PRRef, log *slog.Logger) (prState, error) {
	var st prState
	var err error
	if st.pr, err = f.GetPR(ctx, ref); err != nil {
		return st, fmt.Errorf("get pull request: %w", err)
	}
	// The event ref carries ids that only the webhook knows, such as the
	// GitHub installation id that Clone and PostFindings need for a token.
	st.pr.Ref = ref
	summary, err := f.GetSummary(ctx, ref)
	if err != nil {
		return st, fmt.Errorf("get summary: %w", err)
	}
	if summary != nil {
		st.present = true
		m, derr := state.Decode(summary.Body)
		switch {
		case derr != nil:
			log.Error("summary marker unreadable; automatic rounds stop until a manual review rewrites it", "error", derr.Error())
		case m == nil:
			log.Error("summary has no marker; automatic rounds stop until a manual review rewrites it")
		}
		st.marker = m
	}
	threads, err := f.ListOCRThreads(ctx, ref)
	if err != nil {
		return st, fmt.Errorf("list threads: %w", err)
	}
	st.open, st.openIDs = countOpen(threads), openFindingIDs(threads)
	if st.repo, err = s.repoConfig(ctx, f, ref, st.pr, log); err != nil {
		return st, err
	}
	return st, nil
}

// repoConfig reads serve.json at the base SHA, never the PR head, so a PR
// cannot change its own limits. A missing or invalid file falls back to the
// defaults. A read that fails returns an error instead: the defaults would
// turn review on for a repository that set "enabled": false.
func (s *Server) repoConfig(ctx context.Context, f forge.Forge, ref forge.PRRef, pr forge.PR, log *slog.Logger) (serveconfig.RepoConfig, error) {
	base := s.cfg.RepoDefaults()
	raw, found, err := f.ReadFile(ctx, ref, pr.BaseSHA, serveconfig.RepoConfigPath, serveconfig.MaxRepoConfigBytes)
	if err != nil {
		return base, fmt.Errorf("read repository config: %w", err)
	}
	if !found {
		return base, nil
	}
	out, ignoredKeys, err := serveconfig.MergeRepo(base, raw)
	if len(ignoredKeys) > 0 {
		log.Warn("repository config keys ignored", "keys", ignoredKeys)
	}
	if err != nil {
		log.Error("repository config invalid; using defaults", "error", err.Error())
	}
	return out, nil
}

func (s *Server) noteLimit(ctx context.Context, f forge.Forge, ref forge.PRRef, st prState, a state.Action, log *slog.Logger) error {
	limit := st.repo.MaxRounds
	if err := f.PostNote(ctx, ref, state.RenderLimitNote(limit, s.loc)); err != nil {
		log.Error("post limit note", "error", err.Error())
		return err
	}
	next := state.Apply(st.marker, a, "", "", "", nil, limit)
	if err := f.UpsertSummary(ctx, ref, state.RenderSummary(next, st.open, s.loc, "")); err != nil {
		log.Error("record limit note", "error", err.Error())
		return err
	}
	s.unwait(ref, log)
	return nil
}

// run performs a review. The summary is written first with a pending line
// and the old marker, and last with the new marker; that last write is the
// commit point, so a run that fails anywhere before it consumes no round.
//
// Time spent waiting for a free worker does not count toward the review
// timeout, so a PR queued behind long reviews still gets its full budget.
func (s *Server) run(f forge.Forge, ref forge.PRRef, st prState, a state.Action, j job, log *slog.Logger) error {
	select {
	case s.workers <- struct{}{}:
		defer func() { <-s.workers }()
	case <-s.runCtx.Done():
		return s.runFailed(f, ref, st, a, j, "wait for worker", s.runCtx.Err(), false, log)
	}
	ctx, cancel := context.WithTimeout(s.runCtx, s.cfg.ReviewTimeout)
	defer cancel()

	limit := st.repo.MaxRounds
	before := state.Initial(limit)
	if st.marker != nil {
		before = *st.marker
	}
	pending := s.loc.Text("pending.manual")
	if a.Kind == state.RunAuto {
		pending = s.loc.Text("pending.round", "round", strconv.Itoa(a.Round), "max", strconv.Itoa(limit))
	}
	if err := f.UpsertSummary(ctx, ref, state.RenderSummary(before, st.open, s.loc, pending)); err != nil {
		return s.runFailed(f, ref, st, a, j, "summary", err, false, log)
	}

	ws, release, err := s.workspaces.Acquire(ctx)
	if err != nil {
		return s.runFailed(f, ref, st, a, j, "workspace", err, true, log)
	}
	defer release()
	if err := s.fetch(ctx, f, ref, st.pr, ws); err != nil {
		if errors.Is(err, errHeadMoved) {
			s.clearPending(f, ref, before, st.open, log)
			return err
		}
		return s.runFailed(f, ref, st, a, j, "checkout", err, true, log)
	}

	ancestry := state.AncestryYes
	if a.Kind == state.RunAuto && a.Mode == state.ModeIncremental {
		ancestry = ws.IsAncestor(ctx, a.From, st.pr.HeadSHA)
	}
	from, to, kind := state.ResolveRange(a, st.pr.BaseSHA, st.pr.HeadSHA, ancestry)
	req := review.Request{RepoDir: ws.Dir(), From: from, To: to,
		Background: st.repo.Background, Effort: st.repo.Effort, Excludes: st.repo.Exclude,
		ProjectRule: st.rule, PR: st.pr}
	if a.Kind == state.RunManualCommit {
		req.From, req.To, req.Commit = "", "", st.pr.HeadSHA
	}
	if j.cmd != nil && j.cmd.Effort != "" {
		req.Effort = j.cmd.Effort
	}
	res, err := s.reviewer.Review(ctx, req)
	// Logged before a failed run returns, so its cause is in the log too.
	if len(res.Warnings) > 0 {
		warnings := make([]string, len(res.Warnings))
		for i, w := range res.Warnings {
			warnings[i] = truncate(w, maxLoggedReason)
		}
		log.Warn("review warnings", "terminal", res.Terminal, "warnings", warnings)
	}
	logCoverage(log, res)
	if err == nil && res.Terminal == review.TerminalFailed {
		err = errors.New("review ended in the failed state")
	}
	if err != nil {
		return s.runFailed(f, ref, st, a, j, "review", err, true, log)
	}
	// A full review diffs against the merge base, which differs from the
	// base branch tip once that branch moves on.
	if res.Base != "" && (kind == state.KindFull || kind == state.KindManualFull) {
		from = res.Base
	}

	findings := s.findings(a, st.pr.HeadSHA, res.Findings)
	render := func(fd forge.Finding, p forge.Placement) string {
		return state.RenderFinding(fd, p, ref.Platform, s.loc)
	}
	posted, err := f.PostFindings(ctx, st.pr, findings, render)
	if err != nil {
		return s.runFailed(f, ref, st, a, j, stagePost, err, true, log)
	}
	// Without a fresh list, the ids open before the run are what the marker
	// keeps; the new ids sit on the newest run, which keeps all of them.
	open, openIDs := st.open+len(findings), st.openIDs
	if threads, err := f.ListOCRThreads(ctx, ref); err != nil {
		log.Warn("count open threads after posting; using an estimate", "error", err.Error())
	} else {
		open, openIDs = countOpen(threads), openFindingIDs(threads)
	}

	ids := make([]string, len(findings))
	for i, fd := range findings {
		ids[i] = fd.ID
	}
	next := state.Apply(st.marker, a, from, to, st.pr.BaseSHA, ids, limit)
	line := state.RenderCoverage(res.Coverage, s.loc)
	if res.Terminal == review.TerminalSkipped {
		line = s.loc.Text("summary.skipped")
		if a.Kind == state.RunAuto {
			next = state.ApplySkipped(st.marker, to, st.pr.BaseSHA, limit)
		}
	}
	next = state.PruneIDs(next, openIDs)
	if err := f.UpsertSummary(ctx, ref, state.RenderSummary(next, open, s.loc, line)); err != nil {
		return s.runFailed(f, ref, st, a, j, stageCommit, err, true, log)
	}
	if open > 0 {
		s.wait(ref, log)
	} else {
		s.unwait(ref, log)
	}
	log.Info("review done", "kind", kind, "from", from, "to", to, "terminal", res.Terminal,
		"files", res.Coverage.Total, "reviewed_files", res.Coverage.Reviewed, "skipped_files", res.Coverage.Skipped,
		"findings", len(findings),
		"posted", posted.Posted, "relocated", posted.Relocated, "file_level", posted.FileLevel, "general", posted.General, "skipped", posted.Skipped, "open_threads", open)
	s.commandDone(f, ref, j.cmd, len(findings), log)
	return nil
}

// maxLoggedReason bounds each warning and failed file's error text in the
// log; a large PR can fail many files with the same long provider error.
const maxLoggedReason = 300

// logCoverage names each file a run could not review, with its failure
// class and error text, so a partial or failed run can be explained.
func logCoverage(log *slog.Logger, res review.Result) {
	c := res.Coverage
	if len(c.Failed) == 0 {
		return
	}
	items := make([]string, len(c.Failed))
	for i, it := range c.Failed {
		name := it.Path
		if it.OldPath != "" && it.OldPath != it.Path {
			name = it.OldPath + " -> " + it.Path
		}
		items[i] = name + " (" + it.Class + "): " + truncate(it.Reason, maxLoggedReason)
	}
	log.Warn("review coverage incomplete", "terminal", res.Terminal, "files", c.Total,
		"reviewed_files", c.Reviewed, "failed_files", len(c.Failed), "failed", items)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// projectRule reads rule.json, and the rule files it names, at the base SHA
// for the same reason as serve.json: the checkout is the PR head, so a PR
// could otherwise exclude its own changes or rewrite its own review rules.
// It runs before anything is posted. When rule.json cannot be read, the run
// does not start: a review without the repository's excludes would post
// findings on files it excludes. A rule file that cannot be read leaves only
// its own rule empty, as in the CLI. An invalid rule.json is the
// repository's choice, so it reviews with the built-in rules.
func (s *Server) projectRule(ctx context.Context, f forge.Forge, ref forge.PRRef, pr forge.PR, log *slog.Logger) (*rules.ProjectRule, error) {
	raw, found, err := f.ReadFile(ctx, ref, pr.BaseSHA, rules.ProjectRulePath, rules.MaxRuleFileBytes)
	if err != nil || !found {
		return nil, err
	}
	out, err := rules.ParseProjectRule(raw, func(path string) ([]byte, bool, error) {
		b, ok, err := f.ReadFile(ctx, ref, pr.BaseSHA, path, rules.MaxRuleFileBytes)
		if err != nil {
			log.Warn("read rule file failed; its rule is left empty", "path", path, "error", err.Error())
		}
		return b, ok, err
	})
	if err != nil {
		log.Error("project rules invalid; using built-in rules only", "error", err.Error())
		return nil, nil
	}
	return out, nil
}

// fetch checks out HEAD. When the fetch fails because HEAD moved, the PR is
// read again so the caller can tell a new push from a real failure.
func (s *Server) fetch(ctx context.Context, f forge.Forge, ref forge.PRRef, pr forge.PR, ws review.Workspace) error {
	spec, err := f.Clone(ctx, pr)
	if err != nil {
		return fmt.Errorf("clone spec: %w", err)
	}
	ferr := ws.Fetch(ctx, spec, pr.HeadSHA, pr.BaseSHA)
	if ferr == nil {
		return nil
	}
	if cur, err := f.GetPR(ctx, ref); err == nil && cur.HeadSHA != pr.HeadSHA {
		return fmt.Errorf("%w: %s -> %s", errHeadMoved, pr.HeadSHA, cur.HeadSHA)
	}
	return ferr
}

// findings converts review comments and gives each a finding id. Two
// findings with the same span and category would share an id, so later ones
// get an occurrence suffix; the order of the review output keeps it stable.
func (s *Server) findings(a state.Action, head string, cs []model.LlmComment) []forge.Finding {
	now := s.now()
	seen := map[string]int{}
	out := make([]forge.Finding, 0, len(cs))
	for _, c := range cs {
		fd := forge.Finding{
			Path: c.Path, StartLine: c.StartLine, EndLine: c.EndLine,
			Category: c.Category, Severity: c.Severity, Content: c.Content,
			ExistingCode: c.ExistingCode, SuggestionCode: c.SuggestionCode,
		}
		id := state.FindingID(a, head, fd, now)
		seen[id]++
		if n := seen[id]; n > 1 {
			id = fmt.Sprintf("%s-%d", id, n)
		}
		fd.ID = id
		out = append(out, fd)
	}
	return out
}

// Stages of a run that fail after findings may already be on the PR.
const (
	stagePost   = "post findings"
	stageCommit = "commit summary"
)

// runFailed logs a failed run, replaces the pending line when one was
// written, and answers a command. The marker is not touched, so the run
// consumed no round. The forge calls use a fresh deadline because ctx may
// be the one that expired.
// A run stopped by shutdown keeps its waiting entry, so the next start polls
// it again; see settleFailedAuto for other automatic runs.
func (s *Server) runFailed(f forge.Forge, ref forge.PRRef, st prState, a state.Action, j job, stage string, err error, pendingWritten bool, log *slog.Logger) error {
	log.Error("review run failed", "stage", stage, "action", a.Kind.String(), "round", a.Round, "error", err.Error())
	if a.Kind == state.RunAuto && s.runCtx.Err() == nil {
		s.settleFailedAuto(f, ref, stage, log)
	}
	if pendingWritten {
		before := state.Initial(st.repo.MaxRounds)
		if st.marker != nil {
			before = *st.marker
		}
		line := ""
		if a.Kind == state.RunAuto {
			line = s.loc.Text("pending.failed")
		}
		ctx, cancel := s.sideContext()
		defer cancel()
		if uerr := f.UpsertSummary(ctx, ref, state.RenderSummary(before, st.open, s.loc, line)); uerr != nil {
			log.Error("replace pending summary line", "error", uerr.Error())
		}
	}
	s.commandFailed(f, ref, j.cmd, s.failureReason(stage, j.requestID), log)
	return err
}

// settleFailedAuto decides whether the poller keeps a merge request after a
// failed automatic run. With no open OCR thread it leaves the store: polling
// would rerun a failure that repeats (a timeout on a large diff, an LLM 4xx)
// on every tick, and the next push or note adds it again. A run that failed
// after posting can leave open threads, and GitLab reports their resolution
// only to the poller; while one is open, Decide waits without a review.
func (s *Server) settleFailedAuto(f forge.Forge, ref forge.PRRef, stage string, log *slog.Logger) {
	if ref.Platform == forge.GitLab && (stage == stagePost || stage == stageCommit) {
		ctx, cancel := s.sideContext()
		defer cancel()
		threads, err := f.ListOCRThreads(ctx, ref)
		if err == nil && countOpen(threads) > 0 {
			s.wait(ref, log)
			return
		}
		if err != nil {
			log.Warn("count open threads after a failed run", "error", err.Error())
		}
	}
	s.unwait(ref, log)
}

func (s *Server) clearPending(f forge.Forge, ref forge.PRRef, m state.Marker, open int, log *slog.Logger) {
	ctx, cancel := s.sideContext()
	defer cancel()
	if err := f.UpsertSummary(ctx, ref, state.RenderSummary(m, open, s.loc, "")); err != nil {
		log.Error("clear pending summary line", "error", err.Error())
	}
}

// failureReason keeps internal error text out of PR comments; the request id
// leads an operator to the full error in the log.
func (s *Server) failureReason(stage, requestID string) string {
	return fmt.Sprintf("review failed at %s (request %s)", stage, requestID)
}

func (s *Server) commandDone(f forge.Forge, ref forge.PRRef, cmd *cmdJob, count int, log *slog.Logger) {
	if cmd == nil || cmd.commentID == "" {
		return
	}
	ctx, cancel := s.sideContext()
	defer cancel()
	if err := f.React(ctx, ref, cmd.commentID, forge.ReactionSuccess); err != nil {
		log.Warn("reaction failed", "reaction", string(forge.ReactionSuccess), "error", err.Error())
	}
	if err := f.PostNote(ctx, ref, "✅ "+s.loc.Text("command.done", "count", strconv.Itoa(count))); err != nil {
		log.Error("post command reply", "error", err.Error())
	}
}

func (s *Server) commandFailed(f forge.Forge, ref forge.PRRef, cmd *cmdJob, reason string, log *slog.Logger) {
	if cmd == nil || cmd.commentID == "" {
		return
	}
	ctx, cancel := s.sideContext()
	defer cancel()
	if err := f.React(ctx, ref, cmd.commentID, forge.ReactionFailure); err != nil {
		log.Warn("reaction failed", "reaction", string(forge.ReactionFailure), "error", err.Error())
	}
	if err := f.PostNote(ctx, ref, state.RenderCommandError(reason, s.loc)); err != nil {
		log.Error("post command reply", "error", err.Error())
	}
}

// sideContext outlives a cancelled review so the result can still be
// reported, but stays bounded so shutdown is not held up for long.
func (s *Server) sideContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.runCtx), sideCallTimeout)
}

// wait records a GitLab merge request for the poller; GitHub reports thread
// resolution by webhook and needs no entry.
func (s *Server) wait(ref forge.PRRef, log *slog.Logger) {
	if ref.Platform != forge.GitLab {
		return
	}
	if err := s.store.Put(Waiting{Ref: ref, Since: s.now()}); err != nil {
		log.Error("update waiting store", "error", err.Error())
	}
}

func (s *Server) unwait(ref forge.PRRef, log *slog.Logger) {
	if ref.Platform != forge.GitLab {
		return
	}
	if err := s.store.Delete(ref.Key()); err != nil {
		log.Error("update waiting store", "error", err.Error())
	}
}

func openFindingIDs(threads []forge.Thread) map[string]bool {
	ids := map[string]bool{}
	for _, t := range threads {
		if !t.Resolved {
			ids[t.FindingID] = true
		}
	}
	return ids
}

func countOpen(threads []forge.Thread) int {
	n := 0
	for _, t := range threads {
		if !t.Resolved {
			n++
		}
	}
	return n
}
