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

func TestRenderCommandBusy(t *testing.T) {
	en, err := LoadLocale("en")
	if err != nil {
		t.Fatal(err)
	}
	vi, err := LoadLocale("vi")
	if err != nil {
		t.Fatal(err)
	}
	got := RenderCommandBusy(2, 3, ScopeCommit, en)
	if !strings.Contains(got, "round 2/3") || !strings.Contains(got, "the latest commit") || strings.Contains(got, "{") {
		t.Fatalf("en: %q", got)
	}
	if got := RenderCommandBusy(1, 3, ScopeFull, vi); !strings.Contains(got, vi["command.scope_full"]) || vi["command.scope_full"] == en["command.scope_full"] || strings.Contains(got, "{") {
		t.Fatalf("vi: %q", got)
	}
}
