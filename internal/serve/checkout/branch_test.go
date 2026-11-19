// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

var botIdent = forge.Identity{Name: "ocr[bot]", Email: "1+ocr[bot]@users.noreply.github.com"}

func branchSpec(r remote) forge.CloneSpec {
	return forge.CloneSpec{URL: r.url, HeadRef: "refs/heads/feature", BaseRef: "refs/heads/main"}
}

func fetchBranch(t *testing.T, r remote) *workspace {
	t.Helper()
	ws, _ := acquire(t, newManager(t, 1))
	if err := ws.FetchBranch(context.Background(), branchSpec(r), r.head, r.base); err != nil {
		t.Fatalf("FetchBranch: %v", err)
	}
	return ws
}

func TestFetchBranchChecksOutTheBranch(t *testing.T) {
	r := newRemote(t)
	ws := fetchBranch(t, r)
	if got := git(t, ws.Dir(), "symbolic-ref", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("HEAD = %s", got)
	}
	if got := git(t, ws.Dir(), "rev-parse", "HEAD"); got != r.head {
		t.Fatalf("HEAD at %s, want %s", got, r.head)
	}
	files, err := ws.ChangedFiles(context.Background(), r.base)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(files, []string{"a.txt", "b.txt"}) {
		t.Fatalf("changed files %v", files)
	}
	// A base the checkout lacks falls back to the fetched base branch.
	if files, err := ws.ChangedFiles(context.Background(), r.sideTip); err != nil || len(files) != 2 {
		t.Fatalf("fallback files %v err %v", files, err)
	}
}

func TestFetchBranchRejectsInput(t *testing.T) {
	r := newRemote(t)
	ws, _ := acquire(t, newManager(t, 1))
	spec := branchSpec(r)
	spec.HeadRef = "refs/pull/1/head"
	if err := ws.FetchBranch(context.Background(), spec, r.head, r.base); err == nil {
		t.Fatal("pull ref accepted as a branch")
	}
	if err := ws.FetchBranch(context.Background(), branchSpec(r), r.first, r.base); err == nil {
		t.Fatal("moved head accepted")
	}
	if _, err := ws.ChangedFiles(context.Background(), r.base); err == nil {
		t.Fatal("changed files without a branch")
	}
	if _, _, err := ws.Commit(context.Background(), botIdent, "x"); err == nil {
		t.Fatal("commit without a branch")
	}
	if err := ws.Push(context.Background(), branchSpec(r), r.head); err == nil {
		t.Fatal("push without a branch")
	}
}

func TestCommitAndPush(t *testing.T) {
	r := newRemote(t)
	ws := fetchBranch(t, r)
	ctx := context.Background()
	if sha, changed, err := ws.Commit(ctx, botIdent, "fix: nothing"); err != nil || changed || sha != "" {
		t.Fatalf("clean worktree: sha %q changed %v err %v", sha, changed, err)
	}
	// A hook the agent wrote must not run.
	hook := filepath.Join(ws.Dir(), ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, ws.Dir(), "a.txt", "fixed\n")
	write(t, ws.Dir(), "new.txt", "added\n")
	sha, changed, err := ws.Commit(ctx, botIdent, "fix: address review findings")
	if err != nil || !changed || len(sha) != 40 {
		t.Fatalf("commit: sha %q changed %v err %v", sha, changed, err)
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("pre-commit hook ran")
		}
	}
	if got := git(t, ws.Dir(), "log", "-1", "--format=%an <%ae>|%cn|%s"); got != "ocr[bot] <1+ocr[bot]@users.noreply.github.com>|ocr[bot]|fix: address review findings" {
		t.Fatalf("commit meta %q", got)
	}
	if got := git(t, ws.Dir(), "show", "--name-only", "--format=", "HEAD"); got != "a.txt\nnew.txt" {
		t.Fatalf("committed files %q", got)
	}
	if err := ws.Push(ctx, branchSpec(r), sha); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := git(t, r.dir, "rev-parse", "refs/heads/feature"); got != sha {
		t.Fatalf("remote branch at %s, want %s", got, sha)
	}
}

func TestPushRejectedWhenBranchMoved(t *testing.T) {
	r := newRemote(t)
	ws := fetchBranch(t, r)
	ctx := context.Background()
	write(t, ws.Dir(), "a.txt", "fixed\n")
	sha, _, err := ws.Commit(ctx, botIdent, "fix: x")
	if err != nil {
		t.Fatal(err)
	}
	// Someone pushes to the branch while the fix runs.
	git(t, r.dir, "update-ref", "refs/heads/feature", r.sideTip)
	err = ws.Push(ctx, branchSpec(r), sha)
	if err == nil || !strings.Contains(err.Error(), "git push") {
		t.Fatalf("non-fast-forward push: %v", err)
	}
	if got := git(t, r.dir, "rev-parse", "refs/heads/feature"); got != r.sideTip {
		t.Fatalf("remote branch changed to %s", got)
	}
}

func TestPushValidatesInput(t *testing.T) {
	r := newRemote(t)
	ws := fetchBranch(t, r)
	ctx := context.Background()
	bad := []struct {
		name string
		spec forge.CloneSpec
		sha  string
	}{
		{"short sha", branchSpec(r), "abc"},
		{"other branch", forge.CloneSpec{URL: r.url, HeadRef: "refs/heads/main"}, r.head},
		{"bad url", forge.CloneSpec{URL: "-oops", HeadRef: "refs/heads/feature"}, r.head},
		{"multiline header", forge.CloneSpec{URL: r.url, HeadRef: "refs/heads/feature", AuthHeader: "a\nb"}, r.head},
	}
	for _, tc := range bad {
		if err := ws.Push(ctx, tc.spec, tc.sha); err == nil {
			t.Errorf("%s accepted", tc.name)
		}
	}
}

func TestCommitRefusesTamperedCheckout(t *testing.T) {
	ctx := context.Background()
	t.Run("config", func(t *testing.T) {
		ws := fetchBranch(t, newRemote(t))
		cfg := filepath.Join(ws.Dir(), ".git", "config")
		b, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, append(b, []byte("[url \"https://evil.test/\"]\n\tinsteadOf = file://\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		write(t, ws.Dir(), "a.txt", "x\n")
		if _, _, err := ws.Commit(ctx, botIdent, "fix: x"); err == nil || !strings.Contains(err.Error(), "config") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("agent commit", func(t *testing.T) {
		ws := fetchBranch(t, newRemote(t))
		write(t, ws.Dir(), "a.txt", "x\n")
		git(t, ws.Dir(), "commit", "-q", "-am", "by the agent")
		if _, _, err := ws.Commit(ctx, botIdent, "fix: x"); err == nil || !strings.Contains(err.Error(), "moved") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("other branch", func(t *testing.T) {
		ws := fetchBranch(t, newRemote(t))
		git(t, ws.Dir(), "checkout", "-q", "-b", "elsewhere")
		if _, _, err := ws.Commit(ctx, botIdent, "fix: x"); err == nil || !strings.Contains(err.Error(), "no longer") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("identity", func(t *testing.T) {
		ws := fetchBranch(t, newRemote(t))
		for _, id := range []forge.Identity{{}, {Name: "a", Email: "b>\nc"}} {
			if _, _, err := ws.Commit(ctx, id, "fix: x"); err == nil {
				t.Fatalf("identity %+v accepted", id)
			}
		}
	})
	t.Run("missing config", func(t *testing.T) {
		ws := fetchBranch(t, newRemote(t))
		if err := os.Remove(filepath.Join(ws.Dir(), ".git", "config")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ws.Commit(ctx, botIdent, "fix: x"); err == nil {
			t.Fatal("missing config accepted")
		}
	})
}

func TestCommitDiff(t *testing.T) {
	r := newRemote(t)
	ws := fetchBranch(t, r)
	ctx := context.Background()
	write(t, ws.Dir(), "a.txt", "fixed\n")
	sha, changed, err := ws.Commit(ctx, botIdent, "fix: a")
	if err != nil || !changed {
		t.Fatalf("commit: %v %v", changed, err)
	}
	diff, err := ws.CommitDiff(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "+++ b/a.txt") || !strings.Contains(diff, "+fixed") || !strings.Contains(diff, "@@ ") {
		t.Fatalf("diff:\n%s", diff)
	}
	if _, err := ws.CommitDiff(ctx, "HEAD"); err == nil {
		t.Fatal("a ref name was accepted")
	}
	if _, err := ws.CommitDiff(ctx, strings.Repeat("0", 40)); err == nil {
		t.Fatal("a missing commit gave a diff")
	}
	// Commit refuses a branch that moved since the fetch, so the large
	// commit needs a fresh checkout.
	ws = fetchBranch(t, r)
	big := strings.Repeat("line of a large generated file\n", maxCommitDiff/30+100)
	write(t, ws.Dir(), "big.txt", big)
	sha, _, err = ws.Commit(ctx, botIdent, "fix: big")
	if err != nil {
		t.Fatal(err)
	}
	diff, err = ws.CommitDiff(ctx, sha)
	if err != nil || len(diff) > maxCommitDiff || !strings.HasSuffix(diff, "\n") {
		t.Fatalf("large diff: %d bytes, err %v", len(diff), err)
	}
}

// pushOther adds a commit to the remote branch the way another person
// would: from a clone of their own.
func pushOther(t *testing.T, r remote, file, body string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(clone), "clone", "-q", "--branch", "feature", r.url, clone)
	write(t, clone, file, body)
	git(t, clone, "add", ".")
	git(t, clone, "commit", "-q", "-m", "other work")
	git(t, clone, "push", "-q", "origin", "feature")
	return git(t, clone, "rev-parse", "HEAD")
}

func TestRebaseMovesTheFixOntoTheMovedBranch(t *testing.T) {
	r := newRemote(t)
	ws := fetchBranch(t, r)
	ctx := context.Background()
	write(t, ws.Dir(), "new.txt", "fix\n")
	fix, _, err := ws.Commit(ctx, botIdent, "fix: x")
	if err != nil {
		t.Fatal(err)
	}
	moved := pushOther(t, r, "other.txt", "other\n")
	err = ws.Push(ctx, branchSpec(r), fix)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("push on a moved branch: %v", err)
	}
	rb, err := ws.Rebase(ctx, botIdent, branchSpec(r), fix)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if rb.Head != moved || rb.Fix == "" || rb.Fix == fix {
		t.Fatalf("rebased %+v, moved head %s, old fix %s", rb, moved, fix)
	}
	if got := git(t, ws.Dir(), "rev-parse", "HEAD^"); got != moved {
		t.Fatalf("fix parent %s, want %s", got, moved)
	}
	if got := git(t, ws.Dir(), "symbolic-ref", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("HEAD at %s", got)
	}
	if got := git(t, ws.Dir(), "log", "-1", "--format=%an|%cn|%s"); got != "ocr[bot]|ocr[bot]|fix: x" {
		t.Fatalf("commit meta %q", got)
	}
	if got := git(t, ws.Dir(), "status", "--porcelain"); got != "" {
		t.Fatalf("dirty checkout: %s", got)
	}
	// A lease on a tip that is not the remote's refuses; the fetched tip works.
	if err := ws.PushLease(ctx, branchSpec(r), rb.Fix, r.head); err == nil {
		t.Fatal("push accepted with a lease on an old tip")
	}
	if err := ws.PushLease(ctx, branchSpec(r), rb.Fix, rb.Head); err != nil {
		t.Fatalf("PushLease: %v", err)
	}
	if got := git(t, r.dir, "rev-parse", "refs/heads/feature"); got != rb.Fix {
		t.Fatalf("remote branch at %s, want %s", got, rb.Fix)
	}
}

func TestPushLeaseRefusesWhenTheBranchMovedAgain(t *testing.T) {
	r := newRemote(t)
	ws := fetchBranch(t, r)
	ctx := context.Background()
	write(t, ws.Dir(), "new.txt", "fix\n")
	fix, _, err := ws.Commit(ctx, botIdent, "fix: x")
	if err != nil {
		t.Fatal(err)
	}
	pushOther(t, r, "other.txt", "other\n")
	rb, err := ws.Rebase(ctx, botIdent, branchSpec(r), fix)
	if err != nil {
		t.Fatal(err)
	}
	third := pushOther(t, r, "third.txt", "third\n")
	if err := ws.PushLease(ctx, branchSpec(r), rb.Fix, rb.Head); err == nil {
		t.Fatal("push overwrote a commit pushed after the fetch")
	}
	if got := git(t, r.dir, "rev-parse", "refs/heads/feature"); got != third {
		t.Fatalf("remote branch at %s, want %s", got, third)
	}
}

func TestRebaseConflictRestoresTheFix(t *testing.T) {
	r := newRemote(t)
	ws := fetchBranch(t, r)
	ctx := context.Background()
	write(t, ws.Dir(), "a.txt", "fixed\n")
	fix, _, err := ws.Commit(ctx, botIdent, "fix: x")
	if err != nil {
		t.Fatal(err)
	}
	moved := pushOther(t, r, "a.txt", "rewritten\n")
	rb, err := ws.Rebase(ctx, botIdent, branchSpec(r), fix)
	if !errors.Is(err, review.ErrRebaseConflict) {
		t.Fatalf("Rebase error %v, want a conflict", err)
	}
	if rb.Head != moved || rb.Fix != "" {
		t.Fatalf("rebased %+v", rb)
	}
	if got := git(t, ws.Dir(), "rev-parse", "HEAD"); got != fix {
		t.Fatalf("HEAD at %s, want the fix %s", got, fix)
	}
	if got := git(t, ws.Dir(), "status", "--porcelain"); got != "" {
		t.Fatalf("conflict left the checkout dirty: %s", got)
	}
	if got := git(t, ws.Dir(), "symbolic-ref", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("HEAD at %s", got)
	}
}

func TestRebaseValidatesInput(t *testing.T) {
	r := newRemote(t)
	ctx := context.Background()
	if _, err := (&workspace{}).Rebase(ctx, botIdent, branchSpec(r), r.head); err == nil {
		t.Fatal("rebase without a branch")
	}
	ws := fetchBranch(t, r)
	if _, err := ws.Rebase(ctx, botIdent, branchSpec(r), "abc"); err == nil {
		t.Fatal("short sha accepted")
	}
	if _, err := ws.Rebase(ctx, forge.Identity{Name: "x"}, branchSpec(r), r.head); err == nil {
		t.Fatal("empty identity accepted")
	}
	if _, err := ws.Rebase(ctx, botIdent, forge.CloneSpec{URL: r.url, HeadRef: "refs/heads/main"}, r.head); err == nil {
		t.Fatal("other branch accepted")
	}
	if err := ws.PushLease(ctx, branchSpec(r), r.head, "abc"); err == nil {
		t.Fatal("short lease accepted")
	}
	gone := branchSpec(r)
	gone.HeadRef = "refs/heads/feature"
	git(t, r.dir, "update-ref", "-d", "refs/heads/feature")
	if _, err := ws.Rebase(ctx, botIdent, gone, r.head); err == nil || errors.Is(err, review.ErrRebaseConflict) {
		t.Fatalf("deleted branch: %v", err)
	}
	cfg := filepath.Join(ws.Dir(), ".git", "config")
	f, _ := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("[url \"https://evil.test/\"]\n\tinsteadOf = file://\n")
	_ = f.Close()
	if _, err := ws.Rebase(ctx, botIdent, branchSpec(r), r.head); err == nil || !strings.Contains(err.Error(), "config changed") {
		t.Fatalf("tampered config: %v", err)
	}
}

// TestPushEnvForcesEnglish: the round reads git's push errors as English
// text, so a localized host must not translate them.
func TestPushEnvForcesEnglish(t *testing.T) {
	t.Setenv("LC_ALL", "vi_VN.UTF-8")
	t.Setenv("LANGUAGE", "vi")
	r := newRemote(t)
	ws := fetchBranch(t, r)
	env, err := ws.pushEnv(branchSpec(r))
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	if last["LC_ALL"] != "C" || last["LANGUAGE"] != "" {
		t.Fatalf("LC_ALL=%q LANGUAGE=%q", last["LC_ALL"], last["LANGUAGE"])
	}
}
