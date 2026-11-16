// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package clireview

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		Base:  strings.Repeat("a", 40), Head: strings.Repeat("b", 40),
		Findings: []review.Finding{
			{ID: "F1", Path: "a.go", Line: 3, Severity: "high", Category: "bug", Content: "**Missing return**\n\nA returns nothing."},
			{ID: "F2", Path: "a.go", Line: 5, Severity: "low", Category: "style", Content: "Add a doc comment."},
			{ID: "F3", Path: "", Line: 0, Severity: "medium", Category: "documentation", Content: "The PR body lacks a test plan."},
		},
	}
}

const fixAnswerJSON = `{"fixed": [{"id": "F1", "path": "", "line": 0, "problem": " A returns nothing. ", "fix": "Return the value."},
 {"id": "zz", "path": "x.go", "line": 1, "problem": "p", "fix": "f"}, {"id": "F3", "path": "", "line": 0, "problem": "p", "fix": "f"}],
 "report_only": [{"id": "F3", "path": "", "line": 0, "problem": "No test plan.", "scenario": "A reviewer cannot check it.", "proposed_fix": "Add a test plan."}],
 "tests": {"command": " go   test ./... ", "passed": false, "output_tail": "--- FAIL: TestA"},
 "summary": " Fixed A. ", "commit_message": "\n fix: return the value from A\nbody line"}`

func TestFixWritesInputsAndParsesTheAnswer(t *testing.T) {
	cli := &fixCLI{msg: "Done.\n```json\n" + fixAnswerJSON + "\n```"}
	fx, root := newFixer(t, cli)
	res, err := fx.Fix(context.Background(), fixRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixed) != 1 || !reflect.DeepEqual(res.Fixed[0], review.Fixed{ID: "F1", Path: "a.go", Line: 3, Problem: "A returns nothing.", Fix: "Return the value."}) {
		t.Fatalf("fixed %+v", res.Fixed)
	}
	if len(res.Reported) != 1 || res.Reported[0] != (review.Reported{ID: "F3", Problem: "No test plan.", Scenario: "A reviewer cannot check it.", ProposedFix: "Add a test plan."}) {
		t.Fatalf("reported %+v", res.Reported)
	}
	if res.Tests != (review.Tests{Command: "go test ./...", OutputTail: "--- FAIL: TestA"}) ||
		res.Summary != "Fixed A." || res.CommitMessage != "fix: return the value from A" {
		t.Fatalf("result %+v", res)
	}
	if len(res.Warnings) != 2 {
		t.Fatalf("warnings %v", res.Warnings)
	}
	inv := cli.inv
	if !inv.Edit || inv.Shell || len(inv.Agents) != 1 || !strings.HasSuffix(inv.Agents[0], AgentFile) || !json.Valid(inv.Schema) {
		t.Fatalf("invocation %+v", inv)
	}
	for _, want := range []string{"in fix mode", "Service fix mode (ocr serve)", "pr-body-contract.md", "git diff " + strings.Repeat("a", 40) + ".." + strings.Repeat("b", 40),
		`"id": "F2"`, "Output language", "report_only", "Fix only the `real` findings", "Never edit the PR description", "`out_of_scope` is true"} {
		if !strings.Contains(inv.Prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, inv.Prompt)
		}
	}
	var pr prInput
	if err := json.Unmarshal([]byte(cli.prJSON), &pr); err != nil || pr.Mode != ModeFix || pr.Number != 12 || pr.Language != "Vietnamese" {
		t.Fatalf("pr.json %s err %v", cli.prJSON, err)
	}
	var fs []review.Finding
	if err := json.Unmarshal([]byte(cli.findings), &fs); err != nil || len(fs) != 3 || fs[0].ID != "F1" {
		t.Fatalf("findings.json %s err %v", cli.findings, err)
	}
	if runDirsLeft(t, root) != 0 {
		t.Fatal("run dir left")
	}
}

// TestFixReportOnly gives a report-only run no edit tools and turns a fixed
// entry into a reported one.
// TestFixedPRLevelFindingIsReported never counts a finding about the PR as
// a whole as fixed.
func TestFixedPRLevelFindingIsReported(t *testing.T) {
	cli := &fixCLI{msg: `{"fixed": [{"id": "F3", "path": "", "line": 0, "problem": "No test plan.", "fix": "Edited the body."}],
 "report_only": [], "tests": {"command": "", "passed": true, "output_tail": ""}, "summary": "", "commit_message": ""}`}
	fx, _ := newFixer(t, cli)
	res, err := fx.Fix(context.Background(), fixRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixed) != 0 || len(res.Reported) != 1 || res.Reported[0].ID != "F3" || res.Reported[0].ProposedFix != "Edited the body." {
		t.Fatalf("result %+v", res)
	}
}

func TestFixReportOnly(t *testing.T) {
	cli := &fixCLI{msg: fixAnswerJSON}
	fx, _ := newFixer(t, cli)
	req := fixRequest()
	req.ReportOnly, req.Base = true, ""
	res, err := fx.Fix(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixed) != 0 || len(res.Reported) != 2 || res.Reported[1].ID != "F1" || res.Reported[1].ProposedFix != "Return the value." ||
		!strings.Contains(cli.inv.Prompt, "Do not fix anything") {
		t.Fatalf("result %+v", res)
	}
	inv := cli.inv
	if inv.Edit || !inv.Shell || len(inv.Agents) != 0 || !strings.Contains(inv.Prompt, "report-only mode") ||
		!strings.Contains(inv.Prompt, "git show "+strings.Repeat("b", 40)) || !strings.Contains(inv.Prompt, "read-only checkout") {
		t.Fatalf("invocation %+v", inv)
	}
	if !strings.Contains(cli.prJSON, `"mode": "report"`) {
		t.Fatalf("pr.json %s", cli.prJSON)
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
		{"wrong shape", &fixCLI{msg: `{"fixed": "F1"}`}, "fix schema"},
		{"missing keys", &fixCLI{msg: `{"fixed": [], "report_only": [], "summary": ""}`}, "lacks"},
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
	if _, err := writeFixInput(dir, req, ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "pr.json"))
	var pr prInput
	if err := json.Unmarshal(b, &pr); err != nil || pr.Repository != "42" || pr.Files == nil {
		t.Fatalf("pr.json %s err %v", b, err)
	}
	if _, err := writeFixInput(filepath.Join(dir, "gone"), req, ""); err == nil {
		t.Fatal("write into a missing dir accepted")
	}
	if p := buildFixPrompt("skill", "/in", req, "", nil); strings.Contains(p, "Output language") {
		t.Fatalf("prompt\n%s", p)
	}
}

func TestBoundAndTail(t *testing.T) {
	if got := bound(" "+strings.Repeat("x", 12)+" ", 10); got != strings.Repeat("x", 10)+"..." {
		t.Fatalf("bound %q", got)
	}
	if got := tail("abcdef", 3); got != "...def" {
		t.Fatalf("tail %q", got)
	}
	if got := tail("abc", 3); got != "abc" {
		t.Fatalf("tail %q", got)
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

// TestReviewOfAFixCommit tells the agent to review only the fix commit and
// drops a PR-level finding, which the first round already checked.
func TestReviewOfAFixCommit(t *testing.T) {
	r := newRepo(t)
	cli := &fakeCLI{msg: `{"summary": "ok", "findings": [{"path": "", "line": 0, "end_line": null, "severity": "low", "category": "documentation", "title": "PR body", "body": "Add a test plan.", "suggestion": null}]}`}
	rev, _ := newReviewer(t, fakeSelector{sel: Selection{Base: r.base, Head: r.head, Paths: []string{"a.go"}}}, cli)
	res, err := rev.Review(context.Background(), review.Request{RepoDir: r.dir, Commit: r.head, FixCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("findings %+v", res.Findings)
	}
	if !strings.Contains(cli.inv.Prompt, "covers only commit `"+r.head+"`") {
		t.Fatalf("prompt\n%s", cli.inv.Prompt)
	}
}

func TestFixParsesChangesAndProposedDiff(t *testing.T) {
	changes := `[{"path": " a.go ", "start_line": 3, "end_line": 1}, {"path": "", "start_line": 1, "end_line": 2},
 {"path": "gone.go", "start_line": -4, "end_line": -1}, {"path": "a_test.go", "start_line": 10, "end_line": 20}]`
	diff := "Here is the change:\n```diff\n--- a/a.go\n+++ b/a.go\n@@ -5 +5,2 @@\n-x\n+y\n\n+z\n```\nThat is all."
	diffJSON, _ := json.Marshal(diff)
	cli := &fixCLI{msg: `{"fixed": [{"id": "F1", "path": "a.go", "line": 3, "problem": "p", "scenario": " A caller reads zero. ", "fix": "f",
 "changes": ` + changes + `, "tests_added": " TestA "}],
 "report_only": [{"id": "F2", "path": "a.go", "line": 5, "problem": "q", "scenario": "s", "proposed_fix": "g", "proposed_diff": ` + string(diffJSON) + `}],
 "tests": {"command": "go test ./...", "passed": true, "output_tail": ""}, "summary": "", "commit_message": "fix: a"}`}
	fx, _ := newFixer(t, cli)
	res, err := fx.Fix(context.Background(), fixRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := review.Fixed{ID: "F1", Path: "a.go", Line: 3, Problem: "p", Scenario: "A caller reads zero.", Fix: "f", TestsAdded: "TestA",
		Changes: []review.Change{{Path: "a.go", StartLine: 3, EndLine: 3}, {Path: "gone.go"}, {Path: "a_test.go", StartLine: 10, EndLine: 20}}}
	if len(res.Fixed) != 1 || !reflect.DeepEqual(res.Fixed[0], want) {
		t.Fatalf("fixed %+v", res.Fixed)
	}
	if len(res.Reported) != 1 || res.Reported[0].ProposedDiff != "--- a/a.go\n+++ b/a.go\n@@ -5 +5,2 @@\n-x\n+y\n\n+z" {
		t.Fatalf("reported %+v", res.Reported)
	}
	for _, want := range []string{"`changes`", "`tests_added`", "`proposed_diff`", "real `git diff`"} {
		if !strings.Contains(cli.inv.Prompt, want) {
			t.Fatalf("prompt lacks %q", want)
		}
	}
}

func TestProposedDiff(t *testing.T) {
	cases := map[string]string{
		"":                                 "",
		"Change x to y.":                   "",
		"@@ -1 +1 @@\r\n-a\r\n+b\r\n":      "@@ -1 +1 @@\n-a\n+b",
		"diff --git a/x b/x\nindex 1..2\n": "diff --git a/x b/x\nindex 1..2",
		"--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\nProse after.\n+not kept": "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b",
	}
	for in, want := range cases {
		if got := proposedDiff(in); got != want {
			t.Fatalf("proposedDiff(%q) = %q, want %q", in, got, want)
		}
	}
	big := "@@ -1 +1 @@\n" + strings.Repeat("+"+strings.Repeat("x", 99)+"\n", maxProposedDiff/100+10)
	if got := proposedDiff(big); len(got) > maxProposedDiff || !strings.HasSuffix(got, "x") {
		t.Fatalf("big diff: %d bytes", len(got))
	}
}

// TestFixParsesTitlesAndDropped keeps the titles on one line, reads the
// dropped findings after the other lists, and takes the language of the
// request over the one of the Fixer.
func TestFixParsesTitlesAndDropped(t *testing.T) {
	long := strings.Repeat("x", 100)
	cli := &fixCLI{msg: `{"fixed": [{"id": "F1", "path": "a.go", "line": 3, "title": " Missing\n return ", "problem": "p", "fix": "f"}],
 "report_only": [{"id": "F3", "path": "", "line": 0, "title": "` + long + `", "problem": "q"}],
 "dropped": [{"id": "F2", "title": "Doc comment", "reason": " Style only. "}, {"id": "F1", "title": "again", "reason": "r"}, {"id": "F9", "reason": "r"}],
 "tests": {"command": "", "passed": true, "output_tail": ""}, "summary": "", "commit_message": ""}`}
	fx, _ := newFixer(t, cli)
	req := fixRequest()
	req.Language = "English"
	res, err := fx.Fix(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fixed[0].Title != "Missing return" || len([]rune(res.Reported[0].Title)) != maxTitle+3 {
		t.Fatalf("titles %q %q", res.Fixed[0].Title, res.Reported[0].Title)
	}
	if len(res.Dropped) != 1 || res.Dropped[0] != (review.Dropped{ID: "F2", Title: "Doc comment", Reason: "Style only."}) {
		t.Fatalf("dropped %+v", res.Dropped)
	}
	if len(res.Warnings) != 2 {
		t.Fatalf("warnings %v", res.Warnings)
	}
	if !strings.Contains(cli.inv.Prompt, "Output language of `title`") || !strings.Contains(cli.inv.Prompt, ": English.") ||
		!strings.Contains(cli.inv.Prompt, "goes in `dropped`") || !strings.Contains(cli.prJSON, `"language": "English"`) {
		t.Fatalf("prompt or pr.json lacks the request language:\n%s\n%s", cli.inv.Prompt, cli.prJSON)
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Items struct {
				Required []string `json:"required"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(fixSchema), &schema); err != nil {
		t.Fatal(err)
	}
	for _, list := range []string{"fixed", "report_only", "dropped"} {
		if !slices.Contains(schema.Properties[list].Items.Required, "title") {
			t.Fatalf("%s items do not require a title", list)
		}
	}
	if !slices.Contains(schema.Required, "dropped") {
		t.Fatal("dropped is not required")
	}
}
