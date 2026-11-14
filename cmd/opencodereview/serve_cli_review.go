// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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
// executables, the skill and the CLI credential files, and removes run
// directories a stopped service left.
func buildServeEngines(cfg *serveconfig.Config, getenv func(string) string) (review.Reviewer, review.Fixer, error) {
	ocr := newServeReviewer(cfg)
	if cfg.Engine == serveconfig.EngineOCR {
		return ocr, nil, nil
	}
	c := cfg.CLI
	if err := readable(filepath.Join(c.SkillDir, clireview.SkillFile)); err != nil {
		return nil, nil, fmt.Errorf("%s: review skill: %w", serveconfig.EnvSkillDir, err)
	}
	reviewCLI, err := buildAgentCLI(c, c.ReviewCLI)
	if err != nil {
		return nil, nil, err
	}
	fixCLI, err := buildAgentCLI(c, c.FixCLI)
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
		SkillDir: c.SkillDir, Getenv: getenv,
	}
	fix := &clireview.Fixer{CLI: fixCLI, Homes: homes, SkillDir: c.SkillDir, Language: lang, Getenv: getenv}
	return rev, fix, nil
}

// buildAgentCLI resolves the executable and checks the credential file of
// the named CLI.
func buildAgentCLI(c serveconfig.CLIConfig, name string) (agentcli.CLI, error) {
	binEnv := serveconfig.EnvCodexBin
	if name == serveconfig.CLIClaude {
		binEnv = serveconfig.EnvClaudeBin
	}
	bin, err := exec.LookPath(c.Bin(name))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", binEnv, err)
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, fmt.Errorf("%s: %w", binEnv, err)
	}
	if name == serveconfig.CLIClaude {
		if err := readable(c.ClaudeSettings); err != nil {
			return nil, fmt.Errorf("%s: %w", serveconfig.EnvClaudeSettings, err)
		}
		return agentcli.Claude{Bin: bin, Model: c.ClaudeModel, SettingsFile: c.ClaudeSettings}, nil
	}
	if err := readable(c.CodexAuth); err != nil {
		return nil, fmt.Errorf("%s: %w", serveconfig.EnvCodexAuth, err)
	}
	return agentcli.Codex{Bin: bin, Model: c.CodexModel, Effort: c.CodexEffort, AuthFile: c.CodexAuth}, nil
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
