// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

const coverageItemsShown = 20

// codePath puts a path from the PR in a code span that the path cannot close.
func codePath(p string) string {
	return "`" + neutralizeTags(strings.ReplaceAll(p, "`", "'")) + "`"
}

// neutralizeTags stops LLM text from carrying a service tag. Forges only
// trust tags in bot-authored comments, and the service is the bot, so a
// tag echoed from reviewed code must not survive into a posted body.
func neutralizeTags(s string) string {
	return strings.ReplaceAll(s, serviceTagPrefix, "&lt;!-- ocr-")
}

const serviceTagPrefix = "<!-- ocr-"

// safeFence returns a backtick fence longer than any backtick run in s, so
// text that contains a fence cannot close the block early.
func safeFence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// labelFor falls back to the raw value for a key the locale does not know,
// so an unexpected value is still shown.
func labelFor(loc Locale, prefix, value string) string {
	if s, ok := loc[prefix+value]; ok {
		return s
	}
	return value
}

func short(sha string) string {
	return sha[:min(7, len(sha))]
}

func codeSpan(s string) string {
	return "`" + strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "`", "'") + "`"
}

// RoundLabel names round n of a chain with max fix rounds: "round n/max",
// or for the report-only round after them, "round n (report only)".
func RoundLabel(n, limit int, loc Locale) string {
	if n > limit {
		return loc.Text("round.label_report", "round", strconv.Itoa(n))
	}
	return loc.Text("round.label", "round", strconv.Itoa(n), "max", strconv.Itoa(limit))
}

// rangeText shows the reviewed range of a round.
func rangeText(from, to string) string {
	if from == "" {
		return "`" + short(to) + "`"
	}
	return "`" + short(from) + "..." + short(to) + "`"
}

// RenderSummary builds the sticky summary. It starts with forge.SummaryTag,
// so forges find it even when the marker is damaged, and ends with the
// encoded marker. pending is an optional status line such as "reviewing".
// bot is the name users tag the bot with; empty shows slash commands only.
func RenderSummary(m Marker, loc Locale, pending, bot string) string {
	var b strings.Builder
	b.WriteString(forge.SummaryTag)
	b.WriteString("\n### ")
	b.WriteString(loc.Text("summary.title"))
	b.WriteString("\n\n")
	b.WriteString(loc.Text("summary.status." + m.Status))
	b.WriteString("\n")
	if len(m.Rounds) > 0 {
		b.WriteString("\n")
		b.WriteString(loc.Text("summary.rounds_header"))
		b.WriteString("\n|---|---|---|---|---|\n")
		for _, r := range m.Rounds {
			fix := "-"
			if r.Fix != "" {
				fix = "`" + short(r.Fix) + "`"
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %s |\n", RoundLabel(r.N, m.Max, loc), rangeText(r.From, r.To), r.Real, r.Fixed, fix)
		}
	}
	if pending = strings.TrimSpace(pending); pending != "" {
		b.WriteString("\n")
		b.WriteString(pending)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(mentionText(loc, "summary.hint", bot))
	b.WriteString("\n\n")
	b.WriteString(Encode(m))
	return b.String()
}

// RenderCoverage builds the line for a run that could not review every
// file: the files it missed and why. A complete run gives an empty line.
// The reason is the failure class only; the error text can name internal
// endpoints, so it goes to the service log instead.
func RenderCoverage(c review.Coverage, loc Locale) string {
	if c.Total == 0 || len(c.Failed) == 0 {
		return ""
	}
	failed := slices.SortedFunc(slices.Values(c.Failed), func(a, b review.FailedItem) int {
		return cmp.Compare(a.Path, b.Path)
	})
	var b strings.Builder
	b.WriteString(loc.Text("coverage.partial", "reviewed", strconv.Itoa(c.Reviewed), "total", strconv.Itoa(c.Total)))
	b.WriteString("\n")
	for _, it := range failed[:min(len(failed), coverageItemsShown)] {
		name := codePath(it.Path)
		if it.OldPath != "" && it.OldPath != it.Path {
			name = codePath(it.OldPath) + " → " + name
		}
		fmt.Fprintf(&b, "\n- %s: %s", name, neutralizeTags(labelFor(loc, "failure.", it.Class)))
	}
	if rest := len(failed) - coverageItemsShown; rest > 0 {
		b.WriteString("\n- ")
		b.WriteString(loc.Text("coverage.more", "count", strconv.Itoa(rest)))
	}
	return b.String()
}

// Reasons a round reports without fixing.
const (
	ReportLimit  = "limit"
	ReportFork   = "fork"
	ReportPush   = "push"
	ReportEngine = "engine"
	ReportNoEdit = "no-edit"
)

// RoundReport is what one round comment shows. From is empty for a commit
// round. Report names why the round did not fix (one of the Report
// constants), or is empty for a fix round. Branch and PushReason fill the
// text of ReportFork and ReportPush. Next is the round that reviews the fix
// commit, or 0 when the chain ends with this round. FixDiffs holds the
// part of the fix commit diff of each Fixed entry, by index, and OtherDiff
// the part no finding claims; CommitURL is the web page of FixCommit.
type RoundReport struct {
	N, Max     int
	From, To   string
	Coverage   review.Coverage
	Skipped    bool
	Fixed      []review.Fixed
	Reported   []review.Reported
	Tests      review.Tests
	FixCommit  string
	CommitURL  string
	FixDiffs   []string
	OtherDiff  string
	Report     string
	Branch     string
	PushReason string
	Next       int
}

// Bounds of the diffs in a round comment. maxCommentRunes stays under the
// 65536 characters GitHub takes in one comment; GitLab takes more, but one
// bound keeps both forges the same.
const (
	collapseLines   = 40
	maxProposalLine = 150
	maxCommentRunes = 65000
	minDiffKeep     = 5
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

// roundDiffs holds the diff blocks of a comment, nil where there is none.
type roundDiffs struct {
	fixed    []*diffBlock
	other    *diffBlock
	reported []*diffBlock
}

func (d *roundDiffs) all() []*diffBlock {
	out := make([]*diffBlock, 0, len(d.fixed)+len(d.reported)+1)
	for _, b := range append(append(slices.Clone(d.fixed), d.other), d.reported...) {
		if b != nil {
			out = append(out, b)
		}
	}
	return out
}

// RenderRound builds the comment of one round. Agent text is shown as
// markdown, with service tags neutralized; a test output tail and each
// diff go into a fence they cannot close. A comment over maxCommentRunes
// first collapses every diff, then shows fewer lines of the largest diff
// until it fits, and as a last resort is cut.
func RenderRound(r RoundReport, loc Locale) string {
	d := &roundDiffs{fixed: make([]*diffBlock, len(r.Fixed)), reported: make([]*diffBlock, len(r.Reported))}
	for i := range r.Fixed {
		if i < len(r.FixDiffs) {
			d.fixed[i] = newDiffBlock(r.FixDiffs[i], false)
		}
	}
	if len(r.Fixed) > 0 {
		d.other = newDiffBlock(r.OtherDiff, false)
	}
	for i, f := range r.Reported {
		d.reported[i] = newDiffBlock(f.ProposedDiff, true)
	}
	for {
		body := renderRound(r, loc, d)
		if utf8.RuneCountInString(body) <= maxCommentRunes {
			return body
		}
		if !collapseAll(d.all()) && !shrinkLargest(d.all()) {
			return cutBody(body, loc)
		}
	}
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

// cutBody cuts a comment that is too long even without diffs at a line
// break and says so.
func cutBody(body string, loc Locale) string {
	note := "\n\n" + loc.Text("round.body_cut")
	r := []rune(body)
	cut := string(r[:max(0, maxCommentRunes-utf8.RuneCountInString(note))])
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + note
}

func renderRound(r RoundReport, loc Locale, d *roundDiffs) string {
	var b strings.Builder
	b.WriteString("### ")
	b.WriteString(loc.Text("round.title", "round", RoundLabel(r.N, r.Max, loc)))
	b.WriteString("\n\n")
	b.WriteString(loc.Text("round.range", "range", rangeText(r.From, r.To)))
	b.WriteString("\n")
	if c := RenderCoverage(r.Coverage, loc); c != "" {
		b.WriteString("\n")
		b.WriteString(c)
		b.WriteString("\n")
	}
	switch {
	case r.Skipped:
		b.WriteString("\n")
		b.WriteString(loc.Text("round.skipped"))
		b.WriteString("\n")
	case len(r.Fixed) == 0 && len(r.Reported) == 0:
		b.WriteString("\n")
		b.WriteString(loc.Text("round.clean"))
		b.WriteString("\n")
	}
	commit := commitRef(r.FixCommit, r.CommitURL)
	if len(r.Fixed) > 0 {
		b.WriteString("\n**")
		b.WriteString(loc.Text("round.fixed_title", "count", strconv.Itoa(len(r.Fixed))))
		b.WriteString("**\n")
		for i, f := range r.Fixed {
			fmt.Fprintf(&b, "\n%d. %s: %s\n", i+1, where(f.Path, f.Line, loc), neutralizeTags(f.Problem))
			writeLine(&b, loc, "round.scenario", "scenario", f.Scenario)
			if blk := d.fixed[i]; blk != nil {
				writeDiff(&b, blk, loc, commit)
			} else {
				writeLine(&b, loc, "round.how_fixed", "fix", f.Fix)
			}
			writeLine(&b, loc, "round.tests_added", "tests", f.TestsAdded)
		}
		if d.other != nil {
			b.WriteString("\n**")
			b.WriteString(loc.Text("round.other_changes"))
			b.WriteString("**\n")
			writeDiff(&b, d.other, loc, commit)
		}
		if r.FixCommit != "" {
			b.WriteString("\n")
			b.WriteString(loc.Text("round.fix_commit", "sha", commit))
			b.WriteString("\n")
		}
		writeTests(&b, r.Tests, loc)
	}
	if len(r.Reported) > 0 {
		b.WriteString("\n**")
		b.WriteString(loc.Text("round.reported_title", "count", strconv.Itoa(len(r.Reported))))
		b.WriteString("**\n")
		for i, f := range r.Reported {
			fmt.Fprintf(&b, "\n%d. %s: %s\n", i+1, where(f.Path, f.Line, loc), neutralizeTags(f.Problem))
			writeLine(&b, loc, "round.scenario", "scenario", f.Scenario)
			writeLine(&b, loc, "round.proposed_fix", "fix", f.ProposedFix)
			if blk := d.reported[i]; blk != nil {
				b.WriteString("\n")
				b.WriteString(loc.Text("round.proposed_diff"))
				b.WriteString("\n")
				writeDiff(&b, blk, loc, "")
			}
		}
		if r.Report != "" {
			b.WriteString("\n")
			b.WriteString(loc.Text("round.report."+r.Report, "max", strconv.Itoa(r.Max),
				"branch", codeSpan(r.Branch), "reason", codeSpan(r.PushReason)))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	if r.Next > 0 {
		b.WriteString(loc.Text("round.next", "round", RoundLabel(r.Next, r.Max, loc)))
	} else {
		b.WriteString(loc.Text("round.end"))
	}
	return b.String()
}

// writeLine writes the locale text key with agent text v as its value, or
// nothing when v is empty.
func writeLine(b *strings.Builder, loc Locale, key, name, v string) {
	if v = neutralizeTags(v); v != "" {
		b.WriteString("\n")
		b.WriteString(loc.Text(key, name, v))
		b.WriteString("\n")
	}
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
		b.WriteString(loc.Text("round.diff_summary", "lines", strconv.Itoa(len(blk.lines))))
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

// where names the place of a finding, or the PR as a whole.
func where(path string, line int, loc Locale) string {
	if path == "" {
		return loc.Text("round.pr_level")
	}
	if line > 0 {
		return codePath(path) + " L" + strconv.Itoa(line)
	}
	return codePath(path)
}

func writeTests(b *strings.Builder, t review.Tests, loc Locale) {
	b.WriteString("\n")
	switch {
	case t.Command == "":
		b.WriteString(loc.Text("round.tests_none"))
	case t.Passed:
		b.WriteString(loc.Text("round.tests_passed", "command", codeSpan(t.Command)))
	default:
		b.WriteString(loc.Text("round.tests_failed", "command", codeSpan(t.Command)))
	}
	b.WriteString("\n")
	if out := neutralizeTags(t.OutputTail); out != "" && !t.Passed {
		fence := safeFence(out)
		b.WriteString("\n" + fence + "\n" + out + "\n" + fence + "\n")
	}
}

// StatusText is the one-line description of the commit status for the
// chain state; round is the round that runs, used while the chain runs.
func StatusText(m Marker, round int, loc Locale) string {
	if m.Status == ChainRunning {
		return loc.Text("status.running", "round", RoundLabel(round, m.Max, loc))
	}
	return loc.Text("status." + m.Status)
}

// RenderCommandError answers a rejected or failed command. reason may quote
// user input, so it is put in a code span on one line to keep mentions and
// markdown inert. bot is as in RenderSummary.
func RenderCommandError(reason, bot string, loc Locale) string {
	return "❌ " + loc.Text("command.error", "reason", codeSpan(reason)) + "\n\n" + mentionText(loc, "command.help", bot)
}

// mentionText picks the "<key>_mention" text that shows the tag form of the
// commands, or the slash-only text when the bot name is not known.
func mentionText(loc Locale, key, bot string) string {
	if bot == "" {
		return loc.Text(key)
	}
	return loc.Text(key+"_mention", "bot", bot)
}
