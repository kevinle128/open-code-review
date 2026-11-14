//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"os/exec"
	"syscall"
	"time"
)

// startGroup puts the CLI in a process group of its own, so a cancel
// reaches the tools it started too. Cancel asks the group to stop with
// SIGTERM; WaitDelay kills the leader when it ignores that for grace.
func startGroup(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = grace
}

// killGroup removes what is left of the group after the leader exited: a
// tool the CLI started must not outlive the run.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
