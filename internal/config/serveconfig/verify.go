// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"slices"
	"strings"
)

// Verify pass settings.
const (
	EnvVerifyCLI   = "OCR_SERVE_VERIFY_CLI"
	EnvVerifyModel = "OCR_SERVE_VERIFY_MODEL"
	EnvVerifyPost  = "OCR_SERVE_VERIFY_POST"
)

// VerifyNone turns the verify pass off.
const VerifyNone = "none"

// Verdicts of the verify pass.
const (
	VerdictReal       = "real"
	VerdictMinor      = "minor"
	VerdictOutOfScope = "out_of_scope"
	VerdictFalse      = "false"
)

// Verdicts lists every verdict the verifier may give.
var Verdicts = []string{VerdictReal, VerdictMinor, VerdictOutOfScope, VerdictFalse}

// DefaultVerifyPost is the verdict list posted when OCR_SERVE_VERIFY_POST
// is unset.
var DefaultVerifyPost = []string{VerdictReal}

// VerifyConfig holds the settings of the verify pass, which has a second
// CLI check the findings of a CLI review before they are posted. CLI is
// VerifyNone, CLIClaude or CLICodex. Model, when set, replaces the
// configured model of that CLI for the verify run. Post lists the verdicts
// whose findings are posted.
type VerifyConfig struct {
	CLI   string
	Model string
	Post  []string
}

// Enabled reports whether the verify pass runs.
func (v VerifyConfig) Enabled() bool {
	return v.CLI != "" && v.CLI != VerifyNone
}

// verify reads the verify pass settings. agy gets its own error, like for
// the fix CLI, since it is a valid review CLI.
func (l *loader) verify() VerifyConfig {
	v := VerifyConfig{Model: l.matching(EnvVerifyModel, modelPattern), Post: l.verifyPost()}
	if strings.EqualFold(l.str(EnvVerifyCLI, ""), CLIAgy) {
		l.fail("%s=%s is not supported: agy runs reviews only, set %s to %s, %s or %s",
			EnvVerifyCLI, CLIAgy, EnvVerifyCLI, VerifyNone, CLIClaude, CLICodex)
		v.CLI = VerifyNone
		return v
	}
	v.CLI = l.oneOf(EnvVerifyCLI, VerifyNone, VerifyNone, CLIClaude, CLICodex)
	return v
}

func (l *loader) verifyPost() []string {
	raw := splitList(l.getenv(EnvVerifyPost))
	if len(raw) == 0 {
		return slices.Clone(DefaultVerifyPost)
	}
	var out []string
	for _, p := range raw {
		p = strings.ToLower(p)
		if !slices.Contains(Verdicts, p) {
			l.fail("%s has an invalid verdict %q, want a comma list of %s", EnvVerifyPost, p, strings.Join(Verdicts, ", "))
			continue
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return slices.Clone(DefaultVerifyPost)
	}
	return out
}
