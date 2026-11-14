// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ContainerPrefix starts the name of every sandbox container, so the
// startup sweep finds the ones a stopped service left.
const ContainerPrefix = "ocr-run-"

// dockerCleanupTimeout bounds a docker kill or rm that runs after the run
// context ended.
const dockerCleanupTimeout = 30 * time.Second

// sandboxPath covers the toolchains of the sandbox images: Go, Rust (the
// rustup proxies stay in /usr/local/cargo/bin when CARGO_HOME moves to the
// cache), .NET, Node and Python install under the usual system paths.
const sandboxPath = "/usr/local/go/bin:/go/bin:/usr/local/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// cacheLangs are the toolchain cache directories under the cache root.
var cacheLangs = []string{"go", "npm", "nuget", "cargo", "pip"}

// dockerEnvKeys are the service variables the docker client may see. The
// container gets its environment from an env file instead, so nothing here
// reaches the CLI.
var dockerEnvKeys = []string{
	"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT",
	"DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY", "XDG_RUNTIME_DIR",
}

// Images are the sandbox images by toolchain.
type Images struct {
	Go, Dotnet, Rust, Node, Python, Default string
}

// Mount is a host path bound into the container at the same path.
type Mount struct {
	Path     string
	ReadOnly bool
}

// Sandbox runs each CLI process in a throwaway docker container. The
// container sees only its mounts: the checkout, the run directory, the
// static Mounts (CLI install, credential files, skill) and the toolchain
// caches under CacheDir; never the docker socket. UID and GID are the
// service user, so the files the CLI writes stay the service's own.
type Sandbox struct {
	Docker   string
	UID, GID int
	Memory   string
	CPUs     string
	Images   Images
	CacheDir string
	Mounts   []Mount
	AddHosts []string
	Log      *slog.Logger
	// Getenv supplies the docker client environment; nil means os.Getenv.
	Getenv func(string) string
}

// WithMounts returns a copy of s with extra static mounts, for one CLI.
func (s *Sandbox) WithMounts(m ...Mount) *Sandbox {
	c := *s
	c.Mounts = append(append([]Mount(nil), s.Mounts...), m...)
	return &c
}

// Init creates the cache directories and removes the containers a stopped
// service left. Call it before the first run and before the run
// directories are swept, so no leftover container still writes into them.
func (s *Sandbox) Init(ctx context.Context) error {
	for _, l := range cacheLangs {
		if err := os.MkdirAll(filepath.Join(s.CacheDir, l), 0o700); err != nil {
			return fmt.Errorf("create sandbox cache: %w", err)
		}
	}
	out, err := s.docker(ctx, "ps", "-a", "--filter", "name=^"+ContainerPrefix, "--format", "{{.Names}}")
	if err != nil {
		return fmt.Errorf("list sandbox containers: %w", err)
	}
	var errs []error
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(name, ContainerPrefix) {
			continue
		}
		if _, err := s.docker(ctx, "rm", "-f", name); err != nil {
			errs = append(errs, fmt.Errorf("remove container %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// DetectImage picks the image for a checkout from the build files in its
// root and one directory level below, for monorepos. When several match,
// Go wins over .NET, then Rust, Node and Python.
func DetectImage(dir string, im Images) string {
	names := map[string]bool{}
	exts := map[string]bool{}
	var subdirs []string
	scan := func(d string, top bool) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			switch n := e.Name(); {
			case !e.IsDir():
				names[n] = true
				exts[filepath.Ext(n)] = true
			case top && !strings.HasPrefix(n, "."):
				subdirs = append(subdirs, filepath.Join(d, n))
			}
		}
	}
	scan(dir, true)
	for _, d := range subdirs {
		scan(d, false)
	}
	switch {
	case names["go.mod"]:
		return im.Go
	case exts[".csproj"] || exts[".sln"] || names["global.json"]:
		return im.Dotnet
	case names["Cargo.toml"]:
		return im.Rust
	case names["package.json"]:
		return im.Node
	case names["pyproject.toml"] || names["requirements.txt"]:
		return im.Python
	default:
		return im.Default
	}
}

// run executes p inside a container named after the run. When ctx ends the
// container is killed, which ends the docker client; the container is
// removed in every case, also when the client was killed before it could.
func (s *Sandbox) run(ctx context.Context, inv Invocation, p Process) error {
	name := ContainerPrefix + strings.TrimPrefix(filepath.Base(inv.Run.Dir), runPrefix)
	image := DetectImage(inv.RepoDir, s.Images)
	if err := s.ensureImage(ctx, image); err != nil {
		return err
	}
	args, err := s.runArgs(inv, p, name, image)
	if err != nil {
		return err
	}
	dp := p
	dp.Name = p.name()
	dp.Bin, dp.Args, dp.Env, dp.Dir = s.Docker, args, s.clientEnv(), inv.Run.Dir

	killed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(killed)
		kctx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer cancel()
		_, _ = s.docker(kctx, "kill", name)
	})
	runErr := Run(ctx, dp)
	if !stop() {
		<-killed
	}
	rctx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()
	// --rm usually removed the container already; rm then reports a
	// missing container, which is the state wanted.
	_, _ = s.docker(rctx, "rm", "-f", name)
	return runErr
}

// runArgs builds the docker run arguments and writes the container
// environment and user files into the run directory.
func (s *Sandbox) runArgs(inv Invocation, p Process, name, image string) ([]string, error) {
	envFile := filepath.Join(inv.Run.Dir, "sandbox.env")
	if err := writeEnvFile(envFile, s.containerEnv(p.Env)); err != nil {
		return nil, err
	}
	args := []string{"run", "--rm", "-i", "--name", name, "--init",
		"--user", strconv.Itoa(s.UID) + ":" + strconv.Itoa(s.GID),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", "1024", "--memory", s.Memory, "--cpus", s.CPUs,
		"--network", "bridge", "-w", inv.RepoDir, "--env-file", envFile}
	for _, h := range s.AddHosts {
		args = append(args, "--add-host", h)
	}
	users, err := s.writeUserFiles(inv.Run.Dir)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		args = append(args, "--mount", "type=bind,source="+u[0]+",target="+u[1]+",readonly")
	}
	for _, m := range s.mounts(inv) {
		if strings.Contains(m.Path, ",") {
			return nil, fmt.Errorf("sandbox mount %q contains a comma", m.Path)
		}
		opt := "type=bind,source=" + m.Path + ",target=" + m.Path
		if m.ReadOnly {
			opt += ",readonly"
		}
		args = append(args, "--mount", opt)
	}
	return append(append(args, image, p.Bin), p.Args...), nil
}

// mounts lists the bind mounts of one run: the checkout (writable only for
// an edit run), the run directory, the read directories not already
// inside another mount, the static mounts and the caches.
func (s *Sandbox) mounts(inv Invocation) []Mount {
	ms := []Mount{{Path: inv.RepoDir, ReadOnly: !inv.Edit}, {Path: inv.Run.Dir}}
	ms = append(ms, s.Mounts...)
	for _, d := range inv.ReadDirs {
		if !covered(d, ms) {
			ms = append(ms, Mount{Path: d, ReadOnly: true})
		}
	}
	for _, l := range cacheLangs {
		ms = append(ms, Mount{Path: filepath.Join(s.CacheDir, l)})
	}
	return ms
}

func covered(path string, ms []Mount) bool {
	for _, m := range ms {
		if rel, err := filepath.Rel(m.Path, path); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

// containerEnv is the CLI environment with the sandbox PATH and the
// toolchain caches. GOFLAGS=-modcacherw keeps the module cache deletable.
func (s *Sandbox) containerEnv(env []string) []string {
	out := make([]string, 0, len(env)+8)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	c := func(l string) string { return filepath.Join(s.CacheDir, l) }
	return append(out,
		"PATH="+filepath.Join(c("cargo"), "bin")+":"+sandboxPath,
		"GOMODCACHE="+filepath.Join(c("go"), "mod"),
		"GOCACHE="+filepath.Join(c("go"), "build"),
		"GOFLAGS=-modcacherw",
		"npm_config_cache="+c("npm"),
		"NUGET_PACKAGES="+c("nuget"),
		"CARGO_HOME="+c("cargo"),
		"PIP_CACHE_DIR="+c("pip"),
	)
}

// writeEnvFile writes a docker env file. Values stay out of the docker
// arguments, where any local user could read them in the process list.
func writeEnvFile(path string, env []string) error {
	var b strings.Builder
	for _, kv := range env {
		if strings.ContainsAny(kv, "\r\n") {
			return fmt.Errorf("sandbox environment %q holds a line break", strings.SplitN(kv, "=", 2)[0])
		}
		b.WriteString(kv)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write sandbox env: %w", err)
	}
	return nil
}

// writeUserFiles gives the container user a passwd and group entry. The
// service uid exists in no image, and tools that look the user up (Node's
// os.userInfo, Python's getpass) fail without one. The files replace the
// image's own, which only needs root besides. Every user gets an empty
// /etc/profile.
func (s *Sandbox) writeUserFiles(dir string) ([][2]string, error) {
	// Codex runs its tools through a login shell (sh -lc). Debian's
	// /etc/profile then resets PATH for a non-root user and drops the image
	// toolchain (/usr/local/go/bin, /usr/local/cargo/bin), so `go test`
	// fails with "not found". An empty profile keeps the container PATH.
	files := [][3]string{{"profile", "/etc/profile", "# PATH comes from the container environment.\n"}}
	if s.UID != 0 {
		home := filepath.Join(dir, "home")
		files = append(files,
			[3]string{"passwd", "/etc/passwd", fmt.Sprintf("root:x:0:0:root:/root:/bin/sh\nocr:x:%d:%d:ocr:%s:/bin/sh\n", s.UID, s.GID, home)},
			[3]string{"group", "/etc/group", fmt.Sprintf("root:x:0:\nocr:x:%d:\n", s.GID)})
	}
	var out [][2]string
	for _, f := range files {
		p := filepath.Join(dir, f[0])
		if err := os.WriteFile(p, []byte(f[2]), 0o644); err != nil {
			return nil, fmt.Errorf("write sandbox %s: %w", f[0], err)
		}
		out = append(out, [2]string{p, f[1]})
	}
	return out, nil
}

// ensureImage pulls the image when it is not present locally; the run
// context bounds the pull.
func (s *Sandbox) ensureImage(ctx context.Context, image string) error {
	if _, err := s.docker(ctx, "image", "inspect", "--format", "{{.Id}}", image); err == nil {
		s.logger().Info("sandbox image", "image", image, "pulled", false)
		return nil
	}
	if _, err := s.docker(ctx, "pull", "--quiet", image); err != nil {
		return fmt.Errorf("pull sandbox image %s: %w", image, err)
	}
	s.logger().Info("sandbox image", "image", image, "pulled", true)
	return nil
}

func (s *Sandbox) docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, s.Docker, args...)
	cmd.Env = s.clientEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("docker %s: %w: %s", args[0], err, truncate(msg, stderrTail))
		}
		return out, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return out, nil
}

func (s *Sandbox) clientEnv() []string {
	getenv := s.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	var env []string
	for _, k := range dockerEnvKeys {
		if v := getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (s *Sandbox) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
