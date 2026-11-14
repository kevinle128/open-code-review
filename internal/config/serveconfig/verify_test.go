// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestLoad_VerifyDefaults(t *testing.T) {
	c, err := Load(env(nil), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Verify.CLI != VerifyNone || c.Verify.Model != "" || !slices.Equal(c.Verify.Post, []string{VerdictReal}) {
		t.Fatalf("verify %+v", c.Verify)
	}
	if c.Verify.Enabled() || (VerifyConfig{}).Enabled() {
		t.Fatal("verify is on by default")
	}
}

func TestLoad_VerifySettings(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvVerifyCLI:   "Claude",
		EnvVerifyModel: "opus",
		EnvVerifyPost:  " REAL, minor,real ,out_of_scope",
	}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{VerdictReal, VerdictMinor, VerdictOutOfScope}
	if c.Verify.CLI != CLIClaude || c.Verify.Model != "opus" || !slices.Equal(c.Verify.Post, want) || !c.Verify.Enabled() {
		t.Fatalf("verify %+v", c.Verify)
	}
	c, err = Load(env(map[string]string{EnvVerifyCLI: "codex", EnvVerifyPost: " , "}), files(nil))
	if err != nil || c.Verify.CLI != CLICodex || !slices.Equal(c.Verify.Post, DefaultVerifyPost) {
		t.Fatalf("verify %+v %v", c.Verify, err)
	}
}

func TestLoad_VerifyRejectsBadValues(t *testing.T) {
	cases := map[string]struct{ key, val, want string }{
		"agy":       {EnvVerifyCLI, "agy", EnvVerifyCLI + "=agy is not supported: agy runs reviews only"},
		"unknown":   {EnvVerifyCLI, "gemini", EnvVerifyCLI + " must be one of none, claude, codex"},
		"model":     {EnvVerifyModel, "--dangerously-skip-permissions", EnvVerifyModel + " has an invalid value"},
		"verdict":   {EnvVerifyPost, "real,maybe", EnvVerifyPost + ` has an invalid verdict "maybe"`},
		"uppercase": {EnvVerifyPost, "REAL,BOGUS", `invalid verdict "bogus"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{tc.key: tc.val}), files(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRequireServe_VerifyCLICredentials(t *testing.T) {
	gh := &GitHubConfig{AppID: 1}
	agyReview := CLIConfig{ReviewCLI: CLIAgy, FixCLI: CLIClaude, SkillDir: "/s", AgyToken: "/t"}
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
	if err == nil || !strings.Contains(err.Error(), EnvClaudeSettings+" is required when "+EnvFixCLI) {
		t.Fatalf("err %v", err)
	}
}
