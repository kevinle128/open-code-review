// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/alibaba/open-code-review/internal/agent"
	"github.com/alibaba/open-code-review/internal/config/rules"
	"github.com/alibaba/open-code-review/internal/serve/checkout"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/session"
	"github.com/alibaba/open-code-review/internal/tool"
)

// Settings that the `ocr review` flags would give. A zero reviewOptions would
// mean zero for these, not the flag default. The file-group limit is longer
// than the review flag default so that it does not end a serve review before
// the run limit (OCR_SERVE_REVIEW_TIMEOUT, default 2h) does.
const (
	serveReviewConcurrency    = 8
	serveReviewTimeoutMinutes = 120
	serveReviewMaxGitProcs    = 16
)

// serveReviewer runs the `ocr review` pipeline in-process for `ocr serve`.
// Several reviews run at the same time, so it changes no process-wide
// state: no stdout quieting, no output writer and no raw capture file.
// Each review's session file is removed when the review ends.
type serveReviewer struct{}

var _ review.Reviewer = serveReviewer{}

func (serveReviewer) Review(ctx context.Context, req review.Request) (review.Result, error) {
	failed := review.Result{Terminal: review.TerminalFailed}
	if req.RepoDir == "" {
		return failed, errors.New("review repo dir is required")
	}
	// An empty mode would review the working tree, which a checkout never
	// has changes in.
	if req.Commit == "" && (req.From == "" || req.To == "") {
		return failed, errors.New("review needs a commit or a from and to range")
	}
	lang, err := serveAppLanguage()
	if err != nil {
		return failed, err
	}

	opts := reviewOptions{
		repoDir:               req.RepoDir,
		from:                  req.From,
		to:                    req.To,
		commit:                req.Commit,
		background:            req.Background,
		effort:                req.Effort,
		outputFormat:          "text",
		audience:              "human",
		concurrency:           serveReviewConcurrency,
		concurrentTaskTimeout: serveReviewTimeoutMinutes,
		maxGitProcs:           serveReviewMaxGitProcs,
	}
	if err := validateReviewOptions(&opts); err != nil {
		return failed, err
	}

	contentRef, _ := tool.ParseReviewMode(opts.from, opts.to, opts.commit).RefValue(opts.to, opts.commit)
	cc, err := loadCommonContextWith(opts.repoDir, "", contentRef, 0, opts.maxGitProcs, true,
		rules.ResolverOptions{OverrideProject: true, Project: req.ProjectRule})
	if err != nil {
		return failed, err
	}
	cc.Template.MaxToolRequestTimes = resolveReviewMaxTools(cc.Template.MaxToolRequestTimes, opts.maxTools)
	applyCLIExcludes(cc, req.Excludes)

	opts.from = checkout.RangeStart(ctx, cc.RepoDir, opts.from)
	if err := validateReviewRefs(cc.RepoDir, opts); err != nil {
		return failed, err
	}
	bg, err := resolveBackground(cc.RepoDir, opts.background, "", opts.commit)
	if err != nil {
		return failed, err
	}
	opts.background = withOutputLanguage(bg, lang)

	prepared, err := prepareReviewAgent(ctx, cc, opts, nil)
	if err != nil {
		return failed, err
	}
	defer prepared.closeMCP()
	ag := prepared.ag

	comments, runErr := ag.Run(ctx)
	manifest := ag.RunManifest()
	res := review.Result{
		Findings: comments,
		Terminal: terminalState(manifest, runErr),
		Warnings: warningTexts(warningsForOutput(ag.Warnings(), manifest)),
	}
	if err := deleteServeSession(cc.RepoDir, ag.SessionID()); err != nil {
		res.Warnings = append(res.Warnings, err.Error())
	}
	return res, reviewResultError(runErr, manifest)
}

// serveAppLanguage returns the configured output language. It stops a
// review when MCP servers are configured: their setup commands would run
// inside the checkout of an untrusted PR.
func serveAppLanguage() (string, error) {
	path, err := defaultConfigPath()
	if err != nil {
		return "", err
	}
	cfg, err := LoadAppConfig(path)
	if err != nil {
		return "", fmt.Errorf("load app config: %w", err)
	}
	if cfg == nil {
		return "", nil
	}
	if len(cfg.MCPServers) > 0 {
		return "", fmt.Errorf("mcp_servers is set in %s; ocr serve does not run MCP servers", path)
	}
	return strings.TrimSpace(cfg.Language), nil
}

// withOutputLanguage appends an output-language directive to the review
// background. The system-prompt directive alone does not always hold: a
// model can still write its findings in English. The background goes into
// the user task message, so the rule is repeated next to the diff.
func withOutputLanguage(background, lang string) string {
	if lang == "" || strings.EqualFold(lang, "English") {
		return background
	}
	directive := "Write every review comment, explanation and suggestion text in " + lang +
		". Keep code, identifiers and file paths unchanged."
	if strings.TrimSpace(background) == "" {
		return directive
	}
	return background + "\n\n" + directive
}

func terminalState(manifest *session.RunManifest, runErr error) string {
	if manifest != nil {
		return string(manifest.TerminalState)
	}
	if runErr != nil {
		return review.TerminalFailed
	}
	// `ocr review` publishes a run without a manifest as a success too.
	return review.TerminalComplete
}

func warningTexts(warnings []agent.AgentWarning) []string {
	if len(warnings) == 0 {
		return nil
	}
	out := make([]string, 0, len(warnings))
	for _, w := range warnings {
		if w.File != "" {
			out = append(out, w.File+": "+w.Message)
			continue
		}
		out = append(out, w.Message)
	}
	return out
}

func deleteServeSession(repoDir, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	if err := session.DeleteSession(repoDir, sessionID); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
		return fmt.Errorf("delete review session: %w", err)
	}
	return nil
}
