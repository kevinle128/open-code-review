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

// buildServeReviewer returns the review engine the configuration selects.
// For the CLI engine it checks the executable, the skill and the CLI
// credential file, and removes run directories a stopped service left.
func buildServeReviewer(cfg *serveconfig.Config, getenv func(string) string) (review.Reviewer, error) {
	ocr := newServeReviewer(cfg)
	if cfg.Engine == serveconfig.EngineOCR {
		return ocr, nil
	}
	c := cfg.CLI
	binEnv := serveconfig.EnvCodexBin
	if c.ReviewCLI == serveconfig.CLIClaude {
		binEnv = serveconfig.EnvClaudeBin
	}
	bin, err := exec.LookPath(c.Bin(c.ReviewCLI))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", binEnv, err)
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, fmt.Errorf("%s: %w", binEnv, err)
	}
	if err := readable(filepath.Join(c.SkillDir, clireview.SkillFile)); err != nil {
		return nil, fmt.Errorf("%s: review skill: %w", serveconfig.EnvSkillDir, err)
	}
	var cli agentcli.CLI
	switch c.ReviewCLI {
	case serveconfig.CLIClaude:
		if err := readable(c.ClaudeSettings); err != nil {
			return nil, fmt.Errorf("%s: %w", serveconfig.EnvClaudeSettings, err)
		}
		cli = agentcli.Claude{Bin: bin, Model: c.ClaudeModel, SettingsFile: c.ClaudeSettings}
	default:
		if err := readable(c.CodexAuth); err != nil {
			return nil, fmt.Errorf("%s: %w", serveconfig.EnvCodexAuth, err)
		}
		cli = agentcli.Codex{Bin: bin, Model: c.CodexModel, Effort: c.CodexEffort, AuthFile: c.CodexAuth}
	}
	homes, err := agentcli.NewHomes(c.Home)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", serveconfig.EnvCLIHome, err)
	}
	if err := homes.Sweep(); err != nil {
		return nil, fmt.Errorf("%s: remove stale run dirs: %w", serveconfig.EnvCLIHome, err)
	}
	return &clireview.Reviewer{
		Selector: serveSelector{ocr: ocr}, CLI: cli, Homes: homes,
		SkillDir: c.SkillDir, Getenv: getenv,
	}, nil
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
