// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"strings"
	"testing"
)

func TestLoad_VerifyDefaults(t *testing.T) {
	c, err := Load(env(nil), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Verify != (VerifyConfig{CLI: DefaultVerifyCLI}) {
		t.Fatalf("verify %+v", c.Verify)
	}
	c, err = Load(env(map[string]string{EnvVerifyCLI: " Codex ", EnvVerifyModel: "opus"}), files(nil))
	if err != nil || c.Verify != (VerifyConfig{CLI: CLICodex, Model: "opus"}) {
		t.Fatalf("verify %+v err %v", c.Verify, err)
	}
}

func TestLoad_VerifyInvalid(t *testing.T) {
	cases := map[string]struct{ key, value, want string }{
		"none":  {EnvVerifyCLI, "none", EnvVerifyCLI + "=none is not supported"},
		"agy":   {EnvVerifyCLI, "agy", EnvVerifyCLI + "=agy is not supported"},
		"other": {EnvVerifyCLI, "aider", EnvVerifyCLI + " must be one of"},
		"model": {EnvVerifyModel, "--yolo", EnvVerifyModel + " has an invalid value"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{tc.key: tc.value}), files(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRequireServe_VerifyCLICredentials(t *testing.T) {
	gh := &GitHubConfig{AppID: 1}
	agyReview := CLIConfig{ReviewCLI: CLIAgy, SkillDir: "/s", AgyToken: "/t"}
	cfg := Config{GitHub: gh, Engine: EngineCLI, CLI: agyReview, Verify: VerifyConfig{CLI: CLICodex}}
	err := cfg.RequireServe()
	if err == nil || !strings.Contains(err.Error(), EnvCodexAuth+" is required when "+EnvVerifyCLI) {
		t.Fatalf("err %v", err)
	}
	cfg.CLI.CodexAuth, cfg.CLI.ClaudeSettings = "/a", "/c"
	if err := cfg.RequireServe(); err != nil {
		t.Fatal(err)
	}
	cfg = Config{GitHub: gh, Engine: EngineCLI, CLI: agyReview, Verify: VerifyConfig{CLI: CLIClaude}}
	cfg.CLI.ClaudeSettings = ""
	err = cfg.RequireServe()
	if err == nil || !strings.Contains(err.Error(), EnvClaudeSettings+" is required when "+EnvVerifyCLI) {
		t.Fatalf("err %v", err)
	}
}
