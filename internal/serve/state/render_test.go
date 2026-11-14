// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"strings"
	"testing"

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
