// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"errors"
	"regexp"
	"strings"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// Command kinds.
const (
	CommandReview       = "review"
	CommandReviewCommit = "review-commit"
)

// Command is a parsed /review or /review-commit comment.
type Command struct {
	Kind   string
	Effort string
}

func (c Command) trigger() state.Trigger {
	if c.Kind == CommandReviewCommit {
		return state.TriggerCmdReviewCommit
	}
	return state.TriggerCmdReview
}

var slashWordRE = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*$`)

// ParseCommand reads the first non-empty line of a comment. Only that line
// counts, so prose below a command cannot add arguments. The bool is false
// when the line is not /review or /review-commit; the error rejects an
// argument outside the override allowlist and is safe to quote in a reply.
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

// unknownSlashCommand returns the first word of a comment that looks like a
// slash command but is not one the service knows. GitLab removes its own
// quick-action lines before a note is stored, so a word left here was meant
// for a bot. A path such as /etc/hosts does not match.
func unknownSlashCommand(body string) (string, bool) {
	word, _ := firstWord(body)
	if !slashWordRE.MatchString(word) {
		return "", false
	}
	if _, ok, _ := ParseCommand(body); ok {
		return "", false
	}
	return word, true
}

// commandAdded reports whether the command line of body was not already a
// line of prev, so editing a comment does not run a command again.
func commandAdded(body, prev string) bool {
	if prev == "" {
		return true
	}
	line := normalizeLine(firstLine(body))
	for l := range strings.SplitSeq(prev, "\n") {
		if normalizeLine(l) == line {
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
