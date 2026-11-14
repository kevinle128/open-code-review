//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/agentcli"
	"github.com/alibaba/open-code-review/internal/serve/clireview"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

// noLLMEnv clears the OCR LLM settings: the CLI engine must not need them.
func noLLMEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"OCR_LLM_URL", "OCR_LLM_TOKEN", "OCR_LLM_MODEL", "OCR_LLM_PROTOCOL"} {
		t.Setenv(k, "")
	}
}

func commitFiles(t *testing.T, dir string, files map[string]string, msg string) string {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	retryTestGit(t, dir, "add", ".")
	retryTestGit(t, dir, "commit", "-q", "-m", msg)
	return serveGitOut(t, dir, "rev-parse", "HEAD")
}

func testSelector(t *testing.T) serveSelector {
	t.Helper()
	return serveSelector{ocr: testServeReviewer(t)}
}

func TestServeSelectorAppliesTheServeFilters(t *testing.T) {
	writeServeAppConfig(t, `{"language": "Vietnamese"}`)
	noLLMEnv(t)
	repo := newServeRepo(t)
	big := strings.Repeat("  \"k\": 1,\n", serveconfig.MaxDataChangedLines+1)
	head := commitFiles(t, repo.dir, map[string]string{
		"a_test.go":         "package p\n\nfunc TestA() {}\n",
		"pkg/testdata/x.go": "package x\n",
		"data/big.json":     "{\n" + big + "}\n",
		"docs/notes.go":     "package docs\n",
		"node_modules/x.js": "x\n",
	}, "more files")

	sel, err := testSelector(t).Select(context.Background(), review.Request{
		RepoDir: repo.dir, From: repo.base, To: head, Excludes: []string{"docs/**"}, Background: "Payments.",
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(sel.Paths)
	if strings.Join(sel.Paths, ",") != "a.go,a_test.go" {
		t.Fatalf("paths %v", sel.Paths)
	}
	if sel.Skipped["large_data"] != 1 || sel.Skipped["default_path"] != 1 || sel.Skipped["user_exclude"] != 1 ||
		sel.Skipped["provider_directory"] != 0 {
		t.Fatalf("skipped %v", sel.Skipped)
	}
	if sel.Base != repo.base || sel.Head != head || sel.Language != "Vietnamese" ||
		!strings.HasPrefix(sel.Background, "Payments.") || !strings.Contains(sel.Background, "in Vietnamese") {
		t.Fatalf("selection %+v", sel)
	}
}

func TestServeSelectorUsesMergeBaseAndCommitParent(t *testing.T) {
	writeServeAppConfig(t, `{"language": "English"}`)
	noLLMEnv(t)
	repo := newServeRepo(t)
	retryTestGit(t, repo.dir, "checkout", "-q", "-b", "feature", repo.head)
	retryTestGit(t, repo.dir, "checkout", "-q", "main")
	retryTestGit(t, repo.dir, "reset", "-q", "--hard", repo.base)
	tip := commitFiles(t, repo.dir, map[string]string{"b.go": "package p\n"}, "main moves on")
	retryTestGit(t, repo.dir, "checkout", "-q", "feature")

	sel, err := testSelector(t).Select(context.Background(), review.Request{RepoDir: repo.dir, From: tip, To: repo.head})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Base != repo.base || strings.Join(sel.Paths, ",") != "a.go" || sel.Language != "English" || sel.Background != "" {
		t.Fatalf("range selection %+v", sel)
	}
	sel, err = testSelector(t).Select(context.Background(), review.Request{RepoDir: repo.dir, Commit: repo.head})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Base != repo.base || sel.Head != repo.head {
		t.Fatalf("commit selection %+v", sel)
	}
}

func TestServeSelectorErrors(t *testing.T) {
	writeServeAppConfig(t, `{"language": "Vietnamese"}`)
	noLLMEnv(t)
	repo := newServeRepo(t)
	if _, err := testSelector(t).Select(context.Background(), review.Request{RepoDir: repo.dir}); err == nil {
		t.Fatal("a request without a range was accepted")
	}
	if _, err := testSelector(t).Select(context.Background(), review.Request{
		RepoDir: repo.dir, From: strings.Repeat("1", 40), To: repo.head,
	}); err == nil {
		t.Fatal("an unknown base was accepted")
	}
	retryTestGit(t, repo.dir, "checkout", "-q", "--orphan", "unrelated")
	orphan := commitFiles(t, repo.dir, map[string]string{"z.go": "package z\n"}, "unrelated")
	_, err := testSelector(t).Select(context.Background(), review.Request{RepoDir: repo.dir, From: orphan, To: repo.head})
	if err == nil || !strings.Contains(err.Error(), "resolve the review range") {
		t.Fatalf("unrelated histories: %v", err)
	}
}

func TestServeSelectorDefaultsTheLanguage(t *testing.T) {
	writeServeAppConfig(t, `{}`)
	noLLMEnv(t)
	repo := newServeRepo(t)
	sel, err := testSelector(t).Select(context.Background(), review.Request{RepoDir: repo.dir, From: repo.base, To: repo.head})
	if err != nil || sel.Language != defaultServeLanguage {
		t.Fatalf("selection %+v err %v", sel, err)
	}
}

// cliEngineConfig is a CLI engine configuration with a fake codex that
// answers with the content of answer.
func cliEngineConfig(t *testing.T, answer string) *serveconfig.Config {
	t.Helper()
	dir := t.TempDir()
	answerFile := filepath.Join(dir, "answer.json")
	if err := os.WriteFile(answerFile, []byte(answer), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\ncat >/dev/null\nout=\"\"; prev=\"\"\n" +
		"for a in \"$@\"; do if [ \"$prev\" = \"-o\" ]; then out=\"$a\"; fi; prev=\"$a\"; done\n" +
		"cp " + answerFile + " \"$out\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(dir, "skills-root")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(skillDir, clireview.SkillFile)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, clireview.SkillFile), []byte("# skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(dir, "auth.json")
	settings := filepath.Join(dir, "settings.json")
	for _, f := range []string{auth, settings} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &serveconfig.Config{
		Engine: serveconfig.EngineCLI, ReviewConcurrency: 1, BundleLines: 300,
		CLI: serveconfig.CLIConfig{
			ReviewCLI: serveconfig.CLICodex, FixCLI: serveconfig.CLIClaude, CodexBin: bin, ClaudeBin: bin, SkillDir: skillDir,
			Home: filepath.Join(dir, "cli-home"), CodexAuth: auth, ClaudeSettings: settings,
		},
	}
}

func TestBuildServeReviewerRunsTheCLIEngine(t *testing.T) {
	writeServeAppConfig(t, `{"language": "Vietnamese"}`)
	noLLMEnv(t)
	repo := newServeRepo(t)
	cfg := cliEngineConfig(t, `{"summary": "ok", "findings": [
 {"path": "a.go", "line": 4, "end_line": null, "severity": "medium", "category": "maintainability",
  "title": "Magic number", "body": "Name the constant.", "suggestion": null}]}`)
	stale := filepath.Join(cfg.CLI.Home, "run-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}

	rev, fixer, err := buildServeEngines(context.Background(), cfg, os.Getenv, nil)
	if err != nil {
		t.Fatal(err)
	}
	fx, ok := fixer.(*clireview.Fixer)
	if !ok || fx.Language != "Vietnamese" || fx.SkillDir != cfg.CLI.SkillDir {
		t.Fatalf("fixer %+v", fixer)
	}
	if c, ok := fx.CLI.(agentcli.Claude); !ok || c.SettingsFile != cfg.CLI.ClaudeSettings {
		t.Fatalf("fix cli %+v", fx.CLI)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale run dir not swept")
	}
	pr := forge.PR{Ref: forge.PRRef{Platform: forge.GitHub, Repo: "acme/app", Number: 3}, BaseSHA: repo.base, HeadSHA: repo.head}
	res, err := rev.Review(context.Background(), review.Request{RepoDir: repo.dir, From: repo.base, To: repo.head, PR: pr})
	if err != nil {
		t.Fatal(err)
	}
	if res.Terminal != review.TerminalComplete || res.Base != repo.base || res.Coverage.Total != 1 || res.Coverage.Reviewed != 1 ||
		len(res.Findings) != 1 || res.Findings[0].StartLine != 4 || res.Findings[0].Category != "maintainability" {
		t.Fatalf("result %+v", res)
	}

	res, err = rev.Review(context.Background(), review.Request{RepoDir: repo.dir, From: repo.base, To: repo.head,
		Excludes: []string{"*.go"}, PR: pr})
	if err != nil || res.Terminal != review.TerminalSkipped || res.Coverage.Skipped["user_exclude"] != 1 {
		t.Fatalf("excluded change: %+v %v", res, err)
	}
	if entries, _ := os.ReadDir(cfg.CLI.Home); len(entries) != 0 {
		t.Fatalf("run dirs left: %v", entries)
	}
}

func TestBuildServeReviewerPicksTheEngine(t *testing.T) {
	cfg := cliEngineConfig(t, "{}")
	cfg.Engine = serveconfig.EngineOCR
	rev, fixer, err := buildServeEngines(context.Background(), cfg, os.Getenv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rev.(serveReviewer); !ok || fixer != nil {
		t.Fatalf("ocr engine built %T and fixer %v", rev, fixer)
	}

	cfg = cliEngineConfig(t, "{}")
	cfg.CLI.ReviewCLI = serveconfig.CLIClaude
	cfg.CLI.ClaudeModel = "sonnet"
	cfg.CLI.FixCLI = serveconfig.CLICodex
	cfg.CLI.CodexEffort = "high"
	rev, fixer, err = buildServeEngines(context.Background(), cfg, os.Getenv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := fixer.(*clireview.Fixer).CLI.(agentcli.Codex); !ok || c.Effort != "high" || c.AuthFile != cfg.CLI.CodexAuth {
		t.Fatalf("codex fix cli %+v", fixer.(*clireview.Fixer).CLI)
	}
	cr, ok := rev.(*clireview.Reviewer)
	if !ok {
		t.Fatalf("cli engine built %T", rev)
	}
	if c, ok := cr.CLI.(agentcli.Claude); !ok || c.Model != "sonnet" || c.SettingsFile != cfg.CLI.ClaudeSettings {
		t.Fatalf("claude cli %+v", cr.CLI)
	}
}

func TestBuildServeReviewerChecks(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		edit func(*serveconfig.Config)
		want string
	}{
		{"codex missing", func(c *serveconfig.Config) { c.CLI.CodexBin = "/no/such/codex" }, serveconfig.EnvCodexBin},
		{"claude missing", func(c *serveconfig.Config) {
			c.CLI.ReviewCLI = serveconfig.CLIClaude
			c.CLI.ClaudeBin = "no-such-claude-binary"
		}, serveconfig.EnvClaudeBin},
		{"skill missing", func(c *serveconfig.Config) { c.CLI.SkillDir = t.TempDir() }, serveconfig.EnvSkillDir + ": review skill"},
		{"codex auth missing", func(c *serveconfig.Config) { c.CLI.CodexAuth = "/no/auth.json" }, serveconfig.EnvCodexAuth},
		{"codex auth is a dir", func(c *serveconfig.Config) { c.CLI.CodexAuth = t.TempDir() }, "is a directory"},
		{"claude settings missing", func(c *serveconfig.Config) {
			c.CLI.ReviewCLI = serveconfig.CLIClaude
			c.CLI.ClaudeSettings = "/no/settings.json"
		}, serveconfig.EnvClaudeSettings},
		{"home unusable", func(c *serveconfig.Config) { c.CLI.Home = filepath.Join(notADir, "home") }, serveconfig.EnvCLIHome},
		{"fix claude missing", func(c *serveconfig.Config) { c.CLI.ClaudeBin = "no-such-claude-binary" }, "fix cli: " + serveconfig.EnvClaudeBin},
		{"fix claude settings missing", func(c *serveconfig.Config) { c.CLI.ClaudeSettings = "/no/settings.json" }, "fix cli: " + serveconfig.EnvClaudeSettings},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cliEngineConfig(t, "{}")
			tc.edit(cfg)
			_, _, err := buildServeEngines(context.Background(), cfg, os.Getenv, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestEngineName(t *testing.T) {
	if got := engineName(&serveconfig.Config{Engine: serveconfig.EngineOCR}); got != "ocr" {
		t.Fatal(got)
	}
	cfg := &serveconfig.Config{Engine: serveconfig.EngineCLI, CLI: serveconfig.CLIConfig{ReviewCLI: serveconfig.CLIClaude}}
	if got := engineName(cfg); got != "cli/claude" {
		t.Fatal(got)
	}
	cfg.CLI.FixCLI = serveconfig.CLICodex
	if got := fixCLIName(cfg); got != "codex" {
		t.Fatal(got)
	}
	if got := fixCLIName(&serveconfig.Config{Engine: serveconfig.EngineOCR}); got != "none" {
		t.Fatal(got)
	}
}
