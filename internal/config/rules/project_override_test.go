// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package rules

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProjectRuleReadsReferencedFiles(t *testing.T) {
	files := map[string]string{"docs/review.md": "base rule text"}
	var asked []string
	read := func(path string) ([]byte, bool, error) {
		asked = append(asked, path)
		v, ok := files[path]
		return []byte(v), ok, nil
	}
	raw := `{"rules":[
		{"path":"a/**","rule":"docs/review.md"},
		{"path":"b/**","rule":"./docs/../docs/review.md"},
		{"path":"c/**","rule":"docs/missing.md"},
		{"path":"d/**","rule":"../outside.md"},
		{"path":"e/**","rule":"/etc/rules.md"},
		{"path":"f/**","rule":"inline rule text"}
	],"exclude":["gen/**"]}`
	pr, err := ParseProjectRule([]byte(raw), read)
	if err != nil {
		t.Fatalf("ParseProjectRule: %v", err)
	}
	want := []string{"base rule text", "base rule text", "", "", "", "inline rule text"}
	for i, w := range want {
		if pr.Rules[i].Rule != w {
			t.Errorf("rule %d = %q, want %q", i, pr.Rules[i].Rule, w)
		}
	}
	if len(pr.Exclude) != 1 || pr.Exclude[0] != "gen/**" {
		t.Errorf("exclude = %v", pr.Exclude)
	}
	if strings.Join(asked, ",") != "docs/review.md,docs/review.md,docs/missing.md" {
		t.Errorf("read paths = %v; paths outside the repository must not be read", asked)
	}
}

func TestParseProjectRuleErrors(t *testing.T) {
	if _, err := ParseProjectRule([]byte(`{`), nil); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestParseProjectRuleBlanksOnlyUnreadableEntries(t *testing.T) {
	// Forges answer a file over the size limit with an error, like any other
	// failed read; the CLI also blanks such an entry and keeps the rest.
	read := func(path string) ([]byte, bool, error) {
		switch path {
		case "big.md":
			return nil, false, errors.New("file exceeds 524288 bytes")
		case "down.md":
			return nil, false, errors.New("503")
		}
		return []byte("ok rule"), true, nil
	}
	raw := `{"rules":[{"path":"a/**","rule":"big.md"},{"path":"b/**","rule":"down.md"},{"path":"c/**","rule":"ok.md"}],"exclude":["gen/**"]}`
	pr, err := ParseProjectRule([]byte(raw), read)
	if err != nil {
		t.Fatalf("ParseProjectRule: %v", err)
	}
	if pr.Rules[0].Rule != "" || pr.Rules[1].Rule != "" || pr.Rules[2].Rule != "ok rule" || len(pr.Exclude) != 1 {
		t.Fatalf("project rule %+v", pr)
	}
}

func TestNewResolverProjectOverrideIgnoresCheckout(t *testing.T) {
	setTestHome(t, t.TempDir())
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".opencodereview"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := `{"rules":[{"path":"**/*.go","rule":"head rule"}],"exclude":["**"]}`
	if err := os.WriteFile(filepath.Join(dir, ProjectRulePath), []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}

	resolver, filter, err := NewResolver(dir, "", ResolverOptions{OverrideProject: true})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if filter != nil || strings.Contains(resolver.Resolve("a.go"), "head rule") {
		t.Fatalf("checkout rule.json used despite the override: filter %+v", filter)
	}

	base := &ProjectRule{Rules: []ProjectRuleEntry{{Path: "**/*.go", Rule: "base rule"}}}
	resolver, filter, err = NewResolver(dir, "", ResolverOptions{OverrideProject: true, Project: base})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if filter != nil || resolver.Resolve("a.go") != "base rule" {
		t.Fatalf("override not used: filter %+v rule %q", filter, resolver.Resolve("a.go"))
	}

	_, filter, err = NewResolver(dir, "", ResolverOptions{})
	if err != nil || filter == nil || filter.Exclude[0] != "**" {
		t.Fatalf("without the override the checkout file must still load: %+v %v", filter, err)
	}
}
