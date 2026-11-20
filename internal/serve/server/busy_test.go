// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func busyReplies(f *fakeForge) []string {
	var out []string
	for _, n := range f.noteList() {
		if strings.Contains(n, "already in progress") {
			out = append(out, n)
		}
	}
	return out
}

func commandEvent(ref forge.PRRef, id, body string) forge.Event {
	return forge.Event{Kind: forge.EventCommand, Ref: ref, DeliveryID: id, CommentID: id, CommentBody: body}
}

// holdRoundTwo makes the review of the fix round block until release is
// closed; round 1 reports one finding.
func holdRoundTwo(h *harness) (started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	var full atomic.Int32
	h.rev.set(func(ctx context.Context, req review.Request) (review.Result, error) {
		if req.FixCommit {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return review.Result{}, ctx.Err()
			}
			return review.Result{Terminal: review.TerminalComplete}, nil
		}
		if full.Add(1) == 1 {
			return review.Result{Findings: []model.LlmComment{comment("a.go", 3)}, Terminal: review.TerminalComplete}, nil
		}
		return review.Result{Terminal: review.TerminalComplete}, nil
	})
	return started, release
}

// TestReviewCommandWhileRunningIsIgnored keeps the running round: the
// command gets a reply that names the round, and nothing is cancelled.
func TestReviewCommandWhileRunningIsIgnored(t *testing.T) {
	for _, ref := range []forge.PRRef{ghRef(21), glRef(21)} {
		t.Run(string(ref.Platform), func(t *testing.T) {
			h := newHarness(t)
			f := h.forgeFor(ref.Platform)
			chainPR(h, ref)
			started, release := holdRoundTwo(h)
			h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r-open"})
			<-started
			for i, body := range []string{"/review", "/review-commit"} {
				got := h.srv.dispatch(commandEvent(ref, "c"+string(rune('1'+i)), body), nil, "r-cmd")
				if got != "ignored: chain running" {
					t.Fatalf("%s: %s", body, got)
				}
			}
			waitUntil(t, "both replies", func() bool { return len(busyReplies(f)) == 2 })
			if r := busyReplies(f)[0]; !strings.Contains(r, "round 2/3") || !strings.Contains(r, "the latest commit") {
				t.Fatalf("reply %q", r)
			}
			close(release)
			h.drain()
			if len(h.rev.requests()) != 2 || h.logs.has("round cancelled: a newer review replaced its chain") {
				t.Fatalf("reviews %d logs:\n%s", len(h.rev.requests()), h.logs.String())
			}
			if !h.logs.has("decided") || !strings.Contains(h.logs.String(), `"action":"ignored: chain running"`) {
				t.Fatalf("decision not logged:\n%s", h.logs.String())
			}
			if m := f.marker(t, ref); m.Status != state.ChainSuccess || len(m.Rounds) != 2 {
				t.Fatalf("marker %+v", m)
			}
		})
	}
}

// TestReviewCommandWhileQueuedIsIgnored covers a chain whose first round
// still waits for a free worker, and a second command that arrives before it.
func TestReviewCommandWhileQueuedIsIgnored(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(22)
	chainPR(h, ref)
	for range cap(h.srv.workers) {
		h.srv.workers <- struct{}{}
	}
	if got := h.srv.dispatch(commandEvent(ref, "c1", "/review"), nil, "r1"); got != "queued command review" {
		t.Fatal(got)
	}
	waitUntil(t, "the round to wait for a worker", func() bool { return h.logs.has("round queued") })
	if got := h.srv.dispatch(commandEvent(ref, "c2", "/review"), nil, "r2"); got != "ignored: chain running" {
		t.Fatal(got)
	}
	waitUntil(t, "the reply", func() bool { return len(busyReplies(h.gh)) == 1 })
	if r := busyReplies(h.gh)[0]; !strings.Contains(r, "round 1/3") || !strings.Contains(r, "the whole PR") {
		t.Fatalf("reply %q", r)
	}
	for range cap(h.srv.workers) {
		<-h.srv.workers
	}
	h.drain()
	if len(h.rev.requests()) != 1 || h.logs.has("round cancelled: a newer review replaced its chain") {
		t.Fatalf("reviews %d logs:\n%s", len(h.rev.requests()), h.logs.String())
	}
}

// TestReviewCommandBeforeTheRoundStartsIsIgnored covers two commands that
// arrive together: only the first one starts a chain.
func TestReviewCommandBeforeTheRoundStartsIsIgnored(t *testing.T) {
	h := newHarness(t)
	ref := glRef(23)
	chainPR(h, ref)
	got1 := h.srv.dispatch(commandEvent(ref, "c1", "/review"), nil, "r1")
	got2 := h.srv.dispatch(commandEvent(ref, "c2", "/review"), nil, "r2")
	if got1 != "queued command review" || got2 != "ignored: chain running" {
		t.Fatalf("%s / %s", got1, got2)
	}
	h.drain()
	if len(h.rev.requests()) != 1 {
		t.Fatalf("reviews %d", len(h.rev.requests()))
	}
}

// TestReviewCommandAfterTheChainEndedStartsAChain checks that a finished
// chain leaves the PR idle for the next command.
func TestReviewCommandAfterTheChainEndedStartsAChain(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(24)
	chainPR(h, ref)
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r-open"})
	h.drain()
	before := len(h.rev.requests())
	if got := h.srv.dispatch(commandEvent(ref, "c1", "/review"), nil, "r-cmd"); got != "queued command review" {
		t.Fatal(got)
	}
	h.drain()
	if len(h.rev.requests()) != before+1 || len(busyReplies(h.gh)) != 0 {
		t.Fatalf("reviews %d, busy replies %v", len(h.rev.requests()), busyReplies(h.gh))
	}
}

// TestPushWhileRunningKeepsTheChain shows that a push replaces nothing: it
// only copies the status, and the running round finishes.
func TestPushWhileRunningKeepsTheChain(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(25)
	chainPR(h, ref)
	started, release := holdRoundTwo(h)
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r-open"})
	<-started
	if got := h.srv.dispatch(prEvent(forge.EventPush, ref, "p1"), nil, "r-push"); got != "status copy" {
		t.Fatal(got)
	}
	close(release)
	h.drain()
	if h.logs.has("round cancelled: a newer review replaced its chain") || len(busyReplies(h.gh)) != 0 {
		t.Fatalf("logs:\n%s", h.logs.String())
	}
	if m := h.gh.marker(t, ref); m.Status != state.ChainSuccess || len(m.Rounds) != 2 {
		t.Fatalf("marker %+v", m)
	}
}

// TestOpenedKeepsACommandChainStartedDuringItsRead covers a command accepted
// while the opened evaluation read the PR: the evaluation keeps that chain.
func TestOpenedKeepsACommandChainStartedDuringItsRead(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(26)
	chainPR(h, ref)
	gen, _, started := h.srv.startChainIfIdle(ref, 1, state.ScopeCommit)
	if !started {
		t.Fatal("chain not started")
	}
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r-open"})
	h.drain()
	if !h.srv.current(ref, gen) || len(h.rev.requests()) != 0 || !strings.Contains(h.logs.String(), `"reason":"chain running"`) {
		t.Fatalf("current %v reviews %d logs:\n%s", h.srv.current(ref, gen), len(h.rev.requests()), h.logs.String())
	}
}

// TestReviewerAssignmentWhileRunningGetsNoReply covers a GitLab reviewer
// assignment, a command without a comment, while a chain runs.
func TestReviewerAssignmentWhileRunningGetsNoReply(t *testing.T) {
	h := newHarness(t)
	ref := glRef(27)
	f := h.forgeFor(ref.Platform)
	chainPR(h, ref)
	started, release := holdRoundTwo(h)
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r-open"})
	<-started
	ev := forge.Event{Kind: forge.EventCommand, Ref: ref, DeliveryID: "a1", CommentBody: "/review"}
	if got := h.srv.dispatch(ev, nil, "r-assign"); got != "ignored: chain running" {
		t.Fatal(got)
	}
	close(release)
	h.drain()
	if len(busyReplies(f)) != 0 {
		t.Fatalf("busy replies %v", busyReplies(f))
	}
}
