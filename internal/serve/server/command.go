// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"errors"
	"regexp"
	"strings"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// Command kinds.
const (
	CommandReview       = "review"
	CommandReviewCommit = "review-commit"
)

// Command is a parsed /review or /review-commit comment. A comment that
// tags the bot ("@bot review") is put in slash form before parsing.
type Command struct {
	Kind   string
	Effort string
}

var slashWordRE = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*$`)

// ParseCommand reads the first non-empty line of a comment. Only that line
// counts, so prose below a command cannot add arguments. The bool is false
// when the line is not a known command; the error rejects an argument
// outside the override allowlist and is safe to quote in a reply.
func ParseCommand(body string) (Command, bool, error) {
	name, rest := firstWord(body)
	var cmd Command
	switch name {
	case "/review":
		cmd.Kind = CommandReview
	case "/review-commit":
		cmd.Kind = CommandReviewCommit
	default:
		return Command{}, false, nil
	}
	args, err := splitArgs(rest)
	if err != nil {
		return cmd, true, err
	}
	effort, err := serveconfig.ParseCommandOverrides(args)
	if err != nil {
		return cmd, true, err
	}
	cmd.Effort = effort
	return cmd, true, nil
}

// unknownCommand returns the first word of a command line (from
// forge.CommandLine) that is meant for the bot but is not a command the
// service knows. A line that tags the bot is always meant for it; a bare tag
// gives an empty word. An untagged line counts only when it looks like a
// slash command: GitLab removes its own quick-action lines before a note is
// stored, so a word left there was meant for a bot, while a path such as
// /etc/hosts does not match.
func unknownCommand(line string, mentioned bool) (string, bool) {
	if _, ok, _ := ParseCommand(line); ok {
		return "", false
	}
	word, _ := firstWord(line)
	if mentioned {
		return word, true
	}
	if !slashWordRE.MatchString(word) {
		return "", false
	}
	return word, true
}

// commandAdded reports whether line, the command line of an edited comment,
// was not already a line of prev, so editing a comment does not run a
// command again. Each line of prev is put in slash form with names first,
// so an edit from "@bot review" to "/review" does not run the review twice.
func commandAdded(line, prev string, names []string) bool {
	if prev == "" {
		return true
	}
	want := normalizeLine(line)
	for l := range strings.SplitSeq(prev, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if p, _ := forge.CommandLine(l, names); normalizeLine(p) == want {
			return false
		}
	}
	return true
}

func firstLine(body string) string {
	for l := range strings.SplitSeq(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// firstWord splits the first non-empty line into its first word and the rest.
func firstWord(body string) (word, rest string) {
	line := firstLine(body)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], line[i+1:]
	}
	return line, ""
}

func normalizeLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// splitArgs splits on whitespace and groups double-quoted text, the subset
// of shell quoting that pr-agent's command lexer accepts. A single quote and
// '#' are ordinary characters.
func splitArgs(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inQuote bool
		inArg   bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			inArg = true
		case !inQuote && (r == ' ' || r == '\t'):
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inQuote {
		return nil, errors.New("unterminated quote")
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}
