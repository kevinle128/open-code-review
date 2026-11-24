// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package diff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/gitcmd"
)

func TestStatsDiffMatchesFull(t *testing.T) {
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-q", "-b", "main")
	runGitTest(t, repo, "config", "user.email", "test@example.com")
	runGitTest(t, repo, "config", "user.name", "Test User")
	runGitTest(t, repo, "config", "commit.gpgsign", "false")
	writeRepoFile(t, repo, "keep.txt", "one\ntwo\n")
	writeRepoFile(t, repo, "gone.txt", "bye\n")
	writeRepoFile(t, repo, "pic.bin", string([]byte{0, 1, 2, 3}))
	runGitTest(t, repo, "add", ".")
	runGitTest(t, repo, "commit", "-q", "-m", "base")
	base := gitRev(t, repo)
	writeRepoFile(t, repo, "keep.txt", "one\nchanged\n")
	if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, repo, "pic.bin", string([]byte{0, 1, 2, 9, 0}))
	runGitTest(t, repo, "add", "-A")
	runGitTest(t, repo, "commit", "-q", "-m", "change")
	head := gitRev(t, repo)

	full := NewProvider(repo, base, head, gitcmd.New(0))
	statsP := NewProvider(repo, base, head, gitcmd.New(0))
	statsP.EnableStats()
	fullSet, err := full.GetDiffSet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	statSet, err := statsP.GetDiffSet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fullSet.Included) == 0 || len(fullSet.Included) != len(statSet.Included) {
		t.Fatalf("included full %d stats %d", len(fullSet.Included), len(statSet.Included))
	}
	for i := range fullSet.Included {
		got, want := statSet.Included[i], fullSet.Included[i]
		if got.NewPath != want.NewPath || got.Insertions != want.Insertions || got.Deletions != want.Deletions || got.IsBinary != want.IsBinary || got.IsDeleted != want.IsDeleted {
			t.Fatalf("file %d stats %+v full %+v", i, got, want)
		}
		if got.Diff != "" || got.NewFileContent != "" {
			t.Fatalf("stats kept a body for %s", got.NewPath)
		}
		if got.StatBytes <= 0 || !got.TokensCounted {
			t.Fatalf("stats missing size for %s", got.NewPath)
		}
	}
}

func TestStatsDiffAllocatesLess(t *testing.T) {
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-q", "-b", "main")
	runGitTest(t, repo, "config", "user.email", "test@example.com")
	runGitTest(t, repo, "config", "user.name", "Test User")
	runGitTest(t, repo, "config", "commit.gpgsign", "false")
	writeRepoFile(t, repo, "seed.txt", "seed\n")
	runGitTest(t, repo, "add", ".")
	runGitTest(t, repo, "commit", "-q", "-m", "base")
	base := gitRev(t, repo)
	body := make([]byte, 80<<10)
	for i := range body {
		body[i] = 'a'
	}
	body[0] = '\n'
	for i := range 8 {
		writeRepoFile(t, repo, fmt.Sprintf("f%02d.txt", i), string(body))
	}
	runGitTest(t, repo, "add", ".")
	runGitTest(t, repo, "commit", "-q", "-m", "bulk")
	head := gitRev(t, repo)

	measure := func(stats bool) (bodies int, heap uint64) {
		t.Helper()
		p := NewProvider(repo, base, head, gitcmd.New(0))
		if stats {
			p.EnableStats()
		}
		set, err := p.GetDiffSet(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range set.Included {
			bodies += len(d.Diff) + len(d.NewFileContent)
		}
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		heap = m.HeapAlloc
		runtime.KeepAlive(set)
		return bodies, heap
	}
	statsBodies, statsHeap := measure(true)
	fullBodies, fullHeap := measure(false)
	t.Logf("retained patch+file bytes: stats %d, full %d; heap while held: stats %d, full %d", statsBodies, fullBodies, statsHeap, fullHeap)
	if statsBodies != 0 {
		t.Fatalf("stats kept %d body bytes", statsBodies)
	}
	if fullBodies < 8*(80<<10) {
		t.Fatalf("full kept %d body bytes, want the file contents", fullBodies)
	}
}

func TestStatsDiffKeepsDeadline(t *testing.T) {
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-q", "-b", "main")
	runGitTest(t, repo, "config", "user.email", "test@example.com")
	runGitTest(t, repo, "config", "user.name", "Test User")
	runGitTest(t, repo, "config", "commit.gpgsign", "false")
	writeRepoFile(t, repo, "a.txt", "one\n")
	runGitTest(t, repo, "add", ".")
	runGitTest(t, repo, "commit", "-q", "-m", "base")
	base := gitRev(t, repo)
	writeRepoFile(t, repo, "a.txt", "two\n")
	runGitTest(t, repo, "commit", "-q", "-am", "change")
	head := gitRev(t, repo)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	p := NewProvider(repo, base, head, gitcmd.New(0))
	p.EnableStats()
	if _, err := p.statsDiffSet(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want the deadline", err)
	}
}

func writeRepoFile(t *testing.T, repo, name, body string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRev(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
