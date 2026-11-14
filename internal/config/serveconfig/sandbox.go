// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"fmt"
	"regexp"
	"strings"
)

// Sandbox settings of the CLI engine.
const (
	EnvSandbox            = "OCR_SERVE_SANDBOX"
	EnvDockerBin          = "OCR_SERVE_DOCKER_BIN"
	EnvSandboxMemory      = "OCR_SERVE_SANDBOX_MEMORY"
	EnvSandboxCPUs        = "OCR_SERVE_SANDBOX_CPUS"
	EnvSandboxCodexDir    = "OCR_SERVE_SANDBOX_CODEX_DIR"
	EnvSandboxClaudeDir   = "OCR_SERVE_SANDBOX_CLAUDE_DIR"
	EnvSandboxImagePrefix = "OCR_SERVE_SANDBOX_IMAGE_"
)

// Sandbox modes.
const (
	SandboxNone   = "none"
	SandboxDocker = "docker"

	DefaultSandbox       = SandboxNone
	DefaultDockerBin     = "docker"
	DefaultSandboxMemory = "6g"
	DefaultSandboxCPUs   = "4"
)

// Toolchain kinds of a sandbox image, in the order the image detection
// prefers them when a checkout matches several.
const (
	ImageGo      = "GO"
	ImageDotnet  = "DOTNET"
	ImageRust    = "RUST"
	ImageNode    = "NODE"
	ImagePython  = "PYTHON"
	ImageDefault = "DEFAULT"
)

// DefaultSandboxImages are Debian (glibc) images: the Claude Code binary
// links against glibc and does not start on Alpine.
var DefaultSandboxImages = map[string]string{
	ImageGo:      "golang:1-bookworm",
	ImageDotnet:  "mcr.microsoft.com/dotnet/sdk:9.0",
	ImageRust:    "rust:1-bookworm",
	ImageNode:    "node:22-bookworm",
	ImagePython:  "python:3.12-bookworm",
	ImageDefault: "buildpack-deps:bookworm",
}

// The memory and CPU values and the image references become docker
// arguments; tight patterns keep options and shell text out of them.
var (
	memoryPattern = regexp.MustCompile(`^[0-9]+[bkmgBKMG]?$`)
	cpusPattern   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	imagePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
)

// SandboxConfig holds how each CLI run is isolated. Mode SandboxNone runs
// the CLI as a child process of the service; SandboxDocker runs it in a
// throwaway container. CodexDir and ClaudeDir are the install directories
// mounted into the container; empty means the directory of the resolved
// binary. Images maps each toolchain kind to its image.
type SandboxConfig struct {
	Mode      string
	DockerBin string
	Memory    string
	CPUs      string
	CodexDir  string
	ClaudeDir string
	Images    map[string]string
}

func (l *loader) sandbox() SandboxConfig {
	s := SandboxConfig{
		Mode:      l.oneOf(EnvSandbox, DefaultSandbox, SandboxNone, SandboxDocker),
		DockerBin: l.str(EnvDockerBin, DefaultDockerBin),
		Memory:    l.pattern(EnvSandboxMemory, DefaultSandboxMemory, memoryPattern),
		CPUs:      l.pattern(EnvSandboxCPUs, DefaultSandboxCPUs, cpusPattern),
		CodexDir:  l.absPath(EnvSandboxCodexDir, ""),
		ClaudeDir: l.absPath(EnvSandboxClaudeDir, ""),
		Images:    make(map[string]string, len(DefaultSandboxImages)),
	}
	for kind, def := range DefaultSandboxImages {
		s.Images[kind] = l.pattern(EnvSandboxImagePrefix+kind, def, imagePattern)
	}
	return s
}

// pattern returns the value of key, or def when it is empty, and fails
// when the value does not match re.
func (l *loader) pattern(key, def string, re *regexp.Regexp) string {
	v := l.str(key, def)
	if !re.MatchString(v) {
		l.fail("%s has an invalid value %q", key, v)
		return def
	}
	return v
}

// requireSandbox checks the paths that become bind mounts: the docker
// --mount option is comma-separated, so a path with a comma cannot be
// mounted.
func (c *Config) requireSandbox() error {
	if c.Engine != EngineCLI || c.Sandbox.Mode != SandboxDocker {
		return nil
	}
	for _, p := range []struct{ env, path string }{
		{EnvDataDir, c.DataDir}, {EnvWorkDir, c.WorkDir}, {EnvCLIHome, c.CLI.Home},
		{EnvSkillDir, c.CLI.SkillDir}, {EnvCodexAuth, c.CLI.CodexAuth},
		{EnvClaudeSettings, c.CLI.ClaudeSettings},
		{EnvSandboxCodexDir, c.Sandbox.CodexDir}, {EnvSandboxClaudeDir, c.Sandbox.ClaudeDir},
	} {
		if strings.Contains(p.path, ",") {
			return fmt.Errorf("%s must not contain a comma with %s=%s, got %q", p.env, EnvSandbox, SandboxDocker, p.path)
		}
	}
	return nil
}
