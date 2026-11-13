// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const runPrefix = "run-"

// Homes holds the per-run directories under the service-owned CLI home.
type Homes struct {
	root string
}

// NewHomes creates root when it is missing.
func NewHomes(root string) (*Homes, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("cli home %q is not an absolute path", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create cli home: %w", err)
	}
	return &Homes{root: root}, nil
}

// Sweep removes the run directories a stopped service left behind. Call it
// before the first run: every run directory present then is stale.
func (h *Homes) Sweep() error {
	entries, err := os.ReadDir(h.root)
	if err != nil {
		return fmt.Errorf("read cli home: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), runPrefix) {
			if err := os.RemoveAll(filepath.Join(h.root, e.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// RunDir is the directory of one CLI run.
//
//   - Home is the CLI config home (CODEX_HOME, CLAUDE_CONFIG_DIR, HOME). It
//     holds the credentials, so it is never offered to the agent to read.
//   - Input holds the files the agent reads besides the repository.
//   - Out holds the prompt, the CLI output and the files the service reads
//     back.
type RunDir struct {
	Dir   string
	Home  string
	Input string
	Out   string
	Tmp   string
}

// New creates an empty run directory.
func (h *Homes) New() (*RunDir, error) {
	dir, err := os.MkdirTemp(h.root, runPrefix)
	if err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	r := &RunDir{
		Dir:   dir,
		Home:  filepath.Join(dir, "home"),
		Input: filepath.Join(dir, "input"),
		Out:   filepath.Join(dir, "out"),
		Tmp:   filepath.Join(dir, "tmp"),
	}
	for _, d := range []string{r.Home, r.Input, r.Out, r.Tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("create run dir: %w", err)
		}
	}
	return r, nil
}

// Remove deletes the run directory and everything the CLI wrote into it.
func (r *RunDir) Remove() error {
	if err := os.RemoveAll(r.Dir); err != nil {
		return fmt.Errorf("remove run dir: %w", err)
	}
	return nil
}
