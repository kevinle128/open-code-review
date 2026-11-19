// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"slices"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/review"
)

// fixDiff is a commit diff with two hunks in a.go, a new file, a file
// whose lines are only deleted, a deleted file and a binary file.
const fixDiff = `diff --git a/a.go b/a.go
index 1111111..2222222 100644
--- a/a.go
+++ b/a.go
@@ -1,4 +1,5 @@ package a
 package a

-func A() {}
+func A() int { return 1 }
+
 // end
@@ -40,3 +41,3 @@ func B() {
 	x := 1
-	y := 2
+	y := 3
 	return
diff --git a/new_test.go b/new_test.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/new_test.go
@@ -0,0 +1,3 @@
+package a
+
+func TestA(t *testing.T) {}
diff --git a/c.go b/c.go
index 4444444..5555555 100644
--- a/c.go
+++ b/c.go
@@ -10,2 +9,0 @@ func C() {
-	debug()
-	debug()
diff --git a/gone.go b/gone.go
deleted file mode 100644
index 6666666..0000000
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package a
-var gone = 1
diff --git a/logo.png b/logo.png
index 7777777..8888888 100644
Binary files a/logo.png and b/logo.png differ
`

func fixed(id string, cs ...review.Change) review.Fixed {
	return review.Fixed{ID: id, Changes: cs}
}

func TestParseDiff(t *testing.T) {
	files := parseDiff("preamble\n" + fixDiff)
	if len(files) != 5 {
		t.Fatalf("files %d", len(files))
	}
	a := files[0]
	if a.oldPath != "a.go" || a.newPath != "a.go" || len(a.hunks) != 2 || a.hunks[1].newStart != 41 || a.hunks[1].newCount != 3 {
		t.Fatalf("a.go %+v", a)
	}
	if n := files[1]; n.oldPath != "" || n.newPath != "new_test.go" || n.hunks[0].newStart != 1 {
		t.Fatalf("new file %+v", n)
	}
	if c := files[2]; c.hunks[0].newStart != 9 || c.hunks[0].newCount != 0 {
		t.Fatalf("deletion hunk %+v", c.hunks[0])
	}
	if g := files[3]; g.oldPath != "gone.go" || g.newPath != "" {
		t.Fatalf("deleted file %+v", g)
	}
	if b := files[4]; b.newPath != "logo.png" || len(b.hunks) != 0 || len(b.header) != 3 {
		t.Fatalf("binary file %+v", b)
	}
	renamed := parseDiff("diff --git a/old name.go b/new name.go\nsimilarity index 90%\nrename from old name.go\nrename to new name.go\n")
	if renamed[0].oldPath != "old name.go" || renamed[0].newPath != "new name.go" {
		t.Fatalf("rename %+v", renamed[0])
	}
	if o, n := gitLinePaths("a/x b/y z"); o != "x" || n != "y z" {
		t.Fatalf("uneven paths %q %q", o, n)
	}
	if o, n := gitLinePaths("weird"); o != "" || n != "" {
		t.Fatalf("bad line %q %q", o, n)
	}
	spaced := parseDiff("diff --git a/my file.go b/my file.go\n--- a/my file.go\t\n+++ b/my file.go\t\n@@ -1 +1 @@\n-a\n+b\n")
	if spaced[0].newPath != "my file.go" || spaced[0].hunks[0].newCount != 1 {
		t.Fatalf("spaced %+v", spaced[0])
	}
}

func TestSplitDiff_Attribution(t *testing.T) {
	findings := []review.Fixed{
		fixed("F1", review.Change{Path: "a.go", StartLine: 3, EndLine: 4}, review.Change{Path: "./new_test.go", StartLine: 1, EndLine: 3}),
		// Overlaps the same first hunk as F1 and also claims the second.
		fixed("F2", review.Change{Path: "a.go", StartLine: 1, EndLine: 1}, review.Change{Path: "a.go", StartLine: 42, EndLine: 42}),
		// A deletion-only hunk and a deleted file.
		fixed("F3", review.Change{Path: "c.go", StartLine: 10, EndLine: 9}, review.Change{Path: "gone.go"}),
		fixed("F4", review.Change{Path: "elsewhere.go", StartLine: 1, EndLine: 9}),
		fixed("F5"),
	}
	per, other, unmatched := SplitDiff(fixDiff, findings)
	if !slices.Equal(unmatched, []string{"F4", "F5"}) || per[3] != "" || per[4] != "" {
		t.Fatalf("unmatched %v", unmatched)
	}
	f1 := per[0]
	for _, want := range []string{"+++ b/a.go", "+func A() int { return 1 }", "+++ b/new_test.go", "+func TestA"} {
		if !strings.Contains(f1, want) {
			t.Fatalf("F1 lacks %q:\n%s", want, f1)
		}
	}
	if strings.Contains(f1, "y := 3") || strings.Count(f1, "diff --git a/a.go") != 1 {
		t.Fatalf("F1 took the wrong hunks:\n%s", f1)
	}
	f2 := per[1]
	if !strings.Contains(f2, "-func A() {}") || !strings.Contains(f2, "+\ty := 3") || strings.Count(f2, "+++ b/a.go") != 1 {
		t.Fatalf("F2:\n%s", f2)
	}
	if !strings.Contains(per[2], "-\tdebug()") || !strings.Contains(per[2], "-var gone = 1") {
		t.Fatalf("F3:\n%s", per[2])
	}
	if other != "diff --git a/logo.png b/logo.png\nindex 7777777..8888888 100644\nBinary files a/logo.png and b/logo.png differ" {
		t.Fatalf("other:\n%s", other)
	}
}

func TestSplitDiff_UnclaimedHunksAndWholeFiles(t *testing.T) {
	per, other, unmatched := SplitDiff(fixDiff, []review.Fixed{fixed("F1", review.Change{Path: "a.go", StartLine: 41, EndLine: 43}),
		fixed("F2", review.Change{Path: "logo.png", StartLine: 1, EndLine: 1})})
	if len(unmatched) != 0 {
		t.Fatalf("unmatched %v", unmatched)
	}
	if strings.Contains(per[0], "func A() int") || !strings.Contains(per[0], "y := 3") {
		t.Fatalf("F1:\n%s", per[0])
	}
	if !strings.Contains(per[1], "Binary files") {
		t.Fatalf("F2:\n%s", per[1])
	}
	for _, want := range []string{"+func A() int", "+++ b/new_test.go", "-\tdebug()", "-var gone = 1"} {
		if !strings.Contains(other, want) {
			t.Fatalf("other lacks %q:\n%s", want, other)
		}
	}
	if strings.Contains(other, "y := 3") || strings.Contains(other, "Binary files") {
		t.Fatalf("other holds claimed hunks:\n%s", other)
	}
	per, other, unmatched = SplitDiff("", []review.Fixed{fixed("F1", review.Change{Path: "a.go"})})
	if per[0] != "" || other != "" || !slices.Equal(unmatched, []string{"F1"}) {
		t.Fatalf("empty diff: %q %q %v", per, other, unmatched)
	}
	if cleanPath(" /x/../a.go ") != "a.go" || cleanPath("./") != "" || cleanPath(`dir\a.go`) != "dir/a.go" || cleanPath("") != "" {
		t.Fatal("cleanPath")
	}
}

func TestShiftChangesFollowsTheReplayedHunks(t *testing.T) {
	from := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3 +3 @@\n-x\n+y\n@@ -20,2 +20,3 @@\n a\n+b\n c\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -5 +5 @@\n-x\n+y\n"
	to := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3 +3 @@\n-x\n+y\n@@ -27,2 +27,3 @@\n a\n+b\n c\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -5 +5 @@\n-x\n+y\n@@ -9 +9 @@\n-p\n+q\n"
	fixed := []review.Fixed{
		{ID: "F1", Changes: []review.Change{{Path: "a.go", StartLine: 3, EndLine: 3}, {Path: "./a.go", StartLine: 21}}},
		{ID: "F2", Changes: []review.Change{{Path: "a.go", StartLine: 3, EndLine: 21}, {Path: "a.go", StartLine: 50, EndLine: 51}}},
		{ID: "F3", Changes: []review.Change{{Path: "b.go", StartLine: 5, EndLine: 5}, {Path: "c.go"}}},
	}
	got := ShiftChanges(from, to, fixed)
	want := [][]review.Change{
		{{Path: "a.go", StartLine: 3, EndLine: 3}, {Path: "./a.go", StartLine: 28, EndLine: 28}},
		{{Path: "a.go", StartLine: 3, EndLine: 28}, {Path: "a.go", StartLine: 50, EndLine: 51}},
		{{Path: "b.go", StartLine: 5, EndLine: 5}, {Path: "c.go"}},
	}
	for k := range want {
		if !slices.Equal(got[k].Changes, want[k]) {
			t.Fatalf("%s changes %+v, want %+v", got[k].ID, got[k].Changes, want[k])
		}
	}
	if fixed[0].Changes[1].StartLine != 21 {
		t.Fatal("ShiftChanges changed its input")
	}
	per, other, unmatched := SplitDiff(to, got[:1])
	if len(unmatched) != 0 || !strings.Contains(per[0], "@@ -27,2 +27,3 @@") || strings.Contains(other, "@@ -27") {
		t.Fatalf("per %q other %q unmatched %v", per, other, unmatched)
	}
}

func TestShiftChangesClampsToTheReplayedHunks(t *testing.T) {
	// The moved branch deleted lines above both hunks of a.go, and the
	// edit at the end of c.go moved to line 3, where its hunk has only two
	// lines of leading context.
	from := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -20,2 +20,3 @@\n a\n+b\n c\n@@ -40 +41 @@\n-x\n+y\n" +
		"diff --git a/c.go b/c.go\n--- a/c.go\n+++ b/c.go\n@@ -9,4 +9,4 @@\n a\n b\n c\n-x\n+y\n"
	to := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -10,2 +10,3 @@\n a\n+b\n c\n@@ -30 +31 @@\n-x\n+y\n" +
		"diff --git a/c.go b/c.go\n--- a/c.go\n+++ b/c.go\n@@ -1,3 +1,3 @@\n b\n c\n-x\n+y\n"
	fixed := []review.Fixed{
		{ID: "F1", Changes: []review.Change{{Path: "a.go", StartLine: 5, EndLine: 21}}},
		{ID: "F2", Changes: []review.Change{{Path: "c.go", StartLine: 12, EndLine: 12}}},
	}
	got := ShiftChanges(from, to, fixed)
	want := [][]review.Change{
		{{Path: "a.go", StartLine: 10, EndLine: 11}},
		{{Path: "c.go", StartLine: 3, EndLine: 3}},
	}
	for k := range want {
		if !slices.Equal(got[k].Changes, want[k]) {
			t.Fatalf("%s changes %+v, want %+v", got[k].ID, got[k].Changes, want[k])
		}
	}
	per, other, unmatched := SplitDiff(to, got)
	if len(unmatched) != 0 || strings.Contains(per[0], "@@ -30 +31 @@") || !strings.Contains(other, "@@ -30 +31 @@") ||
		!strings.Contains(per[1], "@@ -1,3 +1,3 @@") {
		t.Fatalf("per %q other %q unmatched %v", per, other, unmatched)
	}
}
