//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"os/exec"
	"time"
)

// startGroup only bounds the wait on Windows: the service runs on Linux,
// and stopping a whole process tree there would need a Job Object.
func startGroup(cmd *exec.Cmd, grace time.Duration) {
	cmd.WaitDelay = grace
}

func killGroup(*exec.Cmd) {}
