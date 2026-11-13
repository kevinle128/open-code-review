//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/agentcli"
	"github.com/alibaba/open-code-review/internal/serve/clireview"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

type staticSelector struct{ sel clireview.Selection }

func (s staticSelector) Select(context.Context, review.Request) (clireview.Selection, error) {
	return s.sel, nil
}

func cliGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.email=ocr@example.test", "-c", "user.name=ocr",
		"-c", "commit.gpgsign=false", "-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestCLIEngineFindingsBecomeThreads runs an automatic round through the
// serve runner with the CLI engine and a fake codex executable that answers
// with two findings.
func TestCLIEngineFindingsBecomeThreads(t *testing.T) {
	repo := t.TempDir()
	cliGit(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cliGit(t, repo, "add", ".")
	cliGit(t, repo, "commit", "-q", "-m", "base")
	base := cliGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nfunc A() {}\nfunc B() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cliGit(t, repo, "commit", "-q", "-am", "change")
	head := cliGit(t, repo, "rev-parse", "HEAD")

	answer := filepath.Join(t.TempDir(), "answer.json")
	if err := os.WriteFile(answer, []byte(`{"summary": "Verdict: Request changes", "findings": [
 {"path": "a.go", "line": 3, "end_line": null, "severity": "high", "category": "bug",
  "title": "A ignores errors", "body": "Return the error.", "suggestion": null},
 {"path": "a.go", "line": 4, "end_line": null, "severity": "low", "category": "style",
  "title": "B is unused", "body": "Remove B.", "suggestion": null}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
cat >/dev/null
out=""; prev=""
for a in "$@"; do if [ "$prev" = "-o" ]; then out="$a"; fi; prev="$a"; done
test -f "$CODEX_HOME/../input/diff.patch" || exit 7
cp ` + answer + ` "$out"
`
	if err := os.WriteFile(codex, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	skillDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(skillDir, "skills", "ak-review-pr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, clireview.SkillFile), []byte("# skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	homeRoot := filepath.Join(t.TempDir(), "cli-home")
	homes, err := agentcli.NewHomes(homeRoot)
	if err != nil {
		t.Fatal(err)
	}
	engine := &clireview.Reviewer{
		Selector: staticSelector{clireview.Selection{Base: base, Head: head, Paths: []string{"a.go"}, Language: "English"}},
		CLI:      agentcli.Codex{Bin: codex, AuthFile: "/host/auth.json"},
		Homes:    homes, SkillDir: skillDir,
	}

	h := newHarness(t)
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	// The fake workspace has no files; the review reads the real repository.
	h.rev.set(func(ctx context.Context, req review.Request) (review.Result, error) {
		req.RepoDir = repo
		return engine.Review(ctx, req)
	})
	h.auto(ref, state.TriggerOpened)

	posted := h.gh.postedList()
	if len(posted) != 1 || len(posted[0]) != 2 {
		t.Fatalf("posted %+v\nlogs:\n%s", posted, h.logs.String())
	}
	if posted[0][0].Path != "a.go" || posted[0][0].StartLine != 3 || posted[0][0].Severity != "high" ||
		posted[0][1].StartLine != 4 || !strings.Contains(posted[0][1].Content, "**B is unused**") {
		t.Fatalf("findings %+v", posted[0])
	}
	m := h.gh.marker(t, ref)
	if m.Auto != 1 || len(m.Runs) != 1 || len(m.Runs[0].IDs) != 2 {
		t.Fatalf("marker %+v", m)
	}
	mustContain(t, h.gh.summary(ref), h.loc.Text("summary.open", "count", "2"))
	if entries, _ := os.ReadDir(homeRoot); len(entries) != 0 {
		t.Fatal("run dir left after the review")
	}
}
