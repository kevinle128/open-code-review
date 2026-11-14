// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Invocation is one prompt for a CLI.
//
// RepoDir is the working directory. ReadDirs are directories outside it
// that the agent must be able to read. Schema, when set, is the JSON Schema
// of the final answer. Edit lets the agent change files in RepoDir. Agents
// are subagent definition files a CLI that supports subagents installs for
// the run; a missing file is left out. Getenv supplies the process
// variables BaseEnv passes on; nil means os.Getenv.
type Invocation struct {
	RepoDir  string
	Prompt   string
	Schema   []byte
	ReadDirs []string
	Edit     bool
	Agents   []string
	Run      *RunDir
	Grace    time.Duration
	Getenv   func(string) string
}

// CLI is one coding-agent CLI. Exec runs the prompt and returns
// the agent's final message.
type CLI interface {
	Name() string
	Exec(ctx context.Context, inv Invocation) (string, error)
}

func (inv Invocation) env(extra ...string) []string {
	getenv := inv.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return append(BaseEnv(getenv, inv.Run.Home, inv.Run.Tmp), extra...)
}

// execute runs p as a child process, or inside a container when sb is set.
func execute(ctx context.Context, sb *Sandbox, inv Invocation, p Process) error {
	if sb == nil {
		return Run(ctx, p)
	}
	return sb.run(ctx, inv, p)
}

// process writes the prompt to a file and returns the process around it.
func (inv Invocation) process(bin string, args, env []string) (Process, error) {
	stdin := filepath.Join(inv.Run.Out, "prompt.txt")
	if err := os.WriteFile(stdin, []byte(inv.Prompt), 0o600); err != nil {
		return Process{}, fmt.Errorf("write prompt: %w", err)
	}
	return Process{
		Bin: bin, Args: args, Env: env, Dir: inv.RepoDir, Stdin: stdin,
		Stdout: filepath.Join(inv.Run.Out, "stdout"),
		Stderr: filepath.Join(inv.Run.Out, "stderr"),
		Grace:  inv.Grace,
	}, nil
}

// Codex runs `codex exec` without its own sandbox: the bubblewrap sandbox
// needs unprivileged user namespaces, which the container host does not
// allow, and with it codex cannot run a single tool. The container is the
// boundary instead, the service container or, with Sandbox set, one
// container per run; a review prompt forbids edits, and every prompt
// forbids network commands, so Edit changes nothing here. AuthFile is the
// host auth.json; the run home links to it, so a token refresh reaches the
// host file and the host's config, sessions and logs stay out of the run.
type Codex struct {
	Bin      string
	Model    string
	Effort   string
	AuthFile string
	Sandbox  *Sandbox
}

// Name implements CLI.
func (Codex) Name() string { return "codex" }

// Exec implements CLI. The project_doc_max_bytes=0 override keeps codex
// from loading the checkout's AGENTS.md, which the PR author controls.
func (c Codex) Exec(ctx context.Context, inv Invocation) (string, error) {
	link := filepath.Join(inv.Run.Home, "auth.json")
	if err := os.Symlink(c.AuthFile, link); err != nil {
		return "", fmt.Errorf("link codex auth: %w", err)
	}
	defer syncAuthBack(link, c.AuthFile)
	last := filepath.Join(inv.Run.Out, "last.json")
	args := []string{"exec", "--json", "--skip-git-repo-check", "--ephemeral",
		"--dangerously-bypass-approvals-and-sandbox", "-C", inv.RepoDir, "-o", last,
		"-c", "project_doc_max_bytes=0"}
	if len(inv.Schema) > 0 {
		schema := filepath.Join(inv.Run.Out, "schema.json")
		if err := os.WriteFile(schema, inv.Schema, 0o600); err != nil {
			return "", fmt.Errorf("write output schema: %w", err)
		}
		args = append(args, "--output-schema", schema)
	}
	if c.Model != "" {
		args = append(args, "-m", c.Model)
	}
	if c.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", c.Effort))
	}
	args = append(args, "-")
	p, err := inv.process(c.Bin, args, inv.env("CODEX_HOME="+inv.Run.Home))
	if err != nil {
		return "", err
	}
	if err := execute(ctx, c.Sandbox, inv, p); err != nil {
		return "", err
	}
	if b, err := os.ReadFile(last); err == nil && strings.TrimSpace(string(b)) != "" {
		return string(b), nil
	}
	return codexStreamMessage(p.Stdout)
}

// syncAuthBack writes a refreshed token back to the host auth file. Codex
// can save auth.json by replacing the file, which turns the link into a
// regular file that the run dir removal would delete. The host file is
// written in place because it is a bind mount and cannot be replaced.
func syncAuthBack(link, host string) {
	fi, err := os.Lstat(link)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	b, err := os.ReadFile(link)
	if err != nil || len(bytes.TrimSpace(b)) == 0 || !json.Valid(b) {
		return
	}
	if old, err := os.ReadFile(host); err == nil && bytes.Equal(old, b) {
		return
	}
	_ = os.WriteFile(host, b, 0o600)
}

// codexStreamMessage returns the last agent message of a `codex exec --json`
// stream. Two event shapes are read: the item events of current releases
// and the msg events of older ones.
func codexStreamMessage(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read codex output: %w", err)
	}
	defer f.Close()
	var msg, failure string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Msg struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"msg"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "item.completed" && ev.Item.Type == "agent_message":
			msg = ev.Item.Text
		case ev.Msg.Type == "agent_message":
			msg = ev.Msg.Message
		case ev.Type == "turn.failed" && ev.Error.Message != "":
			failure = ev.Error.Message
		case ev.Type == "error" && ev.Message != "":
			failure = ev.Message
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read codex output: %w", err)
	}
	if strings.TrimSpace(msg) != "" {
		return msg, nil
	}
	if failure != "" {
		return "", fmt.Errorf("codex failed: %s", truncate(failure, stderrTail))
	}
	return "", errors.New("codex returned no agent message")
}

// Claude runs `claude -p` with only the read tools, or for an Edit run the
// read and edit tools plus Task for the installed agents. An Edit run gets
// the Bash tool only with Sandbox set, so it can build and test the
// project inside the run's container; without one a shell would run where
// the service runs. SettingsFile is the host settings.json; the run home
// gets a copy of its env and model keys only, so the host's hooks, plugins
// and permission rules do not apply.
type Claude struct {
	Bin          string
	Model        string
	SettingsFile string
	Sandbox      *Sandbox
}

// Name implements CLI.
func (Claude) Name() string { return "claude" }

// Exec implements CLI. --setting-sources user and --strict-mcp-config keep
// the checkout's settings and MCP servers, which the PR author controls,
// from loading.
func (c Claude) Exec(ctx context.Context, inv Invocation) (string, error) {
	if err := writeClaudeSettings(c.SettingsFile, filepath.Join(inv.Run.Home, "settings.json")); err != nil {
		return "", err
	}
	mode, tools := "default", claudeReadTools
	if inv.Edit {
		mode, tools = "acceptEdits", claudeEditTools
		if c.Sandbox != nil {
			tools += ",Bash"
		}
		if err := installAgents(inv.Agents, filepath.Join(inv.Run.Home, "agents")); err != nil {
			return "", err
		}
	}
	args := []string{"-p", "--output-format", "json", "--no-session-persistence",
		"--permission-mode", mode, "--allowedTools", tools,
		"--setting-sources", "user", "--strict-mcp-config"}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	for _, d := range inv.ReadDirs {
		args = append(args, "--add-dir", d)
	}
	env := inv.env("CLAUDE_CONFIG_DIR="+inv.Run.Home,
		"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	p, err := inv.process(c.Bin, args, env)
	if err != nil {
		return "", err
	}
	if err := execute(ctx, c.Sandbox, inv, p); err != nil {
		return "", err
	}
	return claudeResult(p.Stdout)
}

const (
	claudeReadTools = "Read,Grep,Glob"
	claudeEditTools = "Read,Edit,MultiEdit,Write,Glob,Grep,Task"
	// agentTools are the tools an installed agent keeps: the agents a fix
	// run consults advise, and a write from one would land in the commit.
	agentTools = "Glob, Grep, Read"
)

// installAgents copies agent definitions into the run's agents directory.
// The front matter is rewritten: model becomes inherit, so the agent runs
// on the run's model through the same endpoint; memory is dropped, since
// project memory would be written into the checkout and committed; tools
// become read-only.
func installAgents(paths []string, dir string) error {
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read agent %s: %w", filepath.Base(p), err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create agents dir: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(p)), rewriteAgent(b), 0o600); err != nil {
			return fmt.Errorf("install agent %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

func rewriteAgent(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return b
	}
	out := []string{lines[0]}
	end := -1
	for i := 1; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "---" {
			end = i
			break
		}
		// Only top-level keys: an indented line belongs to a block value.
		switch key, _, _ := strings.Cut(l, ":"); key {
		case "model":
			out = append(out, "model: inherit")
		case "tools":
			out = append(out, "tools: "+agentTools)
		case "memory":
		default:
			out = append(out, l)
		}
	}
	if end < 0 {
		return b
	}
	return []byte(strings.Join(append(out, lines[end:]...), "\n"))
}

// claudeSettingsKeys are the host settings a run keeps: the API endpoint
// and token live in env, the default model in model.
var claudeSettingsKeys = []string{"env", "model"}

func writeClaudeSettings(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read claude settings: %w", err)
	}
	var host map[string]json.RawMessage
	if err := json.Unmarshal(b, &host); err != nil || host == nil {
		return fmt.Errorf("claude settings %s is not a JSON object", src)
	}
	kept := map[string]json.RawMessage{}
	for _, k := range claudeSettingsKeys {
		if v, ok := host[k]; ok {
			kept[k] = v
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("encode claude settings: %w", err)
	}
	if err := os.WriteFile(dst, out, 0o600); err != nil {
		return fmt.Errorf("write claude settings: %w", err)
	}
	return nil
}

// claudeResult reads the result object of `claude -p --output-format json`.
func claudeResult(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read claude output: %w", err)
	}
	var res struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), &res); err != nil {
		return "", fmt.Errorf("claude output is not a JSON result: %w", err)
	}
	if res.IsError {
		return "", fmt.Errorf("claude reported an error (%s): %s", res.Subtype, truncate(res.Result, stderrTail))
	}
	if strings.TrimSpace(res.Result) == "" {
		return "", errors.New("claude returned an empty result")
	}
	return res.Result, nil
}

// ExtractJSON returns the JSON object in an agent message. Agents wrap it in
// a Markdown fence or add a sentence around it now and then, despite the
// prompt; the outermost braces are taken.
func ExtractJSON(text string) ([]byte, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return nil, errors.New("no JSON object in the agent answer")
	}
	b := []byte(text[start : end+1])
	if !json.Valid(b) {
		return nil, errors.New("the agent answer holds malformed JSON")
	}
	return b, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}
