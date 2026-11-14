// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package clireview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/agentcli"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

// verifyCLI answers a verify run with msg and keeps what the verifier saw.
type verifyCLI struct {
	msg      string
	err      error
	calls    int
	inv      agentcli.Invocation
	findings []verifyItem
	diff     string
	pr       prInput
	deadline time.Time
}

func (*verifyCLI) Name() string { return "verifier" }

func (c *verifyCLI) Exec(ctx context.Context, inv agentcli.Invocation) (string, error) {
	c.calls++
	c.inv = inv
	c.deadline, _ = ctx.Deadline()
	b, _ := os.ReadFile(filepath.Join(inv.Run.Input, FindingsFile))
	_ = json.Unmarshal(b, &c.findings)
	b, _ = os.ReadFile(filepath.Join(inv.Run.Input, "diff.patch"))
	c.diff = string(b)
	b, _ = os.ReadFile(filepath.Join(inv.Run.Input, "pr.json"))
	_ = json.Unmarshal(b, &c.pr)
	return c.msg, c.err
}

func verdictsJSON(vs ...string) string {
	return `{"verdicts":[` + strings.Join(vs, ",") + `]}`
}

func verdictJSON(id, verdict, severity, reason string) string {
	return fmt.Sprintf(`{"id":%q,"verdict":%q,"severity":%q,"reason":%q}`, id, verdict, severity, reason)
}

// verifyFixture reviews the batch repo in two batches. The second batch
// repeats the first finding, which the merge drops, so the verifier sees
// three findings: F1 on api/a.go, F2 on api/b.go and F3 on web/c.go.
type verifyFixture struct {
	rev    *Reviewer
	req    review.Request
	cli    *verifyCLI
	log    *bytes.Buffer
	root   string
	review *batchCLI
}

func newVerifyFixture(t *testing.T, msg string, post ...string) verifyFixture {
	t.Helper()
	r, sel := batchRepo(t)
	sel.Language = "Vietnamese"
	reviewCLI := &batchCLI{answer: func(i int) (string, error) {
		if i == 1 {
			return answerJSON(findingJSON("api/a.go", 1, "A"), findingJSON("api/b.go", 2, "B")), nil
		}
		return answerJSON(findingJSON("api/a.go", 1, "A"), findingJSON("web/c.go", 3, "C")), nil
	}}
	rev, root := newReviewer(t, fakeSelector{sel: sel}, reviewCLI)
	writeReferences(t, rev.SkillDir, []string{prBodyContract})
	rev.Batching = Batching{Files: 2}
	var log bytes.Buffer
	rev.Log = slog.New(slog.NewTextHandler(&log, nil))
	cli := &verifyCLI{msg: msg}
	rev.Verify = &Verifier{CLI: cli, Post: post, Note: func(reason string) string { return "VERIFIED: " + reason }}
	req := review.Request{RepoDir: r.dir, From: r.base, To: r.head, PR: forge.PR{
		Ref:     forge.PRRef{Platform: forge.GitHub, Repo: "acme/pay", Number: 7},
		BaseSHA: r.base, HeadSHA: r.head, Title: "Add A, B and C",
	}}
	return verifyFixture{rev: rev, req: req, cli: cli, log: &log, root: root, review: reviewCLI}
}

func paths(fs []model.LlmComment) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Path
	}
	return out
}

func TestVerifyKeepsTheVerdictsToPost(t *testing.T) {
	answer := verdictsJSON(
		verdictJSON("F1", "real", "critical", "A panics when\n  the input is nil."),
		verdictJSON("F2", "minor", "low", "Style only."),
		verdictJSON("F3", "false", "low", "Already handled in C."),
	)
	cases := []struct {
		name string
		post []string
		want []string
	}{
		{"real only", []string{"real"}, []string{"api/a.go"}},
		{"real and minor", []string{"real", "minor"}, []string{"api/a.go", "api/b.go"}},
		{"everything", []string{"real", "minor", "out_of_scope", "false"}, []string{"api/a.go", "api/b.go", "web/c.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerifyFixture(t, answer, tc.post...)
			res, err := f.rev.Review(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			if got := paths(res.Findings); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("posted %v, want %v", got, tc.want)
			}
			if res.Verify != (review.Verification{Checked: 3, Posted: len(tc.want)}) {
				t.Fatalf("verify %+v", res.Verify)
			}
			if res.Terminal != review.TerminalComplete {
				t.Fatalf("terminal %q", res.Terminal)
			}
			a := res.Findings[0]
			if a.Severity != "critical" || !strings.HasSuffix(a.Content, "\n\nVERIFIED: A panics when the input is nil.") {
				t.Fatalf("finding %+v", a)
			}
			if !strings.Contains(f.log.String(), "msg=\"review verify\" cli=verifier checked=3 real=1 minor=1 out_of_scope=0 false=1 missing=0 posted="+fmt.Sprint(len(tc.want))) {
				t.Fatalf("log %s", f.log.String())
			}
			if n := runDirsLeft(t, f.root); n != 0 {
				t.Fatalf("%d run dirs left", n)
			}
		})
	}
}

func TestVerifyRunInput(t *testing.T) {
	f := newVerifyFixture(t, verdictsJSON(), "real")
	if _, err := f.rev.Review(context.Background(), f.req); err != nil {
		t.Fatal(err)
	}
	c := f.cli
	if c.calls != 1 || len(f.review.runs) != 2 {
		t.Fatalf("verify calls %d, review runs %d", c.calls, len(f.review.runs))
	}
	if len(c.findings) != 3 || c.findings[0].ID != "F1" || c.findings[2].ID != "F3" ||
		c.findings[1].Path != "api/b.go" || c.findings[1].StartLine != 2 || !strings.Contains(c.findings[1].Content, "**B**") {
		t.Fatalf("findings %+v", c.findings)
	}
	for _, p := range []string{"api/a.go", "api/b.go", "web/c.go"} {
		if !strings.Contains(c.diff, "+++ b/"+p) {
			t.Fatalf("diff.patch misses %s:\n%s", p, c.diff)
		}
	}
	if c.pr.Number != 7 || c.pr.Mode != ModeFull || c.pr.Batch != nil || len(c.pr.Files) != 3 {
		t.Fatalf("pr.json %+v", c.pr)
	}
	inv := c.inv
	if inv.Edit || !inv.Shell || inv.RepoDir != f.req.RepoDir || len(inv.ReadDirs) != 1 || inv.ReadDirs[0] != inv.Run.Input ||
		string(inv.Schema) != verifySchema || len(inv.Agents) != 0 {
		t.Fatalf("invocation %+v", inv)
	}
	for _, want := range []string{
		"You are an independent senior reviewer.",
		"Do not trust the finding text",
		"`out_of_scope`: true, but this PR did not introduce or change the code at fault.",
		"Do not report new findings.",
		fmt.Sprintf("`git diff %s..%s` shows the reviewed changes.", f.req.From, f.req.To),
		fmt.Sprintf("`git diff %s...%s` shows the whole PR.", f.req.PR.BaseSHA, f.req.PR.HeadSHA),
		"Output language of `reason` and of the prose in `fix`: Vietnamese.",
		"## PR body contract\n\n" + refText(prBodyContract),
		`"id": "F2"`,
		"Body of C.",
		verifySchema,
	} {
		if !strings.Contains(inv.Prompt, want) {
			t.Errorf("prompt misses %q", want)
		}
	}
	// A verdict list that names no finding keeps every finding unverified.
	if !strings.Contains(f.log.String(), "missing=3 posted=3") {
		t.Fatalf("log %s", f.log.String())
	}
}

func TestVerifyKeepsFindingsWithoutAUsableVerdict(t *testing.T) {
	f := newVerifyFixture(t, verdictsJSON(
		verdictJSON("F1", "FALSE", "low", "Wrong."),
		verdictJSON("F1", "real", "high", "Repeated."),
		verdictJSON("F2", "maybe", "high", "Unsure."),
		verdictJSON("F9", "real", "high", "Unknown id."),
		verdictJSON(" F3 ", "real", "urgent", ""),
	), "real")
	res, err := f.rev.Review(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(res.Findings), ","); got != "api/b.go,web/c.go" {
		t.Fatalf("posted %s", got)
	}
	b, c := res.Findings[0], res.Findings[1]
	if b.Severity != "medium" || strings.Contains(b.Content, "VERIFIED") {
		t.Fatalf("unverified finding changed: %+v", b)
	}
	if c.Severity != "medium" || strings.Contains(c.Content, "VERIFIED") {
		t.Fatalf("an invalid severity or an empty reason changed the finding: %+v", c)
	}
	warnings := strings.Join(res.Warnings, "\n")
	for _, want := range []string{
		"ignored a repeated verdict for finding F1",
		`ignored the invalid verdict "maybe" for finding F2`,
		`ignored a verdict for unknown finding id "F9"`,
		`no verdict for finding F2 on "api/b.go" line 2; posted unverified`,
	} {
		if !strings.Contains(warnings, want) {
			t.Errorf("warnings miss %q:\n%s", want, warnings)
		}
	}
	if res.Verify != (review.Verification{Checked: 3, Posted: 2}) {
		t.Fatalf("verify %+v", res.Verify)
	}
}

func TestVerifyFailureKeepsTheFindings(t *testing.T) {
	cases := map[string]*verifyCLI{
		"cli error":   {err: errors.New("exit status 1")},
		"no json":     {msg: "I could not decide."},
		"bad schema":  {msg: `{"verdicts": "all real"}`},
		"no verdicts": {msg: `{"summary": "ok"}`},
	}
	for name, cli := range cases {
		t.Run(name, func(t *testing.T) {
			f := newVerifyFixture(t, "", "real")
			f.rev.Verify.CLI = cli
			res, err := f.rev.Review(context.Background(), f.req)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Findings) != 3 || res.Terminal != review.TerminalComplete {
				t.Fatalf("findings %d terminal %q", len(res.Findings), res.Terminal)
			}
			for _, fd := range res.Findings {
				if strings.Contains(fd.Content, "VERIFIED") || fd.Severity != "medium" {
					t.Fatalf("finding changed: %+v", fd)
				}
			}
			if res.Verify != (review.Verification{Checked: 3, Posted: 3, Failed: true}) {
				t.Fatalf("verify %+v", res.Verify)
			}
			if !strings.Contains(f.log.String(), `msg="review verify failed" cli=verifier checked=3 posted=3`) ||
				!strings.Contains(f.log.String(), "verifier verify:") {
				t.Fatalf("log %s", f.log.String())
			}
			if n := runDirsLeft(t, f.root); n != 0 {
				t.Fatalf("%d run dirs left", n)
			}
		})
	}
}

func TestVerifySkippedWithoutFindings(t *testing.T) {
	f := newVerifyFixture(t, verdictsJSON(), "real")
	f.review.answer = func(int) (string, error) { return answerJSON(), nil }
	res, err := f.rev.Review(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if f.cli.calls != 0 || res.Verify != (review.Verification{}) || strings.Contains(f.log.String(), "review verify") {
		t.Fatalf("verify ran: calls %d verify %+v", f.cli.calls, res.Verify)
	}
}

func TestVerifyKeepsAPartialReviewPartial(t *testing.T) {
	f := newVerifyFixture(t, verdictsJSON(verdictJSON("F1", "real", "high", "Bug.")), "real")
	f.review.answer = func(i int) (string, error) {
		if i == 2 {
			return "", errors.New("boom")
		}
		return answerJSON(findingJSON("api/a.go", 1, "A")), nil
	}
	res, err := f.rev.Review(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Terminal != review.TerminalPartial || len(res.Findings) != 1 || res.Verify.Posted != 1 {
		t.Fatalf("terminal %q findings %d verify %+v", res.Terminal, len(res.Findings), res.Verify)
	}
}

func TestVerifyTimeLimits(t *testing.T) {
	v := &Verifier{}
	ctx, cancel, err := v.context(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := ctx.Deadline()
	cancel()
	if left := time.Until(dl); left > DefaultVerifyTimeout || left < DefaultVerifyTimeout-time.Minute {
		t.Fatalf("default limit %s", left)
	}

	parent, stop := context.WithTimeout(context.Background(), verifyReserve+10*time.Minute)
	defer stop()
	v.Timeout = time.Hour
	ctx, cancel, err = v.context(parent)
	if err != nil {
		t.Fatal(err)
	}
	dl, _ = ctx.Deadline()
	cancel()
	if left := time.Until(dl); left > 10*time.Minute || left < 9*time.Minute {
		t.Fatalf("the verify run does not stop before the reserve: %s left", left)
	}

	short, stopShort := context.WithTimeout(context.Background(), verifyReserve-time.Minute)
	defer stopShort()
	if _, _, err := v.context(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}

func TestVerifyWithoutTimeLeftPostsUnverified(t *testing.T) {
	f := newVerifyFixture(t, verdictsJSON(), "real")
	ctx, cancel := context.WithTimeout(context.Background(), verifyReserve-time.Minute)
	defer cancel()
	res, err := f.rev.Review(ctx, f.req)
	if err != nil {
		t.Fatal(err)
	}
	if f.cli.calls != 0 || !res.Verify.Failed || len(res.Findings) != 3 {
		t.Fatalf("calls %d verify %+v findings %d", f.cli.calls, res.Verify, len(res.Findings))
	}
}

func TestVerifyWithoutTheContractReference(t *testing.T) {
	f := newVerifyFixture(t, verdictsJSON(), "real")
	if err := os.Remove(filepath.Join(f.rev.SkillDir, filepath.Dir(SkillFile), "references", prBodyContract)); err != nil {
		t.Fatal(err)
	}
	res, err := f.rev.Review(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.cli.inv.Prompt, "PR body contract") || !strings.Contains(f.cli.inv.Prompt, "PR scope is `real`") {
		t.Fatal("prompt has an empty contract section")
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "skill reference "+prBodyContract+" left out of the prompt: missing") {
		t.Fatalf("warnings %v", res.Warnings)
	}
}

func TestVerifyRunDirError(t *testing.T) {
	f := newVerifyFixture(t, verdictsJSON(), "real")
	f.rev.Batching = Batching{}
	f.review.answer = func(int) (string, error) {
		// The review run comes first; the verify run then has no home root.
		if err := os.RemoveAll(f.root); err != nil {
			return "", err
		}
		return answerJSON(findingJSON("api/a.go", 1, "A")), nil
	}
	res, err := f.rev.Review(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if f.cli.calls != 0 || !res.Verify.Failed || len(res.Findings) != 1 ||
		!strings.Contains(f.log.String(), "create run dir") {
		t.Fatalf("calls %d verify %+v findings %d log %s", f.cli.calls, res.Verify, len(res.Findings), f.log.String())
	}
}

func TestBuildVerifyPromptForARootCommit(t *testing.T) {
	p := buildVerifyPrompt("", "/run/in", review.Request{}, Selection{Head: "abc"}, []byte(`[]`))
	if !strings.Contains(p, "`git show abc` shows the reviewed changes.") || strings.Contains(p, "shows the whole PR") ||
		strings.Contains(p, "Output language") || strings.Contains(p, "PR body contract") {
		t.Fatalf("prompt %s", p)
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("  a\n\tb  c "); got != "a b c" {
		t.Fatalf("got %q", got)
	}
	long := oneLine(strings.Repeat("x", maxVerifyReason+10))
	if len([]rune(long)) != maxVerifyReason+3 || !strings.HasSuffix(long, "...") {
		t.Fatalf("long reason %d runes", len([]rune(long)))
	}
}

func TestWriteVerifyInputUsesProjectID(t *testing.T) {
	r := newRepo(t)
	dir := t.TempDir()
	req := review.Request{RepoDir: r.dir, PR: forge.PR{Ref: forge.PRRef{Platform: forge.GitLab, ProjectID: 42, Number: 3}}}
	if _, err := writeVerifyInput(context.Background(), dir, req, Selection{Base: r.base, Head: r.head, Paths: []string{"a.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	var pr prInput
	b, _ := os.ReadFile(filepath.Join(dir, "pr.json"))
	if err := json.Unmarshal(b, &pr); err != nil || pr.Repository != "42" {
		t.Fatalf("pr.json %s %v", b, err)
	}
	if _, err := writeVerifyInput(context.Background(), dir, req, Selection{Base: "nope", Head: r.head, Paths: []string{"a.go"}}, nil); err == nil {
		t.Fatal("want a diff error")
	}
}

func TestVerifyAppendsTheProposedFix(t *testing.T) {
	v := &Verifier{Post: []string{"real"},
		Note:    func(reason string) string { return "VERIFIED: " + reason },
		FixNote: func(fix string) string { return "FIX:\n" + fix }}
	fs := []model.LlmComment{{Path: "a.go", StartLine: 3, Severity: "medium", Content: "body"}, {Path: "b.go", StartLine: 1, Content: "other"}}
	items := verifyItems(fs)
	kept, _, _ := v.apply(fs, items, []verdict{
		{ID: "F1", Verdict: "real", Severity: "high", Reason: "It panics.", Fix: "Check the index.\n```go\nif i < len(xs) {}\n```"},
		{ID: "F2", Verdict: "real", Severity: "low", Reason: "Minor.", Fix: "  "},
	})
	if len(kept) != 2 {
		t.Fatalf("kept %d", len(kept))
	}
	if want := "body\n\nVERIFIED: It panics.\n\nFIX:\nCheck the index.\n```go\nif i < len(xs) {}\n```"; kept[0].Content != want {
		t.Fatalf("content %q", kept[0].Content)
	}
	if strings.Contains(kept[1].Content, "FIX:") {
		t.Fatalf("an empty fix was appended: %q", kept[1].Content)
	}
}
