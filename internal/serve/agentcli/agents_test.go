//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const kongmingAgent = `---
name: kongming
description: >-
  Autonomous counsel.
model: fable
memory: project
tools: Glob, Grep, Read, Bash, Write
---

Advise only.
model: this line is body text
`

// TestClaudeExecEditMode runs a fix: edit tools without a shell, accepted
// edits, and the agents installed into the run home with inherit model,
// read-only tools and no project memory.
func TestClaudeExecEditMode(t *testing.T) {
	f := newFixture(t)
	agents := t.TempDir()
	kongming := filepath.Join(agents, "kongming.md")
	if err := os.WriteFile(kongming, []byte(kongmingAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, t.TempDir(), "claude", recorder(f.rec)+
		`cp "$CLAUDE_CONFIG_DIR/agents/kongming.md" `+f.rec+`/agent
echo '{"type":"result","subtype":"success","is_error":false,"result":"{}"}'
`)
	inv := f.inv("fix")
	inv.Edit = true
	inv.Agents = []string{kongming, filepath.Join(agents, "absent.md")}
	inv.ReadDirs = []string{"/skills/ak-review-pr", f.run.Input}
	if _, err := (Claude{Bin: bin, SettingsFile: writeSettings(t, "{}")}).Exec(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--output-format", "json", "--no-session-persistence", "--permission-mode", "acceptEdits",
		"--allowedTools", "Read,Edit,MultiEdit,Write,Glob,Grep,Task", "--setting-sources", "user", "--strict-mcp-config",
		"--add-dir", "/skills/ak-review-pr", "--add-dir", f.run.Input}
	if got := f.lines(t, "args"); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("args\n%q\nwant\n%q", got, want)
	}
	agent := readFile(t, filepath.Join(f.rec, "agent"))
	wantAgent := `---
name: kongming
description: >-
  Autonomous counsel.
model: inherit
tools: Glob, Grep, Read
---

Advise only.
model: this line is body text
`
	if agent != wantAgent {
		t.Fatalf("installed agent\n%s", agent)
	}
	if _, err := os.Stat(filepath.Join(f.run.Home, "agents", "absent.md")); err == nil {
		t.Fatal("absent agent installed")
	}
}

func TestRewriteAgentLeavesOtherFilesAlone(t *testing.T) {
	for _, in := range []string{"no front matter\nmodel: x\n", "---\nmodel: x\nno closing line\n"} {
		if got := string(rewriteAgent([]byte(in))); got != in {
			t.Fatalf("rewrote %q to %q", in, got)
		}
	}
}

func TestInstallAgentsErrors(t *testing.T) {
	dirAsAgent := t.TempDir()
	if err := installAgents([]string{dirAsAgent}, filepath.Join(t.TempDir(), "agents")); err == nil {
		t.Fatal("unreadable agent accepted")
	}
	src := filepath.Join(t.TempDir(), "a.md")
	if err := os.WriteFile(src, []byte(kongmingAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installAgents([]string{src}, filepath.Join(blocker, "agents")); err == nil {
		t.Fatal("agents dir under a file accepted")
	}
	f := newFixture(t)
	inv := f.inv("fix")
	inv.Edit = true
	inv.Agents = []string{dirAsAgent}
	if _, err := (Claude{Bin: "/bin/false", SettingsFile: writeSettings(t, "{}")}).Exec(context.Background(), inv); err == nil {
		t.Fatal("agent install failure ignored")
	}
}
