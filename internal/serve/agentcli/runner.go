// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package agentcli runs an installed coding-agent CLI (Codex CLI, Claude
// Code or the Antigravity CLI) as a child process of the service: each run gets a fresh home
// directory that the service owns, an environment that holds none of the
// service secrets, and a process group that a cancel stops as a whole.
package agentcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultGrace is how long a cancelled CLI may take to stop after SIGTERM.
const DefaultGrace = 10 * time.Second

// stderrTail bounds the stderr text that goes into an error.
const stderrTail = 2000

// Process is one CLI invocation. Stdin, Stdout and Stderr are file paths;
// files keep the output for a failed run and spare the pipe copies that
// would hold Wait open while a stray tool keeps a pipe. Name labels errors;
// empty means the base name of Bin.
type Process struct {
	Name   string
	Bin    string
	Args   []string
	Env    []string
	Dir    string
	Stdin  string
	Stdout string
	Stderr string
	Grace  time.Duration
}

// ExitError is a CLI that ended with a non-zero exit code.
type ExitError struct {
	Name   string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("%s exited with code %d", e.Name, e.Code)
	}
	return fmt.Sprintf("%s exited with code %d: %s", e.Name, e.Code, e.Stderr)
}

// Run starts the process and waits for it. When ctx ends, the process group
// gets SIGTERM, then the leader SIGKILL after the grace period; whatever is
// left of the group is killed once the leader is gone, also after a normal
// exit.
func Run(ctx context.Context, p Process) error {
	name := p.name()
	stdin, err := os.Open(p.Stdin)
	if err != nil {
		return fmt.Errorf("open %s input: %w", name, err)
	}
	defer stdin.Close()
	stdout, err := os.OpenFile(p.Stdout, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s output: %w", name, err)
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(p.Stderr, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s error output: %w", name, err)
	}
	defer stderr.Close()

	cmd := exec.CommandContext(ctx, p.Bin, p.Args...)
	cmd.Dir = p.Dir
	cmd.Env = p.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	grace := p.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	startGroup(cmd, grace)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	waitErr := cmd.Wait()
	killGroup(cmd)
	if ctx.Err() != nil {
		return fmt.Errorf("%s stopped: %w", name, ctx.Err())
	}
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return &ExitError{Name: name, Code: exitErr.ExitCode(), Stderr: tail(p.Stderr, stderrTail)}
	}
	return fmt.Errorf("run %s: %w", name, waitErr)
}

func (p Process) name() string {
	if p.Name != "" {
		return p.Name
	}
	return filepath.Base(p.Bin)
}

// tail returns the last n bytes of a file, trimmed, or "" when it cannot be
// read.
func tail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
}

// passedEnv are the process variables a CLI may see. Everything else, the
// forge keys, webhook secrets and LLM tokens of the service among them,
// stays out: the CLI reads a checkout that the PR author controls.
var passedEnv = []string{
	"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy",
}

// BaseEnv returns the environment of a CLI run: the allowed process
// variables, with HOME and TMPDIR inside the run so the CLI writes nothing
// to the real home directory.
func BaseEnv(getenv func(string) string, home, tmp string) []string {
	var env []string
	for _, k := range passedEnv {
		if v := getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return append(env, "HOME="+home, "TMPDIR="+tmp)
}
