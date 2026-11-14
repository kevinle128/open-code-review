// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package checkout

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
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
