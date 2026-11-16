// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"slices"
	"testing"
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
