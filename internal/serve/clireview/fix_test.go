// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package clireview

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/agentcli"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

// fixCLI answers with msg and keeps the inputs of the run.
type fixCLI struct {
	msg      string
	err      error
	inv      agentcli.Invocation
	prJSON   string
	findings string
}

func (*fixCLI) Name() string { return "fake" }

func (c *fixCLI) Exec(_ context.Context, inv agentcli.Invocation) (string, error) {
	c.inv = inv
	b, _ := os.ReadFile(filepath.Join(inv.Run.Input, "pr.json"))
	c.prJSON = string(b)
	b, _ = os.ReadFile(filepath.Join(inv.Run.Input, FindingsFile))
	c.findings = string(b)
	return c.msg, c.err
}

func newFixer(t *testing.T, cli agentcli.CLI) (*Fixer, string) {
	t.Helper()
	skillDir := t.TempDir()
	write(t, skillDir, SkillFile, "# ak-review-pr\n")
	writeReferences(t, skillDir, fixReferences)
	root := filepath.Join(t.TempDir(), "cli-home")
	homes, err := agentcli.NewHomes(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Fixer{CLI: cli, Homes: homes, SkillDir: skillDir, Language: "Vietnamese"}, root
}

func fixRequest() review.FixRequest {
	return review.FixRequest{
		RepoDir: "/work/slot-0",
		PR: forge.PR{Ref: forge.PRRef{Platform: forge.GitHub, Repo: "acme/app", Number: 12},
			Title: "Add A", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40)},
		Files: []string{"a.go"},
		Findings: []review.FixFinding{
			{ID: "f1", Path: "a.go", Line: 3, Severity: "high", Title: "Missing return", Body: "A returns nothing.",
				Replies: []review.FixReply{{Author: "alice", Body: "Intended, see the spec."}}},
			{ID: "f2", Path: "a.go", Line: 5, Severity: "low", Body: "Add a doc comment."},
			{ID: "f3", Path: "b.go", Line: 1, Severity: "medium", Body: "Check the error."},
		},
	}
}

func TestFixWritesInputsAndParsesTheAnswer(t *testing.T) {
	cli := &fixCLI{msg: "Done.\n```json\n" + `{"fixed": ["f2", "f3", "zz", "f2"], "skipped": [{"id": "f1", "reason": " The author disputed it. "}, {"id": "f3", "reason": "also"}],
 "summary": " Added a doc comment. ", "commit_message": "\n fix: add a doc comment\nbody line"}` + "\n```"}
	fx, root := newFixer(t, cli)
	res, err := fx.Fix(context.Background(), fixRequest())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Fixed, ",") != "f2" || len(res.Skipped) != 2 || res.Skipped[0] != (review.FixSkip{ID: "f1", Reason: "The author disputed it."}) ||
		res.Skipped[1].ID != "f3" || res.Summary != "Added a doc comment." || res.CommitMessage != "fix: add a doc comment" {
		t.Fatalf("result %+v", res)
	}
	if len(res.Warnings) != 3 {
		t.Fatalf("warnings %v", res.Warnings)
	}
	inv := cli.inv
	if !inv.Edit || inv.RepoDir != "/work/slot-0" || len(inv.Agents) != 1 || !strings.HasSuffix(inv.Agents[0], "agents/kongming.md") ||
		len(inv.ReadDirs) != 2 || !strings.Contains(string(inv.Schema), `"commit_message"`) {
		t.Fatalf("invocation %+v", inv)
	}
	for _, want := range []string{"Arguments: `12 --advice --fix --reply`", "Service fix mode (ocr serve `/fix`)",
		"references/mode-fix.md", FindingsFile, "Output language of `summary` and `reason`: Vietnamese"} {
		if !strings.Contains(inv.Prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, inv.Prompt)
		}
	}
	var pr prInput
	if err := json.Unmarshal([]byte(cli.prJSON), &pr); err != nil || pr.Mode != ModeFix || pr.Number != 12 ||
		pr.Language != "Vietnamese" || len(pr.Files) != 1 || pr.HeadSHA != strings.Repeat("b", 40) {
		t.Fatalf("pr.json %s err %v", cli.prJSON, err)
	}
	var fs []review.FixFinding
	if err := json.Unmarshal([]byte(cli.findings), &fs); err != nil || len(fs) != 3 || fs[0].Replies[0].Author != "alice" {
		t.Fatalf("findings.json %s err %v", cli.findings, err)
	}
	if !strings.Contains(cli.findings, `"replies": []`) {
		t.Fatalf("a finding without replies must list an empty array:\n%s", cli.findings)
	}
	if runDirsLeft(t, root) != 0 {
		t.Fatal("run dir left")
	}
}

func TestFixErrors(t *testing.T) {
	cases := []struct {
		name string
		cli  *fixCLI
		want string
	}{
		{"cli error", &fixCLI{err: errors.New("claude exited with code 1")}, "fake fix: claude exited"},
		{"no json", &fixCLI{msg: "I fixed it."}, "no JSON object"},
		{"wrong shape", &fixCLI{msg: `{"fixed": "f1"}`}, "fix schema"},
		{"missing keys", &fixCLI{msg: `{"fixed": []}`}, "lacks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx, root := newFixer(t, tc.cli)
			_, err := fx.Fix(context.Background(), fixRequest())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v", err)
			}
			if runDirsLeft(t, root) != 0 {
				t.Fatal("run dir left")
			}
		})
	}
	fx, _ := newFixer(t, &fixCLI{})
	if _, err := fx.Fix(context.Background(), review.FixRequest{}); err == nil {
		t.Fatal("no findings accepted")
	}
	fx.SkillDir = t.TempDir()
	if _, err := fx.Fix(context.Background(), fixRequest()); err == nil || !strings.Contains(err.Error(), "read review skill") {
		t.Fatalf("err %v", err)
	}
}

func TestWriteFixInputDefaults(t *testing.T) {
	dir := t.TempDir()
	req := fixRequest()
	req.PR.Ref = forge.PRRef{Platform: forge.GitLab, ProjectID: 42, Number: 5}
	req.Files = nil
	if err := writeFixInput(dir, req, ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "pr.json"))
	var pr prInput
	if err := json.Unmarshal(b, &pr); err != nil || pr.Repository != "42" || pr.Files == nil {
		t.Fatalf("pr.json %s err %v", b, err)
	}
	if err := writeFixInput(filepath.Join(dir, "gone"), req, ""); err == nil {
		t.Fatal("write into a missing dir accepted")
	}
	if p := buildFixPrompt("skill", "/in", 5, ""); strings.Contains(p, "Output language") {
		t.Fatalf("prompt\n%s", p)
	}
}

func TestCommitMessage(t *testing.T) {
	cases := map[string]string{
		"":                             DefaultCommitMessage,
		" \n \n":                       DefaultCommitMessage,
		"fix: a\tb\x00c":               "fix: a b c",
		"\n\nfix: first\nsecond":       "fix: first",
		strings.Repeat("x", 150) + "y": strings.Repeat("x", 100),
	}
	for in, want := range cases {
		if got := commitMessage(in); got != want {
			t.Errorf("commitMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReviewWritesDismissed passes the findings the author dismissed to the
// agent and names the file in the prompt.
func TestReviewWritesDismissed(t *testing.T) {
	r := newRepo(t)
	var dismissed string
	cli := &dismissedCLI{fakeCLI: fakeCLI{msg: `{"summary": "ok", "findings": []}`}, got: &dismissed}
	rev, _ := newReviewer(t, fakeSelector{sel: Selection{Base: r.base, Head: r.head, Paths: []string{"a.go"}}}, cli)
	req := review.Request{RepoDir: r.dir, From: r.base, To: r.head,
		Dismissed: []review.Dismissed{{Path: "a.go", Line: 3, Title: "Missing return", Body: "A returns nothing.", Reason: "alice: intended"}}}
	if _, err := rev.Review(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var got []review.Dismissed
	if err := json.Unmarshal([]byte(dismissed), &got); err != nil || len(got) != 1 || got[0].Reason != "alice: intended" || got[0].Line != 3 {
		t.Fatalf("dismissed.json %s err %v", dismissed, err)
	}
	if !strings.Contains(cli.inv.Prompt, DismissedFile) || !strings.Contains(cli.inv.Prompt, "Do not report any of them again") {
		t.Fatalf("prompt\n%s", cli.inv.Prompt)
	}

	dismissed = ""
	req.Dismissed = nil
	if _, err := rev.Review(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if dismissed != "" || strings.Contains(cli.inv.Prompt, DismissedFile) {
		t.Fatal("dismissed.json written without dismissed findings")
	}
}

type dismissedCLI struct {
	fakeCLI
	got *string
}

func (c *dismissedCLI) Exec(ctx context.Context, inv agentcli.Invocation) (string, error) {
	b, _ := os.ReadFile(filepath.Join(inv.Run.Input, DismissedFile))
	*c.got = string(b)
	return c.fakeCLI.Exec(ctx, inv)
}
