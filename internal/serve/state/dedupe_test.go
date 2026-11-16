// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"slices"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/review"
)

const sharedHunkDiff = `diff --git a/q.go b/q.go
--- a/q.go
+++ b/q.go
@@ -10,2 +10,3 @@
 a
+b
 c
@@ -40,1 +41,2 @@
 x
+y`

const binaryDiff = `diff --git a/img.png b/img.png
Binary files a/img.png and b/img.png differ`

func TestDedupeDiffs_SharedHunkShownOnce(t *testing.T) {
	first := sharedHunkDiff
	second := "diff --git a/q.go b/q.go\n--- a/q.go\n+++ b/q.go\n@@ -10,2 +10,3 @@\n a\n+b\n c"
	third := second + "\n" + binaryDiff
	out, refs := dedupeDiffs([]string{first, second, "", third, binaryDiff, "not a diff"})
	if out[0] != first || refs[0] != nil {
		t.Fatalf("first finding must keep its diff: %q %v", out[0], refs[0])
	}
	if out[1] != "" || !slices.Equal(refs[1], []int{1}) {
		t.Fatalf("second finding must refer to item 1: %q %v", out[1], refs[1])
	}
	if out[2] != "" || refs[2] != nil {
		t.Fatalf("empty diff must stay empty: %q %v", out[2], refs[2])
	}
	if out[3] != binaryDiff || !slices.Equal(refs[3], []int{1}) {
		t.Fatalf("fourth finding keeps only its own file: %q %v", out[3], refs[3])
	}
	if out[4] != "" || !slices.Equal(refs[4], []int{4}) {
		t.Fatalf("hunkless file shared with item 4: %q %v", out[4], refs[4])
	}
	if out[5] != "not a diff" || refs[5] != nil {
		t.Fatalf("plain text must be kept: %q %v", out[5], refs[5])
	}
}

func TestRenderRound_SharedDiffNamedOnce(t *testing.T) {
	loc := mustLocale(t, "vi")
	r := RoundReport{N: 1, Max: 3, From: "a", To: "b", FixCommit: "0123456789abcdef",
		Fixed: []review.Fixed{
			{ID: "F1", Path: "q.go", Line: 11, Problem: "p1", Fix: "fix1"},
			{ID: "F2", Path: "q.go", Line: 11, Problem: "p2", Fix: "fix2"},
		},
		FixDiffs: []string{sharedHunkDiff, sharedHunkDiff},
		Reported: []review.Reported{
			{ID: "R1", Path: "q.go", Line: 11, Problem: "r1", ProposedDiff: sharedHunkDiff},
			{ID: "R2", Path: "q.go", Line: 11, Problem: "r2", ProposedDiff: sharedHunkDiff},
		},
	}
	body := RenderRound(r, loc)
	if n := strings.Count(body, "@@ -10,2 +10,3 @@"); n != 2 {
		t.Fatalf("shared hunk must show once per list, got %d:\n%s", n, body)
	}
	if n := strings.Count(body, loc.Text("round.diff_shared", "items", "1")); n != 2 {
		t.Fatalf("each repeat must name item 1, got %d:\n%s", n, body)
	}
	if strings.Contains(body, "fix2") {
		t.Fatalf("a finding with a shared diff needs no prose fix:\n%s", body)
	}
}
