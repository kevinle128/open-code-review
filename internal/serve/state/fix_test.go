// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func TestParseFindingReversesRender(t *testing.T) {
	for _, name := range []string{"en", "vi"} {
		loc, err := LoadLocale(name)
		if err != nil {
			t.Fatal(err)
		}
		fd := forge.Finding{ID: "r1-aaaa-01", Path: "a.go", StartLine: 3, EndLine: 3, Category: "bug", Severity: "high",
			Content: "**Missing return**\n\nA returns nothing.\n\nfunc A() error { return nil }"}
		body := RenderFinding(fd, forge.Placement{Kind: forge.Line}, forge.GitHub, loc)
		got := ParseFinding(body, loc)
		if got.Severity != "high" || got.Title != "Missing return" || got.Body != "A returns nothing.\n\nfunc A() error { return nil }" {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}

func TestParseFindingOtherShapes(t *testing.T) {
	loc := enLocale(t)
	cases := []struct {
		body string
		want ParsedFinding
	}{
		// An OCR engine finding has no title line.
		{forge.FindingTag("x") + "\n**[Bug · Low]**\n\nPlain content.", ParsedFinding{Severity: "low", Body: "Plain content."}},
		// A severity the locale does not know keeps its label.
		{"**[Bug · Blocker]**\nText", ParsedFinding{Severity: "Blocker", Body: "Text"}},
		{"**[Other]**\n\n**Title**", ParsedFinding{Title: "Title"}},
		{"no header at all", ParsedFinding{Body: "no header at all"}},
		{forge.FindingTag("x"), ParsedFinding{}},
		{"**a** and **b**\nrest", ParsedFinding{Body: "**a** and **b**\nrest"}},
	}
	for _, tc := range cases {
		if got := ParseFinding(tc.body, loc); got != tc.want {
			t.Errorf("ParseFinding(%q) = %+v, want %+v", tc.body, got, tc.want)
		}
	}
}

func TestRenderFixTexts(t *testing.T) {
	loc := enLocale(t)
	skipped := []FixSkipped{
		{Path: "a.go", Line: 3, Title: "Missing return", Reason: "The author\ndisputed it."},
		{Path: "b.go", Reason: "  "},
		{Reason: "needs a decision " + serviceTagPrefix + "summary -->"},
	}
	done := RenderFixDone(strings.Repeat("f", 40), 2, "Added a check. "+serviceTagPrefix+"x -->", skipped, loc)
	for _, want := range []string{
		"✅ " + loc.Text("fix.done", "sha", "`fffffff`", "fixed", "2", "skipped", "3"),
		"Added a check. &lt;!-- ocr-x -->",
		"**" + loc.Text("fix.skipped_title") + "**",
		"- Missing return (`a.go` L3): The author disputed it.",
		"- (`b.go`): " + loc.Text("fix.no_reason"),
		"\n- needs a decision &lt;!-- ocr-summary -->",
	} {
		if !strings.Contains(done, want) {
			t.Fatalf("done note lacks %q:\n%s", want, done)
		}
	}
	if strings.Contains(done, serviceTagPrefix) {
		t.Fatalf("service tag survived:\n%s", done)
	}
	if got := RenderFixNoChanges("", nil, loc); got != "ℹ️ "+loc.Text("fix.no_changes") {
		t.Fatalf("no changes %q", got)
	}
	if got := RenderFixNothing(loc); got != "✅ "+loc.Text("fix.nothing") {
		t.Fatalf("nothing %q", got)
	}
	if got := RenderFixCannotPush("feat/`x`", "remote: Permission\ndenied", loc); got != "❌ "+loc.Text("fix.cannot_push", "branch", "`feat/'x'`", "reason", "`remote: Permission denied`") {
		t.Fatalf("cannot push %q", got)
	}
	if got := RenderFixSkipReply("", loc); got != loc.Text("fix.skip_reply", "reason", loc.Text("fix.no_reason")) {
		t.Fatalf("skip reply %q", got)
	}
}

func enLocale(t *testing.T) Locale {
	t.Helper()
	loc, err := LoadLocale("en")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
