// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

func TestReviewBudgetSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	use := func(c *serveconfig.Config) {
		c.DataDir = dir
		c.ReviewTimeout = time.Hour
	}
	h := newHarness(t, use)
	ref := ghRef(40)
	h.gh.setPR(openPR(ref, sha1))
	started := make(chan struct{})
	h.rev.set(func(ctx context.Context, _ review.Request) (review.Result, error) {
		close(started)
		<-ctx.Done()
		return review.Result{}, ctx.Err()
	})
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r"})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
	h.drain()

	h2 := newHarness(t, use)
	h2.clock = h2.clock.Add(2 * time.Hour)
	h2.gh.setPR(openPR(ref, sha1))
	if err := h2.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h2.drain()
	if n := len(h2.rev.requests()); n != 0 {
		t.Fatalf("review ran %d times after the budget was spent", n)
	}
	if got := lastStatus(h2.gh); got != "1:failure" {
		t.Fatalf("status %q", got)
	}
	notes := h2.gh.noteList()
	if len(notes) != 1 || !strings.Contains(notes[0], "time limit of 1h") || !strings.Contains(notes[0], "`/review`") {
		t.Fatalf("notes %q", notes)
	}
	if n := len(h2.srv.journal.pending()); n != 0 {
		t.Fatalf("journal keeps %d rounds", n)
	}
}

func TestShutdownCutCapFailsTheRound(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, func(c *serveconfig.Config) {
		c.DataDir = dir
		c.MaxShutdownCuts = 1
	})
	ref := ghRef(41)
	token, _, err := h.srv.journal.put(ref, roundJob{n: 1, scope: state.ScopeFull}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.journal.noteStart(ref, token, h.clock); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := h.srv.journal.noteShutdown(ref, token); err != nil {
			t.Fatal(err)
		}
	}
	h.gh.setPR(openPR(ref, sha1))
	if err := h.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if n := len(h.rev.requests()); n != 0 {
		t.Fatalf("review ran %d times past the cut cap", n)
	}
	notes := h.gh.noteList()
	if got := lastStatus(h.gh); got != "1:failure" || len(notes) != 1 || !strings.Contains(notes[0], "restarted 2 times") {
		t.Fatalf("status %q notes %q", lastStatus(h.gh), notes)
	}
}

func TestDeployResetDoesNotInheritCuts(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, func(c *serveconfig.Config) {
		c.DataDir = dir
		c.MaxShutdownCuts = 1
	})
	ref := ghRef(42)
	token, _, err := h.srv.journal.put(ref, roundJob{n: 1, scope: state.ScopeFull}, "old")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.journal.noteStart(ref, token, h.clock); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := h.srv.journal.noteShutdown(ref, token); err != nil {
			t.Fatal(err)
		}
	}
	h2 := newHarness(t, func(c *serveconfig.Config) {
		c.DataDir = dir
		c.MaxShutdownCuts = 1
	})
	h2.srv.version = "v2"
	h2.gh.setPR(openPR(ref, sha1))
	if err := h2.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h2.drain()
	if n := len(h2.rev.requests()); n != 1 {
		t.Fatalf("deployed round ran %d reviews", n)
	}
	if got := lastStatus(h2.gh); got != "1:success" {
		t.Fatalf("status %q", got)
	}
}

func TestQuietCancelPostsNothing(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(43)
	h.gh.setPR(openPR(ref, sha1))
	started := make(chan struct{})
	h.rev.set(func(ctx context.Context, _ review.Request) (review.Result, error) {
		close(started)
		<-ctx.Done()
		return review.Result{}, ctx.Err()
	})
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r"})
	<-started
	h.srv.restartChain(ref)
	h.drain()
	if !h.logs.has("round cancelled: a newer review replaced its chain") {
		t.Fatal("the replaced round did not fail quietly")
	}
	if notes := strings.Join(h.gh.noteList(), "\n"); strings.Contains(notes, "time limit") || strings.Contains(notes, "review engine failed") || strings.Contains(notes, "the review was cancelled") {
		t.Fatalf("quiet failure posted %q", notes)
	}
}

func TestOversizeSkipPostsSuccess(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(44)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Terminal: review.TerminalSkipped, Notice: review.NoticeOversize, NoticeFiles: 400, NoticeBytes: 9 << 20}, nil
	})
	h.opened(ref)
	h.drain()
	notes := h.gh.noteList()
	if len(notes) != 1 || !strings.Contains(notes[0], "too large") || !strings.Contains(notes[0], "400") {
		t.Fatalf("notes %q", notes)
	}
	if got := lastStatus(h.gh); got != "1:success" {
		t.Fatalf("status %q", got)
	}
}

func TestFailureNoticeSurvivesPostError(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(45)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.errs["PostNote"] = errors.New("forge down")
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{}, errors.New("agy exited: NOT_FOUND token=secret /tmp/ocr/run")
	})
	h.opened(ref)
	h.drain()
	if got := lastStatus(h.gh); got != "1:failure" {
		t.Fatalf("status %q", got)
	}
	if !h.logs.has("post failure notice") {
		t.Fatal("a failed notice was not logged")
	}
	if n := len(h.gh.noteList()); n != 0 {
		t.Fatalf("notes %d", n)
	}
}

func TestDeadlineClassFollowsTheBudget(t *testing.T) {
	h := newHarness(t)
	r := &roundRun{s: h.srv, ref: ghRef(1), budget: "review", rj: roundJob{started: h.clock.Add(-2 * time.Hour)}}
	for _, stage := range []string{"read", "workspace", "checkout"} {
		n := r.failureNotice(stage, context.DeadlineExceeded)
		if n.Class != state.FailureTime || n.Stage != "review" {
			t.Fatalf("%s classified %+v", stage, n)
		}
	}
	r.budget = "fix"
	r.rj.fixStarted = h.clock.Add(-time.Hour)
	if n := r.failureNotice("read", context.DeadlineExceeded); n.Class != state.FailureTime || n.Stage != "fix" {
		t.Fatalf("fix deadline %+v", n)
	}
	r.budget = ""
	if n := r.failureNotice("read", context.DeadlineExceeded); n.Class != state.FailureEngine {
		t.Fatalf("side timeout %+v", n)
	}
	body := state.RenderFailureNotice(r.failureNotice("wait for worker", context.Canceled), h.loc)
	if !strings.Contains(body, h.loc.Text("notice.cancelled")) {
		t.Fatalf("cancelled %q", body)
	}
}

func TestReviewBudgetDeadlineDuringRead(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.ReviewTimeout = 30 * time.Millisecond })
	ref := ghRef(46)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.blockGetPR = true
	// A round job reads the PR on the budget context. An opened event would
	// read it first on a side context, and that read is not the budget.
	gen := h.srv.startChain(ref, 1, state.ScopeFull)
	h.srv.process(ref, job{round: &roundJob{n: 1, scope: state.ScopeFull, gen: gen}, requestID: "r"})
	notes := h.gh.noteList()
	if len(notes) != 1 || !strings.Contains(notes[0], "time limit") || strings.Contains(notes[0], "context deadline exceeded") {
		t.Fatalf("notes %q", notes)
	}
	if n := len(h.rev.requests()); n != 0 {
		t.Fatalf("review ran %d times", n)
	}
}

func TestFixBudgetSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	use := func(c *serveconfig.Config) {
		c.DataDir = dir
		c.ReviewTimeout = 2 * time.Hour
		c.FixTimeout = time.Hour
	}
	h := newHarness(t, use)
	ref := ghRef(47)
	chainPR(h, ref)
	h.rev.set(findingsResult(comment("a.go", 3)))
	started := make(chan struct{})
	h.fix.set(func(ctx context.Context, _ review.FixRequest) (review.FixResult, error) {
		close(started)
		<-ctx.Done()
		return review.FixResult{}, ctx.Err()
	})
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r"})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
	h.drain()

	h2 := newHarness(t, use)
	h2.clock = h2.clock.Add(2 * time.Hour)
	chainPR(h2, ref)
	if err := h2.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h2.drain()
	if n := len(h2.rev.requests()); n != 0 {
		t.Fatalf("review ran %d times after the fix budget was spent", n)
	}
	if n := len(h2.fix.requests()); n != 0 {
		t.Fatalf("fix ran %d times after its budget was spent", n)
	}
	notes := h2.gh.noteList()
	if len(notes) != 1 || !strings.Contains(notes[0], "time limit of 1h") || !strings.Contains(notes[0], h2.loc.Text("notice.stage_fix")) {
		t.Fatalf("notes %q", notes)
	}
}

func TestSavedReviewUsesRepositoryEffort(t *testing.T) {
	dir := t.TempDir()
	use := func(c *serveconfig.Config) {
		c.DataDir = dir
		c.ReviewTimeout = time.Hour
	}
	h := newHarness(t, use)
	ref := ghRef(48)
	token, _, err := h.srv.journal.put(ref, roundJob{n: 1, scope: state.ScopeFull}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.journal.noteStart(ref, token, h.clock); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.journal.noteShutdown(ref, token); err != nil {
		t.Fatal(err)
	}
	h2 := newHarness(t, use)
	h2.clock = h2.clock.Add(2 * time.Hour)
	h2.gh.setPR(openPR(ref, sha1))
	h2.gh.files[shaBase+":.opencodereview/serve.json"] = []byte(`{"effort":"low"}`)
	if err := saveReview(h2.srv.roundCheckpoints(ref, 1), savedReview{Base: shaBase, Head: sha1, Scope: state.ScopeFull, Effort: "low",
		Result: review.Result{Terminal: review.TerminalComplete}}); err != nil {
		t.Fatal(err)
	}
	if err := h2.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h2.drain()
	if n := len(h2.rev.requests()); n != 0 {
		t.Fatalf("review ran %d times", n)
	}
	if !h2.logs.has("review reused from a checkpoint") {
		t.Fatal("the saved review was not reused")
	}
	if got := lastStatus(h2.gh); got != "1:success" {
		t.Fatalf("status %q", got)
	}
}

func TestClosedPRStaysQuietWhenTheRoundCannotRun(t *testing.T) {
	t.Run("spent budget", func(t *testing.T) {
		dir := t.TempDir()
		h := newHarness(t, func(c *serveconfig.Config) {
			c.DataDir = dir
			c.ReviewTimeout = time.Hour
		})
		ref := ghRef(49)
		token, _, err := h.srv.journal.put(ref, roundJob{n: 1, scope: state.ScopeFull}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.srv.journal.noteStart(ref, token, h.clock); err != nil {
			t.Fatal(err)
		}
		h2 := newHarness(t, func(c *serveconfig.Config) {
			c.DataDir = dir
			c.ReviewTimeout = time.Hour
		})
		h2.clock = h2.clock.Add(2 * time.Hour)
		pr := openPR(ref, sha1)
		pr.State = forge.StateMerged
		h2.gh.setPR(pr)
		if err := h2.srv.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		h2.drain()
		if notes := h2.gh.noteList(); len(notes) != 0 || lastStatus(h2.gh) != "" {
			t.Fatalf("status %q notes %q", lastStatus(h2.gh), notes)
		}
	})

	t.Run("too many cuts", func(t *testing.T) {
		dir := t.TempDir()
		h := newHarness(t, func(c *serveconfig.Config) {
			c.DataDir = dir
			c.MaxShutdownCuts = 1
		})
		ref := ghRef(50)
		token, _, err := h.srv.journal.put(ref, roundJob{n: 1, scope: state.ScopeFull}, "test")
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := h.srv.journal.noteShutdown(ref, token); err != nil {
				t.Fatal(err)
			}
		}
		h2 := newHarness(t, func(c *serveconfig.Config) {
			c.DataDir = dir
			c.MaxShutdownCuts = 1
		})
		pr := openPR(ref, sha1)
		pr.State = forge.StateClosed
		h2.gh.setPR(pr)
		if err := h2.srv.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		h2.drain()
		if notes := h2.gh.noteList(); len(notes) != 0 || lastStatus(h2.gh) != "" || len(h2.rev.requests()) != 0 {
			t.Fatalf("status %q notes %q reviews %d", lastStatus(h2.gh), notes, len(h2.rev.requests()))
		}
	})
}

func TestQueuedCutsReachTheCapAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	use := func(c *serveconfig.Config) {
		c.DataDir = dir
		c.Workers = 1
		c.MaxShutdownCuts = 1
	}
	ref := ghRef(52)
	h := newHarness(t, use)
	h.gh.setPR(openPR(ref, sha1))
	h.srv.workers <- struct{}{}
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "q"})
	waitLog(t, h, "round queued")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
	h.drain()
	got := h.srv.journal.pending()
	if len(got) != 1 || got[0].Cuts != 1 || got[0].StartedUnixNano != 0 {
		t.Fatalf("queued cut %+v", got)
	}

	// The resumed round is still waiting for a worker. Its cut count was 1,
	// so this shutdown is the one that passes the cap.
	h2 := newHarness(t, use)
	h2.gh.setPR(openPR(ref, sha1))
	h2.srv.workers <- struct{}{}
	if err := h2.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitLog(t, h2, "round queued")
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = h2.srv.Shutdown(ctx)
	h2.drain()
	if n := len(h2.rev.requests()); n != 0 {
		t.Fatalf("review ran %d times", n)
	}
	notes := h2.gh.noteList()
	if len(notes) != 1 || !strings.Contains(notes[0], "restarted 2 times") {
		t.Fatalf("notes %q", notes)
	}
}

func TestQueuedShutdownOnClosedPRIsQuiet(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, func(c *serveconfig.Config) {
		c.DataDir = dir
		c.Workers = 1
	})
	ref := ghRef(55)
	h.gh.setPR(openPR(ref, sha1))
	h.srv.workers <- struct{}{}
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "q"})
	waitLog(t, h, "round queued")
	pr := openPR(ref, sha1)
	pr.State = forge.StateMerged
	h.gh.setPR(pr)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
	h.drain()
	if notes := h.gh.noteList(); len(notes) != 0 || len(h.srv.journal.pending()) != 0 {
		t.Fatalf("notes %q journal %d", notes, len(h.srv.journal.pending()))
	}
}

func TestHeadMovedRerunKeepsTheCutCount(t *testing.T) {
	dir := t.TempDir()
	use := func(c *serveconfig.Config) { c.DataDir = dir }
	h := newHarness(t, use)
	ref := ghRef(53)
	token, _, err := h.srv.journal.put(ref, roundJob{n: 1, scope: state.ScopeFull}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.journal.noteStart(ref, token, h.clock); err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.journal.noteShutdown(ref, token); err != nil {
		t.Fatal(err)
	}
	h2 := newHarness(t, use)
	h2.gh.setPR(openPR(ref, sha1))
	h2.ws.set(func(w *fakeWorkspaces) { w.branchErrs = []error{errTest} })
	h2.gh.headAfter[ref.Key()] = sha2
	h2.rev.set(func(_ context.Context, req review.Request) (review.Result, error) {
		got := h2.srv.journal.pending()
		if len(got) != 1 || got[0].Cuts != 1 || got[0].StartedUnixNano == 0 {
			t.Errorf("journal after the rerun %+v", got)
		}
		if req.To != sha2 {
			t.Errorf("reviewed %s", req.To)
		}
		return review.Result{Terminal: review.TerminalComplete}, nil
	})
	if err := h2.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h2.drain()
	if n := len(h2.rev.requests()); n != 1 {
		t.Fatalf("reviews %d", n)
	}
}

func TestNextRoundStartsWithNoCuts(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(54)
	chainPR(h, ref)
	h.rev.set(func(_ context.Context, req review.Request) (review.Result, error) {
		if req.FixCommit {
			got := h.srv.journal.pending()
			if len(got) != 1 || got[0].Round != 2 || got[0].Cuts != 0 {
				t.Errorf("round 2 journal %+v", got)
			}
			return review.Result{Terminal: review.TerminalComplete}, nil
		}
		return findingsResult(comment("a.go", 3))(context.Background(), req)
	})
	h.opened(ref)
	h.drain()
	if n := len(h.rev.requests()); n != 2 {
		t.Fatalf("reviews %d", n)
	}
}

func waitLog(t *testing.T, h *harness, text string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !h.logs.has(text) {
		if time.Now().After(deadline) {
			t.Fatalf("log never contained %q:\n%s", text, h.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
