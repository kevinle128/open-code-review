//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/agentcli"
	"github.com/alibaba/open-code-review/internal/serve/clireview"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

// writeFakeDocker writes a docker stand-in that logs its calls to
// rec/calls, lists rec/ps for `ps`, has every image, and runs the command
// of `docker run` with the env file in the -w directory.
func writeFakeDocker(t *testing.T, rec string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
rec=` + rec + `
echo "$*" >> $rec/calls
case "$1" in
ps) [ -f $rec/ps-fails ] && exit 1; cat $rec/ps 2>/dev/null; exit 0 ;;
run) ;;
*) exit 0 ;;
esac
printf '%s\n' "$@" > $rec/run-args
shift
while [ $# -gt 0 ]; do
  case "$1" in
  --rm|-i|--init) shift ;;
  --env-file) envf=$2; shift 2 ;;
  -w) wd=$2; shift 2 ;;
  -*) shift 2 ;;
  *) break ;;
  esac
done
shift
while IFS= read -r kv; do export "$kv"; done < "$envf"
cd "$wd" || exit 125
exec "$@"
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// sandboxEngineConfig is cliEngineConfig with the docker sandbox on and the
// codex binary reached through a link, as ~/.local/bin/codex is on a host.
func sandboxEngineConfig(t *testing.T, answer, rec string) *serveconfig.Config {
	t.Helper()
	cfg := cliEngineConfig(t, answer)
	link := filepath.Join(t.TempDir(), "codex")
	if err := os.Symlink(cfg.CLI.CodexBin, link); err != nil {
		t.Fatal(err)
	}
	cfg.CLI.CodexBin = link
	cfg.DataDir = t.TempDir()
	cfg.Sandbox = serveconfig.SandboxConfig{
		Mode: serveconfig.SandboxDocker, DockerBin: writeFakeDocker(t, rec), Memory: "6g", CPUs: "4",
		Images: serveconfig.DefaultSandboxImages,
	}
	return cfg
}

func TestBuildServeEnginesRunsTheReviewInTheSandbox(t *testing.T) {
	writeServeAppConfig(t, `{"language": "Vietnamese"}`)
	noLLMEnv(t)
	repo := newServeRepo(t)
	rec := t.TempDir()
	if err := os.WriteFile(filepath.Join(rec, "ps"), []byte("ocr-run-old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := sandboxEngineConfig(t, `{"summary": "ok", "findings": [
 {"path": "a.go", "line": 4, "end_line": null, "severity": "medium", "category": "bug",
  "title": "Bug", "body": "Fix it.", "suggestion": null}]}`, rec)

	rev, fixer, err := buildServeEngines(context.Background(), cfg, os.Getenv, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := readCalls(t, rec)
	if !strings.Contains(calls, "rm -f ocr-run-old\n") {
		t.Fatalf("leftover container not swept: %q", calls)
	}
	for _, l := range []string{"go", "npm", "nuget", "cargo", "pip"} {
		if st, err := os.Stat(filepath.Join(cfg.DataDir, "cache", l)); err != nil || !st.IsDir() {
			t.Fatalf("cache %s: %v", l, err)
		}
	}

	codex := rev.(*clireview.Reviewer).CLI.(agentcli.Codex)
	realBin, _ := filepath.EvalSymlinks(codex.Bin)
	if codex.Sandbox == nil || codex.Bin != realBin {
		t.Fatalf("codex %+v", codex)
	}
	authDir := filepath.Dir(cfg.CLI.CodexAuth)
	wantCodex := []agentcli.Mount{{Path: cfg.CLI.SkillDir, ReadOnly: true}, {Path: filepath.Dir(realBin), ReadOnly: true}, {Path: authDir}}
	if !mountsEqual(codex.Sandbox.Mounts, wantCodex) || codex.Sandbox.UID != os.Getuid() || codex.Sandbox.GID != os.Getgid() {
		t.Fatalf("codex sandbox %+v", codex.Sandbox)
	}
	claude := fixer.(*clireview.Fixer).CLI.(agentcli.Claude)
	if claude.Sandbox == nil || !mountsEqual(claude.Sandbox.Mounts[2:], []agentcli.Mount{{Path: cfg.CLI.ClaudeSettings, ReadOnly: true}}) {
		t.Fatalf("claude sandbox %+v", claude.Sandbox)
	}

	pr := forge.PR{Ref: forge.PRRef{Platform: forge.GitHub, Repo: "acme/app", Number: 3}, BaseSHA: repo.base, HeadSHA: repo.head}
	res, err := rev.Review(context.Background(), review.Request{RepoDir: repo.dir, From: repo.base, To: repo.head, PR: pr})
	if err != nil {
		t.Fatal(err)
	}
	if res.Terminal != review.TerminalComplete || len(res.Findings) != 1 {
		t.Fatalf("result %+v", res)
	}
	runArgs, err := os.ReadFile(filepath.Join(rec, "run-args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runArgs), "\n"+serveconfig.DefaultSandboxImages[serveconfig.ImageDefault]+"\n"+realBin+"\nexec\n") {
		t.Fatalf("run args %s", runArgs)
	}
	if entries, _ := os.ReadDir(cfg.CLI.Home); len(entries) != 0 {
		t.Fatalf("run dirs left: %v", entries)
	}
}

func TestBuildServeEnginesSandboxChecks(t *testing.T) {
	cases := []struct {
		name string
		edit func(*serveconfig.Config, string)
		want string
	}{
		{"docker missing", func(c *serveconfig.Config, _ string) { c.Sandbox.DockerBin = "/no/such/docker" }, serveconfig.EnvDockerBin},
		{"docker ps fails", func(_ *serveconfig.Config, rec string) {
			if err := os.WriteFile(filepath.Join(rec, "ps-fails"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, serveconfig.EnvSandbox + "=docker"},
		{"codex outside its install dir", func(c *serveconfig.Config, _ string) { c.Sandbox.CodexDir = t.TempDir() }, serveconfig.EnvSandboxCodexDir},
		{"claude outside its install dir", func(c *serveconfig.Config, _ string) { c.Sandbox.ClaudeDir = t.TempDir() }, "fix cli: " + serveconfig.EnvSandboxClaudeDir},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := t.TempDir()
			cfg := sandboxEngineConfig(t, "{}", rec)
			tc.edit(cfg, rec)
			_, _, err := buildServeEngines(context.Background(), cfg, os.Getenv, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestSandboxInstall(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "releases", "1.0", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "codex")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	realBin, _ := filepath.EvalSymlinks(bin)
	realDir, _ := filepath.EvalSymlinks(dir)
	got, install, err := sandboxInstall(link, dir)
	if err != nil || got != realBin || install != realDir {
		t.Fatalf("bin %q dir %q err %v", got, install, err)
	}
	if got, install, err = sandboxInstall(link, ""); err != nil || got != realBin || install != filepath.Dir(realBin) {
		t.Fatalf("default dir: bin %q dir %q err %v", got, install, err)
	}
	if _, _, err := sandboxInstall(filepath.Join(dir, "missing"), ""); err == nil {
		t.Fatal("a missing binary passed")
	}
	if _, _, err := sandboxInstall(link, filepath.Join(realDir, "releases", "2.0")); err == nil {
		t.Fatal("a binary outside the install dir passed")
	}
}

func TestSandboxName(t *testing.T) {
	cfg := &serveconfig.Config{Engine: serveconfig.EngineCLI, Sandbox: serveconfig.SandboxConfig{Mode: serveconfig.SandboxDocker}}
	if got := sandboxName(cfg); got != "docker" {
		t.Fatal(got)
	}
	cfg.Engine = serveconfig.EngineOCR
	if got := sandboxName(cfg); got != "none" {
		t.Fatal(got)
	}
	if got := sandboxName(&serveconfig.Config{Engine: serveconfig.EngineCLI}); got != "none" {
		t.Fatal(got)
	}
}

func readCalls(t *testing.T, rec string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rec, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mountsEqual(a, b []agentcli.Mount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
