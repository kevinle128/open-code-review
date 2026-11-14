// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alibaba/open-code-review/internal/agent"
	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/diff"
	"github.com/alibaba/open-code-review/internal/serve/agentcli"
	"github.com/alibaba/open-code-review/internal/serve/clireview"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

// defaultServeLanguage is the output language when the app config names
// none; serve refuses to start without one, so it only fills a gap.
const defaultServeLanguage = "Vietnamese"

// serveSelector picks the files of a CLI review through the same path the
// OCR engine takes: the request checks, the project rule read at the PR
// base, the request and service excludes, the serve filter, and the
// pre-dispatch selection of `ocr review --preview`.
type serveSelector struct {
	ocr serveReviewer
}

var _ clireview.Selector = serveSelector{}

func (s serveSelector) Select(ctx context.Context, req review.Request) (clireview.Selection, error) {
	var sel clireview.Selection
	cc, opts, err := s.ocr.prepare(ctx, req)
	if err != nil {
		return sel, err
	}
	provider := diff.NewProvider(cc.RepoDir, opts.from, opts.to, cc.GitRunner)
	if opts.commit != "" {
		provider = diff.NewCommitProvider(cc.RepoDir, opts.commit, cc.GitRunner)
	}
	in := provider.ResolveInput(ctx)
	if in.ResolvedHead == "" || (opts.commit == "" && in.ResolvedBase == "") {
		return sel, fmt.Errorf("resolve the review range %s..%s%s", opts.from, opts.to, opts.commit)
	}
	maxTokens, err := previewMaxTokens(cc.Template.MaxTokens, 0)
	if err != nil {
		return sel, err
	}
	tpl := *cc.Template
	tpl.MaxTokens = maxTokens
	preview, err := agent.Preview(ctx, agent.Args{
		RepoDir: cc.RepoDir, From: opts.from, To: opts.to, Commit: opts.commit,
		Template: tpl, FileFilter: cc.FileFilter, GitRunner: cc.GitRunner, SealedInput: &in,
	})
	if err != nil {
		return sel, fmt.Errorf("select review files: %w", err)
	}
	lang, err := serveAppLanguage()
	if err != nil {
		return sel, err
	}
	if lang == "" {
		lang = defaultServeLanguage
	}
	sel = clireview.Selection{Base: in.ResolvedBase, Head: in.ResolvedHead,
		Background: opts.background, Language: lang}
	for _, e := range preview.Entries {
		switch {
		case e.WillReview:
			sel.Paths = append(sel.Paths, e.Path)
			if sel.Lines == nil {
				sel.Lines = map[string]int{}
			}
			sel.Lines[e.Path] = int(e.Insertions + e.Deletions)
		// The OCR engine drops provider directories before it selects, so
		// its skipped counts leave them out; keep the counts comparable.
		case e.ExcludeReason != agent.ExcludeProviderDirectory:
			if sel.Skipped == nil {
				sel.Skipped = map[string]int{}
			}
			sel.Skipped[string(e.ExcludeReason)]++
		}
	}
	return sel, nil
}

// buildServeEngines returns the review engine the configuration selects
// and, for the CLI engine, the fix engine; the OCR engine has none, so /fix
// answers that it is not available. For the CLI engine it checks the
// executables, the skill and the CLI credential files, and removes the
// sandbox containers and run directories a stopped service left.
func buildServeEngines(ctx context.Context, cfg *serveconfig.Config, getenv func(string) string, log *slog.Logger) (review.Reviewer, review.Fixer, error) {
	ocr := newServeReviewer(cfg)
	if cfg.Engine == serveconfig.EngineOCR {
		return ocr, nil, nil
	}
	c := cfg.CLI
	if err := readable(filepath.Join(c.SkillDir, clireview.SkillFile)); err != nil {
		return nil, nil, fmt.Errorf("%s: review skill: %w", serveconfig.EnvSkillDir, err)
	}
	var sb *agentcli.Sandbox
	if cfg.Sandbox.Mode == serveconfig.SandboxDocker {
		var err error
		if sb, err = buildSandbox(ctx, cfg, log); err != nil {
			return nil, nil, err
		}
	}
	reviewCLI, err := buildAgentCLI(c, cfg.Sandbox, sb, c.ReviewCLI)
	if err != nil {
		return nil, nil, err
	}
	fixCLI, err := buildAgentCLI(c, cfg.Sandbox, sb, c.FixCLI)
	if err != nil {
		return nil, nil, fmt.Errorf("fix cli: %w", err)
	}
	homes, err := agentcli.NewHomes(c.Home)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", serveconfig.EnvCLIHome, err)
	}
	if err := homes.Sweep(); err != nil {
		return nil, nil, fmt.Errorf("%s: remove stale run dirs: %w", serveconfig.EnvCLIHome, err)
	}
	lang, err := serveAppLanguage()
	if err != nil {
		return nil, nil, err
	}
	if lang == "" {
		lang = defaultServeLanguage
	}
	rev := &clireview.Reviewer{
		Selector: serveSelector{ocr: ocr}, CLI: reviewCLI, Homes: homes,
		SkillDir: c.SkillDir, Getenv: getenv, Log: log,
		Batching: clireview.Batching{Lines: c.BatchLines, Files: c.BatchFiles, Concurrency: c.BatchConcurrency},
	}
	fix := &clireview.Fixer{CLI: fixCLI, Homes: homes, SkillDir: c.SkillDir, Language: lang, Getenv: getenv}
	return rev, fix, nil
}

// buildSandbox checks the docker client, creates the toolchain caches under
// the data dir and removes the containers a stopped service left. The
// skill directory is mounted for every CLI.
func buildSandbox(ctx context.Context, cfg *serveconfig.Config, log *slog.Logger) (*agentcli.Sandbox, error) {
	docker, err := exec.LookPath(cfg.Sandbox.DockerBin)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", serveconfig.EnvDockerBin, err)
	}
	im := cfg.Sandbox.Images
	sb := &agentcli.Sandbox{
		Docker: docker, UID: os.Getuid(), GID: os.Getgid(),
		Memory: cfg.Sandbox.Memory, CPUs: cfg.Sandbox.CPUs, AddHosts: cfg.Sandbox.AddHosts,
		Images: agentcli.Images{
			Go: im[serveconfig.ImageGo], Dotnet: im[serveconfig.ImageDotnet], Rust: im[serveconfig.ImageRust],
			Node: im[serveconfig.ImageNode], Python: im[serveconfig.ImagePython], Default: im[serveconfig.ImageDefault],
		},
		CacheDir: filepath.Join(cfg.DataDir, "cache"),
		Mounts:   []agentcli.Mount{{Path: cfg.CLI.SkillDir, ReadOnly: true}},
		Log:      log,
	}
	initCtx, cancel := context.WithTimeout(ctx, serveStartupTimeout)
	defer cancel()
	if err := sb.Init(initCtx); err != nil {
		return nil, fmt.Errorf("%s=%s: %w", serveconfig.EnvSandbox, serveconfig.SandboxDocker, err)
	}
	return sb, nil
}

// cliSetup is what buildAgentCLI needs to know of one CLI: the settings
// that name its executable, install directory and credential file, and how
// a sandbox mounts that file.
type cliSetup struct {
	binEnv, dirEnv, credEnv string
	installDir, cred        string
	credMount               agentcli.Mount
}

// cliSetups describes each CLI. The codex run home links auth.json to the
// host file, so its directory is mounted writable for a token refresh.
// The agy run home links its token file the same way, but only the file
// is mounted writable: a refresh written through the link reaches it, and
// the service writes back a token that replaced the link. The Claude
// settings file is only read.
func cliSetups(c serveconfig.CLIConfig, sc serveconfig.SandboxConfig) map[string]cliSetup {
	return map[string]cliSetup{
		serveconfig.CLICodex: {serveconfig.EnvCodexBin, serveconfig.EnvSandboxCodexDir, serveconfig.EnvCodexAuth,
			sc.CodexDir, c.CodexAuth, agentcli.Mount{Path: filepath.Dir(c.CodexAuth)}},
		serveconfig.CLIClaude: {serveconfig.EnvClaudeBin, serveconfig.EnvSandboxClaudeDir, serveconfig.EnvClaudeSettings,
			sc.ClaudeDir, c.ClaudeSettings, agentcli.Mount{Path: c.ClaudeSettings, ReadOnly: true}},
		serveconfig.CLIAgy: {serveconfig.EnvAgyBin, serveconfig.EnvSandboxAgyDir, serveconfig.EnvAgyToken,
			sc.AgyDir, c.AgyToken, agentcli.Mount{Path: c.AgyToken}},
	}
}

// buildAgentCLI resolves the executable and checks the credential file of
// the named CLI. With a sandbox, the CLI gets a copy of it that also mounts
// the CLI install and its credential file.
func buildAgentCLI(c serveconfig.CLIConfig, sc serveconfig.SandboxConfig, sb *agentcli.Sandbox, name string) (agentcli.CLI, error) {
	s, ok := cliSetups(c, sc)[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent cli %q", name)
	}
	bin, err := exec.LookPath(c.Bin(name))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.binEnv, err)
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, fmt.Errorf("%s: %w", s.binEnv, err)
	}
	if err := readable(s.cred); err != nil {
		return nil, fmt.Errorf("%s: %w", s.credEnv, err)
	}
	if sb != nil {
		var installDir string
		if bin, installDir, err = sandboxInstall(bin, s.installDir); err != nil {
			return nil, fmt.Errorf("%s: %w", s.dirEnv, err)
		}
		sb = sb.WithMounts(agentcli.Mount{Path: installDir, ReadOnly: true}, s.credMount)
	}
	switch name {
	case serveconfig.CLIClaude:
		return agentcli.Claude{Bin: bin, Model: c.ClaudeModel, SettingsFile: s.cred, Sandbox: sb}, nil
	case serveconfig.CLIAgy:
		return agentcli.Agy{Bin: bin, Model: c.AgyModel, Effort: c.AgyEffort, TokenFile: s.cred, Sandbox: sb}, nil
	}
	return agentcli.Codex{Bin: bin, Model: c.CodexModel, Effort: c.CodexEffort, AuthFile: s.cred, Sandbox: sb}, nil
}

// sandboxInstall resolves the binary through its links, since a link on
// the host (~/.local/bin/codex) points outside the mounted install, and
// checks that the install directory, by default the binary's own
// directory, holds it.
func sandboxInstall(bin, dir string) (string, string, error) {
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return "", "", err
	}
	if dir == "" {
		return resolved, filepath.Dir(resolved), nil
	}
	if realDir, err := filepath.EvalSymlinks(dir); err == nil {
		dir = realDir
	}
	if rel, err := filepath.Rel(dir, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%s is not inside the install directory %s", resolved, dir)
	}
	return resolved, dir, nil
}

// readable opens path for reading, so a missing file or a mount with the
// wrong owner shows at startup instead of in the first review.
func readable(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return err
	} else if st.IsDir() {
		return errors.New(path + " is a directory")
	}
	return nil
}
