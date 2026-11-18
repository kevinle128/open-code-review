// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
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
		return review.Result{}, ctx.Err()
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
	if err := saveReview(h2.srv.roundCheckpoints(ref, 1), savedReview{Head: sha1, Scope: state.ScopeFull,
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
	if tok, err := none.put(ghRef(1), roundJob{n: 1}); tok != "" || err != nil || none.remove(ghRef(1), "") != nil || none.pending() != nil {
		t.Fatal("a nil journal records nothing")
	}

	dir := t.TempDir()
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := j.put(ghRef(1), roundJob{n: 1, scope: state.ScopeFull})
	cur, _ := j.put(ghRef(1), roundJob{n: 2, scope: state.ScopeCommit, commit: sha1, fixCommit: true, effort: "high"})
	if _, err := j.put(glRef(2), roundJob{n: 1, scope: state.ScopeFull}); err != nil {
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
