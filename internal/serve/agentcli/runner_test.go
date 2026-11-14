//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeScript writes an executable shell script and returns its path.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func newProcess(t *testing.T, bin string) Process {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	if err := os.WriteFile(in, []byte("the prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Process{
		Bin: bin, Dir: dir, Stdin: in, Env: []string{"PATH=/usr/bin:/bin"},
		Stdout: filepath.Join(dir, "stdout"), Stderr: filepath.Join(dir, "stderr"),
		Grace: 200 * time.Millisecond,
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunPassesInputAndCollectsOutput(t *testing.T) {
	bin := writeScript(t, t.TempDir(), "cli", `cat; echo " seen in $PWD"; echo progress >&2`)
	p := newProcess(t, bin)
	if err := Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.EvalSymlinks(p.Dir)
	if got := readFile(t, p.Stdout); !strings.Contains(got, "the prompt seen in ") || !strings.Contains(got, filepath.Base(dir)) {
		t.Fatalf("stdout %q", got)
	}
	if got := readFile(t, p.Stderr); got != "progress\n" {
		t.Fatalf("stderr %q", got)
	}
}

func TestRunReportsExitCodeWithStderrTail(t *testing.T) {
	bin := writeScript(t, t.TempDir(), "cli", `i=0; while [ $i -lt 400 ]; do echo "noise line $i" >&2; i=$((i+1)); done; echo "last words" >&2; exit 3`)
	err := Run(context.Background(), newProcess(t, bin))
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 || exitErr.Name != "cli" {
		t.Fatalf("err %v", err)
	}
	if !strings.HasSuffix(exitErr.Stderr, "last words") || len(exitErr.Stderr) > stderrTail {
		t.Fatalf("stderr tail %d bytes: %q", len(exitErr.Stderr), exitErr.Stderr)
	}
	if !strings.Contains(err.Error(), "cli exited with code 3: ") {
		t.Fatalf("message %q", err.Error())
	}
	if (&ExitError{Name: "x", Code: 1}).Error() != "x exited with code 1" {
		t.Fatal("message without stderr")
	}
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunCancelStopsTheWholeGroup(t *testing.T) {
	rec := t.TempDir()
	// The CLI and its tool ignore SIGTERM, so only the kill after the grace
	// period stops them.
	bin := writeScript(t, t.TempDir(), "cli", `trap '' TERM
sleep 300 &
echo $! > `+rec+`/child.tmp && mv `+rec+`/child.tmp `+rec+`/child
wait
`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, newProcess(t, bin)) }()
	child := readPID(t, filepath.Join(rec, "child"))
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "cli stopped") {
			t.Fatalf("err %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("the leader was killed before the grace period")
	}
	waitGone(t, child)
}

func TestRunKillsToolsLeftAfterExit(t *testing.T) {
	rec := t.TempDir()
	bin := writeScript(t, t.TempDir(), "cli", `sleep 300 &
echo $! > `+rec+`/child
exit 0
`)
	if err := Run(context.Background(), newProcess(t, bin)); err != nil {
		t.Fatal(err)
	}
	waitGone(t, readPID(t, filepath.Join(rec, "child")))
}

func TestRunSetupErrors(t *testing.T) {
	bin := writeScript(t, t.TempDir(), "cli", "exit 0\n")
	cases := map[string]func(*Process){
		"open cli input":    func(p *Process) { p.Stdin = filepath.Join(p.Dir, "missing") },
		"create cli output": func(p *Process) { p.Stdout = filepath.Join(p.Dir, "no", "out") },
		"create cli error":  func(p *Process) { p.Stderr = filepath.Join(p.Dir, "no", "err") },
		"start cli":         func(p *Process) { p.Bin = filepath.Join(p.Dir, "cli") },
	}
	for want, edit := range cases {
		t.Run(want, func(t *testing.T) {
			p := newProcess(t, bin)
			edit(&p)
			err := Run(context.Background(), p)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestTail(t *testing.T) {
	if tail(filepath.Join(t.TempDir(), "none"), 10) != "" {
		t.Fatal("missing file")
	}
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("  0123456789abc \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tail(path, 6); got != "9abc" {
		t.Fatalf("tail %q", got)
	}
}

func TestBaseEnvKeepsServiceSecretsOut(t *testing.T) {
	vars := map[string]string{
		"PATH": "/usr/bin", "LANG": "C.UTF-8", "HTTPS_PROXY": "http://proxy:3128",
		"OCR_SERVE_GITHUB_PRIVATE_KEY": "key", "OCR_SERVE_GITLAB_TOKEN": "glpat", "OCR_LLM_TOKEN": "tok",
		"HOME": "/home/real", "TMPDIR": "/tmp/real",
	}
	env := BaseEnv(func(k string) string { return vars[k] }, "/run/home", "/run/tmp")
	got := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/usr/bin", "LANG=C.UTF-8", "HTTPS_PROXY=http://proxy:3128", "HOME=/run/home", "TMPDIR=/run/tmp"} {
		if !strings.Contains(got, want) {
			t.Errorf("env misses %s", want)
		}
	}
	for _, leak := range []string{"OCR_", "key", "glpat", "tok", "/home/real", "/tmp/real"} {
		if strings.Contains(got, leak) {
			t.Errorf("env leaks %s:\n%s", leak, got)
		}
	}
}
