// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/review"
)

// diffFile is one file section of a git diff: its header lines up to the
// first hunk, and the hunks. A binary or mode-only change has no hunk.
type diffFile struct {
	oldPath, newPath string
	header           []string
	hunks            []diffHunk
}

// diffHunk is one hunk, its "@@" line first. newStart and newCount are the
// new-file range from that line.
type diffHunk struct {
	lines              []string
	newStart, newCount int
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseDiff splits the output of git diff into file sections and hunks.
// Text before the first file section is dropped.
func parseDiff(diff string) []diffFile {
	var files []diffFile
	var cur *diffFile
	for _, line := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files = append(files, diffFile{header: []string{line}})
			cur = &files[len(files)-1]
			cur.oldPath, cur.newPath = gitLinePaths(strings.TrimPrefix(line, "diff --git "))
		case cur == nil:
		case strings.HasPrefix(line, "@@ "):
			h := diffHunk{lines: []string{line}}
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				h.newStart, _ = strconv.Atoi(m[1])
				h.newCount = 1
				if m[2] != "" {
					h.newCount, _ = strconv.Atoi(m[2])
				}
			}
			cur.hunks = append(cur.hunks, h)
		case len(cur.hunks) > 0:
			last := &cur.hunks[len(cur.hunks)-1]
			last.lines = append(last.lines, line)
		default:
			cur.header = append(cur.header, line)
			if p, ok := headerPath(line, "--- ", "a/"); ok {
				cur.oldPath = p
			} else if p, ok := headerPath(line, "+++ ", "b/"); ok {
				cur.newPath = p
			} else if p, ok := strings.CutPrefix(line, "rename from "); ok {
				cur.oldPath = p
			} else if p, ok := strings.CutPrefix(line, "rename to "); ok {
				cur.newPath = p
			}
		}
	}
	return files
}

// headerPath reads the path of a "--- a/x" or "+++ b/x" line; /dev/null
// gives an empty path. git ends a path that holds a space with a tab.
func headerPath(line, marker, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(line, marker)
	if !ok {
		return "", false
	}
	rest = strings.TrimRight(rest, "\t")
	if rest == "/dev/null" {
		return "", true
	}
	rest = strings.Trim(rest, `"`)
	return strings.TrimPrefix(rest, prefix), true
}

// gitLinePaths reads "a/<old> b/<new>" from a diff --git line. The two
// halves are equal unless the file was renamed, which the rename lines
// then correct, so an even split is tried before the last " b/".
func gitLinePaths(rest string) (string, string) {
	if n := len(rest); n%2 == 1 {
		a, b := rest[:n/2], rest[n/2+1:]
		if strings.HasPrefix(a, "a/") && strings.HasPrefix(b, "b/") && a[2:] == b[2:] {
			return a[2:], b[2:]
		}
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 && strings.HasPrefix(rest, "a/") {
		return rest[2:i], rest[i+3:]
	}
	return "", ""
}

// claims reports whether change c covers a hunk of f; a nil h stands for
// a file section without hunks.
func claims(c review.Change, f diffFile, h *diffHunk) bool {
	p := cleanPath(c.Path)
	if p == "" || (p != f.newPath && p != f.oldPath) {
		return false
	}
	if c.StartLine <= 0 || h == nil {
		return true
	}
	end := max(c.EndLine, c.StartLine)
	lo, hi := h.newStart, h.newStart+h.newCount-1
	if h.newCount == 0 {
		// A hunk that only deletes lines sits between new lines newStart
		// and newStart+1.
		hi = h.newStart + 1
	}
	return c.StartLine <= hi && end >= lo
}

// cleanPath turns an agent path into the repository-relative form git
// prints.
func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.TrimLeft(path.Clean(strings.ReplaceAll(p, `\`, "/")), "/")
	if p == "." {
		return ""
	}
	return p
}

// SplitDiff gives each fixed finding the hunks of diff that its Changes
// cover, as diff text with the file headers. One hunk can serve several
// findings. other holds the hunks no finding claims, so the comment hides
// nothing of the commit. unmatched lists the ids of findings that claim no
// hunk.
func SplitDiff(diff string, fixed []review.Fixed) (per []string, other string, unmatched []string) {
	files := parseDiff(diff)
	parts := make([][]string, len(fixed))
	var rest []string
	for _, f := range files {
		if len(f.hunks) == 0 {
			taken := false
			for k, fx := range fixed {
				if anyClaims(fx.Changes, f, nil) {
					parts[k] = append(parts[k], f.header...)
					taken = true
				}
			}
			if !taken {
				rest = append(rest, f.header...)
			}
			continue
		}
		owned := make([][]int, len(fixed))
		var orphans []int
		for i := range f.hunks {
			taken := false
			for k, fx := range fixed {
				if anyClaims(fx.Changes, f, &f.hunks[i]) {
					owned[k] = append(owned[k], i)
					taken = true
				}
			}
			if !taken {
				orphans = append(orphans, i)
			}
		}
		for k, idx := range owned {
			parts[k] = appendHunks(parts[k], f, idx)
		}
		rest = appendHunks(rest, f, orphans)
	}
	per = make([]string, len(fixed))
	for k, p := range parts {
		per[k] = strings.Join(p, "\n")
		if per[k] == "" {
			unmatched = append(unmatched, fixed[k].ID)
		}
	}
	return per, strings.Join(rest, "\n"), unmatched
}

func anyClaims(cs []review.Change, f diffFile, h *diffHunk) bool {
	for _, c := range cs {
		if claims(c, f, h) {
			return true
		}
	}
	return false
}

// appendHunks appends the header of f and its hunks at idx; no index adds
// nothing.
func appendHunks(out []string, f diffFile, idx []int) []string {
	if len(idx) == 0 {
		return out
	}
	out = append(out, f.header...)
	for _, i := range idx {
		out = append(out, f.hunks[i].lines...)
	}
	return out
}

// ShiftChanges moves the line ranges of fixed, recorded on the commit whose
// diff is from, to the same fix replayed elsewhere, whose diff is to. A file
// keeps its ranges unless both diffs hold it with the same number of hunks;
// then each range moves by the offset of the hunks it covers. fixed is not
// changed.
func ShiftChanges(from, to string, fixed []review.Fixed) []review.Fixed {
	byPath := map[string]diffFile{}
	for _, f := range parseDiff(to) {
		byPath[f.newPath] = f
	}
	type pair struct{ old, cur diffFile }
	pairs := map[string]pair{}
	for _, f := range parseDiff(from) {
		if g, ok := byPath[f.newPath]; ok && f.newPath != "" && len(f.hunks) > 0 && len(f.hunks) == len(g.hunks) {
			pairs[f.newPath] = pair{f, g}
		}
	}
	out := make([]review.Fixed, len(fixed))
	for k, fx := range fixed {
		out[k] = fx
		out[k].Changes = make([]review.Change, len(fx.Changes))
		for i, c := range fx.Changes {
			out[k].Changes[i] = c
			p, ok := pairs[cleanPath(c.Path)]
			if !ok || c.StartLine <= 0 {
				continue
			}
			first, last := -1, -1
			for j := range p.old.hunks {
				if claims(c, p.old, &p.old.hunks[j]) {
					if first < 0 {
						first = j
					}
					last = j
				}
			}
			if first < 0 {
				continue
			}
			end := max(c.EndLine, c.StartLine)
			out[k].Changes[i].StartLine = c.StartLine + p.cur.hunks[first].newStart - p.old.hunks[first].newStart
			out[k].Changes[i].EndLine = end + p.cur.hunks[last].newStart - p.old.hunks[last].newStart
		}
	}
	return out
}
