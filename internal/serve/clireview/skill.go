// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package clireview

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The reference files of the review skill that go into the prompt, in this
// order, relative to the skill's references/ directory. An agent told to
// open them on its own does not, so their text is part of the prompt.
var (
	reviewReferences = []string{
		"review-workflow.md",
		"review-checklist.md",
		"anti-ai-slop.md",
		"pr-body-contract.md",
		"writing-language.md",
	}
	fixReferences = []string{
		"mode-fix.md",
		"review-checklist.md",
	}
)

// skillText is the skill prompt with its reference files appended.
type skillText struct {
	Dir      string
	Text     string
	Warnings []string
}

// loadSkill reads SKILL.md and appends each named reference under a heading
// with its file name. SKILL.md must exist; a missing reference is left out
// with a warning, because the rest of the skill still gives a usable review.
func loadSkill(root string, refs []string) (skillText, error) {
	main := filepath.Join(root, SkillFile)
	out := skillText{Dir: filepath.Dir(main)}
	raw, err := os.ReadFile(main)
	if err != nil {
		return out, fmt.Errorf("read review skill: %w", err)
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(string(raw)))
	for _, name := range refs {
		ref, err := os.ReadFile(filepath.Join(out.Dir, "references", name))
		if err != nil {
			reason := err.Error()
			if errors.Is(err, fs.ErrNotExist) {
				reason = "missing"
			}
			out.Warnings = append(out.Warnings, fmt.Sprintf("skill reference %s left out of the prompt: %s", name, reason))
			continue
		}
		fmt.Fprintf(&b, "\n\n---\n\n# Reference: references/%s\n\n%s", name, strings.TrimSpace(string(ref)))
	}
	out.Text = b.String()
	return out, nil
}
