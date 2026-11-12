// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package rules

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// ProjectRulePath is the repository-relative path of the project rule file.
const ProjectRulePath = ".opencodereview/rule.json"

// ParseProjectRule decodes a project rule file that was not read from the
// working tree, such as one read at a commit through a forge API. An entry
// that names a rule file is read with readFile, which gets the cleaned path
// relative to the repository root and reports false when the file does not
// exist; each path is read once. As with the working-tree loader, an entry
// whose file is missing, outside the repository, too large or unreadable gets
// an empty rule, and the other entries and the include and exclude lists stay
// in force. readFile reports its own errors.
func ParseProjectRule(data []byte, readFile func(path string) ([]byte, bool, error)) (*ProjectRule, error) {
	var pr ProjectRule
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, fmt.Errorf("unmarshal project rule: %w", err)
	}
	rulesByPath := map[string]string{}
	for i := range pr.Rules {
		e := &pr.Rules[i]
		if strings.TrimSpace(e.Rule) == "" || !looksLikeFilePath(e.Rule) {
			continue
		}
		rel, ok := repoRelativePath(e.Rule)
		if !ok {
			e.Rule = ""
			continue
		}
		rule, seen := rulesByPath[rel]
		if !seen {
			content, found, err := readFile(rel)
			if err == nil && found && len(content) <= MaxRuleFileBytes {
				rule = strings.TrimRight(string(content), "\n")
			}
			rulesByPath[rel] = rule
		}
		e.Rule = rule
	}
	return &pr, nil
}

// repoRelativePath cleans a rule file reference and refuses one that is
// absolute or leaves the repository root.
func repoRelativePath(ref string) (string, bool) {
	p := filepath.ToSlash(ref)
	if path.IsAbs(p) || filepath.IsAbs(ref) || filepath.VolumeName(ref) != "" {
		return "", false
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}
