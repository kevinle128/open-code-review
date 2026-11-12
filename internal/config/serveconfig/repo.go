// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
)

// RepoConfig is the configuration in force for one repository.
type RepoConfig struct {
	Enabled    bool
	MaxRounds  int
	Exclude    []string
	Background string
	Effort     string
}

// Per-repository file limits.
const (
	RepoConfigPath     = ".opencodereview/serve.json"
	MaxRepoConfigBytes = 64 << 10
	MaxBackgroundChars = 4000
	MaxExcludePatterns = 50
	maxShownArgChars   = 64
)

var efforts = []string{"low", "medium", "high"}

// MergeRepo applies a serve.json document on top of base. Only the keys
// enabled, max_rounds, exclude, background and effort are honored; the others
// are returned sorted so the caller can log them. A null value leaves the
// base value unchanged. Exclude patterns are appended to base.Exclude. Empty
// or whitespace-only input returns base unchanged. On error base is returned
// so the caller can keep running with defaults.
func MergeRepo(base RepoConfig, raw []byte) (RepoConfig, []string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return base, nil, nil
	}
	if len(raw) > MaxRepoConfigBytes {
		return base, nil, fmt.Errorf("%s exceeds %d bytes", RepoConfigPath, MaxRepoConfigBytes)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return base, nil, fmt.Errorf("%s: must be a JSON object: %w", RepoConfigPath, err)
	}

	out := base
	out.Exclude = slices.Clone(base.Exclude)
	var ignored []string
	var errs []error
	for key, value := range doc {
		if string(bytes.TrimSpace(value)) == "null" {
			continue
		}
		var err error
		switch key {
		case "enabled":
			err = json.Unmarshal(value, &out.Enabled)
		case "max_rounds":
			err = decodeMaxRounds(value, &out.MaxRounds)
		case "exclude":
			err = decodeExclude(value, &out.Exclude)
		case "background":
			err = decodeBackground(value, &out.Background)
		case "effort":
			err = decodeEffort(value, &out.Effort)
		default:
			ignored = append(ignored, key)
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %s: %w", RepoConfigPath, key, err))
		}
	}
	sort.Strings(ignored)
	if err := errors.Join(errs...); err != nil {
		return base, ignored, err
	}
	return out, ignored, nil
}

func decodeMaxRounds(raw json.RawMessage, dst *int) error {
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	if n < MinRounds || n > MaxRounds {
		return fmt.Errorf("must be in %d..%d, got %d", MinRounds, MaxRounds, n)
	}
	*dst = n
	return nil
}

func decodeExclude(raw json.RawMessage, dst *[]string) error {
	var globs []string
	if err := json.Unmarshal(raw, &globs); err != nil {
		return err
	}
	if len(globs) > MaxExcludePatterns {
		return fmt.Errorf("at most %d patterns, got %d", MaxExcludePatterns, len(globs))
	}
	for _, g := range globs {
		if strings.TrimSpace(g) == "" || !doublestar.ValidatePattern(g) {
			return fmt.Errorf("invalid glob %q", g)
		}
	}
	*dst = append(*dst, globs...)
	return nil
}

func decodeBackground(raw json.RawMessage, dst *string) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(s); n > MaxBackgroundChars {
		return fmt.Errorf("at most %d characters, got %d", MaxBackgroundChars, n)
	}
	*dst = s
	return nil
}

func decodeEffort(raw json.RawMessage, dst *string) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	if !slices.Contains(efforts, s) {
		return fmt.Errorf("must be one of %s, got %q", strings.Join(efforts, ", "), s)
	}
	*dst = s
	return nil
}

// ParseCommandOverrides reads the arguments after /review or /review-commit.
// Only --effort=low|medium|high is accepted, at most once; anything else is
// an error whose text is safe to quote back in a PR comment.
func ParseCommandOverrides(args []string) (string, error) {
	effort := ""
	for _, arg := range args {
		value, ok := strings.CutPrefix(arg, "--effort=")
		if !ok {
			return "", fmt.Errorf("unsupported option %q; only --effort=%s is allowed", clip(arg), strings.Join(efforts, "|"))
		}
		if !slices.Contains(efforts, value) {
			return "", fmt.Errorf("invalid effort %q; use %s", clip(value), strings.Join(efforts, ", "))
		}
		if effort != "" {
			return "", errors.New("--effort given more than once")
		}
		effort = value
	}
	return effort, nil
}

// clip keeps a user-supplied argument short in the error reply.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxShownArgChars {
		return s
	}
	return string([]rune(s)[:maxShownArgChars]) + "..."
}
