// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// dispatch filters a verified event and queues its work. It runs inside the
// webhook handler, so it makes no forge call; forge calls run in the
// background. The returned text is the outcome for the access log.
//
// Only an opened or ready PR starts a chain on its own. The service's own
// events are dropped here: the round that pushed a fix queues the next round
// itself. Another bot (for example an agent that opens PRs) counts as a
// person for PR events, so its PR is reviewed and its push copies the status;
// without that the PR has no status and "pipelines must succeed" blocks it.
// Comments from any bot are dropped so two bots cannot answer each other.
// A push, or a reopened PR, starts no round; it only copies the chain's
// status to the new head. An open PR that the service reviews but whose
// chain has not started gets a pending status (see holdStatus), so the merge
// waits for the review.
func (s *Server) dispatch(ev forge.Event, body []byte, requestID string) string {
	switch {
	case ev.Kind == forge.EventIgnored:
		return "ignored event"
	case ev.SenderIsSelf:
		return "bot sender"
	case ev.SenderIsBot && ev.CommentID != "" && (ev.Kind == forge.EventCommand || ev.Kind == forge.EventNote):
		// A command without a comment is a GitLab reviewer assignment, which
		// cannot start a reply loop between two bots.
		return "bot sender"
	case ev.Kind == forge.EventClosed:
		s.forgetChain(ev.Ref)
		return "closed"
	}
	if s.seen.Seen(deliveryKey(ev, body)) {
		return "duplicate delivery"
	}
	switch ev.Kind {
	case forge.EventCommand, forge.EventNote:
		outcome, _ := s.dispatchComment(ev, requestID)
		return outcome
	case forge.EventPush:
		s.copyStatusAsync(ev.Ref, requestID, false)
		return "status copy"
	case forge.EventReopened:
		s.copyStatusAsync(ev.Ref, requestID, true)
		return "status copy"
	case forge.EventOpened, forge.EventReady:
	default:
		return "unsupported event"
	}
	if ev.PR != nil {
		if matched, rule := s.cfg.Ignore.Match(ignoreSubject(ev.Ref, *ev.PR)); matched {
			// An ignored PR still needs a success status, or "pipelines must
			// succeed" blocks it; an ignored repository stays silent.
			if !strings.HasPrefix(rule, "repo:") {
				s.copyStatusAsync(ev.Ref, requestID, true)
			}
			return "ignore rule " + rule
		}
		if ev.PR.Draft {
			// A draft starts no round, but it gets a pending status, so it
			// cannot merge in the moment between "ready" and the review.
			s.copyStatusAsync(ev.Ref, requestID, true)
			return "draft"
		}
	}
	t := triggerOpened
	if ev.Kind == forge.EventReady {
		t = triggerReady
	}
	// The evaluation may wait behind other work of the PR, and a ready event
	// may carry a new commit; the head is held at pending until the round
	// publishes its own status.
	s.copyStatusAsync(ev.Ref, requestID, true)
	s.queue.submit(ev.Ref, job{trigger: t, requestID: requestID})
	return "queued " + string(t)
}

// dispatchComment handles the comment part of a command or note event.
// handled is false when the comment holds no command.
func (s *Server) dispatchComment(ev forge.Event, requestID string) (string, bool) {
	names := s.commandNames(ev.Ref.Platform)
	line, mentioned := forge.CommandLine(ev.CommentBody, names)
	cmd, isCmd, perr := ParseCommand(line)
	unknown, isUnknown := unknownCommand(line, mentioned)
	if !isCmd && !isUnknown {
		return "not a command", false
	}
	if isUnknown && ev.CommentID == "" {
		return "unknown platform command", true
	}
	if ev.PrevCommentBody != "" && !commandAdded(line, ev.PrevCommentBody, names) {
		return "edit added no command", true
	}
	// Commands bypass every ignore rule except the repository one: a person
	// asked for the review, but an ignored repository must stay silent.
	if matched, rule := s.cfg.Ignore.Match(serveconfig.IgnoreSubject{Repo: ev.Ref.Repo}); matched {
		return "ignore rule " + rule, true
	}
	if ev.CommentID != "" && s.seen.Seen(commentKey(ev, line)) {
		return "duplicate command", true
	}
	cj := &cmdJob{Command: cmd, commentID: ev.CommentID}
	switch {
	case isUnknown && unknown == "":
		cj.reject = "no command given"
	case isUnknown:
		cj.reject = "unknown command " + unknown
	case perr != nil:
		cj.reject = perr.Error()
	default:
		scope := state.ScopeFull
		if cmd.Kind == CommandReviewCommit {
			scope = state.ScopeCommit
		}
		// A chain that is queued or running is not replaced: its work is
		// kept and the command is answered and dropped. Only an idle PR
		// starts a new chain.
		gen, busy, started := s.startChainIfIdle(ev.Ref, 1, scope)
		if !started {
			s.replyBusyAsync(ev.Ref, ev.CommentID, busy, cmd, requestID)
			return "ignored: chain running", true
		}
		cj.gen = gen
	}
	if ev.CommentID != "" {
		s.reactAsync(ev.Ref, ev.CommentID, forge.ReactionEyes, requestID)
	}
	s.queue.submit(ev.Ref, job{cmd: cj, requestID: requestID})
	if cj.reject != "" {
		return "queued command reply", true
	}
	return "queued command " + cmd.Kind, true
}

// replyBusyAsync answers a command that was dropped because the chain of the
// PR is still queued or running. A command without a comment, such as a
// GitLab reviewer assignment, gets no reply.
func (s *Server) replyBusyAsync(ref forge.PRRef, commentID string, busy busyChain, cmd Command, requestID string) {
	log := withPR(withRequest(s.log, requestID), ref)
	log.Info("decided", "trigger", "command "+cmd.Kind, "action", "ignored: chain running", "round", busy.n, "scope", busy.scope)
	if commentID == "" {
		return
	}
	f := s.forges[ref.Platform]
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(s.runCtx, sideCallTimeout)
		defer cancel()
		if err := f.PostNote(ctx, ref, state.RenderCommandBusy(busy.n, busy.limit, busy.scope, s.locales[ref.Platform])); err != nil {
			log.Error("post command reply", "error", err.Error())
		}
	}()
}

// reactAsync acknowledges a command at once, even while the PR is busy with
// a long review.
func (s *Server) reactAsync(ref forge.PRRef, commentID string, r forge.Reaction, requestID string) {
	f := s.forges[ref.Platform]
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(s.runCtx, sideCallTimeout)
		defer cancel()
		if err := f.React(ctx, ref, commentID, r); err != nil {
			withPR(withRequest(s.log, requestID), ref).Warn("reaction failed", "reaction", string(r), "error", err.Error())
		}
	}()
}

func deliveryKey(ev forge.Event, body []byte) string {
	if ev.DeliveryID != "" {
		return "delivery:" + string(ev.Ref.Platform) + ":" + ev.DeliveryID
	}
	sum := sha256.Sum256(body)
	return "body:" + hex.EncodeToString(sum[:])
}

// commentKey also guards edits when a forge cannot report the old body: the
// same command line on the same comment runs once within the TTL. line is in
// slash form, so "@bot review" and "/review" share a key.
func commentKey(ev forge.Event, line string) string {
	return "comment:" + ev.Ref.Key() + ":" + ev.CommentID + ":" + normalizeLine(line)
}

// commandNames are the names users tag the bot with on a platform.
func (s *Server) commandNames(p forge.Platform) []string {
	if f := s.forges[p]; f != nil {
		return f.CommandNames()
	}
	return nil
}

// mention is the name shown in help texts for a platform, or "" when the bot
// name is not known yet and the texts show only slash commands.
func (s *Server) mention(p forge.Platform) string {
	if names := s.commandNames(p); len(names) > 0 {
		return names[0]
	}
	return ""
}

func ignoreSubject(ref forge.PRRef, pr forge.PR) serveconfig.IgnoreSubject {
	return serveconfig.IgnoreSubject{
		Repo: ref.Repo, Author: pr.Author, Title: pr.Title,
		SourceBranch: pr.SourceBranch, TargetBranch: pr.TargetBranch, Labels: pr.Labels,
		GitLab: ref.Platform == forge.GitLab,
	}
}
