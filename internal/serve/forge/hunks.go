// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package forge

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// LineRange is a closed range of right-side (new file) lines inside one hunk.
type LineRange struct {
	Start int
	End   int
}

// LinePos describes one right-side line of a patch. New is true for an added
// line; context lines carry their old-file line number in OldLine. GitLab
// needs both to build a line_code.
type LinePos struct {
	New     bool
	OldLine int
}

// Inventory is the right-side view of a PR diff that placement works against.
//
//   - Known holds every changed path, including paths without a patch.
//   - Ranges and Positions hold only paths whose patch parsed complete; a
//     clipped or missing patch proves nothing about its lines.
//   - Complete is true only when the changed-file list was fully enumerated;
//     without it a path missing from Known is not proof that it is outside
//     the diff.
type Inventory struct {
	Complete  bool
	Known     map[string]bool
	Ranges    map[string][]LineRange
	Positions map[string]map[int]LinePos
}

var hunkHeaderRE = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ParsePatch reads a unified-diff patch into one right-side range per hunk and
// a map of each right-side line. complete is false when the patch has no hunk
// or when a hunk shows fewer lines than its header declares, which is how a
// patch clipped by the API looks; treating that prefix as the whole hunk would
// wrongly prove later lines outside the diff.
//
// The ranges and positions seen so far are returned even when complete is
// false; callers must then leave the path out of Inventory.Ranges so Place
// treats it as a file without a usable patch.
//
// Ranges stay per hunk because a multi-line comment must sit inside a single
// hunk: a span that straddles two hunks is rejected by the forge.
func ParsePatch(patch string) (ranges []LineRange, positions map[int]LinePos, complete bool) {
	positions = map[int]LinePos{}
	if patch == "" {
		return nil, positions, false
	}
	type hunk struct{ start, next, oldNext, expected, observed int }
	var cur *hunk
	sawHunk := false
	complete = true
	flush := func() {
		if cur == nil {
			return
		}
		if cur.observed != cur.expected {
			complete = false
		}
		// A pure-deletion hunk ("+N,0") has no right-side line to comment on.
		if cur.next > cur.start {
			ranges = append(ranges, LineRange{Start: cur.start, End: cur.next - 1})
		}
	}
	for _, line := range strings.Split(patch, "\n") {
		if m := hunkHeaderRE.FindStringSubmatch(line); m != nil {
			flush()
			oldStart, _ := strconv.Atoi(m[1])
			start, _ := strconv.Atoi(m[2])
			expected := 1
			if m[3] != "" {
				expected, _ = strconv.Atoi(m[3])
			}
			cur = &hunk{start: start, next: start, oldNext: oldStart, expected: expected}
			sawHunk = true
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, `\`):
			// "\ No newline at end of file" is not a diff line.
		case strings.HasPrefix(line, "-"):
			cur.oldNext++
		case strings.HasPrefix(line, "+"):
			positions[cur.next] = LinePos{New: true}
			cur.next++
			cur.observed++
		case strings.HasPrefix(line, " "):
			positions[cur.next] = LinePos{OldLine: cur.oldNext}
			cur.next++
			cur.oldNext++
			cur.observed++
		}
		// Anything else, such as the "" after a trailing newline, is not a
		// body line and must not advance the counters.
	}
	flush()
	return ranges, positions, sawHunk && complete
}

// PlacementKind says where a finding is posted. Every kind is a resolvable
// thread, so the merge gate covers all of them.
type PlacementKind int

const (
	// Line anchors the finding on its own lines.
	Line PlacementKind = iota
	// NearestLine anchors it on the closest right-side line of the same file.
	NearestLine
	// FileLevel posts a file-level thread on Placement.Path.
	FileLevel
	// General posts a thread with no file position.
	General
)

// Placement is where a finding goes. Relocated is true when the anchor is not
// the finding's own path and lines, so the body must name the original
// location.
type Placement struct {
	Kind      PlacementKind
	Path      string
	StartLine int
	EndLine   int
	Relocated bool
}

// Place picks the anchor for f:
//
//  1. a span inside one hunk of its file -> Line on the exact span;
//  2. a file with right-side lines but the span outside every hunk ->
//     NearestLine on the closest right-side line (ties go to the lower line);
//  3. a changed file without right-side lines (pure deletion) or without a
//     usable patch -> FileLevel on that file;
//  4. a file that is provably not in the diff -> FileLevel on
//     firstChangedPath, or General when firstChangedPath is empty (GitLab
//     passes "" because it has no file-level threads). A finding with an
//     empty path is about the PR as a whole and takes the same route.
//
// When the inventory cannot prove the path is outside the diff, Place returns
// Line and leaves the forge to fall back after IsLineResolutionFailure.
func Place(f Finding, inv Inventory, firstChangedPath string) Placement {
	start, end := normalizeSpan(f.StartLine, f.EndLine)
	exact := Placement{Kind: Line, Path: f.Path, StartLine: start, EndLine: end}

	if !inv.Known[f.Path] {
		if !inv.Complete && f.Path != "" && start > 0 {
			return exact
		}
		if firstChangedPath == "" {
			return Placement{Kind: General, Relocated: true}
		}
		return Placement{Kind: FileLevel, Path: firstChangedPath, Relocated: true}
	}

	ranges, ok := inv.Ranges[f.Path]
	if !ok || len(ranges) == 0 || start < 1 {
		return Placement{Kind: FileLevel, Path: f.Path}
	}
	for _, r := range ranges {
		if start >= r.Start && end <= r.End {
			return exact
		}
	}
	line := nearestLine(ranges, start, end)
	return Placement{Kind: NearestLine, Path: f.Path, StartLine: line, EndLine: line, Relocated: true}
}

func normalizeSpan(start, end int) (int, int) {
	if start < 1 {
		start = end
	}
	if end < start {
		end = start
	}
	return start, end
}

// nearestLine returns the right-side line with the smallest distance to the
// span [start, end]; a line inside the span has distance 0.
func nearestLine(ranges []LineRange, start, end int) int {
	best, bestDist := 0, -1
	for _, r := range ranges {
		var line, dist int
		switch {
		case r.End < start:
			line, dist = r.End, start-r.End
		case r.Start > end:
			line, dist = r.Start, r.Start-end
		default:
			line, dist = max(r.Start, start), 0
		}
		if bestDist < 0 || dist < bestDist || (dist == bestDist && line < best) {
			best, bestDist = line, dist
		}
	}
	return best
}

// GitHub answers an unresolvable line with 422 and wording only; it does not
// say which comment failed. "could not be resolved" is the wording seen live;
// the others are defensive, so do not remove them without re-probing GitHub.
var githubLinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)must be part of the diff`),
	regexp.MustCompile(`(?i)not part of the diff`),
	regexp.MustCompile(`(?i)could not be resolved`),
	regexp.MustCompile(`(?i)outside the diff`),
	regexp.MustCompile(`(?i)diff hunk`),
}

var githubLineFields = map[string]bool{
	"line": true, "start_line": true, "position": true, "original_line": true, "original_start_line": true,
}

// GitLab answers a bad position with 400 and a message that names the
// position field.
var gitlabLinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)new_line|old_line|line_range|line_code|position`),
	regexp.MustCompile(`(?i)out of (the )?diff`),
	regexp.MustCompile(`(?i)could not be resolved`),
	regexp.MustCompile(`(?i)is invalid`),
	regexp.MustCompile(`(?i)line (range )?out of range`),
}

// IsLineResolutionFailure reports whether an error response means the comment
// position could not be resolved against the diff, so the caller should retry
// the finding as a file-level or general thread. GitHub uses 422 and GitLab
// uses 400; the patterns are scoped by status so an unrelated validation error
// on one platform is not misread.
func IsLineResolutionFailure(status int, body []byte) bool {
	switch status {
	case http.StatusUnprocessableEntity:
		var payload struct {
			Errors []json.RawMessage `json:"errors"`
		}
		if json.Unmarshal(body, &payload) == nil {
			for _, raw := range payload.Errors {
				var entry struct {
					Field string `json:"field"`
				}
				if json.Unmarshal(raw, &entry) == nil && githubLineFields[entry.Field] {
					return true
				}
			}
		}
		return matchAny(githubLinePatterns, string(body))
	case http.StatusBadRequest:
		return matchAny(gitlabLinePatterns, string(body))
	default:
		return false
	}
}

func matchAny(res []*regexp.Regexp, text string) bool {
	if text == "" {
		return false
	}
	for _, re := range res {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
