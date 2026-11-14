// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package clireview

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/agentcli"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.email=ocr@example.test", "-c", "user.name=ocr",
		"-c", "commit.gpgsign=false", "-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type repo struct {
	dir, base, head string
}

func newRepo(t *testing.T) repo {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.go", "package a\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	write(t, dir, "a.go", "package a\n\nfunc A() {}\n")
	write(t, dir, "secret.env", "TOKEN=x\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "change")
	return repo{dir: dir, base: base, head: git(t, dir, "rev-parse", "HEAD")}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakeSelector struct {
	sel Selection
	err error
}

func (s fakeSelector) Select(context.Context, review.Request) (Selection, error) { return s.sel, s.err }

// fakeCLI answers with msg and keeps what the agent would have seen.
type fakeCLI struct {
	msg    string
	err    error
	inv    agentcli.Invocation
	prJSON string
	diff   string
}

func (*fakeCLI) Name() string { return "fake" }

func (c *fakeCLI) Exec(_ context.Context, inv agentcli.Invocation) (string, error) {
	c.inv = inv
	b, _ := os.ReadFile(filepath.Join(inv.Run.Input, "pr.json"))
	c.prJSON = string(b)
	b, _ = os.ReadFile(filepath.Join(inv.Run.Input, "diff.patch"))
	c.diff = string(b)
	return c.msg, c.err
}

func newReviewer(t *testing.T, sel Selector, cli agentcli.CLI) (*Reviewer, string) {
	t.Helper()
	skillDir := t.TempDir()
	write(t, skillDir, SkillFile, "# ak-review-pr\n\n## Service mode (ocr serve)\n")
	writeReferences(t, skillDir, reviewReferences)
	root := filepath.Join(t.TempDir(), "cli-home")
	homes, err := agentcli.NewHomes(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Reviewer{Selector: sel, CLI: cli, Homes: homes, SkillDir: skillDir}, root
}

// writeReferences writes a fake reference file for each name; its text is
// refText(name).
func writeReferences(t *testing.T, skillDir string, names []string) {
	t.Helper()
	for _, n := range names {
		write(t, skillDir, filepath.Join(filepath.Dir(SkillFile), "references", n), refText(n)+"\n")
	}
}

func refText(name string) string { return "Reference body of " + name + "." }

func runDirsLeft(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

const twoFindings = "```json\n" + `{"summary": "Summary: ok\nRisk: low\nVerdict: Comment", "findings": [
 {"path": "./a.go", "line": 3, "end_line": null, "severity": "High", "category": "bug",
  "title": "Missing return", "body": "A returns nothing.", "suggestion": "func A() error { return nil }"},
 {"path": "b/a.go", "line": 1, "end_line": 3, "severity": "low", "category": "style",
  "title": "", "body": "Add a doc comment.", "suggestion": null},
 {"path": "secret.env", "line": 1, "end_line": null, "severity": "critical", "category": "security",
  "title": "Token", "body": "A token is committed.", "suggestion": null},
 {"path": "a.go", "line": 2, "end_line": null, "severity": "low", "category": "other",
  "title": " ", "body": " ", "suggestion": null}
]}` + "\n```"

func TestReviewMapsFindings(t *testing.T) {
	r := newRepo(t)
	cli := &fakeCLI{msg: twoFindings}
	sel := Selection{Base: r.base, Head: r.head, Paths: []string{"a.go"},
		Skipped: map[string]int{"secret": 1}, Background: "Payments service.\n\nWrite in Vietnamese.", Language: "Vietnamese"}
	rev, root := newReviewer(t, fakeSelector{sel: sel}, cli)
	req := review.Request{RepoDir: r.dir, From: r.base, To: r.head, PR: forge.PR{
		Ref:   forge.PRRef{Platform: forge.GitHub, Repo: "acme/pay", Number: 7},
		Title: "Add A", Description: "Adds A.", BaseSHA: r.base, HeadSHA: r.head,
	}}
	res, err := rev.Review(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Terminal != review.TerminalComplete || res.Base != r.base ||
		res.Coverage.Total != 1 || res.Coverage.Reviewed != 1 || res.Coverage.Skipped["secret"] != 1 {
		t.Fatalf("result %+v", res)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("findings %+v", res.Findings)
	}
	f0, f1 := res.Findings[0], res.Findings[1]
	if f0.Path != "a.go" || f0.StartLine != 3 || f0.EndLine != 3 || f0.Severity != "high" || f0.Category != "bug" ||
		f0.Content != "**Missing return**\n\nA returns nothing.\n\nfunc A() error { return nil }" || f0.SuggestionCode != "" {
		t.Fatalf("first finding %+v", f0)
	}
	if f1.Path != "a.go" || f1.StartLine != 1 || f1.EndLine != 3 || f1.Content != "Add a doc comment." {
		t.Fatalf("second finding %+v", f1)
	}
	if len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], `"secret.env"`) {
		t.Fatalf("warnings %v", res.Warnings)
	}

	var pr prInput
	if err := json.Unmarshal([]byte(cli.prJSON), &pr); err != nil {
		t.Fatal(err)
	}
	want := prInput{Platform: "github", Repository: "acme/pay", Number: 7, Title: "Add A", Description: "Adds A.",
		BaseSHA: r.base, HeadSHA: r.head, Mode: ModeFull, Files: []string{"a.go"}, Language: "Vietnamese"}
	if strings.Join(pr.Files, ",") != "a.go" || pr.Platform != want.Platform || pr.Repository != want.Repository ||
		pr.Number != 7 || pr.Title != want.Title || pr.Description != want.Description || pr.BaseSHA != r.base ||
		pr.HeadSHA != r.head || pr.Mode != ModeFull || pr.Language != "Vietnamese" {
		t.Fatalf("pr.json %+v", pr)
	}
	if !strings.Contains(cli.diff, "+func A() {}") || strings.Contains(cli.diff, "secret.env") {
		t.Fatalf("diff.patch\n%s", cli.diff)
	}

	skillDir := filepath.Join(rev.SkillDir, "skills", "ak-review-pr")
	inv := cli.inv
	if inv.RepoDir != r.dir || len(inv.ReadDirs) != 2 || inv.ReadDirs[0] != skillDir || inv.ReadDirs[1] != inv.Run.Input {
		t.Fatalf("invocation %+v", inv)
	}
	for _, want := range []string{"# ak-review-pr", "You run under `ocr serve`", "do not look for them on disk",
		"Run directory: `" + inv.Run.Input + "`", "Output language: Vietnamese.", "Payments service.", `"required": ["summary", "findings"]`} {
		if !strings.Contains(inv.Prompt, want) {
			t.Errorf("prompt misses %q", want)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal(inv.Schema, &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if runDirsLeft(t, root) != 0 {
		t.Fatal("run dir not removed")
	}
}

func TestReviewWithoutFilesIsSkipped(t *testing.T) {
	cli := &fakeCLI{}
	rev, _ := newReviewer(t, fakeSelector{sel: Selection{Base: "b", Skipped: map[string]int{"user_rule": 2}}}, cli)
	res, err := rev.Review(context.Background(), review.Request{})
	if err != nil || res.Terminal != review.TerminalSkipped || res.Base != "b" || res.Coverage.Skipped["user_rule"] != 2 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if cli.inv.Run != nil {
		t.Fatal("the CLI ran without files")
	}
}

func TestReviewFailures(t *testing.T) {
	r := newRepo(t)
	sel := Selection{Base: r.base, Head: r.head, Paths: []string{"a.go"}, Language: "English"}
	req := review.Request{RepoDir: r.dir, From: r.base, To: r.head}
	cases := []struct {
		name string
		sel  Selector
		cli  *fakeCLI
		edit func(*Reviewer)
		want string
	}{
		{"selector", fakeSelector{err: errors.New("bad ref")}, &fakeCLI{}, nil, "bad ref"},
		{"skill missing", fakeSelector{sel: sel}, &fakeCLI{}, func(rv *Reviewer) { rv.SkillDir = t.TempDir() }, "read review skill"},
		{"diff", fakeSelector{sel: Selection{Base: "bad", Head: r.head, Paths: []string{"a.go"}}}, &fakeCLI{}, nil, "commit ids"},
		{"cli", fakeSelector{sel: sel}, &fakeCLI{err: errors.New("usage limit")}, nil, "fake review: usage limit"},
		{"malformed", fakeSelector{sel: sel}, &fakeCLI{msg: "I found no issues."}, nil, "no JSON object"},
		{"wrong shape", fakeSelector{sel: sel}, &fakeCLI{msg: `{"summary": "x", "findings": {"a": 1}}`}, nil, "does not match"},
		{"no summary", fakeSelector{sel: sel}, &fakeCLI{msg: `{"findings": []}`}, nil, "lacks summary or findings"},
		{"no findings", fakeSelector{sel: sel}, &fakeCLI{msg: `{"summary": "x"}`}, nil, "lacks summary or findings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rev, root := newReviewer(t, tc.sel, tc.cli)
			if tc.edit != nil {
				tc.edit(rev)
			}
			res, err := rev.Review(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) || res.Terminal != review.TerminalFailed {
				t.Fatalf("res %+v err %v", res, err)
			}
			if entries, _ := os.ReadDir(root); len(entries) != 0 {
				t.Fatal("run dir not removed")
			}
		})
	}
}

func TestReviewRunDirErrors(t *testing.T) {
	r := newRepo(t)
	sel := Selection{Base: r.base, Head: r.head, Paths: []string{"a.go"}}
	rev, root := newReviewer(t, fakeSelector{sel: sel}, &fakeCLI{msg: `{"summary":"","findings":[]}`})
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := rev.Review(context.Background(), review.Request{RepoDir: r.dir}); err == nil ||
		!strings.Contains(err.Error(), "create run dir") {
		t.Fatalf("err %v", err)
	}
}

func TestMode(t *testing.T) {
	pr := forge.PR{BaseSHA: "base"}
	cases := []struct {
		req  review.Request
		want string
	}{
		{review.Request{Commit: "c", PR: pr}, ModeCommit},
		{review.Request{From: "base", To: "h", PR: pr}, ModeFull},
		{review.Request{From: "r1", To: "h", PR: pr}, ModeRange},
		{review.Request{From: "", To: "h"}, ModeRange},
	}
	for _, tc := range cases {
		if got := mode(tc.req); got != tc.want {
			t.Errorf("mode(%+v) = %s, want %s", tc.req, got, tc.want)
		}
	}
}

func TestWriteInputUsesProjectIDWithoutRepoPath(t *testing.T) {
	r := newRepo(t)
	dir := t.TempDir()
	req := review.Request{RepoDir: r.dir, Commit: r.head,
		PR: forge.PR{Ref: forge.PRRef{Platform: forge.GitLab, ProjectID: 42, Number: 5}}}
	if err := writeInput(context.Background(), dir, req, Selection{Base: r.base, Head: r.head, Paths: []string{"a.go"}}); err != nil {
		t.Fatal(err)
	}
	var pr prInput
	b, _ := os.ReadFile(filepath.Join(dir, "pr.json"))
	if err := json.Unmarshal(b, &pr); err != nil || pr.Repository != "42" || pr.Platform != "gitlab" || pr.Mode != ModeCommit {
		t.Fatalf("pr.json %s err %v", b, err)
	}
	if err := writeInput(context.Background(), filepath.Join(dir, "gone"), req, Selection{}); err == nil ||
		!strings.Contains(err.Error(), "write pr.json") {
		t.Fatalf("err %v", err)
	}
}

func TestBuildPromptWithoutLanguageOrBackground(t *testing.T) {
	p := buildPrompt("skill", "/in", Selection{}, false)
	if strings.Contains(p, "Output language") || strings.Contains(p, "Review background") {
		t.Fatalf("prompt\n%s", p)
	}
}

func TestToCommentsMapsPRLevelFindings(t *testing.T) {
	repoDir := t.TempDir()
	write(t, repoDir, "a.go", "package a\n")
	write(t, repoDir, "Makefile", "all:\n")
	end := 9
	fs := []finding{
		{Path: "pr.json", Line: 1, EndLine: &end, Title: "PR body", Body: "No test plan.", Severity: "Medium", Category: "documentation"},
		{Path: "", Line: 0, Body: "Missing summary."},
		{Path: "./pr.json", Line: 3, Body: "Bad title."},
		{Path: "/home/ocr/cli-home/run-1/input/pr.json", Line: 2, Body: "No risk section."},
		{Path: "PR description", Line: 1, Body: "Too short."},
		{Path: "x/y.go", Line: 1, Body: "Not in the repo."},
		{Path: "Makefile", Line: 1, Body: "An unselected repo file."},
		{Path: "notes.txt", Line: 1, Body: "A missing file with an extension."},
	}
	got, dropped := toComments(fs, []string{"a.go"}, repoDir)
	if len(got) != 5 || len(dropped) != 3 {
		t.Fatalf("comments %+v dropped %v", got, dropped)
	}
	for _, c := range got {
		if c.Path != "" || c.StartLine != 0 || c.EndLine != 0 {
			t.Fatalf("PR-level comment %+v", c)
		}
	}
	if got[0].Content != "**PR body**\n\nNo test plan." || got[0].Severity != "medium" || got[0].Category != "documentation" {
		t.Fatalf("first comment %+v", got[0])
	}
	for i, want := range []string{`"x/y.go"`, `"Makefile"`, `"notes.txt"`} {
		if !strings.Contains(dropped[i], want) {
			t.Fatalf("dropped %v", dropped)
		}
	}
}

func TestPromptAsksForAnEmptyPathOnPRLevelFindings(t *testing.T) {
	p := buildPrompt("skill", "/in", Selection{}, false)
	if !strings.Contains(p, "`path` is `\"\"` and `line` is 0") {
		t.Fatalf("prompt\n%s", p)
	}
}
