//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recorder is the part of a fake CLI that writes its arguments, environment,
// stdin and working directory into rec.
func recorder(rec string) string {
	return `printf '%s\n' "$@" > ` + rec + `/args
env > ` + rec + `/env
cat > ` + rec + `/stdin
pwd > ` + rec + `/pwd
`
}

// codexOut finds the -o value of a codex invocation.
const codexOut = `out=""; prev=""
for a in "$@"; do if [ "$prev" = "-o" ]; then out="$a"; fi; prev="$a"; done
`

type fixture struct {
	rec  string
	repo string
	run  *RunDir
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	h, err := NewHomes(filepath.Join(t.TempDir(), "cli"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.New()
	if err != nil {
		t.Fatal(err)
	}
	return fixture{rec: t.TempDir(), repo: t.TempDir(), run: r}
}

func (f fixture) inv(prompt string) Invocation {
	vars := map[string]string{"PATH": "/usr/bin:/bin", "OCR_SERVE_GITLAB_TOKEN": "glpat-secret"}
	return Invocation{
		RepoDir: f.repo, Prompt: prompt, Run: f.run, Grace: 200 * time.Millisecond,
		Getenv: func(k string) string { return vars[k] },
	}
}

func (f fixture) lines(t *testing.T, name string) []string {
	t.Helper()
	return strings.Split(strings.TrimSpace(readFile(t, filepath.Join(f.rec, name))), "\n")
}

func TestCodexExecReadsLastMessageFile(t *testing.T) {
	f := newFixture(t)
	auth := filepath.Join(t.TempDir(), "auth.json")
	bin := writeScript(t, t.TempDir(), "codex", recorder(f.rec)+codexOut+
		`readlink "$CODEX_HOME/auth.json" > `+f.rec+`/auth
printf '{"summary":"ok","findings":[]}' > "$out"
echo '{"type":"item.completed","item":{"type":"agent_message","text":"from the stream"}}'
`)
	inv := f.inv("review this")
	inv.Schema = []byte(`{"type":"object"}`)
	c := Codex{Bin: bin, Model: "gpt-6.1-sol", Effort: "medium", AuthFile: auth}
	if c.Name() != "codex" {
		t.Fatal(c.Name())
	}
	got, err := c.Exec(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"summary":"ok","findings":[]}` {
		t.Fatalf("message %q", got)
	}
	want := []string{"exec", "--json", "--skip-git-repo-check", "--ephemeral", "--sandbox", "read-only",
		"-C", f.repo, "-o", filepath.Join(f.run.Out, "last.json"), "-c", "project_doc_max_bytes=0",
		"--output-schema", filepath.Join(f.run.Out, "schema.json"), "-m", "gpt-6.1-sol",
		"-c", `model_reasoning_effort="medium"`, "-"}
	if got := f.lines(t, "args"); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("args\n%q\nwant\n%q", got, want)
	}
	if readFile(t, filepath.Join(f.rec, "stdin")) != "review this" {
		t.Fatal("prompt not on stdin")
	}
	if strings.TrimSpace(readFile(t, filepath.Join(f.rec, "auth"))) != auth {
		t.Fatal("auth.json is not linked to the host file")
	}
	env := readFile(t, filepath.Join(f.rec, "env"))
	for _, want := range []string{"CODEX_HOME=" + f.run.Home, "HOME=" + f.run.Home, "TMPDIR=" + f.run.Tmp} {
		if !strings.Contains(env, want) {
			t.Errorf("env misses %s", want)
		}
	}
	if strings.Contains(env, "glpat-secret") {
		t.Fatal("service secret reached the CLI")
	}
	if readFile(t, filepath.Join(f.run.Out, "schema.json")) != `{"type":"object"}` {
		t.Fatal("schema not written")
	}
}

func TestCodexExecFallsBackToTheStream(t *testing.T) {
	cases := []struct {
		name, stream, want, wantErr string
	}{
		{"item events", `{"type":"thread.started"}
not json
{"type":"item.completed","item":{"type":"agent_message","text":"first"}}
{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}
{"type":"item.completed","item":{"type":"agent_message","text":"final"}}`, "final", ""},
		{"msg events", `{"msg":{"type":"agent_message","message":"old shape"}}`, "old shape", ""},
		{"turn failed", `{"type":"turn.failed","error":{"message":"usage limit reached"}}`, "", "codex failed: usage limit reached"},
		{"error event", `{"type":"error","message":"stream disconnected"}`, "", "codex failed: stream disconnected"},
		{"nothing", `{"type":"turn.completed"}`, "", "no agent message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			stream := filepath.Join(t.TempDir(), "stream")
			if err := os.WriteFile(stream, []byte(tc.stream+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := writeScript(t, t.TempDir(), "codex", "cat >/dev/null; cat "+stream+"\n")
			got, err := Codex{Bin: bin, AuthFile: "/host/auth.json"}.Exec(context.Background(), f.inv("p"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q err %v", got, err)
			}
		})
	}
}

func TestCodexExecMinimalArgs(t *testing.T) {
	f := newFixture(t)
	bin := writeScript(t, t.TempDir(), "codex", recorder(f.rec)+codexOut+`echo '{}' > "$out"`+"\n")
	if _, err := (Codex{Bin: bin, AuthFile: "/a"}).Exec(context.Background(), f.inv("p")); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(f.lines(t, "args"), " ")
	for _, absent := range []string{"--output-schema", "-m", "model_reasoning_effort"} {
		if strings.Contains(args, absent) {
			t.Errorf("args hold %s: %s", absent, args)
		}
	}
}

func TestCodexExecErrors(t *testing.T) {
	f := newFixture(t)
	failing := writeScript(t, t.TempDir(), "codex", "echo 'not logged in' >&2; exit 1\n")
	var exitErr *ExitError
	if _, err := (Codex{Bin: failing, AuthFile: "/a"}).Exec(context.Background(), f.inv("p")); !errors.As(err, &exitErr) ||
		!strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err %v", err)
	}
	// The link from the first run is still there.
	if _, err := (Codex{Bin: failing, AuthFile: "/a"}).Exec(context.Background(), f.inv("p")); err == nil ||
		!strings.Contains(err.Error(), "link codex auth") {
		t.Fatalf("err %v", err)
	}

	f = newFixture(t)
	if err := os.RemoveAll(f.run.Out); err != nil {
		t.Fatal(err)
	}
	inv := f.inv("p")
	inv.Schema = []byte("{}")
	if _, err := (Codex{Bin: failing, AuthFile: "/a"}).Exec(context.Background(), inv); err == nil ||
		!strings.Contains(err.Error(), "write output schema") {
		t.Fatalf("err %v", err)
	}
	f = newFixture(t)
	if err := os.RemoveAll(f.run.Out); err != nil {
		t.Fatal(err)
	}
	if _, err := (Codex{Bin: failing, AuthFile: "/a"}).Exec(context.Background(), f.inv("p")); err == nil ||
		!strings.Contains(err.Error(), "write prompt") {
		t.Fatalf("err %v", err)
	}
	if _, err := codexStreamMessage(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("missing stream accepted")
	}
}

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeExec(t *testing.T) {
	f := newFixture(t)
	settings := writeSettings(t, `{"env":{"ANTHROPIC_BASE_URL":"http://proxy"},"model":"sonnet",
		"hooks":{"Stop":[{"command":"curl evil"}]},"permissions":{"allow":["Bash"]}}`)
	result := filepath.Join(t.TempDir(), "result.json")
	body, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"result": "```json\n{\"findings\":[]}\n```"})
	if err := os.WriteFile(result, body, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, t.TempDir(), "claude", recorder(f.rec)+
		`cp "$CLAUDE_CONFIG_DIR/settings.json" `+f.rec+`/settings
cat `+result+"\n")
	inv := f.inv("review")
	inv.ReadDirs = []string{"/skills/ak-review-pr", f.run.Input}
	c := Claude{Bin: bin, Model: "claude-sonnet-5-5", SettingsFile: settings}
	if c.Name() != "claude" {
		t.Fatal(c.Name())
	}
	got, err := c.Exec(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if got != "```json\n{\"findings\":[]}\n```" {
		t.Fatalf("result %q", got)
	}
	want := []string{"-p", "--output-format", "json", "--no-session-persistence", "--permission-mode", "default",
		"--allowedTools", "Read,Grep,Glob", "--setting-sources", "user", "--strict-mcp-config",
		"--model", "claude-sonnet-5-5", "--add-dir", "/skills/ak-review-pr", "--add-dir", f.run.Input}
	if got := f.lines(t, "args"); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("args\n%q\nwant\n%q", got, want)
	}
	pwd, _ := filepath.EvalSymlinks(strings.TrimSpace(readFile(t, filepath.Join(f.rec, "pwd"))))
	repo, _ := filepath.EvalSymlinks(f.repo)
	if pwd != repo {
		t.Fatalf("cwd %s, want %s", pwd, repo)
	}
	var kept map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(f.rec, "settings"))), &kept); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 || string(kept["model"]) != `"sonnet"` || !strings.Contains(string(kept["env"]), "http://proxy") {
		t.Fatalf("settings %v", kept)
	}
	env := readFile(t, filepath.Join(f.rec, "env"))
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR="+f.run.Home) || strings.Contains(env, "glpat-secret") {
		t.Fatalf("env\n%s", env)
	}
}

func TestClaudeExecResultErrors(t *testing.T) {
	cases := []struct{ name, out, want string }{
		{"is_error", `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 401"}`, "claude reported an error (error_during_execution): API Error: 401"},
		{"not json", `Error: invalid API key`, "not a JSON result"},
		{"empty result", `{"type":"result","subtype":"success","is_error":false,"result":"  "}`, "empty result"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			out := filepath.Join(t.TempDir(), "out")
			if err := os.WriteFile(out, []byte(tc.out), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := writeScript(t, t.TempDir(), "claude", "cat >/dev/null; cat "+out+"\n")
			_, err := Claude{Bin: bin, SettingsFile: writeSettings(t, "{}")}.Exec(context.Background(), f.inv("p"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestClaudeExecSetupErrors(t *testing.T) {
	bin := writeScript(t, t.TempDir(), "claude", "exit 2\n")
	cases := []struct {
		name     string
		settings string
		edit     func(fixture)
		want     string
	}{
		{"missing settings", filepath.Join(t.TempDir(), "none.json"), nil, "read claude settings"},
		{"settings not an object", writeSettings(t, `["env"]`), nil, "is not a JSON object"},
		{"settings null", writeSettings(t, `null`), nil, "is not a JSON object"},
		{"home gone", writeSettings(t, `{}`), func(f fixture) { _ = os.RemoveAll(f.run.Home) }, "write claude settings"},
		{"out gone", writeSettings(t, `{}`), func(f fixture) { _ = os.RemoveAll(f.run.Out) }, "write prompt"},
		{"exit code", writeSettings(t, `{}`), nil, "claude exited with code 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.edit != nil {
				tc.edit(f)
			}
			_, err := Claude{Bin: bin, SettingsFile: tc.settings}.Exec(context.Background(), f.inv("p"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v", err)
			}
		})
	}
	if _, err := claudeResult(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("missing output accepted")
	}
}

func TestInvocationDefaultsToProcessEnv(t *testing.T) {
	t.Setenv("PATH", "/custom/bin")
	inv := Invocation{Run: &RunDir{Home: "/h", Tmp: "/t"}}
	if env := strings.Join(inv.env("X=1"), " "); !strings.Contains(env, "PATH=/custom/bin") || !strings.HasSuffix(env, "X=1") {
		t.Fatalf("env %s", env)
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct{ name, in, want, wantErr string }{
		{"plain", `{"a":1}`, `{"a":1}`, ""},
		{"fenced", "```json\n{\"a\": {\"b\": 2}}\n```", `{"a": {"b": 2}}`, ""},
		{"with prose", "Here it is:\n{\"a\":1}\nDone.", `{"a":1}`, ""},
		{"none", "no findings", "", "no JSON object"},
		{"reversed", "} {", "", "no JSON object"},
		{"malformed", `{"a":}`, "", "malformed JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractJSON(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v", err)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %q err %v", got, err)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if truncate(" short ", 10) != "short" {
		t.Fatal("short text changed")
	}
	if got := truncate("0123456789", 4); got != "0123..." {
		t.Fatalf("truncate %q", got)
	}
}
