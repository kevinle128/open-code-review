// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package clireview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/review"
)

func TestLoadSkillInlinesReferencesInOrder(t *testing.T) {
	root := t.TempDir()
	write(t, root, SkillFile, "# skill\n")
	writeReferences(t, root, reviewReferences)
	got, err := loadSkill(root, reviewReferences)
	if err != nil || len(got.Warnings) != 0 {
		t.Fatalf("skill %+v err %v", got, err)
	}
	if got.Dir != filepath.Join(root, "skills", "ak-review-pr") || !strings.HasPrefix(got.Text, "# skill") {
		t.Fatalf("skill %+v", got)
	}
	last := 0
	for _, n := range reviewReferences {
		i := strings.Index(got.Text, "# Reference: references/"+n+"\n\n"+refText(n))
		if i < last {
			t.Fatalf("reference %s missing or out of order:\n%s", n, got.Text)
		}
		last = i
	}
}

func TestLoadSkillSkipsAMissingReference(t *testing.T) {
	root := t.TempDir()
	write(t, root, SkillFile, "# skill\n")
	writeReferences(t, root, reviewReferences[1:])
	got, err := loadSkill(root, reviewReferences)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], reviewReferences[0]) || !strings.Contains(got.Warnings[0], "missing") {
		t.Fatalf("warnings %v", got.Warnings)
	}
	if strings.Contains(got.Text, reviewReferences[0]) || !strings.Contains(got.Text, refText(reviewReferences[1])) {
		t.Fatalf("text\n%s", got.Text)
	}
	// A reference that exists but cannot be read is left out the same way.
	if err := os.Mkdir(filepath.Join(got.Dir, "references", reviewReferences[0]), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err = loadSkill(root, reviewReferences)
	if err != nil || len(got.Warnings) != 1 || strings.Contains(got.Warnings[0], ": missing") {
		t.Fatalf("warnings %v err %v", got.Warnings, err)
	}
	if _, err := loadSkill(t.TempDir(), reviewReferences); err == nil || !strings.Contains(err.Error(), "read review skill") {
		t.Fatalf("err %v", err)
	}
}

func TestReviewAndFixPromptsHoldTheReferences(t *testing.T) {
	r := newRepo(t)
	cli := &fakeCLI{msg: `{"summary":"ok","findings":[]}`}
	rev, _ := newReviewer(t, fakeSelector{sel: Selection{Base: r.base, Head: r.head, Paths: []string{"a.go"}}}, cli)
	if err := os.Remove(filepath.Join(rev.SkillDir, filepath.Dir(SkillFile), "references", "anti-ai-slop.md")); err != nil {
		t.Fatal(err)
	}
	res, err := rev.Review(context.Background(), review.Request{RepoDir: r.dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "anti-ai-slop.md") {
		t.Fatalf("warnings %v", res.Warnings)
	}
	for _, n := range reviewReferences {
		if n != "anti-ai-slop.md" && !strings.Contains(cli.inv.Prompt, refText(n)) {
			t.Errorf("review prompt misses %s", n)
		}
	}
	if !strings.Contains(cli.inv.Prompt, "overrides the rest of the skill, including the `gh` commands") {
		t.Errorf("review prompt misses the override rule")
	}

	fcli := &fixCLI{msg: `{"fixed":[],"skipped":[],"summary":"none","commit_message":"fix: none"}`}
	fx, _ := newFixer(t, fcli)
	if _, err := fx.Fix(context.Background(), fixRequest()); err != nil {
		t.Fatal(err)
	}
	for _, n := range fixReferences {
		if !strings.Contains(fcli.inv.Prompt, refText(n)) {
			t.Errorf("fix prompt misses %s", n)
		}
	}
	if strings.Contains(fcli.inv.Prompt, refText("anti-ai-slop.md")) {
		t.Error("fix prompt holds a review-only reference")
	}
}
