// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"strings"
	"testing"
)

func TestLoad_CLIEngineSettings(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvEngine:           "OCR",
		EnvReviewCLI:        "claude",
		EnvVerifyCLI:        "codex",
		EnvCodexBin:         "/opt/codex/bin/codex",
		EnvClaudeBin:        "/opt/claude/versions/2.1.322",
		EnvCodexModel:       "gpt-6.1-sol",
		EnvClaudeModel:      "claude-sonnet-5-5[1m]",
		EnvCodexEffort:      "medium",
		EnvSkillDir:         "/opt/ocr-skills/",
		EnvCLIHome:          "/state/cli",
		EnvCodexAuth:        "/run/codex/auth.json",
		EnvClaudeSettings:   "/run/claude/settings.json",
		EnvBatchLines:       "800",
		EnvBatchFiles:       "10",
		EnvBatchConcurrency: "4",
	}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := CLIConfig{
		ReviewCLI: CLIClaude,
		CodexBin:  "/opt/codex/bin/codex", ClaudeBin: "/opt/claude/versions/2.1.322",
		CodexModel: "gpt-6.1-sol", ClaudeModel: "claude-sonnet-5-5[1m]", CodexEffort: "medium",
		SkillDir: "/opt/ocr-skills", Home: "/state/cli",
		CodexAuth: "/run/codex/auth.json", ClaudeSettings: "/run/claude/settings.json", AgyBin: DefaultAgyBin,
		BatchLines: 800, BatchFiles: 10, BatchConcurrency: 4,
	}
	if c.Engine != EngineOCR || c.CLI != want || c.Verify.CLI != CLICodex {
		t.Fatalf("engine %q cli %+v", c.Engine, c.CLI)
	}
	if c.CLI.Bin(CLIClaude) != want.ClaudeBin || c.CLI.Bin(CLICodex) != want.CodexBin {
		t.Fatal("Bin picks the wrong executable")
	}
}

func TestLoad_AgySettings(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvReviewCLI: "AGY",
		EnvAgyBin:    "/home/svc/.local/bin/agy",
		EnvAgyToken:  "/home/svc/.gemini/antigravity-cli/antigravity-oauth-token",
		EnvAgyModel:  "gemini-4-hydrogen-high",
		EnvAgyEffort: "xhigh",
	}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.CLI.ReviewCLI != CLIAgy || c.Verify.CLI != DefaultVerifyCLI || c.CLI.AgyBin != "/home/svc/.local/bin/agy" ||
		c.CLI.AgyToken != "/home/svc/.gemini/antigravity-cli/antigravity-oauth-token" ||
		c.CLI.AgyModel != "gemini-4-hydrogen-high" || c.CLI.AgyEffort != "xhigh" {
		t.Fatalf("cli %+v", c.CLI)
	}
	if c.CLI.Bin(CLIAgy) != c.CLI.AgyBin {
		t.Fatal("Bin picks the wrong executable")
	}
	c, err = Load(env(nil), files(nil))
	if err != nil || c.CLI.AgyBin != DefaultAgyBin || c.CLI.AgyEffort != "" || c.CLI.AgyModel != "" {
		t.Fatalf("agy defaults %+v %v", c.CLI, err)
	}
}

func TestLoad_AgyIsReviewOnly(t *testing.T) {
	_, err := Load(env(map[string]string{EnvVerifyCLI: "agy"}), files(nil))
	if err == nil || !strings.Contains(err.Error(), EnvVerifyCLI+"=agy is not supported: agy runs reviews only") {
		t.Fatalf("err %v", err)
	}
}

func TestLoad_CLIHomeFollowsDataDir(t *testing.T) {
	c, err := Load(env(map[string]string{EnvDataDir: "/srv/ocr"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.CLI.Home != "/srv/ocr/cli-home" {
		t.Fatalf("home %q", c.CLI.Home)
	}
	if c.CLI.BatchLines != DefaultBatchLines || c.CLI.BatchFiles != DefaultBatchFiles || c.CLI.BatchConcurrency != DefaultBatchConcurrency {
		t.Fatalf("batch defaults %+v", c.CLI)
	}
}

func TestLoad_InvalidCLIEngineSettings(t *testing.T) {
	cases := map[string]string{
		EnvEngine:           "claude",
		EnvReviewCLI:        "gemini",
		EnvVerifyCLI:        "aider",
		EnvCodexModel:       `gpt"; rm -rf`,
		EnvClaudeModel:      "--dangerously-skip-permissions",
		EnvCodexEffort:      "Medium high",
		EnvSkillDir:         "skills",
		EnvCLIHome:          "./home",
		EnvCodexAuth:        "auth.json",
		EnvClaudeSettings:   "~/.claude/settings.json",
		EnvAgyToken:         "token",
		EnvAgyModel:         "--sandbox",
		EnvAgyEffort:        "ultra",
		EnvBatchLines:       "99",
		EnvBatchFiles:       "0",
		EnvBatchConcurrency: "5",
	}
	for key, val := range cases {
		t.Run(key, func(t *testing.T) {
			_, err := Load(env(map[string]string{key: val}), files(nil))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestRequireServe_CLIEngine(t *testing.T) {
	gh := &GitHubConfig{AppID: 1}
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"ocr needs nothing", Config{GitHub: gh, Engine: EngineOCR}, nil},
		{"codex needs skill dir and auth", Config{GitHub: gh, Engine: EngineCLI, CLI: CLIConfig{ReviewCLI: CLICodex}},
			[]string{EnvSkillDir, EnvCodexAuth}},
		{"claude needs settings", Config{GitHub: gh, Engine: EngineCLI, CLI: CLIConfig{ReviewCLI: CLIClaude, SkillDir: "/s"}},
			[]string{EnvClaudeSettings}},
		{"codex complete", Config{GitHub: gh, Engine: EngineCLI,
			CLI: CLIConfig{ReviewCLI: CLICodex, SkillDir: "/s", CodexAuth: "/a"}}, nil},
		{"claude complete", Config{GitHub: gh, Engine: EngineCLI,
			CLI: CLIConfig{ReviewCLI: CLIClaude, SkillDir: "/s", ClaudeSettings: "/c"}}, nil},
		{"claude fix needs settings", Config{GitHub: gh, Engine: EngineCLI,
			CLI: CLIConfig{ReviewCLI: CLICodex, SkillDir: "/s", CodexAuth: "/a"}, Verify: VerifyConfig{CLI: CLIClaude}},
			[]string{EnvClaudeSettings + " is required when " + EnvVerifyCLI}},
		{"codex fix needs auth", Config{GitHub: gh, Engine: EngineCLI,
			CLI: CLIConfig{ReviewCLI: CLIClaude, SkillDir: "/s", ClaudeSettings: "/c"}, Verify: VerifyConfig{CLI: CLICodex}},
			[]string{EnvCodexAuth + " is required when " + EnvVerifyCLI}},
		{"agy needs the token", Config{GitHub: gh, Engine: EngineCLI,
			CLI: CLIConfig{ReviewCLI: CLIAgy, SkillDir: "/s", ClaudeSettings: "/c"}, Verify: VerifyConfig{CLI: CLIClaude}},
			[]string{EnvAgyToken + " or " + EnvAgyGatewayURL + " is required when " + EnvReviewCLI}},
		{"agy complete", Config{GitHub: gh, Engine: EngineCLI,
			CLI: CLIConfig{ReviewCLI: CLIAgy, SkillDir: "/s", ClaudeSettings: "/c", AgyToken: "/t"}, Verify: VerifyConfig{CLI: CLIClaude}}, nil},
		{"both complete", Config{GitHub: gh, Engine: EngineCLI,
			CLI: CLIConfig{ReviewCLI: CLICodex, SkillDir: "/s", CodexAuth: "/a", ClaudeSettings: "/c"}, Verify: VerifyConfig{CLI: CLIClaude}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.RequireServe()
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err %v misses %s", err, w)
				}
			}
		})
	}
}

func TestLoad_AgyGatewayURL(t *testing.T) {
	const gw = "https://gw.example/v1/secret-key"
	c, err := Load(env(map[string]string{EnvReviewCLI: "agy", EnvAgyGatewayURL: gw}), files(nil))
	if err != nil || c.CLI.AgyGatewayURL != gw {
		t.Fatalf("gateway %q %v", c.CLI.AgyGatewayURL, err)
	}
	if c, err = Load(env(nil), files(nil)); err != nil || c.CLI.AgyGatewayURL != "" {
		t.Fatalf("default %q %v", c.CLI.AgyGatewayURL, err)
	}
	for _, bad := range []string{"gw.example/secret-key", "ftp://gw.example/secret-key", "https:///secret-key", "http://%zz-secret-key"} {
		_, err := Load(env(map[string]string{EnvAgyGatewayURL: bad}), files(nil))
		if err == nil || !strings.Contains(err.Error(), EnvAgyGatewayURL) || strings.Contains(err.Error(), "secret-key") {
			t.Fatalf("value %q: err %v", bad, err)
		}
	}
}

func TestRequireServe_AgyGatewayReplacesToken(t *testing.T) {
	gh := &GitHubConfig{AppID: 1}
	cfg := Config{GitHub: gh, Engine: EngineCLI, CLI: CLIConfig{ReviewCLI: CLIAgy, SkillDir: "/s", CodexAuth: "/a", ClaudeSettings: "/c"},
		Verify: VerifyConfig{CLI: CLICodex}}
	err := cfg.RequireServe()
	if err == nil || !strings.Contains(err.Error(), EnvAgyToken) || !strings.Contains(err.Error(), EnvAgyGatewayURL) {
		t.Fatalf("err %v", err)
	}
	cfg.CLI.AgyGatewayURL = "https://gw.example/k"
	if err := cfg.RequireServe(); err != nil {
		t.Fatal(err)
	}
}
