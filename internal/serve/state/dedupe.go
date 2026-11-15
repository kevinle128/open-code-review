// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import "strings"

// dedupeDiffs shows each hunk once. One hunk often holds the fixes of
// several findings; it stays with the first finding that has it, and a
// later finding gets the 1-based numbers of the findings that show its
// shared hunks. A text that is not a git diff, or that shares nothing, is
// kept as it is.
func dedupeDiffs(texts []string) ([]string, [][]int) {
	out := make([]string, len(texts))
	refs := make([][]int, len(texts))
	owner := map[string]int{}
	for k, text := range texts {
		out[k] = text
		files := parseDiff(text)
		if len(files) == 0 {
			continue
		}
		var kept []string
		dropped := false
		for _, f := range files {
			if len(f.hunks) == 0 {
				key := strings.Join(f.header, "\n")
				if j, ok := owner[key]; ok {
					dropped = true
					refs[k] = addRef(refs[k], j)
					continue
				}
				owner[key] = k + 1
				kept = append(kept, f.header...)
				continue
			}
			var idx []int
			for i, h := range f.hunks {
				key := f.oldPath + "\x00" + f.newPath + "\x00" + strings.Join(h.lines, "\n")
				if j, ok := owner[key]; ok {
					dropped = true
					refs[k] = addRef(refs[k], j)
					continue
				}
				owner[key] = k + 1
				idx = append(idx, i)
			}
			kept = appendHunks(kept, f, idx)
		}
		if dropped {
			out[k] = strings.Join(kept, "\n")
		}
	}
	return out, refs
}

func addRef(refs []int, n int) []int {
	for _, r := range refs {
		if r == n {
			return refs
		}
	}
	return append(refs, n)
}
