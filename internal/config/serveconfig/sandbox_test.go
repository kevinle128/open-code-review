// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoad_SandboxSettings(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvSandbox:                          "Docker",
		EnvDockerBin:                        "/usr/local/bin/docker",
		EnvSandboxMemory:                    "8G",
		EnvSandboxCPUs:                      "2.5",
		EnvSandboxCodexDir:                  "/opt/codex/",
		EnvSandboxClaudeDir:                 "/opt/claude",
		EnvSandboxImagePrefix + ImageGo:     "golang:1.26-bookworm",
		EnvSandboxImagePrefix + ImageNode:   "registry.example.com/node@sha256:abc",
		EnvSandboxImagePrefix + ImageRust:   "",
		EnvSandboxImagePrefix + ImageDotnet: "mcr.microsoft.com/dotnet/sdk:10.0",
	}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := SandboxConfig{
		Mode: SandboxDocker, DockerBin: "/usr/local/bin/docker", Memory: "8G", CPUs: "2.5",
		CodexDir: "/opt/codex", ClaudeDir: "/opt/claude",
		Images: map[string]string{
			ImageGo:      "golang:1.26-bookworm",
			ImageDotnet:  "mcr.microsoft.com/dotnet/sdk:10.0",
			ImageRust:    DefaultSandboxImages[ImageRust],
			ImageNode:    "registry.example.com/node@sha256:abc",
			ImagePython:  DefaultSandboxImages[ImagePython],
			ImageDefault: DefaultSandboxImages[ImageDefault],
		},
	}
	if !reflect.DeepEqual(c.Sandbox, want) {
		t.Fatalf("sandbox %+v", c.Sandbox)
	}
}

func TestLoad_InvalidSandboxSettings(t *testing.T) {
	cases := map[string]string{
		EnvSandbox:                          "podman",
		EnvSandboxMemory:                    "6 GB",
		EnvSandboxCPUs:                      "--privileged",
		EnvSandboxCodexDir:                  "relative/codex",
		EnvSandboxImagePrefix + ImagePython: "-v /:/host",
	}
	for key, val := range cases {
		t.Run(key, func(t *testing.T) {
			_, err := Load(env(map[string]string{key: val}), files(nil))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestLoad_DirsDefaultUnderHome(t *testing.T) {
	c, err := Load(env(map[string]string{"HOME": "/home/svc"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/home/svc/ocr-review/data" || c.WorkDir != "/home/svc/ocr-review/work" ||
		c.CLI.Home != "/home/svc/ocr-review/data/cli-home" {
		t.Fatalf("data %q work %q cli home %q", c.DataDir, c.WorkDir, c.CLI.Home)
	}
	c, err = Load(env(map[string]string{"HOME": "relative"}), files(nil))
	if err != nil || c.DataDir != DefaultDataDir || c.WorkDir != DefaultWorkDir {
		t.Fatalf("relative HOME: %+v %v", c, err)
	}
}

func TestLoad_RelativeDirsBecomeAbsolute(t *testing.T) {
	c, err := Load(env(map[string]string{EnvDataDir: "state", EnvWorkDir: "./slots"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.DataDir) || filepath.Base(c.DataDir) != "state" ||
		!filepath.IsAbs(c.WorkDir) || filepath.Base(c.WorkDir) != "slots" {
		t.Fatalf("data %q work %q", c.DataDir, c.WorkDir)
	}
}

func TestRequireServe_SandboxRejectsCommaPaths(t *testing.T) {
	base := map[string]string{
		EnvGitLabURL: "https://git.example.com", EnvGitLabToken: "t", EnvGitLabWebhookSecret: "s",
		EnvSkillDir: "/skills", EnvCodexAuth: "/codex/auth.json", EnvClaudeSettings: "/claude/settings.json",
		EnvSandbox: SandboxDocker, EnvDataDir: "/srv/a,b",
	}
	c, err := Load(env(base), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RequireServe(); err == nil || !strings.Contains(err.Error(), EnvDataDir) {
		t.Fatalf("err %v", err)
	}
	base[EnvSandbox] = SandboxNone
	if c, err = Load(env(base), files(nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.RequireServe(); err != nil {
		t.Fatalf("none sandbox: %v", err)
	}
	base[EnvSandbox], base[EnvEngine] = SandboxDocker, EngineOCR
	if c, err = Load(env(base), files(nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.RequireServe(); err != nil {
		t.Fatalf("ocr engine: %v", err)
	}
}

func TestSandboxAddHosts(t *testing.T) {
	l := &loader{getenv: func(k string) string {
		if k == EnvSandboxAddHosts {
			return "vtc.example.ts.net:100.88.18.71, v6.example:fd7a::1"
		}
		return ""
	}}
	s := l.sandbox()
	if len(l.errs) != 0 {
		t.Fatalf("errors: %v", l.errs)
	}
	want := []string{"vtc.example.ts.net:100.88.18.71", "v6.example:fd7a::1"}
	if strings.Join(s.AddHosts, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v", s.AddHosts)
	}
	bad := &loader{getenv: func(k string) string {
		if k == EnvSandboxAddHosts {
			return "--privileged"
		}
		return ""
	}}
	if bad.sandbox(); len(bad.errs) == 0 {
		t.Fatal("want an error for an invalid entry")
	}
}
