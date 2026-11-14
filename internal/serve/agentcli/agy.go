// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// agyTokenPath is where agy reads its OAuth token, relative to HOME,
	// when the host has no keyring.
	agyTokenPath = ".gemini/antigravity-cli/antigravity-oauth-token"
	// agyMaxPromptArg keeps the -p argument under the Linux limit of
	// 128 KiB for one argument (MAX_ARG_STRLEN). A longer prompt goes into
	// agyPromptFile in the run input directory.
	agyMaxPromptArg = 100 << 10
	agyPromptFile   = "agy-prompt.md"
	// agySettings are session settings that agy never saves. Non-workspace
	// access lets agy read the checkout without making it a workspace, and
	// the deny rule blocks its file write tool in every mode.
	agySettings = `{"allowNonWorkspaceAccess":true,"permissions":{"deny":["write_file(*)"]}}`
)

// Agy runs the Google Antigravity CLI (`agy -p`) for reviews only.
//
// agy loads the hooks (.agents/hooks.json), rules, skills, MCP servers and
// AGENTS.md or GEMINI.md of every workspace folder and its parents up to
// the repository root, also in print mode, where it asks nobody whether to
// trust the folder; a hook is a shell command. No flag or setting turns
// this off, and the checkout is controlled by the PR author. So the
// checkout is never a workspace: agy starts in the run input directory,
// which the service writes, and reads the checkout by absolute path
// through non-workspace access.
//
// TokenFile is the host OAuth token file. The run home links to it, so a
// token refresh reaches the host file and agy writes its conversations,
// logs and caches into the run home instead of the host's.
type Agy struct {
	Bin       string
	Model     string
	Effort    string
	TokenFile string
	Sandbox   *Sandbox
}

// Name implements CLI.
func (Agy) Name() string { return "agy" }

// Exec implements CLI.
func (a Agy) Exec(ctx context.Context, inv Invocation) (string, error) {
	if inv.Edit {
		return "", errors.New("agy runs reviews only")
	}
	link := filepath.Join(inv.Run.Home, agyTokenPath)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		return "", fmt.Errorf("create agy home: %w", err)
	}
	if err := os.Symlink(a.TokenFile, link); err != nil {
		return "", fmt.Errorf("link agy token: %w", err)
	}
	defer syncAuthBack(link, a.TokenFile)

	full := inv
	full.Prompt = agyPreface(inv.RepoDir) + inv.Prompt
	arg := full.Prompt
	if len(arg) > agyMaxPromptArg {
		file := filepath.Join(inv.Run.Input, agyPromptFile)
		if err := os.WriteFile(file, []byte(full.Prompt), 0o600); err != nil {
			return "", fmt.Errorf("write agy prompt: %w", err)
		}
		arg = fmt.Sprintf("Read the file `%s` in full before you do anything else. It holds the complete instructions of this run. Follow them exactly, as if they were this prompt.", file)
	}
	args := []string{"-p", arg, "--output-format", "json"}
	if len(inv.Schema) > 0 {
		schema := filepath.Join(inv.Run.Out, "schema.json")
		if err := os.WriteFile(schema, inv.Schema, 0o600); err != nil {
			return "", fmt.Errorf("write output schema: %w", err)
		}
		args = append(args, "--json-schema", schema)
	}
	args = append(args, "--dangerously-skip-permissions", "--disable-slash-commands", "--settings", agySettings)
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.Effort != "" {
		args = append(args, "--effort", a.Effort)
	}
	for _, d := range inv.ReadDirs {
		args = append(args, "--add-dir", d)
	}
	// prompt.txt keeps the full prompt for a kept run directory; the
	// prompt goes in the arguments, and stdin is an empty file.
	p, err := full.process(a.Bin, args, inv.env())
	if err != nil {
		return "", err
	}
	p.Dir = inv.Run.Input
	p.Stdin = filepath.Join(inv.Run.Out, "stdin")
	if err := os.WriteFile(p.Stdin, nil, 0o600); err != nil {
		return "", fmt.Errorf("write agy stdin: %w", err)
	}
	if err := execute(ctx, a.Sandbox, inv, p); err != nil {
		var reported *agyError
		if _, perr := agyResult(p.Stdout); errors.As(perr, &reported) {
			return "", fmt.Errorf("%w (%v)", err, perr)
		}
		return "", err
	}
	return agyResult(p.Stdout)
}

// agyPreface tells agy where the checkout is: the service prompt calls it
// the current directory, which for agy is the run input directory.
func agyPreface(repo string) string {
	return fmt.Sprintf("# Checkout location\n\nThe PR checkout of this run is `%[1]s`. Your current directory is the run directory, not the checkout. Wherever the instructions below say \"the current directory\", \"the checkout\" or \"repository files\", they mean `%[1]s`: read files there by absolute path, and run commands there with `cd %[1]s && ...`.\n\n---\n\n", repo)
}

// agyError is a failure that agy reported in its JSON result.
type agyError struct {
	msg string
}

func (e *agyError) Error() string { return e.msg }

// agyResult reads the result object of `agy -p --output-format json`: the
// structured_output object when the run had a JSON schema, otherwise the
// response text.
func agyResult(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read agy output: %w", err)
	}
	var res struct {
		Status           string          `json:"status"`
		Response         string          `json:"response"`
		Error            json.RawMessage `json:"error"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), &res); err != nil {
		return "", fmt.Errorf("agy output is not a JSON result: %w", err)
	}
	detail := rawText(res.Error)
	if !strings.EqualFold(res.Status, "SUCCESS") {
		if detail == "" {
			detail = res.Response
		}
		return "", &agyError{msg: fmt.Sprintf("agy run ended with status %q: %s", res.Status, truncate(detail, stderrTail))}
	}
	if detail != "" {
		return "", &agyError{msg: "agy reported an error: " + truncate(detail, stderrTail)}
	}
	if out := rawText(res.StructuredOutput); out != "" {
		return out, nil
	}
	if strings.TrimSpace(res.Response) == "" {
		return "", errors.New("agy returned an empty response")
	}
	return res.Response, nil
}

// rawText returns a JSON string value unquoted and any other non-null
// value as compact JSON.
func rawText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var out bytes.Buffer
	if json.Compact(&out, raw) != nil {
		return string(raw)
	}
	return out.String()
}
