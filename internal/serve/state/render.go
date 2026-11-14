// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

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
// commit, or 0 when the chain ends with this round.
type RoundReport struct {
	N, Max     int
	From, To   string
	Coverage   review.Coverage
	Skipped    bool
	Fixed      []review.Fixed
	Reported   []review.Reported
	Tests      review.Tests
	FixCommit  string
	Report     string
	Branch     string
	PushReason string
	Next       int
}

// RenderRound builds the comment of one round. Agent text is shown as
// markdown, with service tags neutralized; a test output tail goes into a
// fence it cannot close.
func RenderRound(r RoundReport, loc Locale) string {
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
	if len(r.Fixed) > 0 {
		b.WriteString("\n**")
		b.WriteString(loc.Text("round.fixed_title", "count", strconv.Itoa(len(r.Fixed))))
		b.WriteString("**\n")
		for i, f := range r.Fixed {
			fmt.Fprintf(&b, "\n%d. %s: %s\n", i+1, where(f.Path, f.Line, loc), neutralizeTags(f.Problem))
			if fix := neutralizeTags(f.Fix); fix != "" {
				b.WriteString("\n")
				b.WriteString(loc.Text("round.how_fixed", "fix", fix))
				b.WriteString("\n")
			}
		}
		if r.FixCommit != "" {
			b.WriteString("\n")
			b.WriteString(loc.Text("round.fix_commit", "sha", "`"+short(r.FixCommit)+"`"))
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
			if s := neutralizeTags(f.Scenario); s != "" {
				b.WriteString("\n")
				b.WriteString(loc.Text("round.scenario", "scenario", s))
				b.WriteString("\n")
			}
			if p := neutralizeTags(f.ProposedFix); p != "" {
				b.WriteString("\n")
				b.WriteString(loc.Text("round.proposed_fix", "fix", p))
				b.WriteString("\n")
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
