// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"slices"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/rules"
)

// The defaults are applied as review exclude globs, so they are checked
// through the same matcher.
func TestDefaultExcludes(t *testing.T) {
	f := rules.FileFilter{Exclude: DefaultExcludes}
	skipped := []string{
		".github/workflows/ci.yml", "a/.vscode/x.json", ".claude/skills/s.md",
		".cursor/rules/a.mdc", "web/.agents/a.md", ".agentkit/x.sh", ".omp/x", ".mcp.json",
		"_bmad-output/sprint-status.yaml", "_bmad-output/stories/a.html", "app/claude-design/page.html",
		"claude-design/support.js", "plans/a/plan.md", "third_party/lib/a.c", "docs/api/index.html",
		"yarn.lock", "web/yarn.lock", "go.sum", "Cargo.lock", "composer.lock", "poetry.lock",
		"dist/app.js.map", "web/locales/vi/app.json", "src/i18n/en/messages.json",
		"web/index.html", "web/page.HTM", "web/site.css",
	}
	for _, p := range skipped {
		if !f.IsUserExcluded(p) {
			t.Errorf("%s: want skipped", p)
		}
	}
	reviewed := []string{
		".eslintrc.js", "src/.hidden.go", "src/main.go", "pkg/foo_test.go", "web/app.ts",
		"web/locales/vi/index.ts", "config/app.json", "docs/guide.md", "deploy/compose.yml",
	}
	for _, p := range reviewed {
		if f.IsUserExcluded(p) {
			t.Errorf("%s: want reviewed", p)
		}
	}
}

func TestLoad_ExcludeAppendsToDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{EnvExclude: " gen/** , ,*.sql"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(DefaultExcludes), "gen/**", "*.sql")
	if !slices.Equal(c.Exclude, want) {
		t.Fatalf("exclude = %v", c.Exclude)
	}
	repo := c.RepoDefaults()
	repo.Exclude[0] = "changed"
	if c.Exclude[0] == "changed" || DefaultExcludes[0] == "changed" {
		t.Fatal("RepoDefaults must return a copy")
	}
}
