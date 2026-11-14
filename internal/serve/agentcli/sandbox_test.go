//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeDocker writes a docker stand-in that logs every call to dir/calls.
// `image inspect` succeeds once dir/have-image exists, which `pull`
// creates unless dir/pull-fails exists; `ps` prints dir/ps; `kill` stops
// the process in dir/pid. `run` records its arguments and image, then runs
// the command with the env file in the -w directory, as a container would.
func fakeDocker(t *testing.T, dir string) string {
	t.Helper()
	return writeScript(t, t.TempDir(), "docker", `rec=`+dir+`
echo "$*" >> $rec/calls
case "$1" in
image) [ -f $rec/have-image ] && exit 0; echo "No such image" >&2; exit 1 ;;
pull) [ -f $rec/pull-fails ] && { echo "pull access denied" >&2; exit 1; }; touch $rec/have-image; exit 0 ;;
ps) [ -f $rec/ps-fails ] && exit 1; cat $rec/ps 2>/dev/null; exit 0 ;;
kill) [ -f $rec/pid ] && kill "$(cat $rec/pid)"; exit 0 ;;
rm) [ -f $rec/rm-fails ] && { echo "rm denied" >&2; exit 1; }; exit 0 ;;
run) ;;
*) exit 2 ;;
esac
printf '%s\n' "$@" > $rec/run-args
shift
while [ $# -gt 0 ]; do
  case "$1" in
  --rm|-i|--init) shift ;;
  --env-file) envf=$2; shift 2 ;;
  -w) wd=$2; shift 2 ;;
  -*) shift 2 ;;
  *) break ;;
  esac
done
echo "$1" > $rec/image
shift
while IFS= read -r kv; do export "$kv"; done < "$envf"
cd "$wd" || exit 125
echo $$ > $rec/pid
exec "$@"
`)
}

type sandboxFixture struct {
	fixture
	dockerRec string
	cache     string
	sb        *Sandbox
}

func newSandboxFixture(t *testing.T) sandboxFixture {
	t.Helper()
	f := sandboxFixture{fixture: newFixture(t), dockerRec: t.TempDir(), cache: filepath.Join(t.TempDir(), "cache")}
	f.sb = &Sandbox{
		Docker: fakeDocker(t, f.dockerRec), UID: 1000, GID: 1001, Memory: "6g", CPUs: "4",
		Images: Images{Go: "golang:1-bookworm", Dotnet: "dotnet-sdk", Rust: "rust", Node: "node",
			Python: "python", Default: "buildpack-deps:bookworm"},
		CacheDir: f.cache,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Getenv:   func(k string) string { return map[string]string{"PATH": "/usr/bin:/bin"}[k] },
	}
	if err := f.sb.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f sandboxFixture) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dockerRec, "calls"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (f sandboxFixture) container() string {
	return ContainerPrefix + strings.TrimPrefix(filepath.Base(f.run.Dir), runPrefix)
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// argsAfter returns every value that follows flag in args.
func argsAfter(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestSandboxRunsCodexReviewInAContainer(t *testing.T) {
	f := newSandboxFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authDir := t.TempDir()
	auth := filepath.Join(authDir, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	bin := writeScript(t, installDir, "codex", recorder(f.rec)+codexOut+
		`printf '{"summary":"ok","findings":[]}' > "$out"
`)
	skill := t.TempDir()
	sb := f.sb.WithMounts(Mount{Path: installDir, ReadOnly: true}, Mount{Path: authDir}, Mount{Path: skill, ReadOnly: true})
	inv := f.inv("review this")
	inv.ReadDirs = []string{filepath.Join(skill, "skills", "ak-review-pr"), f.run.Input, "/elsewhere"}
	got, err := Codex{Bin: bin, AuthFile: auth, Sandbox: sb}.Exec(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"summary":"ok","findings":[]}` {
		t.Fatalf("message %q", got)
	}
	if len(f.sb.Mounts) != 0 {
		t.Fatal("WithMounts changed the shared sandbox")
	}

	name := f.container()
	calls := f.calls(t)
	for _, want := range []string{"image inspect --format {{.Id}} golang:1-bookworm",
		"pull --quiet golang:1-bookworm", "rm -f " + name} {
		if !hasCall(calls, want) {
			t.Errorf("calls %q miss %q", calls, want)
		}
	}
	if hasCall(calls, "kill "+name) {
		t.Error("a finished run was killed")
	}
	runArgs := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(f.dockerRec, "run-args"))), "\n")
	end := 0
	for end < len(runArgs) && runArgs[end] != "--mount" {
		end++
	}
	head := strings.Join(runArgs[:end], " ")
	wantHead := "run --rm -i --name " + name + " --init --user 1000:1001 --cap-drop ALL --security-opt no-new-privileges" +
		" --pids-limit 1024 --memory 6g --cpus 4 --network bridge -w " + f.repo +
		" --env-file " + filepath.Join(f.run.Dir, "sandbox.env")
	if head != wantHead {
		t.Fatalf("run args\n%s\nwant\n%s", head, wantHead)
	}
	mounts := argsAfter(runArgs, "--mount")
	wantMounts := []string{
		"type=bind,source=" + filepath.Join(f.run.Dir, "passwd") + ",target=/etc/passwd,readonly",
		"type=bind,source=" + filepath.Join(f.run.Dir, "group") + ",target=/etc/group,readonly",
		"type=bind,source=" + f.repo + ",target=" + f.repo + ",readonly",
		"type=bind,source=" + f.run.Dir + ",target=" + f.run.Dir,
		"type=bind,source=" + installDir + ",target=" + installDir + ",readonly",
		"type=bind,source=" + authDir + ",target=" + authDir,
		"type=bind,source=" + skill + ",target=" + skill + ",readonly",
		"type=bind,source=/elsewhere,target=/elsewhere,readonly",
	}
	for _, l := range cacheLangs {
		c := filepath.Join(f.cache, l)
		wantMounts = append(wantMounts, "type=bind,source="+c+",target="+c)
	}
	if strings.Join(mounts, "\n") != strings.Join(wantMounts, "\n") {
		t.Fatalf("mounts\n%s\nwant\n%s", strings.Join(mounts, "\n"), strings.Join(wantMounts, "\n"))
	}
	if i := len(runArgs) - 1; runArgs[i] != "-" || !strings.Contains(strings.Join(runArgs, " "), "golang:1-bookworm "+bin+" exec --json") {
		t.Fatalf("command %q", runArgs)
	}
	if readFile(t, filepath.Join(f.rec, "stdin")) != "review this" {
		t.Fatal("prompt did not reach the container stdin")
	}
	env := readFile(t, filepath.Join(f.rec, "env"))
	for _, want := range []string{"CODEX_HOME=" + f.run.Home, "HOME=" + f.run.Home, "TMPDIR=" + f.run.Tmp,
		"PATH=" + filepath.Join(f.cache, "cargo", "bin") + ":" + sandboxPath,
		"GOMODCACHE=" + filepath.Join(f.cache, "go", "mod"), "GOFLAGS=-modcacherw",
		"npm_config_cache=" + filepath.Join(f.cache, "npm"), "NUGET_PACKAGES=" + filepath.Join(f.cache, "nuget"),
		"CARGO_HOME=" + filepath.Join(f.cache, "cargo"), "PIP_CACHE_DIR=" + filepath.Join(f.cache, "pip")} {
		if !strings.Contains(env, want+"\n") {
			t.Errorf("container env misses %s", want)
		}
	}
	if strings.Contains(env, "glpat-secret") || strings.Contains(strings.Join(runArgs, " "), "glpat-secret") {
		t.Fatal("service secret reached the container")
	}
	passwd := readFile(t, filepath.Join(f.run.Dir, "passwd"))
	if !strings.Contains(passwd, "ocr:x:1000:1001:ocr:"+f.run.Home+":/bin/sh") {
		t.Fatalf("passwd %q", passwd)
	}
}

func TestSandboxClaudeFixGetsBashAndAWritableCheckout(t *testing.T) {
	result, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "result": "{}"})
	cases := []struct {
		name    string
		sandbox bool
		edit    bool
		tools   string
	}{
		{"fix with docker", true, true, claudeEditTools + ",Bash"},
		{"fix without sandbox", false, true, claudeEditTools},
		{"review with docker", true, false, claudeReadTools},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSandboxFixture(t)
			if err := os.WriteFile(filepath.Join(f.repo, "package.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := writeScript(t, t.TempDir(), "claude", recorder(f.rec)+"echo '"+string(result)+"'\n")
			c := Claude{Bin: bin, SettingsFile: writeSettings(t, `{"model":"sonnet"}`)}
			if tc.sandbox {
				c.Sandbox = f.sb
			}
			inv := f.inv("fix")
			inv.Edit = tc.edit
			if _, err := c.Exec(context.Background(), inv); err != nil {
				t.Fatal(err)
			}
			if got := argsAfter(f.lines(t, "args"), "--allowedTools"); len(got) != 1 || got[0] != tc.tools {
				t.Fatalf("tools %q, want %q", got, tc.tools)
			}
			if !tc.sandbox {
				for _, c := range f.calls(t) {
					if strings.HasPrefix(c, "run ") {
						t.Fatal("docker ran without a sandbox")
					}
				}
				return
			}
			repoMount := "type=bind,source=" + f.repo + ",target=" + f.repo
			if !tc.edit {
				repoMount += ",readonly"
			}
			runArgs := strings.Split(readFile(t, filepath.Join(f.dockerRec, "run-args")), "\n")
			if !hasCall(argsAfter(runArgs, "--mount"), repoMount) {
				t.Fatalf("checkout mount missing %q in %q", repoMount, runArgs)
			}
			if strings.TrimSpace(readFile(t, filepath.Join(f.dockerRec, "image"))) != "node" {
				t.Fatal("package.json did not pick the Node image")
			}
		})
	}
}

func TestSandboxKillsAndRemovesTheContainerOnTimeout(t *testing.T) {
	f := newSandboxFixture(t)
	if err := os.WriteFile(filepath.Join(f.dockerRec, "have-image"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, t.TempDir(), "codex", "exec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Codex{Bin: bin, AuthFile: filepath.Join(t.TempDir(), "auth.json"), Sandbox: f.sb}.Exec(ctx, f.inv("x"))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "codex stopped") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the run outlived its timeout")
	}
	name := f.container()
	calls := f.calls(t)
	if !hasCall(calls, "kill "+name) || !hasCall(calls, "rm -f "+name) {
		t.Fatalf("calls %q", calls)
	}
	if hasCall(calls, "pull --quiet golang:1-bookworm") {
		t.Fatal("a present image was pulled")
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, filepath.Join(f.dockerRec, "pid")))); err == nil {
		if err := syscall.Kill(pid, 0); err == nil {
			t.Fatalf("process %d still runs", pid)
		}
	}
}

func TestSandboxReportsTheCLIExitCode(t *testing.T) {
	f := newSandboxFixture(t)
	bin := writeScript(t, t.TempDir(), "codex", "echo broken >&2; exit 3\n")
	_, err := Codex{Bin: bin, AuthFile: filepath.Join(t.TempDir(), "auth.json"), Sandbox: f.sb}.Exec(context.Background(), f.inv("x"))
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Name != "codex" || exitErr.Code != 3 || exitErr.Stderr != "broken" {
		t.Fatalf("err %v", err)
	}
	if !hasCall(f.calls(t), "rm -f "+f.container()) {
		t.Fatal("container not removed after a failed run")
	}
}

func TestSandboxPullFailureStopsTheRun(t *testing.T) {
	f := newSandboxFixture(t)
	if err := os.WriteFile(filepath.Join(f.dockerRec, "pull-fails"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Codex{Bin: "/bin/true", AuthFile: filepath.Join(t.TempDir(), "auth.json"), Sandbox: f.sb}.Exec(context.Background(), f.inv("x"))
	if err == nil || !strings.Contains(err.Error(), "pull sandbox image buildpack-deps:bookworm") ||
		!strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("err %v", err)
	}
	for _, c := range f.calls(t) {
		if strings.HasPrefix(c, "run ") {
			t.Fatal("the CLI ran without its image")
		}
	}
}

func TestSandboxRejectsACommaMount(t *testing.T) {
	f := newSandboxFixture(t)
	if err := os.WriteFile(filepath.Join(f.dockerRec, "have-image"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := f.sb.WithMounts(Mount{Path: "/opt/a,b", ReadOnly: true})
	_, err := Codex{Bin: "/bin/true", AuthFile: filepath.Join(t.TempDir(), "auth.json"), Sandbox: sb}.Exec(context.Background(), f.inv("x"))
	if err == nil || !strings.Contains(err.Error(), "contains a comma") {
		t.Fatalf("err %v", err)
	}
}

func TestSandboxInitSweepsLeftoverContainers(t *testing.T) {
	f := newSandboxFixture(t)
	for _, l := range cacheLangs {
		if st, err := os.Stat(filepath.Join(f.cache, l)); err != nil || !st.IsDir() {
			t.Fatalf("cache %s: %v", l, err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.dockerRec, "ps"), []byte("ocr-run-1\nother-app\nocr-run-22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.sb.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	if !hasCall(calls, "ps -a --filter name=^ocr-run- --format {{.Names}}") ||
		!hasCall(calls, "rm -f ocr-run-1") || !hasCall(calls, "rm -f ocr-run-22") || hasCall(calls, "rm -f other-app") {
		t.Fatalf("calls %q", calls)
	}

	if err := os.WriteFile(filepath.Join(f.dockerRec, "rm-fails"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.sb.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "rm denied") {
		t.Fatalf("rm failure: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.dockerRec, "ps-fails"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.sb.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "list sandbox containers") {
		t.Fatalf("ps failure: %v", err)
	}
	blocked := &Sandbox{Docker: f.sb.Docker, CacheDir: filepath.Join(writeScript(t, t.TempDir(), "file", ""), "cache")}
	if err := blocked.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "create sandbox cache") {
		t.Fatalf("cache failure: %v", err)
	}
	missing := &Sandbox{Docker: filepath.Join(t.TempDir(), "no-docker"), CacheDir: t.TempDir()}
	if err := missing.Init(context.Background()); err == nil {
		t.Fatal("a missing docker binary passed")
	}
}

func TestDetectImage(t *testing.T) {
	im := Images{Go: "go", Dotnet: "dotnet", Rust: "rust", Node: "node", Python: "python", Default: "default"}
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{"empty", nil, "default"},
		{"go root", []string{"go.mod"}, "go"},
		{"csproj", []string{"src/App.csproj"}, "dotnet"},
		{"sln", []string{"App.sln"}, "dotnet"},
		{"global.json", []string{"global.json"}, "dotnet"},
		{"cargo", []string{"Cargo.toml"}, "rust"},
		{"node", []string{"package.json"}, "node"},
		{"pyproject", []string{"pyproject.toml"}, "python"},
		{"requirements", []string{"svc/requirements.txt"}, "python"},
		{"go wins over node", []string{"package.json", "backend/go.mod"}, "go"},
		{"dotnet wins over rust", []string{"Cargo.toml", "api/Api.csproj"}, "dotnet"},
		{"rust wins over python", []string{"requirements.txt", "Cargo.toml"}, "rust"},
		{"node wins over python", []string{"pyproject.toml", "web/package.json"}, "node"},
		{"two levels down is ignored", []string{"a/b/go.mod"}, "default"},
		{"hidden dir is ignored", []string{".github/package.json"}, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				p := filepath.Join(dir, f)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := DetectImage(dir, im); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := DetectImage(filepath.Join(t.TempDir(), "missing"), im); got != "default" {
		t.Fatalf("missing dir: %q", got)
	}
}

func TestSandboxRootUserKeepsTheImageUsers(t *testing.T) {
	sb := &Sandbox{}
	users, err := sb.writeUserFiles(t.TempDir())
	if err != nil || users != nil {
		t.Fatalf("users %v err %v", users, err)
	}
	sb.UID = 1000
	if _, err := sb.writeUserFiles(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("write into a missing dir passed")
	}
}

func TestWriteEnvFileRejectsLineBreaks(t *testing.T) {
	err := writeEnvFile(filepath.Join(t.TempDir(), "env"), []string{"A=1", "HTTPS_PROXY=x\ny"})
	if err == nil || !strings.Contains(err.Error(), "HTTPS_PROXY") {
		t.Fatalf("err %v", err)
	}
	if err := writeEnvFile(filepath.Join(t.TempDir(), "missing", "env"), nil); err == nil {
		t.Fatal("write into a missing dir passed")
	}
}

func TestSandboxDefaultsToProcessEnvAndLogger(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	t.Setenv("OCR_SERVE_GITLAB_TOKEN", "glpat-secret")
	sb := &Sandbox{}
	env := strings.Join(sb.clientEnv(), "\n")
	if !strings.Contains(env, "DOCKER_HOST=unix:///run/user/1000/docker.sock") || strings.Contains(env, "glpat-secret") {
		t.Fatalf("client env %q", env)
	}
	if sb.logger() == nil {
		t.Fatal("no logger")
	}
}

func TestRunDirRemoveClearsReadOnlyDirs(t *testing.T) {
	h, err := NewHomes(filepath.Join(t.TempDir(), "cli"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.New()
	if err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(r.Home, "go", "pkg", "mod", "example.com@v1")
	if err := os.MkdirAll(mod, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{mod, filepath.Dir(mod)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Dir); !os.IsNotExist(err) {
		t.Fatalf("run dir left: %v", err)
	}
}
