// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// Reasons quoted in fix replies; English like the other command errors.
const (
	reasonFixUnavailable = "fix needs the cli review engine"
	reasonFork           = "the head branch is in a fork"
	maxPushReason        = 300
)

// fix runs a /fix command: the fix CLI edits a checkout of the head branch,
// the service commits as the bot and pushes, then settles the threads and
// queues a review of the fix commit. Neither the fix nor that review counts
// as an automatic round, and the bot's push is filtered as a bot event.
func (s *Server) fix(ctx context.Context, f forge.Forge, ref forge.PRRef, j job, log *slog.Logger) error {
	if s.fixer == nil {
		s.commandFailed(f, ref, j.cmd, reasonFixUnavailable, log)
		return nil
	}
	pr, err := f.GetPR(ctx, ref)
	if err != nil {
		log.Error("read pull request for fix", "error", err.Error())
		s.commandFailed(f, ref, j.cmd, s.fixFailureReason("read", j.requestID), log)
		return err
	}
	pr.Ref = ref
	switch {
	case pr.State != forge.StateOpen:
		s.commandFailed(f, ref, j.cmd, "pr not open", log)
		return nil
	case pr.Fork:
		s.cannotPush(f, ref, j.cmd, pr.SourceBranch, reasonFork, log)
		return nil
	}
	threads, err := f.ListOCRThreads(ctx, ref)
	if err != nil {
		log.Error("list threads for fix", "error", err.Error())
		s.commandFailed(f, ref, j.cmd, s.fixFailureReason("read", j.requestID), log)
		return err
	}
	open := map[string]forge.Thread{}
	var findings []review.FixFinding
	for _, t := range threads {
		if t.Resolved || open[t.FindingID].ID != "" {
			continue
		}
		open[t.FindingID] = t
		findings = append(findings, s.fixFinding(t))
	}
	if len(findings) == 0 {
		s.fixReply(f, ref, j.cmd, forge.ReactionSuccess, state.RenderFixNothing(s.loc), log)
		return nil
	}

	select {
	case s.workers <- struct{}{}:
		defer func() { <-s.workers }()
	case <-s.runCtx.Done():
		s.commandFailed(f, ref, j.cmd, s.fixFailureReason("wait for worker", j.requestID), log)
		return s.runCtx.Err()
	}
	// The fix gets the full limit from when a worker takes it, as a review.
	ctx, cancel := context.WithTimeout(s.runCtx, s.cfg.ReviewTimeout)
	defer cancel()

	fail := func(stage string, err error) error {
		log.Error("fix run failed", "stage", stage, "error", err.Error())
		s.commandFailed(f, ref, j.cmd, s.fixFailureReason(stage, j.requestID), log)
		return err
	}
	spec, err := f.PushSpec(ctx, pr)
	if errors.Is(err, forge.ErrFork) {
		s.cannotPush(f, ref, j.cmd, pr.SourceBranch, reasonFork, log)
		return nil
	}
	if err != nil {
		return fail("checkout", err)
	}
	ws, release, err := s.workspaces.Acquire(ctx)
	if err != nil {
		return fail("workspace", err)
	}
	defer release()
	if ferr := ws.FetchBranch(ctx, spec, pr.HeadSHA, pr.BaseSHA); ferr != nil {
		if cur, err := f.GetPR(ctx, ref); err == nil && cur.HeadSHA != pr.HeadSHA {
			log.Info("head moved before the fix started", "from", pr.HeadSHA, "to", cur.HeadSHA)
			return fmt.Errorf("%w: %s -> %s", errHeadMoved, pr.HeadSHA, cur.HeadSHA)
		}
		return fail("checkout", ferr)
	}
	files, err := ws.ChangedFiles(ctx, pr.BaseSHA)
	if err != nil {
		return fail("checkout", err)
	}
	res, err := s.fixer.Fix(ctx, review.FixRequest{RepoDir: ws.Dir(), PR: pr, Files: files, Findings: findings})
	if len(res.Warnings) > 0 {
		log.Warn("fix warnings", "warnings", res.Warnings)
	}
	if err != nil {
		return fail("fix", err)
	}
	ident, err := f.CommitIdentity(ctx, ref)
	if err != nil {
		return fail("commit", err)
	}
	sha, changed, err := ws.Commit(ctx, ident, res.CommitMessage)
	if err != nil {
		return fail("commit", err)
	}
	skipped := s.fixSkipped(res.Skipped, open)
	if !changed {
		log.Info("fix changed nothing", "fixed", len(res.Fixed), "skipped", len(res.Skipped))
		s.fixReply(f, ref, j.cmd, forge.ReactionSuccess, state.RenderFixNoChanges(res.Summary, skipped, s.loc), log)
		return nil
	}
	// A fresh token: the one used for the fetch can expire during a long run.
	if spec, err = f.PushSpec(ctx, pr); err != nil {
		return fail("push", err)
	}
	if err := ws.Push(ctx, spec, sha); err != nil {
		log.Warn("fix push rejected", "error", err.Error())
		s.cannotPush(f, ref, j.cmd, pr.SourceBranch, pushReason(err), log)
		return nil
	}

	sctx, scancel := s.sideContext()
	defer scancel()
	for _, id := range res.Fixed {
		if err := f.ResolveThread(sctx, ref, open[id].ID); err != nil {
			log.Warn("resolve fixed thread", "finding", id, "error", err.Error())
		}
	}
	for _, sk := range res.Skipped {
		if err := f.ReplyThread(sctx, ref, open[sk.ID].ID, state.RenderFixSkipReply(sk.Reason, s.loc)); err != nil {
			log.Warn("reply in skipped thread", "finding", sk.ID, "error", err.Error())
		}
	}
	log.Info("fix pushed", "commit", sha, "branch", pr.SourceBranch, "fixed", len(res.Fixed), "skipped", len(res.Skipped))
	s.fixReply(f, ref, j.cmd, forge.ReactionSuccess, state.RenderFixDone(sha, len(res.Fixed), res.Summary, skipped, s.loc), log)
	s.queue.submit(ref, job{cmd: &cmdJob{Command: Command{Kind: CommandReviewCommit}, followUp: true}, requestID: j.requestID})
	return nil
}

// fixFinding turns an open thread into the finding the fix CLI reads.
func (s *Server) fixFinding(t forge.Thread) review.FixFinding {
	p := state.ParseFinding(t.Body, s.loc)
	fd := review.FixFinding{ID: t.FindingID, Path: t.Path, Line: t.Line, Severity: p.Severity, Title: p.Title, Body: p.Body}
	for _, r := range t.Replies {
		fd.Replies = append(fd.Replies, review.FixReply{Author: r.Author, Body: r.Body})
	}
	return fd
}

func (s *Server) fixSkipped(skips []review.FixSkip, open map[string]forge.Thread) []state.FixSkipped {
	out := make([]state.FixSkipped, 0, len(skips))
	for _, sk := range skips {
		t := open[sk.ID]
		out = append(out, state.FixSkipped{Path: t.Path, Line: t.Line,
			Title: state.ParseFinding(t.Body, s.loc).Title, Reason: sk.Reason})
	}
	return out
}

// dismissed lists the findings a person turned down: resolved threads with
// at least one reply that is not the service's. A resolved thread without a
// reply counts as fixed and is not listed.
func (s *Server) dismissed(threads []forge.Thread) []review.Dismissed {
	var out []review.Dismissed
	for _, t := range threads {
		if !t.Resolved || len(t.Replies) == 0 {
			continue
		}
		reasons := make([]string, len(t.Replies))
		for i, r := range t.Replies {
			reasons[i] = r.Author + ": " + strings.TrimSpace(r.Body)
		}
		p := state.ParseFinding(t.Body, s.loc)
		out = append(out, review.Dismissed{Path: t.Path, Line: t.Line, Title: p.Title, Body: p.Body,
			Reason: strings.Join(reasons, "\n\n")})
	}
	return out
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

func (s *Server) cannotPush(f forge.Forge, ref forge.PRRef, cmd *cmdJob, branch, reason string, log *slog.Logger) {
	s.fixReply(f, ref, cmd, forge.ReactionFailure, state.RenderFixCannotPush(branch, reason, s.loc), log)
}

// fixReply reacts on the command comment and posts body as a PR note.
func (s *Server) fixReply(f forge.Forge, ref forge.PRRef, cmd *cmdJob, r forge.Reaction, body string, log *slog.Logger) {
	ctx, cancel := s.sideContext()
	defer cancel()
	if cmd != nil && cmd.commentID != "" {
		if err := f.React(ctx, ref, cmd.commentID, r); err != nil {
			log.Warn("reaction failed", "reaction", string(r), "error", err.Error())
		}
	}
	if err := f.PostNote(ctx, ref, body); err != nil {
		log.Error("post fix reply", "error", err.Error())
	}
}

func (s *Server) fixFailureReason(stage, requestID string) string {
	return fmt.Sprintf("fix failed at %s (request %s)", stage, requestID)
}
