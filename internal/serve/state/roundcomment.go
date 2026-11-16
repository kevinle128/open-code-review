// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alibaba/open-code-review/internal/serve/review"
)

// Reasons a round reports without fixing.
const (
	ReportLimit  = "limit"
	ReportFork   = "fork"
	ReportPush   = "push"
	ReportEngine = "engine"
	ReportNoEdit = "no-edit"
)

// RoundReport is what one round comment shows. From is empty for a commit
// round; FixReview marks that commit as the fix the previous round pushed.
// Findings are the review findings the round judged, in review order; each
// shows as fixed, reported or dropped by its id, and as dropped without a
// reason when no list holds it. Report names why the round did not fix
// (one of the Report constants), or is empty for a fix round. Branch and
// PushReason fill the text of ReportFork and ReportPush. Next is the round
// that reviews the fix commit, or 0 when the chain ends with this round.
// FixDiffs holds the part of the fix commit diff of each Fixed entry, by
// index, and OtherDiff the part no finding claims; CommitURL is the web page
// of FixCommit. LineURL, when set, gives the web page of a line at a commit.
type RoundReport struct {
	N, Max     int
	From, To   string
	FixReview  bool
	Coverage   review.Coverage
	Skipped    bool
	Findings   []review.Finding
	Fixed      []review.Fixed
	Reported   []review.Reported
	Dropped    []review.Dropped
	Tests      review.Tests
	FixCommit  string
	CommitURL  string
	LineURL    func(sha, path string, line int) string
	FixDiffs   []string
	OtherDiff  string
	Report     string
	Branch     string
	PushReason string
	Next       int
}

// Bounds of a round comment. maxCommentRunes stays under the 65536
// characters GitHub takes in one comment; GitLab takes more, but one bound
// keeps both forges the same.
const (
	collapseLines   = 40
	maxProposalLine = 150
	maxCommentRunes = 65000
	minDiffKeep     = 5
	maxTitleRunes   = 80
)

// diffBlock is one diff in the comment. keep is how many lines are shown;
// collapsed puts the diff in a details element.
type diffBlock struct {
	lines     []string
	keep      int
	collapsed bool
	proposal  bool
}

func newDiffBlock(text string, proposal bool) *diffBlock {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	keep := len(lines)
	if proposal {
		keep = min(keep, maxProposalLine)
	}
	return &diffBlock{lines: lines, keep: keep, collapsed: len(lines) > collapseLines, proposal: proposal}
}

// Results of a finding in the round.
const (
	resultFixed    = "fixed"
	resultReported = "reported"
	resultDropped  = "dropped"
)

// resultRank orders results by how much a reader needs them, so a merged
// row keeps the stronger one.
var resultRank = map[string]int{resultFixed: 0, resultReported: 1, resultDropped: 2}

// findingRow is one row of the finding table. text is the fix, the
// proposed fix or the reason to skip, by result. diff is the raw diff
// text, and block with refs what the comment shows of it.
type findingRow struct {
	id, result              string
	path                    string
	line                    int
	title, severity, cat    string
	problem, scenario, text string
	testsAdded, diff        string
	block                   *diffBlock
	refs                    []int
}

// roundView is a round report prepared for the comment: the deduplicated
// and ordered rows and the diff of the fix commit no finding claims.
type roundView struct {
	rows  []*findingRow
	other *diffBlock
}

func (v *roundView) blocks() []*diffBlock {
	out := make([]*diffBlock, 0, len(v.rows)+1)
	for _, r := range v.rows {
		if r.block != nil {
			out = append(out, r.block)
		}
	}
	if v.other != nil {
		out = append(out, v.other)
	}
	return out
}

func (v *roundView) count(result string) int {
	n := 0
	for _, r := range v.rows {
		if r.result == result {
			n++
		}
	}
	return n
}

// RenderRound builds the comment of one round: a status table, then one
// table row per finding with its details collapsed. Agent text is shown as
// markdown, with service tags and table tags neutralized; a test output
// tail and each diff go into a fence they cannot close. A comment over
// maxCommentRunes first collapses every diff, then shows fewer lines of
// the largest diff until it fits, and as a last resort is cut.
func RenderRound(r RoundReport, loc Locale) string {
	v := buildView(r)
	for {
		body := renderRound(r, loc, v)
		if utf8.RuneCountInString(body) <= maxCommentRunes {
			return body
		}
		if !collapseAll(v.blocks()) && !shrinkLargest(v.blocks()) {
			return cutBody(body, loc)
		}
	}
}

// buildView joins the findings with the fix run lists, merges rows that
// name the same problem on the same path, orders the rows by severity and
// shows each shared hunk once.
func buildView(r RoundReport) *roundView {
	var listed []*findingRow
	for i, f := range r.Fixed {
		row := &findingRow{id: f.ID, result: resultFixed, path: f.Path, line: f.Line, title: f.Title,
			problem: f.Problem, scenario: f.Scenario, text: f.Fix, testsAdded: f.TestsAdded}
		if i < len(r.FixDiffs) {
			row.diff = r.FixDiffs[i]
		}
		listed = append(listed, row)
	}
	for _, f := range r.Reported {
		listed = append(listed, &findingRow{id: f.ID, result: resultReported, path: f.Path, line: f.Line, title: f.Title,
			problem: f.Problem, scenario: f.Scenario, text: f.ProposedFix, diff: f.ProposedDiff})
	}
	for _, d := range r.Dropped {
		listed = append(listed, &findingRow{id: d.ID, result: resultDropped, title: d.Title, text: d.Reason})
	}
	byID := map[string]*findingRow{}
	for _, row := range listed {
		if row.id != "" {
			if _, dup := byID[row.id]; !dup {
				byID[row.id] = row
			}
		}
	}
	used := map[*findingRow]bool{}
	var rows []*findingRow
	for _, fd := range r.Findings {
		row := byID[fd.ID]
		if row == nil || used[row] {
			row = &findingRow{id: fd.ID, result: resultDropped}
		}
		used[row] = true
		if row.path == "" && row.line == 0 && row.result == resultDropped {
			row.path, row.line = fd.Path, fd.Line
		}
		row.severity, row.cat = fd.Severity, fd.Category
		fillTitle(row, fd.Content)
		rows = append(rows, row)
	}
	for _, row := range listed {
		// A dropped id that names no finding has nothing to show.
		if !used[row] && row.result != resultDropped {
			fillTitle(row, "")
			rows = append(rows, row)
		}
	}
	for _, row := range rows {
		row.title = capTitle(row.title)
	}
	rows = mergeRows(rows)
	slices.SortStableFunc(rows, func(a, b *findingRow) int { return severityRank(a.severity) - severityRank(b.severity) })
	v := &roundView{rows: rows}
	if len(r.Fixed) > 0 {
		v.other = newDiffBlock(r.OtherDiff, false)
	}
	shareDiffs(rows, resultFixed, false)
	shareDiffs(rows, resultReported, true)
	return v
}

// mergeRows keeps one row per path and title, compared without case. The
// row that comes first stays in place and takes the stronger result, the
// higher severity and the diff of both.
func mergeRows(rows []*findingRow) []*findingRow {
	seen := map[string]*findingRow{}
	out := rows[:0]
	for _, row := range rows {
		key := row.path + "\x00" + strings.ToLower(strings.TrimSpace(row.title))
		first, dup := seen[key]
		if !dup || strings.TrimSpace(row.title) == "" {
			seen[key] = row
			out = append(out, row)
			continue
		}
		if severityRank(row.severity) < severityRank(first.severity) {
			first.severity = row.severity
		}
		if first.cat == "" {
			first.cat = row.cat
		}
		if row.result == first.result {
			first.diff = joinDiffs(first.diff, row.diff)
			continue
		}
		if resultRank[row.result] < resultRank[first.result] {
			keep := *first
			*first = *row
			first.title, first.severity, first.cat = keep.title, keep.severity, keep.cat
			if first.path == "" && first.line == 0 {
				first.path, first.line = keep.path, keep.line
			}
		}
	}
	return out
}

// joinDiffs puts two diffs of one merged row together, each hunk once.
func joinDiffs(a, b string) string {
	parts, _ := dedupeDiffs([]string{a, b})
	var keep []string
	for _, p := range parts {
		if p = strings.TrimRight(p, "\n"); strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n")
}

// shareDiffs makes the diff blocks of the rows with result, in the order
// they are shown, with each shared hunk once; refs name rows by number.
func shareDiffs(rows []*findingRow, result string, proposal bool) {
	var idx []int
	var texts []string
	for i, row := range rows {
		if row.result == result {
			idx = append(idx, i)
			texts = append(texts, row.diff)
		}
	}
	texts, refs := dedupeDiffs(texts)
	for k, i := range idx {
		rows[i].block = newDiffBlock(texts[k], proposal)
		for _, n := range refs[k] {
			rows[i].refs = append(rows[i].refs, idx[n-1]+1)
		}
	}
}

var severityOrder = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}

func severityRank(s string) int {
	if n, ok := severityOrder[strings.ToLower(strings.TrimSpace(s))]; ok {
		return n
	}
	return len(severityOrder)
}

// boldLead takes a title that holds a star, as a Go pointer type does, up
// to the first closing pair on its line.
var boldLead = regexp.MustCompile(`^\*\*([^\n]+?)\*\*`)

// fillTitle names a finding the fix run gave no title. The review writes
// a bold title before the body of a finding, which names it best. A problem
// that is the review text itself, as when no fix run judged it, has that
// lead moved out of it.
func fillTitle(row *findingRow, content string) {
	if row.title != "" {
		return
	}
	p := strings.TrimSpace(row.problem)
	if m := boldLead.FindStringSubmatchIndex(p); m != nil {
		row.title, row.problem = p[m[2]:m[3]], strings.TrimSpace(p[m[1]:])
		return
	}
	if m := boldLead.FindStringSubmatch(strings.TrimSpace(content)); m != nil {
		row.title = m[1]
		return
	}
	row.title = fallbackTitle(p, content)
}

// fallbackTitle is the bold lead the review writes before the body, or
// else the first sentence of the problem or of the review text.
func fallbackTitle(problem, content string) string {
	for _, s := range []string{problem, content} {
		s = strings.TrimSpace(s)
		if m := boldLead.FindStringSubmatch(s); m != nil {
			return m[1]
		}
		if s == "" {
			continue
		}
		line, _, _ := strings.Cut(s, "\n")
		if i := strings.Index(line, ". "); i > 0 {
			line = line[:i]
		}
		return strings.TrimSuffix(strings.TrimSpace(line), ".")
	}
	return ""
}

// capTitle keeps a title on one line of at most maxTitleRunes runes.
func capTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxTitleRunes {
		return strings.TrimSpace(string(r[:maxTitleRunes-1])) + "…"
	}
	return s
}

// collapseAll collapses every diff; it is false when all were collapsed.
func collapseAll(blocks []*diffBlock) bool {
	changed := false
	for _, b := range blocks {
		if !b.collapsed {
			b.collapsed, changed = true, true
		}
	}
	return changed
}

// shrinkLargest halves the shown lines of the largest diff, dropping a
// diff whose rest would be too short to read; it is false when no diff
// shows a line.
func shrinkLargest(blocks []*diffBlock) bool {
	var big *diffBlock
	for _, b := range blocks {
		if b.keep > 0 && (big == nil || b.keep > big.keep) {
			big = b
		}
	}
	if big == nil {
		return false
	}
	if big.keep /= 2; big.keep < minDiffKeep {
		big.keep = 0
	}
	return true
}

const rowEnd = "</td></tr>\n"

// cutBody cuts a comment that is too long even without diffs and says so.
// Inside a table it cuts after the last whole row and closes the table, so
// the note does not end up in a collapsed element; after the last table it
// cuts before a details element it would leave open; elsewhere it cuts at
// a line break.
func cutBody(body string, loc Locale) string {
	note := "\n" + loc.Text("round.body_cut")
	r := []rune(body)
	cut := string(r[:max(0, maxCommentRunes-utf8.RuneCountInString(note)-len("</table>\n"))])
	if t := strings.LastIndex(cut, "<table>"); t >= 0 && !strings.Contains(cut[t:], "</table>") {
		// Only a row of the open table counts; with no whole row yet the
		// cut goes before the table.
		if i := strings.LastIndex(cut[t:], rowEnd); i >= 0 {
			return cut[:t+i+len(rowEnd)] + "</table>\n" + note
		}
		return cut[:t] + note
	}
	if i := strings.LastIndex(cut, "</table>\n"); i >= 0 && strings.Contains(cut[i:], "<details>") {
		// What follows the last table is the other diff, in a details
		// element the cut would leave open.
		return cut[:i+len("</table>\n")] + note
	}
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n" + note
}

func renderRound(r RoundReport, loc Locale, v *roundView) string {
	var b strings.Builder
	b.WriteString("## 🔍 ")
	b.WriteString(loc.Text("round.title", "round", RoundLabel(r.N, r.Max, loc)))
	b.WriteString("\n\n**(")
	switch {
	case r.From != "":
		b.WriteString(loc.Text("round.range_full", "range", rangeText(r.From, r.To)))
	case r.FixReview:
		b.WriteString(loc.Text("round.range_fix", "sha", "`"+short(r.To)+"`"))
	default:
		b.WriteString(loc.Text("round.range_commit", "sha", "`"+short(r.To)+"`"))
	}
	b.WriteString(")**\n")
	if c := RenderCoverage(r.Coverage, loc); c != "" {
		b.WriteString("\n")
		b.WriteString(c)
		b.WriteString("\n")
	}
	if r.Skipped {
		b.WriteString("\n")
		b.WriteString(loc.Text("round.skipped"))
		b.WriteString("\n\n")
		b.WriteString(loc.Text("round.end_text"))
		return b.String()
	}
	writeStatus(&b, r, loc, v)
	if len(v.rows) > 0 {
		writeFindings(&b, r, loc, v)
	}
	if v.other != nil {
		b.WriteString("\n**")
		b.WriteString(loc.Text("round.other_changes"))
		b.WriteString("**\n")
		writeDiff(&b, v.other, loc, commitRef(r.FixCommit, r.CommitURL))
	}
	return strings.TrimRight(b.String(), "\n")
}

// statusRow writes one row of the status table. value is HTML.
func statusRow(b *strings.Builder, emoji, label, value string) {
	b.WriteString("<tr><td>")
	b.WriteString(emoji)
	b.WriteString("&nbsp;<strong>")
	b.WriteString(html.EscapeString(label))
	b.WriteString("</strong>: ")
	b.WriteString(value)
	b.WriteString("</td></tr>\n")
}

// writeStatus writes the status table. Markdown does not render inside a
// one-line HTML table, so its values use HTML code and links.
func writeStatus(b *strings.Builder, r RoundReport, loc Locale, v *roundView) {
	found, fixed := len(v.rows), v.count(resultFixed)
	needs, dropped := v.count(resultReported), v.count(resultDropped)
	b.WriteString("\n<table>\n")
	n := strconv.Itoa(found)
	value := loc.Text("round.found_zero")
	switch {
	case found == 1:
		value = loc.Text("round.found_value_one", "real", strconv.Itoa(fixed+needs), "skipped", strconv.Itoa(dropped))
	case found > 1:
		value = loc.Text("round.found_value", "count", n, "real", strconv.Itoa(fixed+needs), "skipped", strconv.Itoa(dropped))
	}
	statusRow(b, "🔎", loc.Text("round.found"), html.EscapeString(value))
	if found > 0 {
		value = html.EscapeString(loc.Text("round.fixed_nothing"))
		switch {
		case fixed > 0 && r.FixCommit != "":
			value = loc.Text("round.fixed_value", "count", strconv.Itoa(fixed), "commit", htmlCommit(r.FixCommit, r.CommitURL))
		case fixed > 0 || needs > 0:
			value = html.EscapeString(loc.Text("round.fixed_count", "count", strconv.Itoa(fixed)))
		}
		statusRow(b, "🛠️", loc.Text("round.fixed"), value)
	}
	if needs > 0 {
		value = html.EscapeString(loc.Text("round.fixed_count", "count", strconv.Itoa(needs)))
		if r.Report != "" {
			value += "<br>" + loc.Text("round.report."+r.Report, "max", strconv.Itoa(r.Max),
				"branch", htmlCode(r.Branch), "reason", htmlCode(r.PushReason))
		}
		statusRow(b, "⚠️", loc.Text("round.needs_you"), value)
	}
	if fixed > 0 {
		writeTestsRow(b, r.Tests, loc)
	}
	if c := r.Coverage; c.Total > 0 {
		statusRow(b, "📄", loc.Text("round.scope"), html.EscapeString(loc.Text("round.scope_value",
			"reviewed", strconv.Itoa(c.Reviewed), "total", strconv.Itoa(c.Total))))
	}
	if r.Next > 0 {
		statusRow(b, "➡️", loc.Text("round.next"), loc.Text("round.next_value",
			"round", html.EscapeString(RoundLabel(r.Next, r.Max, loc)), "sha", htmlCode(short(r.FixCommit))))
	} else {
		statusRow(b, "🏁", loc.Text("round.end"), html.EscapeString(loc.Text("round.end_value")))
	}
	b.WriteString("</table>\n")
}

// writeTestsRow writes the test run after the fix. A failed run shows the
// end of its output in a fence it cannot close; the blank lines around the
// fence make both forges render it as code inside the table.
func writeTestsRow(b *strings.Builder, t review.Tests, loc Locale) {
	label := loc.Text("round.tests")
	switch {
	case t.Command == "":
		statusRow(b, "🧪", label, html.EscapeString(loc.Text("round.tests_none")))
		return
	case t.Passed:
		statusRow(b, "🧪", label, loc.Text("round.tests_passed", "command", htmlCode(t.Command)))
		return
	}
	value := loc.Text("round.tests_failed", "command", htmlCode(t.Command))
	out := neutralizeTags(t.OutputTail)
	if out == "" {
		statusRow(b, "❌", label, value)
		return
	}
	fence := safeFence(out)
	value += "\n<details><summary>" + html.EscapeString(loc.Text("round.tests_output")) + "</summary>\n\n" +
		fence + "\n" + out + "\n" + fence + "\n\n</details>\n"
	statusRow(b, "❌", label, value)
}

var severityEmoji = map[string]string{"critical": "🔴", "high": "🔴", "medium": "🟠", "low": "⚪"}

var resultEmoji = map[string]string{resultFixed: "✅", resultReported: "⚠️", resultDropped: "➖"}

// writeFindings writes the finding table. The details of a finding sit
// between blank lines inside its cell, so the forges render them as
// markdown.
func writeFindings(b *strings.Builder, r RoundReport, loc Locale, v *roundView) {
	b.WriteString("\n### 🔎 ")
	b.WriteString(loc.Text("finding.section_title", "count", strconv.Itoa(len(v.rows))))
	b.WriteString("\n\n<table><thead><tr><td>")
	for i, k := range []string{"finding.col_number", "finding.col_issue", "finding.col_severity", "finding.col_result"} {
		if i > 0 {
			b.WriteString("</td><td>")
		}
		b.WriteString(html.EscapeString(loc.Text(k)))
	}
	b.WriteString("</td></tr></thead>\n")
	commit := commitRef(r.FixCommit, r.CommitURL)
	for i, row := range v.rows {
		place := rowPlace(row, loc)
		sha := r.To
		if row.result == resultFixed && r.FixCommit != "" {
			sha = r.FixCommit
		}
		link := place
		if row.path != "" && r.LineURL != nil {
			if u := r.LineURL(sha, row.path, row.line); u != "" {
				link = "[" + place + "](" + u + ")"
			}
		}
		where := place
		if row.cat != "" {
			cat := prose(labelFor(loc, "category.", row.cat))
			link += " · " + cat
			where += " · " + cat
		}
		b.WriteString("<tr><td>")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString("</td><td>\n\n<details><summary>")
		b.WriteString(summaryText(row.title))
		b.WriteString("</summary>\n\n📍 ")
		b.WriteString(link)
		b.WriteString("\n")
		writeField(b, loc, "finding.problem", row.problem)
		writeField(b, loc, "finding.scenario", row.scenario)
		switch row.result {
		case resultFixed:
			writeField(b, loc, "finding.fixed", row.text)
			if row.block != nil {
				writeDiff(b, row.block, loc, commit)
			}
		case resultReported:
			writeField(b, loc, "finding.proposed_fix", row.text)
			if row.block != nil {
				b.WriteString("\n")
				b.WriteString(loc.Text("round.proposed_diff"))
				b.WriteString("\n")
				writeDiff(b, row.block, loc, "")
			}
		default:
			writeField(b, loc, "finding.why_skipped", row.text)
		}
		writeShared(b, row.refs, loc)
		if t := prose(row.testsAdded); t != "" {
			b.WriteString("\n🧪 ")
			b.WriteString(loc.Text("finding.tests_added"))
			b.WriteString(": ")
			b.WriteString(t)
			b.WriteString("\n")
		}
		b.WriteString("\n</details>\n\n<sub>")
		b.WriteString(where)
		b.WriteString("</sub>\n\n</td><td>")
		b.WriteString(severityCell(row.severity, loc))
		b.WriteString("</td><td>")
		b.WriteString(resultEmoji[row.result])
		b.WriteString(" ")
		b.WriteString(html.EscapeString(loc.Text("finding.result." + row.result)))
		b.WriteString(rowEnd)
	}
	b.WriteString("</table>\n")
}

// rowPlace names the place of a finding as markdown, or the PR as a whole.
func rowPlace(row *findingRow, loc Locale) string {
	switch {
	case row.path == "":
		return loc.Text("finding.whole_pr")
	case row.line > 0:
		return loc.Text("finding.location", "path", codePath(row.path), "line", strconv.Itoa(row.line))
	}
	return codePath(row.path)
}

func severityCell(s string, loc Locale) string {
	key := strings.ToLower(strings.TrimSpace(s))
	if key == "" {
		return "-"
	}
	label := html.EscapeString(neutralizeTags(labelFor(loc, "severity.", key)))
	if e, ok := severityEmoji[key]; ok {
		return e + " " + label
	}
	return label
}

// writeField writes "**label**: text" as its own paragraph, or nothing when
// the agent text is empty. Text that opens with a code fence starts on its
// own line, where the fence opens a block, and a fence left open is closed
// so it does not take in the rest of the comment.
func writeField(b *strings.Builder, loc Locale, key, text string) {
	if text = prose(text); text != "" {
		b.WriteString("\n**")
		b.WriteString(loc.Text(key))
		if fenceOf(strings.SplitN(text, "\n", 2)[0]) != "" {
			b.WriteString("**:\n\n")
		} else {
			b.WriteString("**: ")
		}
		b.WriteString(text)
		if open := openFence(text); open != "" {
			b.WriteString("\n")
			b.WriteString(open)
		}
		b.WriteString("\n")
	}
}

// fenceOf returns the fence run a line opens or closes a code block with,
// or "" when the line is no fence.
func fenceOf(line string) string {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 || s == "" || (s[0] != '`' && s[0] != '~') {
		return ""
	}
	n := len(s) - len(strings.TrimLeft(s, s[:1]))
	if n < 3 || (s[0] == '`' && strings.Contains(s[n:], "`")) {
		return ""
	}
	return s[:n]
}

// openFence returns the fence that closes the code block text leaves open,
// or "" when text closes every block it opens.
func openFence(text string) string {
	open := ""
	for _, line := range strings.Split(text, "\n") {
		f := fenceOf(line)
		switch {
		case f == "":
		case open == "":
			open = f
		case f[0] == open[0] && len(f) >= len(open) && strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), f[:1])) == "":
			open = ""
		}
	}
	return open
}

// layoutTag matches the HTML tags the comment layout is made of.
var layoutTag = regexp.MustCompile(`(?i)<(/?)(table|thead|tbody|tr|td|th|details|summary)\b`)

// prose prepares agent text for a markdown part of the comment: a service
// tag or a layout tag in it, which a finding about HTML code can quote,
// would end the table or the details early.
func prose(s string) string {
	return layoutTag.ReplaceAllString(neutralizeTags(strings.TrimSpace(s)), "&lt;$1$2")
}

// summaryText escapes a title for a summary element, where markdown does
// not render, and turns its code spans into code elements.
func summaryText(s string) string {
	parts := strings.Split(html.EscapeString(s), "`")
	var b strings.Builder
	for i, p := range parts {
		switch {
		case i == 0:
		case i%2 == 1 && i < len(parts)-1:
			b.WriteString("<code>")
		case i%2 == 0:
			b.WriteString("</code>")
		default:
			b.WriteString("`")
		}
		b.WriteString(p)
	}
	return b.String()
}

// htmlCode puts s on one line in a code element.
func htmlCode(s string) string {
	return "<code>" + html.EscapeString(strings.Join(strings.Fields(s), " ")) + "</code>"
}

// htmlCommit links the fix commit as HTML, or names it without a link.
func htmlCommit(sha, url string) string {
	code := htmlCode(short(sha))
	if url == "" {
		return code
	}
	return `<a href="` + html.EscapeString(url) + `">` + code + "</a>"
}

// writeShared names the items above that show the diff a finding shares
// with them, or writes nothing.
func writeShared(b *strings.Builder, refs []int, loc Locale) {
	if len(refs) == 0 {
		return
	}
	items := make([]string, len(refs))
	for i, n := range refs {
		items[i] = strconv.Itoa(n)
	}
	b.WriteString("\n")
	b.WriteString(loc.Text("round.diff_shared", "items", strings.Join(items, ", ")))
	b.WriteString("\n")
}

// commitRef links the fix commit, or names it when there is no link.
func commitRef(sha, url string) string {
	if sha == "" {
		return ""
	}
	if url == "" {
		return "`" + short(sha) + "`"
	}
	return "[`" + short(sha) + "`](" + url + ")"
}

// writeDiff writes a diff block in a fence it cannot close. A collapsed
// block goes in a details element; the blank lines around the fence make
// GitHub and GitLab render it as code inside the HTML block.
func writeDiff(b *strings.Builder, blk *diffBlock, loc Locale, commit string) {
	var note string
	if hidden := len(blk.lines) - blk.keep; hidden > 0 {
		if blk.proposal {
			note = loc.Text("round.proposal_cut", "count", strconv.Itoa(hidden))
		} else {
			note = loc.Text("round.diff_cut", "count", strconv.Itoa(hidden), "commit", commit)
		}
	}
	var code string
	if blk.keep > 0 {
		text := neutralizeTags(strings.Join(blk.lines[:blk.keep], "\n"))
		fence := safeFence(text)
		code = fence + "diff\n" + text + "\n" + fence + "\n"
	}
	b.WriteString("\n")
	if blk.collapsed {
		b.WriteString("<details><summary>")
		b.WriteString(html.EscapeString(loc.Text("round.diff_summary", "lines", strconv.Itoa(len(blk.lines)))))
		b.WriteString("</summary>\n\n")
	}
	b.WriteString(code)
	if note != "" {
		if code != "" {
			b.WriteString("\n")
		}
		b.WriteString(note)
		b.WriteString("\n")
	}
	if blk.collapsed {
		b.WriteString("\n</details>\n")
	}
}
