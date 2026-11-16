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

// diffLines is a one-file diff with n added lines.
func diffLines(path string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1,0 +1,%d @@\n", path, path, path, path, n)
	for i := range n {
		fmt.Fprintf(&b, "+line %d\n", i)
	}
	return b.String()
}

func ghLines(sha, path string, line int) string {
	return forge.LineLink("https://github.com", "acme/app", "/blob/", sha, path, line)
}

func glLines(sha, path string, line int) string {
	return forge.LineLink("https://git.example", "grp/app", "/-/blob/", sha, path, line)
}

func mustHave(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Fatalf("comment lacks %q:\n%s", w, body)
		}
	}
}

func mustLack(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(body, w) {
			t.Fatalf("comment has %q:\n%s", w, body)
		}
	}
}

// sampleRound is a whole-PR round with one fixed, one reported and two
// dropped findings, one dropped without a reason.
func sampleRound() RoundReport {
	return RoundReport{N: 1, Max: 3, From: shaA, To: shaB, FixCommit: shaC,
		CommitURL: "https://github.com/acme/app/commit/" + shaC, LineURL: ghLines,
		Coverage: review.Coverage{Total: 18, Reviewed: 18},
		Findings: []review.Finding{
			{ID: "F1", Path: "internal/job/env.go", Line: 12, Severity: "low", Category: "style", Content: "**Name `limit` better**\n\nIt is vague."},
			{ID: "F2", Path: "internal/job/env.go", Line: 31, Severity: "medium", Category: "bug", Content: "overflow"},
			{ID: "F3", Path: "", Severity: "medium", Category: "documentation", Content: "description"},
			{ID: "F4", Path: "internal/job/stall.go", Line: 88, Severity: "high", Category: "bug", Content: "race"},
			{ID: "F5", Path: "internal/job/stall.go", Line: 60, Severity: "medium", Category: "bug", Content: "nil"},
		},
		Fixed: []review.Fixed{
			{ID: "F4", Path: "internal/job/stall.go", Line: 88, Title: "Notice after the end <!-- ocr-x -->",
				Problem: "stallFire unlocks first.", Scenario: "The job ends between the steps.", Fix: "Hold the lock.", TestsAdded: "TestStallFire_AfterFinish"},
			{ID: "F2", Path: "internal/job/env.go", Line: 31, Problem: "A large value overflows. It turns negative.", Fix: "Reject it."},
		},
		FixDiffs: []string{"diff --git a/s.go b/s.go\n--- a/s.go\n+++ b/s.go\n@@ -3 +3 @@\n-a\n+b\n", diffLines("e.go", 45)},
		Reported: []review.Reported{{ID: "F3", Title: "The description names a TUI change", Problem: "No TUI file changed.", ProposedFix: "Drop the sentence."}},
		Dropped:  []review.Dropped{{ID: "F5", Title: "No nil check for `j.notify`", Reason: "`j.notify` is always set in `newJob`."}},
		Tests:    review.Tests{Command: "go test ./...", Passed: true},
		Next:     2,
	}
}

func TestRenderRound_Layout(t *testing.T) {
	en := mustLocale(t, "en")
	body := RenderRound(sampleRound(), en)
	mustHave(t, body,
		"## 🔍 OCR review, round 1/3\n\n**(Reviewed the whole PR: `aaaaaaa...bbbbbbb`)**\n\n<table>\n",
		"<tr><td>🔎&nbsp;<strong>Found</strong>: 5 issues → 3 real, 2 skipped</td></tr>",
		`<tr><td>🛠️&nbsp;<strong>Fixed</strong>: 2, commit <a href="https://github.com/acme/app/commit/`+shaC+`"><code>ccccccc</code></a></td></tr>`,
		"<tr><td>⚠️&nbsp;<strong>Needs you</strong>: 1</td></tr>",
		"<tr><td>🧪&nbsp;<strong>Tests after the fix</strong>: <code>go test ./...</code> passed</td></tr>",
		"<tr><td>📄&nbsp;<strong>Scope</strong>: reviewed 18/18 files</td></tr>",
		"<tr><td>➡️&nbsp;<strong>Next</strong>: round 2/3 reviews commit <code>ccccccc</code></td></tr>\n</table>",
		"### 🔎 What was found and what happened (5)\n\n<table><thead><tr><td>#</td><td>Issue</td><td>Severity</td><td>Result</td></tr></thead>\n",
		// High first; the fixed item links the fix commit and shows its diff.
		"<tr><td>1</td><td>\n\n<details><summary>Notice after the end &lt;!-- ocr-x --&gt;</summary>\n\n📍 [`internal/job/stall.go` L88](https://github.com/acme/app/blob/"+shaC+"/internal/job/stall.go#L88) · Bug\n",
		"**Problem**: stallFire unlocks first.",
		"**Failing scenario**: The job ends between the steps.",
		"**Fixed**: Hold the lock.\n\n```diff\ndiff --git a/s.go",
		"🧪 Tests added: TestStallFire_AfterFinish\n\n</details>\n\n<sub>`internal/job/stall.go` L88 · Bug</sub>\n\n</td><td>🔴 High</td><td>✅ Fixed</td></tr>",
		// Medium in review order: the fix with a fallback title and a collapsed diff, the PR-level report, the dropped one.
		"<tr><td>2</td><td>\n\n<details><summary>A large value overflows</summary>",
		"<details><summary>Show the diff (49 lines)</summary>",
		"<tr><td>3</td><td>\n\n<details><summary>The description names a TUI change</summary>\n\n📍 Whole PR · Documentation\n",
		"**Proposed fix**: Drop the sentence.",
		"<sub>Whole PR · Documentation</sub>\n\n</td><td>🟠 Medium</td><td>⚠️ Needs you</td></tr>",
		"<tr><td>4</td><td>\n\n<details><summary>No nil check for <code>j.notify</code></summary>\n\n📍 [`internal/job/stall.go` L60](https://github.com/acme/app/blob/"+shaB+"/internal/job/stall.go#L60) · Bug\n",
		"**Why skipped**: `j.notify` is always set in `newJob`.",
		"</td><td>🟠 Medium</td><td>➖ Skipped</td></tr>",
		// Low last; the title comes from the bold lead of the review text.
		"<tr><td>5</td><td>\n\n<details><summary>Name <code>limit</code> better</summary>",
		"</td><td>⚪ Low</td><td>➖ Skipped</td></tr>\n</table>",
	)
	mustLack(t, body, "<!-- ocr-x", "**Why skipped**: \n", "<style", "style=")
	if strings.Index(body, "<tr><td>4</td>") > strings.Index(body, "<tr><td>5</td>") {
		t.Fatal("rows out of order")
	}
}

func TestRenderRound_GitLabLinksAndVietnamese(t *testing.T) {
	vi, en := mustLocale(t, "vi"), mustLocale(t, "en")
	r := sampleRound()
	r.LineURL, r.CommitURL = glLines, "https://git.example/grp/app/-/commit/"+shaC
	body := RenderRound(r, vi)
	mustHave(t, body,
		"## 🔍 "+vi.Text("round.title", "round", RoundLabel(1, 3, vi)),
		"(https://git.example/grp/app/-/blob/"+shaC+"/internal/job/stall.go#L88)",
		"(https://git.example/grp/app/-/blob/"+shaB+"/internal/job/stall.go#L60)",
		"<strong>"+vi["round.found"]+"</strong>",
		vi["finding.result.fixed"], vi["severity.high"], vi["category.bug"],
	)
	mustLack(t, body, en["round.found"]+"</strong>", en["finding.why_skipped"])
}

func TestRenderRound_ReportReasons(t *testing.T) {
	en := mustLocale(t, "en")
	one := []review.Reported{{Path: "a.go", Line: 1, Problem: "p"}}
	limit := RenderRound(RoundReport{N: 4, Max: 3, To: shaC, Reported: one, Report: ReportLimit}, en)
	mustHave(t, limit, "OCR review, round 4 (report only)", "**(Reviewed commit `ccccccc`)**",
		"<strong>Fixed</strong>: 0</td>", "<strong>Needs you</strong>: 1<br>The 3 fix rounds are used up",
		"<tr><td>🏁&nbsp;<strong>End</strong>: the review chain stops at this round; merge is allowed</td></tr>", "<td>-</td>")
	mustLack(t, limit, "Tests after the fix", "Scope")
	push := RenderRound(RoundReport{N: 2, Max: 3, To: shaC, FixReview: true, Reported: one,
		Report: ReportPush, Branch: "feat/<x>", PushReason: "protected branch"}, en)
	mustHave(t, push, "**(Reviewed fix commit `ccccccc`)**", "branch <code>feat/&lt;x&gt;</code>: <code>protected branch</code>")
	for _, reason := range []string{ReportFork, ReportEngine, ReportNoEdit} {
		body := RenderRound(RoundReport{N: 1, Max: 3, To: shaC, Reported: one, Report: reason}, en)
		mustHave(t, body, en.Text("round.report."+reason, "branch", "<code></code>"))
	}
}

func TestRenderRound_ZeroFindingsAndSkipped(t *testing.T) {
	en := mustLocale(t, "en")
	clean := RenderRound(RoundReport{N: 2, Max: 3, To: shaC, FixReview: true,
		Coverage: review.Coverage{Total: 2, Reviewed: 1, Failed: []review.FailedItem{{Path: "b.go", OldPath: "old.go", Class: "timeout"}}}}, en)
	mustHave(t, clean, "<strong>Found</strong>: no issue</td>", "reviewed 1/2 files",
		"`old.go` → `b.go`: the review of this file timed out", "<strong>End</strong>")
	mustLack(t, clean, "<strong>Fixed</strong>", "<thead>")
	skipped := RenderRound(RoundReport{N: 1, Max: 3, From: shaA, To: shaB, Skipped: true}, en)
	mustHave(t, skipped, en["round.skipped"], en["round.end_text"])
	mustLack(t, skipped, "<table>")
}

func TestRenderRound_Tests(t *testing.T) {
	en := mustLocale(t, "en")
	fixed := []review.Fixed{{Path: "a.go", Problem: "p"}}
	failed := RenderRound(RoundReport{N: 1, Max: 3, To: shaB, FixCommit: shaC, Fixed: fixed,
		Tests: review.Tests{Command: "npm  test", OutputTail: "--- FAIL: TestA\n```"}}, en)
	mustHave(t, failed, "<tr><td>❌&nbsp;<strong>Tests after the fix</strong>: <code>npm test</code> failed or could not run; the fix was pushed anyway\n"+
		"<details><summary>End of the output</summary>\n\n````\n--- FAIL: TestA\n```\n````\n\n</details>\n</td></tr>",
		"<strong>Fixed</strong>: 1, commit <code>ccccccc</code>")
	none := RenderRound(RoundReport{N: 1, Max: 3, To: shaB, Fixed: fixed}, en)
	mustHave(t, none, "<strong>Tests after the fix</strong>: not run", "<strong>Fixed</strong>: 1</td>")
	quiet := RenderRound(RoundReport{N: 1, Max: 3, To: shaB, Fixed: fixed, Tests: review.Tests{Command: "make"}}, en)
	mustHave(t, quiet, "❌&nbsp;<strong>Tests after the fix</strong>: <code>make</code> failed or could not run; the fix was pushed anyway</td>")
}

func TestRenderRound_MergesAndKeepsUnlisted(t *testing.T) {
	en := mustLocale(t, "en")
	body := RenderRound(RoundReport{N: 1, Max: 3, To: shaB, FixCommit: shaC,
		Findings: []review.Finding{
			{ID: "F1", Path: "a.go", Line: 1, Severity: "low", Content: "**Missing check**"},
			{ID: "F2", Path: "a.go", Line: 9, Severity: "high", Content: "**missing check **"},
			{ID: "F3", Path: "b.go", Line: 2, Severity: "weird", Category: "odd", Content: "Unlisted one. More text."},
			{ID: "F4", Path: "c.go", Line: 2, Severity: "medium", Content: "**Missing check**"},
			{ID: "F5", Path: "a.go", Line: 3, Content: "**Same diff**"},
			{ID: "F6", Path: "a.go", Line: 4, Content: "**Same diff**"},
		},
		Fixed: []review.Fixed{
			{ID: "F2", Path: "a.go", Line: 9, Problem: "p", Fix: "f"},
			{ID: "F5", Path: "a.go", Line: 3, Problem: "q", Fix: "g"},
			{ID: "F6", Path: "a.go", Line: 4, Problem: "r", Fix: "h"},
		},
		FixDiffs: []string{diffLines("a.go", 1), sharedHunkDiff, sharedHunkDiff},
		Dropped:  []review.Dropped{{ID: "F1", Reason: "same as the other"}, {ID: "nope"}},
	}, en)
	// F1 and F2 merge: one row, fixed, with the higher severity.
	if n := strings.Count(body, "<summary>Missing check</summary>"); n != 2 {
		t.Fatalf("want the a.go and c.go rows only, got %d:\n%s", n, body)
	}
	mustHave(t, body, "<strong>Found</strong>: 4 issues → 2 real, 2 skipped",
		"<td>🔴 High</td><td>✅ Fixed</td>", "<summary>Unlisted one</summary>", "<td>weird</td><td>➖ Skipped</td>", "· odd")
	mustLack(t, body, "same as the other")
	if n := strings.Count(body, "@@ -10,2 +10,3 @@"); n != 1 {
		t.Fatalf("a merged diff shows each hunk once, got %d:\n%s", n, body)
	}
}

func TestRenderRound_SharedDiffNamesRows(t *testing.T) {
	loc := mustLocale(t, "vi")
	body := RenderRound(RoundReport{N: 1, Max: 3, From: "a", To: "b", FixCommit: "0123456789abcdef",
		Fixed: []review.Fixed{
			{ID: "F1", Path: "q.go", Line: 11, Title: "t1", Fix: "fix1"},
			{ID: "F2", Path: "q.go", Line: 11, Title: "t2", Fix: "fix2"},
		},
		FixDiffs: []string{sharedHunkDiff, sharedHunkDiff},
		Reported: []review.Reported{
			{ID: "R1", Path: "q.go", Line: 11, Title: "t3", ProposedDiff: sharedHunkDiff},
			{ID: "R2", Path: "q.go", Line: 11, Title: "t4", ProposedDiff: sharedHunkDiff},
		},
	}, loc)
	if n := strings.Count(body, "@@ -10,2 +10,3 @@"); n != 2 {
		t.Fatalf("shared hunk must show once per result, got %d:\n%s", n, body)
	}
	mustHave(t, body, loc.Text("round.diff_shared", "items", "1"), loc.Text("round.diff_shared", "items", "3"), "fix2")
}

func TestRenderRound_FixDiffsAndOther(t *testing.T) {
	en := mustLocale(t, "en")
	body := RenderRound(RoundReport{N: 1, Max: 3, To: shaB, FixCommit: shaC,
		Fixed:     []review.Fixed{{Path: "a.go", Problem: "p", Fix: "Guard it."}},
		OtherDiff: "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-x\n+y"}, en)
	mustHave(t, body, "**Fixed**: Guard it.", "</table>\n\n**Other changes in the fix commit**\n\n```diff\ndiff --git a/x.go")
	mustLack(t, body, "<details><summary>Show the diff")
}

func TestRenderRound_FenceAndLayoutTags(t *testing.T) {
	en := mustLocale(t, "en")
	diff := "diff --git a/r.md b/r.md\n--- a/r.md\n+++ b/r.md\n@@ -1 +1,2 @@\n-old\n+````go\n+<!-- ocr-serve-summary -->"
	body := RenderRound(RoundReport{N: 1, Max: 3, To: shaB, FixCommit: shaC,
		Fixed:    []review.Fixed{{Path: "r.md", Title: "a `b", Problem: "it holds </td></tr></TABLE> and <Details>", Fix: "x"}},
		FixDiffs: []string{diff}}, en)
	mustHave(t, body, "`````diff\n", "+&lt;!-- ocr-serve-summary -->\n`````", "<summary>a `b</summary>",
		"it holds &lt;/td>&lt;/tr>&lt;/TABLE> and &lt;Details>")
	mustLack(t, body, "<!-- ocr-")
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
		mustHave(t, body, loc.Text("round.proposed_diff")+"\n\n```diff\n"+short+"\n```",
			"**"+loc.Text("finding.proposed_fix")+"**: change a",
			loc.Text("round.diff_summary", "lines", "204"),
			"+line 145\n```\n\n"+loc.Text("round.proposal_cut", "count", "54"))
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
	mustHave(t, body, "+line 9\n```", "<details><summary>Show the diff (14 lines)</summary>",
		"more lines are not shown here; the fix commit [`ccccccc`](https://github.com/acme/app/commit/"+shaC+") has the full change.")
	mustLack(t, body, "+line 8999")
	// Prose too long even without diffs is cut after the last whole row,
	// with the note outside the table.
	huge := RoundReport{N: 1, Max: 3, To: shaB}
	for i := range 80 {
		huge.Reported = append(huge.Reported, review.Reported{Path: fmt.Sprintf("a%d.go", i), Problem: strings.Repeat("\u1eeb", 1000),
			ProposedDiff: diffLines("a.go", 3)})
	}
	cut := RenderRound(huge, en)
	if n := utf8.RuneCountInString(cut); n > maxCommentRunes || !strings.HasSuffix(cut, "</td></tr>\n</table>\n\n"+en["round.body_cut"]) || strings.Contains(cut, "+line 0") {
		t.Fatalf("cut comment: %d runes, tail %q", n, cut[max(0, len(cut)-200):])
	}
	if strings.Count(cut, "<details>") != strings.Count(cut, "</details>") {
		t.Fatal("the cut left a details element open")
	}
	// A cut in the other diff after the finding table ends after the table.
	tail := "<table>\n<tr><td>s</td></tr>\n</table>\n" + strings.Repeat("y", 100) + "\n<details><summary>d</summary>\n\n" + strings.Repeat("z\n", maxCommentRunes)
	if got := cutBody(tail, en); got != "<table>\n<tr><td>s</td></tr>\n</table>\n\n"+en["round.body_cut"] {
		t.Fatalf("other diff cut tail %q", got[max(0, len(got)-200):])
	}
	// A cut in the first finding row does not close the status table again.
	first := "<table>\n<tr><td>s</td></tr>\n</table>\n\n<table><thead><tr><td>#</td></tr></thead>\n<tr><td>1</td><td>\n\n<details><summary>t</summary>\n\n" + strings.Repeat("w\n", maxCommentRunes)
	if got := cutBody(first, en); got != "<table>\n<tr><td>s</td></tr>\n</table>\n\n\n"+en["round.body_cut"] {
		t.Fatalf("first row cut tail %q", got[max(0, len(got)-200):])
	}
	plain := cutBody(strings.Repeat("x\n", maxCommentRunes), en)
	if !strings.HasSuffix(plain, "x\n\n"+en["round.body_cut"]) || utf8.RuneCountInString(plain) > maxCommentRunes {
		t.Fatalf("plain cut tail %q", plain[len(plain)-100:])
	}
}

func TestTitleHelpers(t *testing.T) {
	for _, c := range []struct{ problem, content, want string }{
		{"First one. Second one.", "", "First one"},
		{"", "**Bold lead** and body", "Bold lead"},
		{"Line one\nline two", "", "Line one"},
		{"Ends with a period.", "", "Ends with a period"},
		{"", "", ""},
	} {
		if got := fallbackTitle(c.problem, c.content); got != c.want {
			t.Fatalf("fallbackTitle(%q, %q) = %q", c.problem, c.content, got)
		}
	}
	long := capTitle(strings.Repeat("word ", 40))
	if n := utf8.RuneCountInString(long); n != maxTitleRunes || !strings.HasSuffix(long, "…") {
		t.Fatalf("cap %d %q", n, long)
	}
	if summaryText("a `b` <c> `d") != "a <code>b</code> &lt;c&gt; `d" {
		t.Fatal(summaryText("a `b` <c> `d"))
	}
	if got := fallbackTitle("", "**Nil `*Server` in `locale/*.txt`** body"); got != "Nil `*Server` in `locale/*.txt`" {
		t.Fatalf("star title %q", got)
	}
	row := &findingRow{problem: "**Lead**\n\nBody text."}
	fillTitle(row, "")
	if row.title != "Lead" || row.problem != "Body text." {
		t.Fatalf("lead %+v", row)
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

func TestWriteField_Fences(t *testing.T) {
	en := mustLocale(t, "en")
	var b strings.Builder
	writeField(&b, en, "finding.proposed_fix", "```go\nif x == nil {\n\treturn nil\n}\n```")
	if got := b.String(); !strings.Contains(got, "**:\n\n```go\n") || strings.Count(got, "```") != 2 {
		t.Fatalf("leading fence %q", got)
	}
	b.Reset()
	writeField(&b, en, "finding.fixed", "Do this:\n~~~~\ncode")
	if got := b.String(); !strings.Contains(got, "**: Do this:") || !strings.HasSuffix(got, "code\n~~~~\n") {
		t.Fatalf("open fence %q", got)
	}
	b.Reset()
	writeField(&b, en, "finding.fixed", "Use ```x``` inline.")
	if got := b.String(); !strings.HasSuffix(got, "**: Use ```x``` inline.\n") {
		t.Fatalf("inline code %q", got)
	}
}
