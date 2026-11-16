// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

func TestRenderSummary(t *testing.T) {
	en := mustLocale(t, "en")
	m := sampleMarker()
	body := RenderSummary(m, en, "Running round 2/3...", "ocr-bot")
	if !strings.HasPrefix(body, forge.SummaryTag+"\n") {
		t.Fatalf("summary does not start with the tag:\n%s", body)
	}
	for _, want := range []string{en["summary.status.running"], "| round 1/3 | `aaaaaaa...bbbbbbb` | 2 | 2 | `ccccccc` |",
		"| round 2/3 | `ccccccc` | 0 | 0 | - |", "Running round 2/3...", "`@ocr-bot review`"} {
		if !strings.Contains(body, want) {
			t.Fatalf("summary lacks %q:\n%s", want, body)
		}
	}
	if got, err := Decode(body); err != nil || got == nil || got.Status != ChainRunning {
		t.Fatalf("marker %v %v", got, err)
	}
	plain := RenderSummary(Marker{Status: ChainSuccess, Max: 3}, en, "", "")
	if strings.Contains(plain, "| Round |") || !strings.Contains(plain, "`/review`") || strings.Contains(plain, "@") {
		t.Fatalf("summary:\n%s", plain)
	}
}

func TestRenderRound_Fixed(t *testing.T) {
	en := mustLocale(t, "en")
	body := RenderRound(RoundReport{N: 1, Max: 3, From: shaA, To: shaB,
		Fixed: []review.Fixed{{Path: "a.go", Line: 3, Problem: "A returns nothing <!-- ocr-x -->", Fix: "Return the value."},
			{Path: "b.go", Problem: "No error check"}},
		Reported:  []review.Reported{{Problem: "No test plan.", Scenario: "A reviewer cannot check it.", ProposedFix: "Add one."}},
		Tests:     review.Tests{Command: "go test ./...", OutputTail: "--- FAIL: TestA\n```"},
		FixCommit: shaC, Next: 2}, en)
	for _, want := range []string{"### OCR review, round 1/3", "Reviewed: `aaaaaaa...bbbbbbb`", "**Fixed (2)**",
		"1. `a.go` L3: A returns nothing &lt;!-- ocr-x -->", "How it was fixed: Return the value.", "2. `b.go`: No error check",
		"Fix commit: `ccccccc`", "Tests: `go test ./...` failed", "````\n--- FAIL: TestA\n```\n````",
		"**Real findings, not fixed (1)**", "1. The PR as a whole: No test plan.", "Scenario: A reviewer cannot check it.",
		"Proposed fix: Add one.", "reviewed next, in round 2/3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("round lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<!-- ocr-x") {
		t.Fatalf("tag not neutralized:\n%s", body)
	}
	passed := RenderRound(RoundReport{N: 2, Max: 3, To: shaC, Fixed: []review.Fixed{{Path: "a.go", Problem: "p"}},
		Tests: review.Tests{Command: "make test", Passed: true, OutputTail: "ok"}}, en)
	if !strings.Contains(passed, "Tests: `make test` passed.") || strings.Contains(passed, "```") || !strings.Contains(passed, en["round.end"]) {
		t.Fatalf("round:\n%s", passed)
	}
	none := RenderRound(RoundReport{N: 2, Max: 3, To: shaC, Fixed: []review.Fixed{{Path: "a.go", Problem: "p"}}}, en)
	if !strings.Contains(none, en["round.tests_none"]) {
		t.Fatalf("round:\n%s", none)
	}
}

func TestRenderRound_ReportOnlyAndClean(t *testing.T) {
	en := mustLocale(t, "en")
	limit := RenderRound(RoundReport{N: 4, Max: 3, To: shaC, Reported: []review.Reported{{Path: "a.go", Line: 1, Problem: "p"}}, Report: ReportLimit}, en)
	for _, want := range []string{"round 4 (report only)", "The 3 fix rounds are used up", en["round.end"]} {
		if !strings.Contains(limit, want) {
			t.Fatalf("round lacks %q:\n%s", want, limit)
		}
	}
	push := RenderRound(RoundReport{N: 1, Max: 3, To: shaC, Reported: []review.Reported{{Path: "a.go", Problem: "p"}},
		Report: ReportPush, Branch: "feat/x", PushReason: "protected branch"}, en)
	if !strings.Contains(push, "branch `feat/x`: `protected branch`") {
		t.Fatalf("round:\n%s", push)
	}
	clean := RenderRound(RoundReport{N: 2, Max: 3, To: shaC,
		Coverage: review.Coverage{Total: 2, Reviewed: 1, Failed: []review.FailedItem{{Path: "b.go", OldPath: "old.go", Class: "timeout"}}}}, en)
	for _, want := range []string{en["round.clean"], "reviewed 1/2 files", "`old.go` → `b.go`: the review of this file timed out"} {
		if !strings.Contains(clean, want) {
			t.Fatalf("round lacks %q:\n%s", want, clean)
		}
	}
	skipped := RenderRound(RoundReport{N: 1, Max: 3, From: shaA, To: shaB, Skipped: true}, en)
	if !strings.Contains(skipped, en["round.skipped"]) || strings.Contains(skipped, en["round.clean"]) {
		t.Fatalf("round:\n%s", skipped)
	}
}

func TestRenderCoverage(t *testing.T) {
	en := mustLocale(t, "en")
	if RenderCoverage(review.Coverage{}, en) != "" || RenderCoverage(review.Coverage{Total: 2, Reviewed: 2}, en) != "" {
		t.Fatal("complete coverage gave a line")
	}
	c := review.Coverage{Total: 30, Reviewed: 5}
	for i := range 25 {
		c.Failed = append(c.Failed, review.FailedItem{Path: string(rune('a'+i)) + ".go", Class: "weird"})
	}
	out := RenderCoverage(c, en)
	if !strings.Contains(out, "and 5 more") || !strings.Contains(out, ": weird") {
		t.Fatalf("coverage:\n%s", out)
	}
}

func TestStatusTextAndCommandError(t *testing.T) {
	en := mustLocale(t, "en")
	if got := StatusText(Marker{Status: ChainRunning, Max: 3}, 4, en); got != "OCR review round 4 (report only) is running" {
		t.Fatalf("status %q", got)
	}
	if got := StatusText(Marker{Status: ChainFailure}, 0, en); got != en["status.failure"] {
		t.Fatalf("status %q", got)
	}
	msg := RenderCommandError("unknown command `x`\nmore", "bot", en)
	if !strings.Contains(msg, "`unknown command 'x' more`") || !strings.Contains(msg, "`@bot review") {
		t.Fatalf("error %q", msg)
	}
	if !strings.Contains(RenderCommandError("x", "", en), "`/review-commit`") {
		t.Fatal("slash help missing")
	}
	if safeFence("no ticks") != "```" || labelFor(en, "failure.", "x") != "x" || short("abc") != "abc" {
		t.Fatal("helpers")
	}
}

// diffLines is a one-file diff with n added lines.
func diffLines(path string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1,0 +1,%d @@\n", path, path, path, path, n)
	for i := range n {
		fmt.Fprintf(&b, "+line %d\n", i)
	}
	return b.String()
}

func TestRenderRound_FixDiffs(t *testing.T) {
	for _, name := range []string{"en", "vi"} {
		loc := mustLocale(t, name)
		long := diffLines("b.go", 45)
		body := RenderRound(RoundReport{N: 1, Max: 3, From: shaA, To: shaB, FixCommit: shaC,
			CommitURL: "https://github.com/acme/app/commit/" + shaC,
			Fixed: []review.Fixed{
				{Path: "a.go", Line: 3, Problem: "A returns nothing", Scenario: "A caller reads zero.", Fix: "Return the value.", TestsAdded: "TestA"},
				{Path: "b.go", Line: 1, Problem: "B is long", Fix: "Rewrite B."},
				{Path: "c.go", Line: 2, Problem: "C has no hunk", Fix: "Guard C."},
			},
			FixDiffs:  []string{"diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3 +3 @@\n-func A() {}\n+func A() int { return 1 }\n", long, ""},
			OtherDiff: "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-x\n+y",
			Tests:     review.Tests{Command: "go test ./...", Passed: true}}, loc)
		link := "[`ccccccc`](https://github.com/acme/app/commit/" + shaC + ")"
		wants := []string{
			loc.Text("round.scenario", "scenario", "A caller reads zero."),
			"```diff\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3 +3 @@\n-func A() {}\n+func A() int { return 1 }\n```",
			loc.Text("round.tests_added", "tests", "TestA"),
			"<details><summary>" + loc.Text("round.diff_summary", "lines", "49") + "</summary>\n\n```diff\ndiff --git a/b.go",
			"+line 44\n```\n\n</details>",
			loc.Text("round.how_fixed", "fix", "Guard C."),
			"**" + loc.Text("round.other_changes") + "**\n\n```diff\ndiff --git a/x.go",
			loc.Text("round.fix_commit", "sha", link),
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Fatalf("%s round lacks %q:\n%s", name, want, body)
			}
		}
		// A diff shown has its prose replaced; the order is problem, scenario, diff, tests added.
		if strings.Contains(body, "Return the value.") || strings.Contains(body, "Rewrite B.") {
			t.Fatalf("%s prose shown beside a diff:\n%s", name, body)
		}
		iProblem, iScenario := strings.Index(body, "A returns nothing"), strings.Index(body, "A caller reads zero.")
		iDiff, iTests := strings.Index(body, "+func A() int"), strings.Index(body, "TestA")
		if iProblem >= iScenario || iScenario >= iDiff || iDiff >= iTests {
			t.Fatalf("%s layout order %d %d %d %d", name, iProblem, iScenario, iDiff, iTests)
		}
		if iLink, iOther := strings.Index(body, link), strings.Index(body, "diff --git a/x.go"); iLink < iOther {
			t.Fatalf("%s commit link before other changes", name)
		}
	}
	plain := RenderRound(RoundReport{N: 1, Max: 3, To: shaB, FixCommit: shaC, Fixed: []review.Fixed{{Path: "a.go", Problem: "p"}},
		FixDiffs: []string{diffLines("a.go", 2)}}, mustLocale(t, "en"))
	if !strings.Contains(plain, "Fix commit: `ccccccc`") || strings.Contains(plain, "<details>") {
		t.Fatalf("round:\n%s", plain)
	}
}

func TestRenderRound_DiffFenceAndTags(t *testing.T) {
	en := mustLocale(t, "en")
	diff := "diff --git a/r.md b/r.md\n--- a/r.md\n+++ b/r.md\n@@ -1 +1,2 @@\n-old\n+````go\n+<!-- ocr-serve-summary -->"
	body := RenderRound(RoundReport{N: 1, Max: 3, To: shaB, FixCommit: shaC, Fixed: []review.Fixed{{Path: "r.md", Problem: "p"}},
		FixDiffs: []string{diff}}, en)
	if !strings.Contains(body, "`````diff\n") || !strings.Contains(body, "+&lt;!-- ocr-serve-summary -->\n`````") || strings.Contains(body, "<!-- ocr-") {
		t.Fatalf("round:\n%s", body)
	}
}

func TestRenderRound_ProposedDiff(t *testing.T) {
	for _, name := range []string{"en", "vi"} {
		loc := mustLocale(t, name)
		short := "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b"
		body := RenderRound(RoundReport{N: 4, Max: 3, To: shaC, Report: ReportLimit, Reported: []review.Reported{
			{Path: "a.go", Line: 1, Problem: "p", Scenario: "s", ProposedFix: "change a", ProposedDiff: short},
			{Path: "b.go", Line: 1, Problem: "q", ProposedDiff: diffLines("b.go", 200)},
			{Path: "c.go", Problem: "r"},
		}}, loc)
		for _, want := range []string{loc.Text("round.proposed_diff") + "\n\n```diff\n" + short + "\n```",
			loc.Text("round.proposed_fix", "fix", "change a"),
			loc.Text("round.diff_summary", "lines", "204"),
			"+line 145\n```\n\n" + loc.Text("round.proposal_cut", "count", "54"),
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s round lacks %q:\n%s", name, want, body)
			}
		}
		if strings.Contains(body, "+line 146") || strings.Count(body, loc.Text("round.proposed_diff")) != 2 {
			t.Fatalf("%s round:\n%s", name, body)
		}
	}
}

func TestRenderRound_SizeCap(t *testing.T) {
	en := mustLocale(t, "en")
	r := RoundReport{N: 1, Max: 3, To: shaB, FixCommit: shaC, CommitURL: "https://github.com/acme/app/commit/" + shaC,
		Fixed:    []review.Fixed{{Path: "a.go", Problem: "small"}, {Path: "big.go", Problem: "big"}},
		FixDiffs: []string{diffLines("a.go", 10), diffLines("big.go", 9000)}}
	body := RenderRound(r, en)
	if n := utf8.RuneCountInString(body); n > maxCommentRunes {
		t.Fatalf("comment has %d runes", n)
	}
	// The small diff collapses but keeps every line; the big one is cut
	// with a note that links the commit.
	for _, want := range []string{"+line 9\n```", "<details><summary>Show the diff (14 lines)</summary>",
		"more lines are not shown here; the fix commit [`ccccccc`](https://github.com/acme/app/commit/" + shaC + ") has the full change."} {
		if !strings.Contains(body, want) {
			t.Fatalf("round lacks %q", want)
		}
	}
	if strings.Contains(body, "+line 8999") {
		t.Fatal("the big diff was not cut")
	}
	// Prose too long even without diffs is cut at the end.
	huge := RoundReport{N: 1, Max: 3, To: shaB}
	for range 80 {
		huge.Reported = append(huge.Reported, review.Reported{Path: "a.go", Problem: strings.Repeat("\u1eeb", 1000),
			ProposedDiff: diffLines("a.go", 3)})
	}
	cut := RenderRound(huge, en)
	if n := utf8.RuneCountInString(cut); n > maxCommentRunes || !strings.HasSuffix(cut, en["round.body_cut"]) || strings.Contains(cut, "+line 0") {
		t.Fatalf("cut comment: %d runes", n)
	}
}

func TestShrinkHelpers(t *testing.T) {
	if shrinkLargest(nil) || collapseAll(nil) {
		t.Fatal("empty blocks changed")
	}
	b := &diffBlock{lines: make([]string, 8), keep: 8}
	if !shrinkLargest([]*diffBlock{b}) || b.keep != 0 {
		t.Fatalf("keep %d", b.keep)
	}
	if newDiffBlock(" \n", false) != nil {
		t.Fatal("blank diff gave a block")
	}
}
