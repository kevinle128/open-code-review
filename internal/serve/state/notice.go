// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alibaba/open-code-review/internal/serve/review"
)

// Classes of a failed round, for the one new comment that tells the author.
const (
	FailureTime   = "time"
	FailureCuts   = "cuts"
	FailureEngine = "engine"
)

// FailureNotice is the public reason a round failed. Error is already safe
// to post: no tokens, paths or stack text.
type FailureNotice struct {
	Class   string
	Stage   string
	Elapsed string
	Limit   string
	Cuts    int
	Error   string
}

// RenderFailureNotice is one new comment, not an edit of the summary, so
// the forge notifies the author. It says what stopped the round, that merge
// stays blocked, and how to retry.
func RenderFailureNotice(n FailureNotice, loc Locale) string {
	var reason string
	switch n.Class {
	case FailureTime:
		stage := loc.Text("notice.stage_" + n.Stage)
		if stage == "notice.stage_"+n.Stage {
			stage = loc.Text("notice.stage_review")
		}
		reason = loc.Text("notice.time", "stage", Inert(stage), "limit", Inert(n.Limit), "elapsed", Inert(n.Elapsed))
	case FailureCuts:
		reason = loc.Text("notice.cuts", "count", Inert(strconv.Itoa(n.Cuts)))
	default:
		text := strings.TrimSpace(n.Error)
		if text == "" {
			text = loc.Text("notice.engine_fallback")
		}
		// Error is already sanitized, including mentions and markup.
		reason = loc.Text("notice.engine", "error", text)
	}
	return reason + "\n\n" + loc.Text("notice.status") + "\n\n" + loc.Text("notice.retry")
}

const maxPublicError = 160

var (
	urlRE = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://\S+`)
	// authSchemeRE eats the credential after Bearer or Basic. A pattern that
	// stops at the first word would keep the token in "Authorization: Bearer <jwt>".
	authSchemeRE = regexp.MustCompile(`(?i)\b(?:authorization\s*[:=]\s*)?(?:bearer|basic)\s+\S+`)
	// A sentence that only mentions keyboard or monkey has no assignment,
	// so those words stay. A key that contains one of these stems and is
	// followed by : or = is removed.
	secretName  = `token|key|secret|password|passwd|auth|credential|cookie`
	assignRE    = regexp.MustCompile(`(?i)\b[\w.-]*(?:` + secretName + `)[\w.-]*\s*[:=]\s*\S+`)
	quotedRE    = regexp.MustCompile(`(?i)["'][^"']*(?:` + secretName + `)[^"']*["']\s*:\s*["'][^"']*["']`)
	pathRE      = regexp.MustCompile(`(?:^|[\s"'(=])(?:/[A-Za-z0-9._~/-]+|[A-Za-z]:\\[^\s]+)`)
	longTokenRE = regexp.MustCompile(`\b(?:ghp_|github_pat_|glpat-|xox[baprs]-|sk-|AKIA)[A-Za-z0-9_\-]{8,}\b`)
)

// SanitizePublicError reduces an engine error to one short sentence that
// can be posted on a pull request. Paths, URLs and credential-shaped tokens
// are removed.
func SanitizePublicError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = urlRE.ReplaceAllString(s, "")
	s = quotedRE.ReplaceAllString(s, "")
	s = authSchemeRE.ReplaceAllString(s, "")
	s = assignRE.ReplaceAllString(s, "")
	s = longTokenRE.ReplaceAllString(s, "")
	s = pathRE.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " :,-")
	if s == "" {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if r := []rune(s); len(r) > maxPublicError {
		s = strings.TrimSpace(string(r[:maxPublicError])) + "..."
	}
	return Inert(s)
}

// Inert makes a fragment safe to insert into a pull request comment. It
// breaks @mentions so neither GitHub nor GitLab resolves them, and it
// escapes HTML and markdown so the fragment cannot change the comment.
func Inert(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "@", "@\u200b")
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '\\', '`', '*', '_', '[', ']', '(', ')', '#', '~', '|':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func skippedCount(c review.Coverage) int {
	n := 0
	for _, v := range c.Skipped {
		n += v
	}
	return n
}

func formatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	switch {
	case n < unit:
		return strconv.FormatInt(n, 10) + " B"
	case n < unit*unit:
		return trimFloat(float64(n)/unit) + " KiB"
	case n < unit*unit*unit:
		return trimFloat(float64(n)/(unit*unit)) + " MiB"
	default:
		return trimFloat(float64(n)/(unit*unit*unit)) + " GiB"
	}
}

func trimFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}
