// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

const summaryRunsShown = 5

// RenderFinding builds the root comment of a finding thread. A suggestion
// block is kept only when the finding sits on its own lines, because applying
// it anywhere else would rewrite the wrong code. A finding that does not sit
// on its own lines names its original location instead.
func RenderFinding(f forge.Finding, p forge.Placement, platform forge.Platform, loc Locale) string {
	var b strings.Builder
	if f.ID != "" {
		b.WriteString(forge.FindingTag(f.ID))
		b.WriteString("\n")
	}
	b.WriteString(findingHeader(f, loc))
	if p.Kind != forge.Line {
		b.WriteString("\n_")
		b.WriteString(loc.Text("finding.original_location", "location", location(f)))
		b.WriteString("_")
	}
	if content := neutralizeTags(strings.TrimSpace(f.Content)); content != "" {
		b.WriteString("\n\n")
		b.WriteString(content)
	}
	// Escaping a tag inside a suggestion would change the code an author
	// applies, so a suggestion that carries one is left out instead.
	if p.Kind == forge.Line && f.SuggestionCode != "" && f.ExistingCode != "" && !hasServiceTag(f.SuggestionCode) {
		b.WriteString("\n\n**")
		b.WriteString(loc.Text("finding.suggestion"))
		b.WriteString("**\n")
		b.WriteString(suggestionBlock(f.SuggestionCode, p, platform))
	}
	return b.String()
}

func findingHeader(f forge.Finding, loc Locale) string {
	category := strings.ToLower(strings.TrimSpace(f.Category))
	if category == "" {
		category = "other"
	}
	label := labelFor(loc, "category.", category)
	if severity := strings.ToLower(strings.TrimSpace(f.Severity)); severity != "" {
		label += " · " + labelFor(loc, "severity.", severity)
	}
	return "**[" + neutralizeTags(label) + "]**"
}

// labelFor falls back to the raw value for a category or severity the locale
// does not know, so an unexpected LLM value is still shown.
func labelFor(loc Locale, prefix, value string) string {
	if s, ok := loc[prefix+value]; ok {
		return s
	}
	return value
}

func location(f forge.Finding) string {
	s := "`" + neutralizeTags(strings.ReplaceAll(f.Path, "`", "'")) + "`"
	switch {
	case f.StartLine > 0 && f.EndLine > f.StartLine:
		s += fmt.Sprintf(" L%d-%d", f.StartLine, f.EndLine)
	case f.StartLine > 0:
		s += fmt.Sprintf(" L%d", f.StartLine)
	case f.EndLine > 0:
		s += fmt.Sprintf(" L%d", f.EndLine)
	}
	return s
}

// neutralizeTags stops LLM text from carrying a service tag. Forges only
// trust tags in bot-authored comments, and the service is the bot, so a
// tag echoed from reviewed code must not survive into a posted body.
func neutralizeTags(s string) string {
	return strings.ReplaceAll(s, serviceTagPrefix, "&lt;!-- ocr-")
}

const serviceTagPrefix = "<!-- ocr-"

func hasServiceTag(s string) bool {
	return strings.Contains(s, serviceTagPrefix)
}

// suggestionBlock uses GitHub's plain suggestion fence, which replaces the
// whole commented span, and GitLab's "suggestion:-N+0", which counts the
// extra lines above the anchor line.
func suggestionBlock(code string, p forge.Placement, platform forge.Platform) string {
	fence := safeFence(code)
	info := "suggestion"
	if platform == forge.GitLab {
		info = fmt.Sprintf("suggestion:-%d+0", max(0, p.EndLine-p.StartLine))
	}
	code = strings.TrimSuffix(code, "\n")
	return fence + info + "\n" + code + "\n" + fence
}

// safeFence returns a backtick fence longer than any backtick run in s, so
// code that contains a fence cannot close the block early.
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

// RenderSummary builds the sticky summary. It starts with forge.SummaryTag,
// so forges find it even when the marker is damaged, and ends with the
// encoded marker. pending is an optional status line such as "reviewing".
func RenderSummary(m Marker, openThreads int, loc Locale, pending string) string {
	var b strings.Builder
	b.WriteString(forge.SummaryTag)
	b.WriteString("\n### ")
	b.WriteString(loc.Text("summary.title"))
	b.WriteString("\n\n- ")
	b.WriteString(loc.Text("summary.round", "round", strconv.Itoa(m.Auto), "max", strconv.Itoa(m.Max)))
	b.WriteString("\n- ")
	if m.Reviewed != "" {
		b.WriteString(loc.Text("summary.reviewed", "sha", "`"+short(m.Reviewed)+"`"))
	} else {
		b.WriteString(loc.Text("summary.reviewed_none"))
	}
	b.WriteString("\n- ")
	b.WriteString(loc.Text("summary.open", "count", strconv.Itoa(max(0, openThreads))))
	b.WriteString("\n")

	if len(m.Runs) > 0 {
		b.WriteString("\n**")
		b.WriteString(loc.Text("summary.runs_title"))
		b.WriteString("**\n\n")
		b.WriteString(loc.Text("summary.runs_header"))
		b.WriteString("\n|---|---|---|---|\n")
		for _, r := range m.Runs[max(0, len(m.Runs)-summaryRunsShown):] {
			fmt.Fprintf(&b, "| %s | %s | %s | %d |\n", runLabel(r, loc), loc.Text("run.kind."+r.Kind), runRange(r), len(r.IDs))
		}
	}
	if pending = strings.TrimSpace(pending); pending != "" {
		b.WriteString("\n")
		b.WriteString(pending)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(loc.Text("summary.hint"))
	b.WriteString("\n\n")
	b.WriteString(Encode(m))
	return b.String()
}

func runLabel(r Run, loc Locale) string {
	if r.N > 0 {
		return loc.Text("run.auto", "n", strconv.Itoa(r.N))
	}
	return loc.Text("run.manual")
}

func runRange(r Run) string {
	if r.From == "" {
		return "`" + short(r.To) + "`"
	}
	return "`" + short(r.From) + "..." + short(r.To) + "`"
}

func short(sha string) string {
	return sha[:min(7, len(sha))]
}

// RenderLimitNote is the note posted once when the round limit stops
// automatic review.
func RenderLimitNote(limit int, loc Locale) string {
	return loc.Text("limit.note", "max", strconv.Itoa(limit))
}

// RenderCommandError answers a rejected or failed command. reason may quote
// user input, so it is put in a code span on one line to keep mentions and
// markdown inert.
func RenderCommandError(reason string, loc Locale) string {
	reason = strings.Join(strings.Fields(reason), " ")
	reason = "`" + strings.ReplaceAll(reason, "`", "'") + "`"
	return "❌ " + loc.Text("command.error", "reason", reason) + "\n\n" + loc.Text("command.help")
}
