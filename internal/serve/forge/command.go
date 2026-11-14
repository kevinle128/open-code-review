// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package forge

import "strings"

// CommandLine returns the first non-empty line of a comment in slash form.
// A line that starts by tagging the bot with one of names ("@bot review",
// "@bot: /review --effort=high") comes back as "/review" or
// "/review --effort=high", and mentioned is true; a bare "@bot" gives an
// empty line. Any other line comes back unchanged, so "/review" still works
// and "@someone review" is not addressed to the bot. Names match without regard to
// case and must end at a space, ":", "," or the end of the line, so the name
// "bot" does not match "@bot-x".
func CommandLine(body string, names []string) (line string, mentioned bool) {
	line = firstLine(body)
	rest, ok := stripMention(line, names)
	if !ok {
		return line, false
	}
	if rest == "" || strings.HasPrefix(rest, "/") {
		return rest, true
	}
	return "/" + rest, true
}

// IsCommand reports whether a comment is meant for the bot: its first line
// is a slash command or tags the bot by name. The orchestrator parses the
// command and answers unknown ones with help.
func IsCommand(body string, names []string) bool {
	line, mentioned := CommandLine(body, names)
	return mentioned || strings.HasPrefix(line, "/")
}

// MergeNames joins name lists in order, dropping blanks and names repeated
// in another case, so the first name stays the one shown to users.
func MergeNames(lists ...[]string) []string {
	var out []string
	for _, list := range lists {
		for _, n := range list {
			n = strings.TrimSpace(n)
			if n == "" || containsFold(out, n) {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

func stripMention(line string, names []string) (string, bool) {
	for _, n := range names {
		if n == "" || len(line) < len(n)+1 || line[0] != '@' || !strings.EqualFold(line[1:len(n)+1], n) {
			continue
		}
		rest := line[len(n)+1:]
		switch {
		case rest == "":
			return "", true
		case rest[0] == ':' || rest[0] == ',':
			return strings.TrimSpace(rest[1:]), true
		case rest[0] == ' ' || rest[0] == '\t':
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

func firstLine(body string) string {
	for l := range strings.SplitSeq(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
