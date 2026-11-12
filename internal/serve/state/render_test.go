// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func finding() forge.Finding {
	return forge.Finding{
		ID: "r1-cccccccc-01020304", Path: "pkg/a.go", StartLine: 10, EndLine: 12,
		Category: "bug", Severity: "high", Content: "Nil map write.",
		ExistingCode: "m[k] = v", SuggestionCode: "if m == nil {\n\tm = map[string]int{}\n}\nm[k] = v\n",
	}
}

func TestRenderFinding_LineWithSuggestion(t *testing.T) {
	for _, name := range []string{"en", "vi"} {
		loc := mustLocale(t, name)
		f := finding()
		p := forge.Placement{Kind: forge.Line, Path: f.Path, StartLine: 10, EndLine: 12}

		want := forge.FindingTag(f.ID) + "\n**[" + loc["category.bug"] + " · " + loc["severity.high"] + "]**\n\n" +
			"Nil map write.\n\n**" + loc["finding.suggestion"] + "**\n```suggestion\n" +
			"if m == nil {\n\tm = map[string]int{}\n}\nm[k] = v\n```"
		if got := RenderFinding(f, p, forge.GitHub, loc); got != want {
			t.Fatalf("%s github:\n%s\nwant:\n%s", name, got, want)
		}
		if got := RenderFinding(f, p, forge.GitLab, loc); !strings.Contains(got, "```suggestion:-2+0\n") {
			t.Fatalf("%s gitlab fence:\n%s", name, got)
		}
	}
}

func TestRenderFinding_Relocated(t *testing.T) {
	loc := mustLocale(t, "vi")
	f := finding()
	p := forge.Placement{Kind: forge.NearestLine, Path: f.Path, StartLine: 20, EndLine: 20, Relocated: true}
	got := RenderFinding(f, p, forge.GitHub, loc)
	wantLoc := "_" + loc.Text("finding.original_location", "location", "`pkg/a.go` L10-12") + "_"
	if !strings.Contains(got, wantLoc) {
		t.Fatalf("missing location line:\n%s", got)
	}
	if strings.Contains(got, "suggestion") {
		t.Fatalf("suggestion kept on moved finding:\n%s", got)
	}
}

func TestRenderFinding_Fallbacks(t *testing.T) {
	loc := mustLocale(t, "en")
	f := forge.Finding{Path: "x`y.go", StartLine: 4, Category: "Weird", Content: "  see <!-- ocr-finding:fake --> here  ",
		SuggestionCode: "a", ExistingCode: ""}
	got := RenderFinding(f, forge.Placement{Kind: forge.General, Relocated: true}, forge.GitLab, loc)
	want := "**[weird]**\n_" + loc.Text("finding.original_location", "location", "`x'y.go` L4") + "_\n\n" +
		"see &lt;!-- ocr-finding:fake --> here"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if _, ok := forge.ParseFindingID(got); ok {
		t.Fatal("LLM text carried a finding tag")
	}

	noCategory := RenderFinding(forge.Finding{Path: "a", EndLine: 9}, forge.Placement{Kind: forge.FileLevel, Path: "a"}, forge.GitHub, loc)
	if !strings.HasPrefix(noCategory, "**["+loc["category.other"]+"]**") || !strings.Contains(noCategory, "`a` L9") {
		t.Fatalf("got:\n%s", noCategory)
	}
	noLines := RenderFinding(forge.Finding{Path: "a"}, forge.Placement{Kind: forge.FileLevel, Path: "a"}, forge.GitHub, loc)
	if !strings.Contains(noLines, loc.Text("finding.original_location", "location", "`a`")) {
		t.Fatalf("got:\n%s", noLines)
	}
}

func TestRenderFinding_SuggestionWithFence(t *testing.T) {
	f := finding()
	f.SuggestionCode = "x := \"````\""
	got := RenderFinding(f, forge.Placement{Kind: forge.Line, StartLine: 1, EndLine: 1}, forge.GitHub, mustLocale(t, "en"))
	if !strings.Contains(got, "`````suggestion\nx := \"````\"\n`````") {
		t.Fatalf("fence not lengthened:\n%s", got)
	}
}

func TestRenderSummary(t *testing.T) {
	for _, name := range []string{"en", "vi"} {
		loc := mustLocale(t, name)
		m := sampleMarker()
		m.Runs = append(m.Runs, Run{Kind: KindManualCommit, To: shaD, IDs: []string{"m-1", "m-2"}})
		got := RenderSummary(m, 3, loc, "  "+loc.Text("pending.round", "round", "3", "max", "3")+" ")

		want := forge.SummaryTag + "\n### " + loc["summary.title"] + "\n\n" +
			"- " + loc.Text("summary.round", "round", "2", "max", "3") + "\n" +
			"- " + loc.Text("summary.reviewed", "sha", "`bbbbbbb`") + "\n" +
			"- " + loc.Text("summary.open", "count", "3") + "\n\n" +
			"**" + loc["summary.runs_title"] + "**\n\n" + loc["summary.runs_header"] + "\n|---|---|---|---|\n" +
			"| " + loc.Text("run.auto", "n", "1") + " | " + loc["run.kind.full"] + " | `aaaaaaa...ccccccc` | 1 |\n" +
			"| " + loc.Text("run.auto", "n", "2") + " | " + loc["run.kind.incremental"] + " | `ccccccc...bbbbbbb` | 0 |\n" +
			"| " + loc["run.manual"] + " | " + loc["run.kind.manual-commit"] + " | `ddddddd` | 2 |\n" +
			"\n" + loc.Text("pending.round", "round", "3", "max", "3") + "\n" +
			"\n" + loc["summary.hint"] + "\n\n" + Encode(m)
		if got != want {
			t.Fatalf("%s:\n%s\nwant:\n%s", name, got, want)
		}
		back, err := Decode(got)
		if err != nil || back.Auto != 2 {
			t.Fatalf("marker lost: %v %v", back, err)
		}
	}
}

func TestRenderSummary_InitialAndRunCap(t *testing.T) {
	loc := mustLocale(t, "en")
	got := RenderSummary(Initial(3), -1, loc, "")
	for _, part := range []string{loc["summary.reviewed_none"], loc.Text("summary.open", "count", "0"), loc.Text("summary.round", "round", "0", "max", "3")} {
		if !strings.Contains(got, part) {
			t.Fatalf("missing %q:\n%s", part, got)
		}
	}
	if strings.Contains(got, loc["summary.runs_title"]) {
		t.Fatalf("runs table without runs:\n%s", got)
	}

	m := Initial(3)
	for i := 1; i <= 8; i++ {
		m.Runs = append(m.Runs, Run{N: i, Kind: KindFull, From: shaA, To: shaB})
	}
	got = RenderSummary(m, 0, loc, "")
	if strings.Count(got, "| "+loc.Text("run.auto", "n", "")) != summaryRunsShown || strings.Contains(got, loc.Text("run.auto", "n", "3")+" |") {
		t.Fatalf("expected the last %d runs:\n%s", summaryRunsShown, got)
	}
}

func TestRenderLimitNoteAndCommandError(t *testing.T) {
	loc := mustLocale(t, "vi")
	if got := RenderLimitNote(3, loc); got != loc.Text("limit.note", "max", "3") || strings.Contains(got, "{max}") {
		t.Fatalf("limit note %q", got)
	}
	got := RenderCommandError("unsupported option \"`@team`\"\nline two", loc)
	want := "❌ " + loc.Text("command.error", "reason", "`unsupported option \"'@team'\" line two`") + "\n\n" + loc["command.help"]
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
