//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAgy writes an agy stand-in that records its call, reports the target
// of the token link and prints body as its stdout.
func fakeAgy(t *testing.T, dir, rec, body string) string {
	t.Helper()
	return writeScript(t, dir, "agy", recorder(rec)+
		`readlink "$HOME/.gemini/antigravity-cli/antigravity-oauth-token" > `+rec+`/token
`+body)
}

func agyToken(t *testing.T) string {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	if err := os.WriteFile(tok, []byte(`{"access_token":"a","refresh_token":"r"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestAgyExecRunsOutsideTheCheckout(t *testing.T) {
	f := newFixture(t)
	tok := agyToken(t)
	bin := fakeAgy(t, t.TempDir(), f.rec,
		`echo '{"conversation_id":"c1","status":"SUCCESS","response":"ignored","structured_output":{"summary":"ok","findings":[]},"num_turns":3}'
`)
	inv := f.inv("review this")
	inv.Schema = []byte(`{"type":"object"}`)
	inv.ReadDirs = []string{"/skills/review", f.run.Input}
	a := Agy{Bin: bin, Model: "gemini-4-hydrogen-high", Effort: "xhigh", TokenFile: tok}
	if a.Name() != "agy" {
		t.Fatal(a.Name())
	}
	got, err := a.Exec(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"summary":"ok","findings":[]}` {
		t.Fatalf("message %q", got)
	}
	prompt := agyPreface(f.repo) + "review this"
	want := []string{"-p", prompt, "--output-format", "json",
		"--json-schema", filepath.Join(f.run.Out, "schema.json"),
		"--dangerously-skip-permissions", "--disable-slash-commands", "--settings", agySettings,
		"--model", "gemini-4-hydrogen-high", "--effort", "xhigh",
		"--add-dir", "/skills/review", "--add-dir", f.run.Input}
	if got := readFile(t, filepath.Join(f.rec, "args")); got != strings.Join(want, "\n")+"\n" {
		t.Fatalf("args\n%q\nwant\n%q", got, want)
	}
	if !strings.Contains(prompt, "`"+f.repo+"`") {
		t.Fatal("the prompt does not name the checkout")
	}
	if readFile(t, filepath.Join(f.rec, "stdin")) != "" {
		t.Fatal("the prompt reached stdin")
	}
	if readFile(t, filepath.Join(f.run.Out, "prompt.txt")) != prompt {
		t.Fatal("prompt.txt does not hold the full prompt")
	}
	pwd, _ := filepath.EvalSymlinks(strings.TrimSpace(readFile(t, filepath.Join(f.rec, "pwd"))))
	if input, _ := filepath.EvalSymlinks(f.run.Input); pwd != input {
		t.Fatalf("cwd %s, want the run input dir %s", pwd, input)
	}
	if strings.TrimSpace(readFile(t, filepath.Join(f.rec, "token"))) != tok {
		t.Fatal("the run home token is not linked to the host file")
	}
	env := readFile(t, filepath.Join(f.rec, "env"))
	for _, want := range []string{"HOME=" + f.run.Home, "TMPDIR=" + f.run.Tmp} {
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
	if _, err := os.Stat(filepath.Join(f.run.Input, agyPromptFile)); !os.IsNotExist(err) {
		t.Fatal("a short prompt went into a file")
	}
}

func TestAgyExecPassesALongPromptAsAFile(t *testing.T) {
	f := newFixture(t)
	bin := fakeAgy(t, t.TempDir(), f.rec, `echo '{"status":"SUCCESS","response":"{}"}'
`)
	long := strings.Repeat("x", agyMaxPromptArg)
	inv := f.inv(long)
	if _, err := (Agy{Bin: bin, TokenFile: agyToken(t)}).Exec(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.run.Input, agyPromptFile)
	if readFile(t, file) != agyPreface(f.repo)+long {
		t.Fatal("the prompt file does not hold the full prompt")
	}
	args := f.lines(t, "args")
	if args[0] != "-p" || len(args[1]) > 1024 || !strings.Contains(args[1], "`"+file+"`") {
		t.Fatalf("short prompt %q", args[1])
	}
	for _, a := range args {
		if strings.Contains(a, "--json-schema") || strings.Contains(a, "--model") || strings.Contains(a, "--effort") {
			t.Fatalf("unset option passed: %q", args)
		}
	}
}

func TestAgyResult(t *testing.T) {
	cases := []struct {
		name, out, want, wantErr string
	}{
		{"structured output", `{"status":"SUCCESS","response":"text","structured_output":{"a": [1, 2]}}`, `{"a":[1,2]}`, ""},
		{"structured output as a string", `{"status":"SUCCESS","structured_output":"{\"a\":1}"}`, `{"a":1}`, ""},
		{"fenced response", "{\"status\":\"SUCCESS\",\"response\":\"```json\\n{\\\"a\\\":1}\\n```\",\"structured_output\":null}", "```json\n{\"a\":1}\n```", ""},
		{"error status", `{"status":"ERROR","error":"quota exhausted"}`, "", `agy run ended with status "ERROR": quota exhausted`},
		{"canceled with a response", `{"status":"CANCELED","response":"stopped early"}`, "", `status "CANCELED": stopped early`},
		{"error object", `{"status":"SUCCESS","response":"x","error":{"code":7,"message":"denied"}}`, "", `agy reported an error: {"code":7,"message":"denied"}`},
		{"empty response", `{"status":"SUCCESS","response":"  "}`, "", "empty response"},
		{"not json", "Signing in...\n", "", "not a JSON result"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stdout")
			if err := os.WriteFile(path, []byte(tc.out), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := agyResult(path)
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
	if _, err := agyResult(filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "read agy output") {
		t.Fatalf("missing output: %v", err)
	}
}

func TestAgyExecFailures(t *testing.T) {
	cases := []struct {
		name, body string
		want       []string
		exit       bool
	}{
		{"non-zero exit with a result", `echo '{"status":"INVALID","error":"unknown model \"nope\""}'; echo boom >&2; exit 1
`, []string{"agy exited with code 1: boom", `status "INVALID": unknown model "nope"`}, true},
		{"non-zero exit without a result", `echo crash >&2; exit 3
`, []string{"agy exited with code 3: crash"}, true},
		{"failed status with exit 0", `echo '{"status":"INTERRUPTED"}'
`, []string{`status "INTERRUPTED"`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			bin := fakeAgy(t, t.TempDir(), f.rec, tc.body)
			_, err := Agy{Bin: bin, TokenFile: agyToken(t)}.Exec(context.Background(), f.inv("p"))
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err %v misses %q", err, w)
				}
			}
			var exitErr *ExitError
			if errors.As(err, &exitErr) != tc.exit {
				t.Fatalf("exit error %v, want %v", err, tc.exit)
			}
		})
	}
}

func TestAgyExecRefusesEditsAndBadHomes(t *testing.T) {
	f := newFixture(t)
	inv := f.inv("fix")
	inv.Edit = true
	if _, err := (Agy{Bin: "/bin/true", TokenFile: agyToken(t)}).Exec(context.Background(), inv); err == nil ||
		!strings.Contains(err.Error(), "reviews only") {
		t.Fatalf("edit run: %v", err)
	}
	link := filepath.Join(f.run.Home, agyTokenPath)
	if err := os.MkdirAll(link, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := (Agy{Bin: "/bin/true", TokenFile: agyToken(t)}).Exec(context.Background(), f.inv("p")); err == nil ||
		!strings.Contains(err.Error(), "link agy token") {
		t.Fatalf("taken link: %v", err)
	}
	g := newFixture(t)
	if err := os.WriteFile(filepath.Join(g.run.Home, ".gemini"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Agy{Bin: "/bin/true", TokenFile: agyToken(t)}).Exec(context.Background(), g.inv("p")); err == nil ||
		!strings.Contains(err.Error(), "create agy home") {
		t.Fatalf("blocked home: %v", err)
	}
}

func TestAgyExecWritesARefreshedTokenBack(t *testing.T) {
	f := newFixture(t)
	tok := agyToken(t)
	bin := fakeAgy(t, t.TempDir(), f.rec, `l="$HOME/.gemini/antigravity-cli/antigravity-oauth-token"
rm "$l"; printf '{"access_token":"new"}' > "$l"
echo '{"status":"SUCCESS","response":"{}"}'
`)
	if _, err := (Agy{Bin: bin, TokenFile: tok}).Exec(context.Background(), f.inv("p")); err != nil {
		t.Fatal(err)
	}
	if readFile(t, tok) != `{"access_token":"new"}` {
		t.Fatal("refreshed token not written back")
	}
}

func TestSandboxRunsAgyInTheRunInputDir(t *testing.T) {
	f := newSandboxFixture(t)
	tok := agyToken(t)
	installDir := t.TempDir()
	bin := fakeAgy(t, installDir, f.rec, `echo '{"status":"SUCCESS","response":"{\"summary\":\"ok\",\"findings\":[]}"}'
`)
	sb := f.sb.WithMounts(Mount{Path: installDir, ReadOnly: true}, Mount{Path: tok})
	got, err := Agy{Bin: bin, TokenFile: tok, Sandbox: sb}.Exec(context.Background(), f.inv("review"))
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"summary":"ok","findings":[]}` {
		t.Fatalf("message %q", got)
	}
	runArgs := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(f.dockerRec, "run-args"))), "\n")
	if w := argsAfter(runArgs, "-w"); len(w) != 1 || w[0] != f.run.Input {
		t.Fatalf("container workdir %v, want %s", w, f.run.Input)
	}
	mounts := strings.Join(argsAfter(runArgs, "--mount"), "\n")
	for _, want := range []string{
		"type=bind,source=" + f.repo + ",target=" + f.repo + ",readonly",
		"type=bind,source=" + installDir + ",target=" + installDir + ",readonly",
		"type=bind,source=" + tok + ",target=" + tok + "\n",
	} {
		if !strings.Contains(mounts+"\n", want) {
			t.Errorf("mounts\n%s\nmiss %s", mounts, want)
		}
	}
}
