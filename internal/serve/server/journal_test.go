// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// TestShutdownKeepsRoundForRestart cuts a running round with a shutdown:
// the PR keeps its pending status, and the next start runs the round again.
func TestShutdownKeepsRoundForRestart(t *testing.T) {
	dir := t.TempDir()
	useDir := func(c *serveconfig.Config) { c.DataDir = dir }
	h := newHarness(t, useDir)
	ref := glRef(4)
	h.gl.setPR(openPR(ref, sha1))
	started := make(chan struct{})
	h.rev.set(func(ctx context.Context, _ review.Request) (review.Result, error) {
		close(started)
		<-ctx.Done()
		// A multi-batch review cut mid-run returns the batches that
		// finished as a partial result with no error.
		return review.Result{Terminal: review.TerminalPartial}, nil
	})
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r"})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error %v", err)
	}
	h.drain()
	if got := lastStatus(h.gl); got != "1:pending" {
		t.Fatalf("status after the cut %q", got)
	}
	if !h.logs.has("round cut by shutdown; it runs again after the restart") {
		t.Fatal("the cut round is not logged")
	}

	cp := h.rev.requests()[0].Checkpoint
	if cp == "" {
		t.Fatal("the review got no checkpoint directory")
	}
	if _, err := os.Stat(filepath.Join(cp, reviewFile)); !os.IsNotExist(err) {
		t.Fatalf("a review cut by the shutdown was saved: %v", err)
	}

	h2 := newHarness(t, useDir)
	h2.gl.setPR(openPR(ref, sha1))
	// A review that finished before the cut is reused, not run again.
	if err := saveReview(h2.srv.roundCheckpoints(ref, 1), savedReview{Base: shaBase, Head: sha1, Scope: state.ScopeFull,
		Result: review.Result{Terminal: review.TerminalComplete}}); err != nil {
		t.Fatal(err)
	}
	if err := h2.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h2.drain()
	if !h2.logs.has("round resumed after a restart") {
		t.Fatal("the round did not resume")
	}
	if got := lastStatus(h2.gl); got != "1:success" {
		t.Fatalf("status after the resumed round %q", got)
	}
	if n := len(h2.srv.journal.pending()); n != 0 {
		t.Fatalf("journal keeps %d rounds after the chain ended", n)
	}
	if n := len(h2.rev.requests()); n != 0 || !h2.logs.has("review reused from a checkpoint") {
		t.Fatalf("the resumed round ran the review again (%d runs)", n)
	}
	if _, err := os.Stat(h2.srv.prCheckpoints(ref)); !os.IsNotExist(err) {
		t.Fatalf("checkpoints left after the round ended: %v", err)
	}
}

func TestJournal(t *testing.T) {
	if j, err := openJournal(""); j != nil || err != nil {
		t.Fatal("no data directory must give no journal")
	}
	var none *journal
	if tok, _, err := none.put(ghRef(1), roundJob{n: 1}, ""); tok != "" || err != nil || none.remove(ghRef(1), "") != nil || none.pending() != nil {
		t.Fatal("a nil journal records nothing")
	}

	dir := t.TempDir()
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	old, _, _ := j.put(ghRef(1), roundJob{n: 1, scope: state.ScopeFull}, "")
	cur, _, _ := j.put(ghRef(1), roundJob{n: 2, scope: state.ScopeCommit, commit: sha1, fixCommit: true, effort: "high"}, "")
	if _, _, err := j.put(glRef(2), roundJob{n: 1, scope: state.ScopeFull}, ""); err != nil {
		t.Fatal(err)
	}
	if err := j.remove(ghRef(1), old); err != nil || len(j.pending()) != 2 {
		t.Fatal("an older token must not remove the newer round")
	}

	j2, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := j2.pending()
	if len(got) != 2 || got[0].ref() != ghRef(1) || got[1].ref() != glRef(2) {
		t.Fatalf("reopened journal %+v", got)
	}
	if rj := got[0].roundJob(); rj.n != 2 || rj.scope != state.ScopeCommit || rj.commit != sha1 || !rj.fixCommit || rj.effort != "high" {
		t.Fatalf("round %+v", rj)
	}
	if err := j2.remove(ghRef(1), cur); err != nil || len(j2.pending()) != 1 {
		t.Fatal("the current token must remove the round")
	}

	if err := os.WriteFile(filepath.Join(dir, journalFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if j3, err := openJournal(dir); err == nil || j3 == nil || len(j3.pending()) != 0 {
		t.Fatal("a broken file must be reported and replaced by an empty journal")
	}
	if _, err := openJournal(filepath.Join(dir, journalFile)); err == nil {
		t.Fatal("a data path that is a file must be reported")
	}
}

func TestJournalKeepsStartAndCuts(t *testing.T) {
	dir := t.TempDir()
	old := `{"github:acme/app#3":{"platform":"github","repo":"acme/app","installation_id":7,"number":3,"round":1,"scope":"full","token":"9"}}`
	if err := os.WriteFile(filepath.Join(dir, journalFile), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := j.pending()
	if len(got) != 1 || got[0].StartedUnixNano != 0 || got[0].Cuts != 0 || got[0].Token != "9" {
		t.Fatalf("old journal %+v", got)
	}

	start := time.Unix(1_700_000_000, 0)
	ref := got[0].ref()
	if err := j.noteStart(ref, "9", start); err != nil {
		t.Fatal(err)
	}
	cuts, err := j.noteShutdown(ref, "9")
	if err != nil || cuts != 1 {
		t.Fatalf("cuts %d err %v", cuts, err)
	}
	j2, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	again := j2.pending()
	if len(again) != 1 || again[0].StartedUnixNano != start.UnixNano() || again[0].Cuts != 1 || again[0].Running {
		t.Fatalf("reopened %+v", again[0])
	}
	keptToken, kept, err := j2.put(ref, again[0].roundJob(), "test")
	if err != nil || kept.StartedUnixNano != start.UnixNano() || kept.Cuts != 1 || kept.Token == "9" {
		t.Fatalf("resume token %s entry %+v err %v", keptToken, kept, err)
	}
	_, deployed, err := j2.put(ref, kept.roundJob(), "v2")
	if err != nil || deployed.Cuts != 0 || deployed.StartedUnixNano != start.UnixNano() {
		t.Fatalf("deploy %+v err %v", deployed, err)
	}
}

func TestCutCountDoesNotDependOnStart(t *testing.T) {
	ref := ghRef(7)
	job := roundJob{n: 1, scope: state.ScopeFull}

	t.Run("queued cut survives a restart", func(t *testing.T) {
		j := openTestJournal(t)
		token, _, err := j.put(ref, job, "test")
		if err != nil {
			t.Fatal(err)
		}
		if cuts, err := j.noteShutdown(ref, token); err != nil || cuts != 1 {
			t.Fatalf("cuts %d err %v", cuts, err)
		}
		j2 := reopenJournal(t, j)
		_, kept, err := j2.put(ref, j2.pending()[0].roundJob(), "test")
		if err != nil || kept.Cuts != 1 || kept.StartedUnixNano != 0 {
			t.Fatalf("resume %+v err %v", kept, err)
		}
	})

	t.Run("crash while queued counts once", func(t *testing.T) {
		j := openTestJournal(t)
		if _, _, err := j.put(ref, job, "test"); err != nil {
			t.Fatal(err)
		}
		j2 := reopenJournal(t, j)
		_, kept, err := j2.put(ref, j2.pending()[0].roundJob(), "test")
		if err != nil || kept.Cuts != 1 || kept.StartedUnixNano != 0 {
			t.Fatalf("resume %+v err %v", kept, err)
		}
		// The same process records the resumed round again. That is not another crash.
		_, again, err := j2.put(ref, kept.roundJob(), "test")
		if err != nil || again.Cuts != 1 {
			t.Fatalf("second put %+v err %v", again, err)
		}
	})

	t.Run("crash while running counts once", func(t *testing.T) {
		j := openTestJournal(t)
		token, _, err := j.put(ref, job, "test")
		if err != nil {
			t.Fatal(err)
		}
		start := time.Unix(1_700_000_000, 0)
		if err := j.noteStart(ref, token, start); err != nil {
			t.Fatal(err)
		}
		j2 := reopenJournal(t, j)
		_, kept, err := j2.put(ref, j2.pending()[0].roundJob(), "test")
		if err != nil || kept.Cuts != 1 || kept.StartedUnixNano != start.UnixNano() {
			t.Fatalf("resume %+v err %v", kept, err)
		}
	})

	t.Run("same process queues the next round without a cut", func(t *testing.T) {
		j := openTestJournal(t)
		if _, _, err := j.put(ref, job, "test"); err != nil {
			t.Fatal(err)
		}
		next := roundJob{n: 2, scope: state.ScopeCommit, commit: sha1, fixCommit: true}
		if _, _, err := j.put(ref, next, "test"); err != nil {
			t.Fatal(err)
		}
		_, kept, err := j.put(ref, next, "test")
		if err != nil || kept.Cuts != 0 || kept.Round != 2 || kept.StartedUnixNano != 0 {
			t.Fatalf("round 2 %+v err %v", kept, err)
		}
	})
}

func TestJournalProcMarkersDiffer(t *testing.T) {
	dir := t.TempDir()
	a, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.proc == "" || b.proc == "" || a.proc == b.proc {
		t.Fatalf("procs %q %q", a.proc, b.proc)
	}
}

func TestJournalCutAcrossOSProcesses(t *testing.T) {
	if os.Getenv("OCR_SERVE_JOURNAL_HELPER") == "1" {
		runJournalHelper()
		return
	}
	ref := ghRef(8)
	for _, mode := range []string{"crash", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			runJournalHelperProcess(t, dir, mode)
			j, err := openJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			got := j.pending()
			if len(got) != 1 || got[0].Running != (mode == "crash") {
				t.Fatalf("saved %+v", got)
			}
			_, kept, err := j.put(ref, got[0].roundJob(), "test")
			if err != nil || kept.Cuts != 1 || kept.StartedUnixNano != 0 {
				t.Fatalf("resume %+v err %v", kept, err)
			}
		})
	}
}

func runJournalHelperProcess(t *testing.T, dir, mode string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestJournalCutAcrossOSProcesses$")
	cmd.Env = append(os.Environ(),
		"OCR_SERVE_JOURNAL_HELPER=1",
		"OCR_SERVE_JOURNAL_DIR="+dir,
		"OCR_SERVE_JOURNAL_MODE="+mode,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper %s: %v\n%s", mode, err, out)
	}
}

func runJournalHelper() {
	dir := os.Getenv("OCR_SERVE_JOURNAL_DIR")
	j, err := openJournal(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ref := ghRef(8)
	token, _, err := j.put(ref, roundJob{n: 1, scope: state.ScopeFull}, "test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if os.Getenv("OCR_SERVE_JOURNAL_MODE") == "shutdown" {
		if _, err := j.noteShutdown(ref, token); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	os.Exit(0)
}

func openTestJournal(t *testing.T) *journal {
	t.Helper()
	j, err := openJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func reopenJournal(t *testing.T, j *journal) *journal {
	t.Helper()
	j2, err := openJournal(filepath.Dir(j.path))
	if err != nil {
		t.Fatal(err)
	}
	return j2
}

func TestFailedRoundReusesCheckpointsOnRetry(t *testing.T) {
	finding := model.LlmComment{Path: "a.go", StartLine: 1, EndLine: 1, Severity: "high", Category: "bug", Content: "nil deref"}

	t.Run("fix failure reuses finished review on retry and clears after success", func(t *testing.T) {
		h := newHarness(t)
		ref := glRef(10)
		h.gl.setPR(openPR(ref, sha1))
		h.rev.set(func(_ context.Context, _ review.Request) (review.Result, error) {
			return review.Result{Terminal: review.TerminalComplete, Findings: []model.LlmComment{finding}}, nil
		})
		h.fix.set(func(_ context.Context, _ review.FixRequest) (review.FixResult, error) {
			return review.FixResult{}, errors.New("provider 503")
		})

		h.opened(ref)
		h.drain()
		if got := lastStatus(h.gl); got != "1:failure" {
			t.Fatalf("status after fix failure %q", got)
		}
		if _, err := os.Stat(filepath.Join(h.srv.roundCheckpoints(ref, 1), reviewFile)); err != nil {
			t.Fatalf("review checkpoint removed after fix failure: %v", err)
		}

		// Retrying with /review skips the finished review stage and runs only fix.
		h.fix.set(func(_ context.Context, _ review.FixRequest) (review.FixResult, error) {
			return review.FixResult{Dropped: []review.Dropped{{ID: "F1", Title: "False positive", Reason: "guarded"}}}, nil
		})
		h.command(ref, "/review", "c1")
		h.drain()
		if got := lastStatus(h.gl); got != "1:success" {
			t.Fatalf("status after retry %q", got)
		}
		if n := len(h.rev.requests()); n != 1 {
			t.Fatalf("retry ran the review again (%d total review calls)", n)
		}
		if !h.logs.has("review reused from a checkpoint") {
			t.Fatal("retry did not log checkpoint reuse")
		}
		if _, err := os.Stat(h.srv.prCheckpoints(ref)); !os.IsNotExist(err) {
			t.Fatalf("checkpoints left after retry succeeded: %v", err)
		}

		// A fresh /review after success runs the review again.
		h.command(ref, "/review", "c2")
		h.drain()
		if n := len(h.rev.requests()); n != 2 {
			t.Fatalf("fresh review after success did not run (%d total review calls)", n)
		}
	})

	t.Run("partial review and failed review keep batch checkpoints without saving review.json", func(t *testing.T) {
		h := newHarness(t)
		ref := glRef(11)
		h.gl.setPR(openPR(ref, sha1))
		var sawBatchFile bool
		calls := 0
		h.rev.set(func(_ context.Context, req review.Request) (review.Result, error) {
			calls++
			batchFile := filepath.Join(req.Checkpoint, "batch-test.json")
			if calls == 1 {
				if err := os.MkdirAll(req.Checkpoint, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(batchFile, []byte(`{"findings":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
				return review.Result{Terminal: review.TerminalPartial, Findings: []model.LlmComment{finding}}, nil
			}
			_, err := os.Stat(batchFile)
			sawBatchFile = err == nil
			return review.Result{Terminal: review.TerminalComplete}, nil
		})
		// The fix run succeeds, so the round finishes without a stage error.
		h.fix.set(func(_ context.Context, _ review.FixRequest) (review.FixResult, error) {
			return review.FixResult{Dropped: []review.Dropped{{ID: "F1", Title: "False positive", Reason: "guarded"}}}, nil
		})

		h.opened(ref)
		h.drain()
		if _, err := os.Stat(filepath.Join(h.srv.roundCheckpoints(ref, 1), reviewFile)); !os.IsNotExist(err) {
			t.Fatalf("partial review must not save review.json: %v", err)
		}
		if _, err := os.Stat(filepath.Join(h.srv.roundCheckpoints(ref, 1), "batch-test.json")); err != nil {
			t.Fatalf("partial round that finished removed its batch checkpoints: %v", err)
		}

		h.command(ref, "/review", "c1")
		h.drain()
		if calls != 2 || !sawBatchFile {
			t.Fatalf("retry calls=%d sawBatchFile=%v", calls, sawBatchFile)
		}
		if _, err := os.Stat(h.srv.prCheckpoints(ref)); !os.IsNotExist(err) {
			t.Fatalf("checkpoints left after the complete retry: %v", err)
		}
	})

	t.Run("changed head or effort does not reuse saved review", func(t *testing.T) {
		h := newHarness(t)
		ref := glRef(12)
		h.gl.setPR(openPR(ref, sha1))
		h.rev.set(func(_ context.Context, _ review.Request) (review.Result, error) {
			return review.Result{Terminal: review.TerminalComplete, Findings: []model.LlmComment{finding}}, nil
		})
		h.fix.set(func(_ context.Context, _ review.FixRequest) (review.FixResult, error) {
			return review.FixResult{}, errors.New("provider error")
		})

		h.opened(ref)
		h.drain()

		// Different effort does not reuse the saved review.
		h.command(ref, "/review --effort=high", "c1")
		h.drain()
		if n := len(h.rev.requests()); n != 2 {
			t.Fatalf("different effort reused checkpoint (%d review calls)", n)
		}

		// New head SHA does not reuse the saved review.
		h.gl.setPR(openPR(ref, sha2))
		h.fix.set(func(_ context.Context, _ review.FixRequest) (review.FixResult, error) {
			return review.FixResult{Dropped: []review.Dropped{{ID: "F1", Title: "False positive", Reason: "guarded"}}}, nil
		})
		h.command(ref, "/review --effort=high", "c2")
		h.drain()
		if n := len(h.rev.requests()); n != 3 {
			t.Fatalf("new head reused checkpoint (%d review calls)", n)
		}
	})
}
