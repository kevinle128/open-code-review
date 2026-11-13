// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Review engine settings.
const (
	EnvEngine         = "OCR_SERVE_ENGINE"
	EnvReviewCLI      = "OCR_SERVE_REVIEW_CLI"
	EnvFixCLI         = "OCR_SERVE_FIX_CLI"
	EnvCodexBin       = "OCR_SERVE_CODEX_BIN"
	EnvClaudeBin      = "OCR_SERVE_CLAUDE_BIN"
	EnvCodexModel     = "OCR_SERVE_CODEX_MODEL"
	EnvClaudeModel    = "OCR_SERVE_CLAUDE_MODEL"
	EnvCodexEffort    = "OCR_SERVE_CODEX_EFFORT"
	EnvSkillDir       = "OCR_SERVE_SKILL_DIR"
	EnvCLIHome        = "OCR_SERVE_CLI_HOME"
	EnvCodexAuth      = "OCR_SERVE_CODEX_AUTH"
	EnvClaudeSettings = "OCR_SERVE_CLAUDE_SETTINGS"
)

// Engines and agent CLIs.
const (
	EngineCLI = "cli"
	EngineOCR = "ocr"

	CLICodex  = "codex"
	CLIClaude = "claude"

	DefaultEngine    = EngineCLI
	DefaultReviewCLI = CLICodex
	DefaultFixCLI    = CLIClaude
	DefaultCodexBin  = "codex"
	DefaultClaudeBin = "claude"
	// cliHomeName is the default CLI home directory under the data dir.
	cliHomeName = "cli-home"
)

// The model and effort values become CLI arguments and, for the effort, a
// TOML value; a tight pattern keeps them from carrying quotes or options.
var (
	modelPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]*$`)
	effortPattern = regexp.MustCompile(`^[a-z]+$`)
)

// CLIConfig holds the settings of the CLI review engine, which runs an
// installed coding-agent CLI on the checkout. ReviewCLI runs reviews;
// FixCLI is the one later fix runs use. Paths are absolute.
type CLIConfig struct {
	ReviewCLI      string
	FixCLI         string
	CodexBin       string
	ClaudeBin      string
	CodexModel     string
	ClaudeModel    string
	CodexEffort    string
	SkillDir       string
	Home           string
	CodexAuth      string
	ClaudeSettings string
}

// Bin returns the configured executable of the named CLI.
func (c CLIConfig) Bin(name string) string {
	if name == CLIClaude {
		return c.ClaudeBin
	}
	return c.CodexBin
}

func (l *loader) engine() string {
	return l.oneOf(EnvEngine, DefaultEngine, EngineCLI, EngineOCR)
}

func (l *loader) cli(dataDir string) CLIConfig {
	return CLIConfig{
		ReviewCLI:      l.oneOf(EnvReviewCLI, DefaultReviewCLI, CLICodex, CLIClaude),
		FixCLI:         l.oneOf(EnvFixCLI, DefaultFixCLI, CLIClaude, CLICodex),
		CodexBin:       l.str(EnvCodexBin, DefaultCodexBin),
		ClaudeBin:      l.str(EnvClaudeBin, DefaultClaudeBin),
		CodexModel:     l.matching(EnvCodexModel, modelPattern),
		ClaudeModel:    l.matching(EnvClaudeModel, modelPattern),
		CodexEffort:    l.matching(EnvCodexEffort, effortPattern),
		SkillDir:       l.absPath(EnvSkillDir, ""),
		Home:           l.absPath(EnvCLIHome, filepath.Join(dataDir, cliHomeName)),
		CodexAuth:      l.absPath(EnvCodexAuth, ""),
		ClaudeSettings: l.absPath(EnvClaudeSettings, ""),
	}
}

func (l *loader) oneOf(key, def string, allowed ...string) string {
	v := strings.ToLower(l.str(key, def))
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.fail("%s must be one of %s, got %q", key, strings.Join(allowed, ", "), v)
	return def
}

func (l *loader) matching(key string, re *regexp.Regexp) string {
	v := strings.TrimSpace(l.getenv(key))
	if v != "" && !re.MatchString(v) {
		l.fail("%s has an invalid value %q", key, v)
		return ""
	}
	return v
}

// absPath requires an absolute path, so the value does not depend on the
// directory the service starts in or the one a CLI runs in.
func (l *loader) absPath(key, def string) string {
	v := l.str(key, def)
	if v != "" && !filepath.IsAbs(v) {
		l.fail("%s must be an absolute path, got %q", key, v)
		return ""
	}
	if v == "" {
		return ""
	}
	return filepath.Clean(v)
}

// requireCLI checks what the CLI engine needs: the skill directory and the
// credential file of the review CLI.
func (c *Config) requireCLI() error {
	if c.Engine != EngineCLI {
		return nil
	}
	var errs []error
	if c.CLI.SkillDir == "" {
		errs = append(errs, fmt.Errorf("%s is required when %s is %s", EnvSkillDir, EnvEngine, EngineCLI))
	}
	switch c.CLI.ReviewCLI {
	case CLICodex:
		if c.CLI.CodexAuth == "" {
			errs = append(errs, fmt.Errorf("%s is required when %s is %s", EnvCodexAuth, EnvReviewCLI, CLICodex))
		}
	case CLIClaude:
		if c.CLI.ClaudeSettings == "" {
			errs = append(errs, fmt.Errorf("%s is required when %s is %s", EnvClaudeSettings, EnvReviewCLI, CLIClaude))
		}
	}
	return errors.Join(errs...)
}
