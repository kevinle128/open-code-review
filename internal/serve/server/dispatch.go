// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

var eventTriggers = map[forge.EventKind]state.Trigger{
	forge.EventOpened:         state.TriggerOpened,
	forge.EventReady:          state.TriggerReady,
	forge.EventReopened:       state.TriggerReopened,
	forge.EventPush:           state.TriggerPush,
	forge.EventThreadResolved: state.TriggerThreadResolved,
	forge.EventNote:           state.TriggerNote,
}

// dispatch filters a verified event and queues its work. It runs inside the
// webhook handler, so it makes no forge call; the eyes reaction runs in the
// background. The returned text is the outcome for the access log.
func (s *Server) dispatch(ev forge.Event, body []byte, requestID string) string {
	switch {
	case ev.Kind == forge.EventIgnored:
		return "ignored event"
	case ev.SenderIsBot:
		return "bot sender"
	case ev.Kind == forge.EventClosed:
		s.unwait(ev.Ref, withPR(withRequest(s.log, requestID), ev.Ref))
		return "closed"
	}
	if s.seen.Seen(deliveryKey(ev, body)) {
		return "duplicate delivery"
	}
	if ev.Kind == forge.EventCommand || ev.Kind == forge.EventNote {
		if outcome, handled := s.dispatchComment(ev, requestID); handled {
			return outcome
		}
	}
	trigger, ok := eventTriggers[ev.Kind]
	if !ok {
		return "unsupported event"
	}
	if ev.PR != nil {
		if ev.PR.Draft {
			return "draft"
		}
		if matched, rule := s.cfg.Ignore.Match(ignoreSubject(ev.Ref, *ev.PR)); matched {
			return "ignore rule " + rule
		}
	}
	s.queue.submit(ev.Ref, job{trigger: trigger, requestID: requestID})
	return "queued " + string(trigger)
}

// dispatchComment handles the comment part of a command or note event.
// handled is false when the event should still go on as a note trigger.
func (s *Server) dispatchComment(ev forge.Event, requestID string) (string, bool) {
	cmd, isCmd, perr := ParseCommand(ev.CommentBody)
	unknown, isUnknown := unknownSlashCommand(ev.CommentBody)
	if !isCmd && !isUnknown {
		if ev.Kind == forge.EventCommand {
			return "not a command", true
		}
		return "", false
	}
	if isUnknown && ev.CommentID == "" {
		return "unknown platform command", true
	}
	if ev.PrevCommentBody != "" && !commandAdded(ev.CommentBody, ev.PrevCommentBody) {
		return "edit added no command", true
	}
	// Commands bypass every ignore rule except the repository one: a person
	// asked for the review, but an ignored repository must stay silent.
	if matched, rule := s.cfg.Ignore.Match(serveconfig.IgnoreSubject{Repo: ev.Ref.Repo}); matched {
		return "ignore rule " + rule, true
	}
	if ev.CommentID != "" && s.seen.Seen(commentKey(ev)) {
		return "duplicate command", true
	}
	cj := &cmdJob{Command: cmd, commentID: ev.CommentID}
	switch {
	case isUnknown:
		cj.reject = "unknown command " + unknown
	case perr != nil:
		cj.reject = perr.Error()
	}
	if ev.CommentID != "" {
		s.reactAsync(ev.Ref, ev.CommentID, forge.ReactionEyes, requestID)
	}
	s.queue.submit(ev.Ref, job{cmd: cj, requestID: requestID})
	if ev.Kind == forge.EventNote {
		s.queue.submit(ev.Ref, job{trigger: state.TriggerNote, requestID: requestID})
	}
	if cj.reject != "" {
		return "queued command reply", true
	}
	return "queued command " + cmd.Kind, true
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
// same command line on the same comment runs once within the TTL.
func commentKey(ev forge.Event) string {
	return "comment:" + ev.Ref.Key() + ":" + ev.CommentID + ":" + normalizeLine(firstLine(ev.CommentBody))
}

func ignoreSubject(ref forge.PRRef, pr forge.PR) serveconfig.IgnoreSubject {
	return serveconfig.IgnoreSubject{
		Repo: ref.Repo, Author: pr.Author, Title: pr.Title,
		SourceBranch: pr.SourceBranch, TargetBranch: pr.TargetBranch, Labels: pr.Labels,
	}
}
