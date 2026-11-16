// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import "strings"

// Settings of the fix run of each round, which judges the review findings,
// fixes the real ones and runs the tests.
const (
	EnvVerifyCLI   = "OCR_SERVE_VERIFY_CLI"
	EnvVerifyModel = "OCR_SERVE_VERIFY_MODEL"
)

// DefaultVerifyCLI runs the fix run when OCR_SERVE_VERIFY_CLI is unset.
const DefaultVerifyCLI = CLIClaude

// VerifyConfig holds the settings of the fix run of each round. CLI is
// CLIClaude or CLICodex. Model, when set, replaces the configured model of
// that CLI for the run.
type VerifyConfig struct {
	CLI   string
	Model string
}

// verify reads the fix run settings. agy and "none" get their own errors:
// agy is a valid review CLI, and "none" turned off the earlier verify pass,
// while every round now needs this run.
func (l *loader) verify() VerifyConfig {
	v := VerifyConfig{Model: l.matching(EnvVerifyModel, modelPattern)}
	switch raw := strings.ToLower(l.str(EnvVerifyCLI, "")); raw {
	case CLIAgy:
		l.fail("%s=%s is not supported: agy runs reviews only, set %s to %s or %s", EnvVerifyCLI, CLIAgy, EnvVerifyCLI, CLIClaude, CLICodex)
		v.CLI = DefaultVerifyCLI
	case "none":
		l.fail("%s=none is not supported: every round runs this CLI to judge and fix the findings, set it to %s or %s", EnvVerifyCLI, CLIClaude, CLICodex)
		v.CLI = DefaultVerifyCLI
	default:
		v.CLI = l.oneOf(EnvVerifyCLI, DefaultVerifyCLI, CLIClaude, CLICodex)
	}
	return v
}
