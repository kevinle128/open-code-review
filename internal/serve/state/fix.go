// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"strconv"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// ParsedFinding is a finding read back from the root comment of its thread.
type ParsedFinding struct {
	Severity string
	Title    string
	Body     string
}

// ParseFinding reverses RenderFinding as far as an agent needs it: the
// severity key (or the label as shown when the locale does not know it),
// the bold first line of the content as the title, and the rest. A body
// that does not have the rendered shape comes back whole as Body.
func ParseFinding(body string, loc Locale) ParsedFinding {
	text := strings.TrimSpace(body)
	if first, rest, ok := strings.Cut(text, "\n"); ok && strings.HasPrefix(first, serviceTagPrefix) {
		text = strings.TrimSpace(rest)
	} else if strings.HasPrefix(text, serviceTagPrefix) {
		text = ""
	}
	var out ParsedFinding
	header, rest, _ := strings.Cut(text, "\n")
	if label, ok := cutWrap(strings.TrimSpace(header), "**[", "]**"); ok {
		if _, sev, ok := strings.Cut(label, " · "); ok {
			out.Severity = severityKey(strings.TrimSpace(sev), loc)
		}
		text = strings.TrimSpace(rest)
	}
	first, rest, _ := strings.Cut(text, "\n")
	if title, ok := cutWrap(strings.TrimSpace(first), "**", "**"); ok && title != "" && !strings.Contains(title, "**") {
		out.Title = title
		text = strings.TrimSpace(rest)
	}
	out.Body = text
	return out
}

func cutWrap(s, prefix, suffix string) (string, bool) {
	if len(s) < len(prefix)+len(suffix) || !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return "", false
	}
	return s[len(prefix) : len(s)-len(suffix)], true
}

func severityKey(label string, loc Locale) string {
	for _, key := range []string{"critical", "high", "medium", "low"} {
		if loc["severity."+key] == label {
			return key
		}
	}
	return label
}

// FixSkipped is one finding a fix run left open, as shown in a fix note.
type FixSkipped struct {
	Path   string
	Line   int
	Title  string
	Reason string
}

// RenderFixNothing answers a fix command when no finding is open.
func RenderFixNothing(loc Locale) string {
	return "✅ " + loc.Text("fix.nothing")
}

// RenderFixCannotPush answers a fix command whose result cannot reach the
// branch. reason may quote git output, so it is kept inert like a command
// error.
func RenderFixCannotPush(branch, reason string, loc Locale) string {
	return "❌ " + loc.Text("fix.cannot_push", "branch", codeSpan(branch), "reason", codeSpan(reason))
}

// RenderFixNoChanges reports a fix run that changed no file.
func RenderFixNoChanges(summary string, skipped []FixSkipped, loc Locale) string {
	var b strings.Builder
	b.WriteString("ℹ️ ")
	b.WriteString(loc.Text("fix.no_changes"))
	writeFixDetails(&b, summary, skipped, loc)
	return b.String()
}

// RenderFixDone reports a pushed fix.
func RenderFixDone(sha string, fixed int, summary string, skipped []FixSkipped, loc Locale) string {
	var b strings.Builder
	b.WriteString("✅ ")
	b.WriteString(loc.Text("fix.done", "sha", "`"+short(sha)+"`",
		"fixed", strconv.Itoa(fixed), "skipped", strconv.Itoa(len(skipped))))
	writeFixDetails(&b, summary, skipped, loc)
	return b.String()
}

// RenderFixSkipReply is the reply in the thread of a finding a fix skipped.
func RenderFixSkipReply(reason string, loc Locale) string {
	return loc.Text("fix.skip_reply", "reason", neutralizeTags(fixReason(reason, loc)))
}

func writeFixDetails(b *strings.Builder, summary string, skipped []FixSkipped, loc Locale) {
	if summary = neutralizeTags(strings.TrimSpace(summary)); summary != "" {
		b.WriteString("\n\n")
		b.WriteString(summary)
	}
	if len(skipped) == 0 {
		return
	}
	b.WriteString("\n\n**")
	b.WriteString(loc.Text("fix.skipped_title"))
	b.WriteString("**\n")
	for _, s := range skipped {
		var name []string
		if title := strings.TrimSpace(s.Title); title != "" {
			name = append(name, neutralizeTags(title))
		}
		if s.Path != "" {
			name = append(name, "("+location(forge.Finding{Path: s.Path, StartLine: s.Line})+")")
		}
		b.WriteString("\n- ")
		if len(name) > 0 {
			b.WriteString(strings.Join(name, " "))
			b.WriteString(": ")
		}
		b.WriteString(neutralizeTags(fixReason(s.Reason, loc)))
	}
}

func fixReason(reason string, loc Locale) string {
	if reason = strings.Join(strings.Fields(reason), " "); reason != "" {
		return reason
	}
	return loc.Text("fix.no_reason")
}

func codeSpan(s string) string {
	return "`" + strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "`", "'") + "`"
}
