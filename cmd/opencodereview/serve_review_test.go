// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alibaba/open-code-review/internal/agent"
	"github.com/alibaba/open-code-review/internal/serve/checkout"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/session"
)

// serveFakeLLM answers the Anthropic messages API for a one-file review:
// each main-task conversation files one finding in its first round and
// finishes in the next, and the comment filter keeps every finding.
type serveFakeLLM struct {
	mu          sync.Mutex
	fail        bool
	requests    int
	sawLanguage bool
}

func (f *serveFakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(r.Body)
	raw := body.Bytes()

	f.mu.Lock()
	f.requests++
	if bytes.Contains(raw, []byte("Always respond in Vietnamese")) {
		f.sawLanguage = true
	}
	fail := f.fail
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case fail:
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"payment required"}}`))
	case bytes.Contains(raw, []byte("approve_all_comments")):
		_, _ = w.Write([]byte(`{"id":"msg_f","type":"message","role":"assistant","model":"claude-test",
			"content":[{"type":"tool_use","id":"tu_f","name":"approve_all_comments","input":{}}],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`))
	case bytes.Contains(raw, []byte(`"tools"`)) && bytes.Contains(raw, []byte(`"tool_result"`)):
		_, _ = w.Write([]byte(`{"id":"msg_d","type":"message","role":"assistant","model":"claude-test",
			"content":[{"type":"tool_use","id":"tu_d","name":"task_done","input":{"state":"DONE"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`))
	case bytes.Contains(raw, []byte(`"tools"`)):
		_, _ = w.Write([]byte(`{"id":"msg_c","type":"message","role":"assistant","model":"claude-test",
			"content":[{"type":"tool_use","id":"tu_c","name":"code_comment","input":{"comments":[
				{"path":"a.go","content":"the new value is never checked","category":"bug","severity":"high","existing_code":"return 2"}]}}],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`))
	default:
		_, _ = w.Write([]byte(`{"id":"msg_p","type":"message","role":"assistant","model":"claude-test",
			"content":[{"type":"text","text":"plan: read the diff, then call task_done"}],
			"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
	}
}

// startServeFakeLLM points the OCR_LLM_* endpoint at f under a fresh home
// whose config.json holds appConfig.
func startServeFakeLLM(t *testing.T, f *serveFakeLLM, appConfig string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if appConfig != "" {
		dir := filepath.Join(home, ".opencodereview")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(appConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	t.Setenv("OCR_LLM_URL", server.URL+"/v1/messages")
	t.Setenv("OCR_LLM_TOKEN", "test-token")
	t.Setenv("OCR_LLM_MODEL", "claude-test")
	t.Setenv("OCR_LLM_PROTOCOL", "anthropic")
	t.Setenv("OCR_LLM_AUTH_HEADER", "x-api-key")
	t.Setenv("OCR_LLM_TIMEOUT", "30")
	return home
}

const vietnameseConfig = `{"language":"Vietnamese"}`

type serveRepo struct {
	dir, top, base, head string
}

func newServeRepo(t *testing.T) serveRepo {
	t.Helper()
	dir := t.TempDir()
	retryTestGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\nfunc A() int { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	retryTestGit(t, dir, "add", ".")
	retryTestGit(t, dir, "commit", "-q", "-m", "base")
	base := serveGitOut(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\nfunc A() int {\n\treturn 2\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	retryTestGit(t, dir, "commit", "-q", "-am", "change A")
	return serveRepo{
		dir:  dir,
		top:  serveGitOut(t, dir, "rev-parse", "--show-toplevel"),
		base: base,
		head: serveGitOut(t, dir, "rev-parse", "HEAD"),
	}
}

func serveGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// assertNoSessionFiles checks that a session was written for repo and then
// removed, so the check cannot pass because nothing was persisted.
func assertNoSessionFiles(t *testing.T, repo serveRepo) {
	t.Helper()
	dir, err := session.SessionsDir(repo.top)
	if err != nil {
		t.Fatal(err)
	}
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("sessions dir %s: %v (no session was written)", dir, err)
	}
	for _, e := range list {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			t.Errorf("session file left behind: %s", e.Name())
		}
	}
}

func TestServeReviewRangeAndCommitModes(t *testing.T) {
	cases := []struct {
		name string
		req  func(r serveRepo) review.Request
	}{
		{"range", func(r serveRepo) review.Request {
			return review.Request{RepoDir: r.dir, From: r.base, To: r.head, Background: "Adds a new return value"}
		}},
		{"commit", func(r serveRepo) review.Request {
			return review.Request{RepoDir: r.dir, Commit: r.head, Effort: "low"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			llm := &serveFakeLLM{}
			startServeFakeLLM(t, llm, vietnameseConfig)
			repo := newServeRepo(t)

			res, err := serveReviewer{}.Review(context.Background(), tc.req(repo))
			if err != nil {
				t.Fatalf("Review: %v", err)
			}
			if res.Terminal != review.TerminalComplete {
				t.Fatalf("terminal = %q, want complete (warnings %v)", res.Terminal, res.Warnings)
			}
			if len(res.Findings) == 0 || res.Findings[0].Path != "a.go" || res.Findings[0].Category != "bug" {
				t.Fatalf("findings = %+v", res.Findings)
			}
			if !llm.sawLanguage {
				t.Error("the configured output language never reached the LLM")
			}
			assertNoSessionFiles(t, repo)
		})
	}
}

func TestServeReviewFailedRunReturnsError(t *testing.T) {
	llm := &serveFakeLLM{fail: true}
	startServeFakeLLM(t, llm, "")
	repo := newServeRepo(t)
	res, err := serveReviewer{}.Review(context.Background(), review.Request{RepoDir: repo.dir, From: repo.base, To: repo.head})
	if err == nil {
		t.Fatal("failed review returned no error")
	}
	if res.Terminal != review.TerminalFailed {
		t.Fatalf("terminal = %q, want failed", res.Terminal)
	}
	assertNoSessionFiles(t, repo)
}

func TestServeReviewExcludedChangeIsSkipped(t *testing.T) {
	llm := &serveFakeLLM{}
	startServeFakeLLM(t, llm, "")
	repo := newServeRepo(t)
	res, err := serveReviewer{}.Review(context.Background(), review.Request{
		RepoDir: repo.dir, From: repo.base, To: repo.head, Excludes: []string{"*.go"},
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Terminal != review.TerminalSkipped || len(res.Findings) != 0 {
		t.Fatalf("result = %+v, want skipped with no findings", res)
	}
}

func TestServeReviewFallsBackToFetchedBase(t *testing.T) {
	llm := &serveFakeLLM{}
	startServeFakeLLM(t, llm, "")
	repo := newServeRepo(t)
	retryTestGit(t, repo.dir, "update-ref", checkout.BaseRef, repo.base)
	gone := strings.Repeat("0", 39) + "1"
	res, err := serveReviewer{}.Review(context.Background(), review.Request{RepoDir: repo.dir, From: gone, To: repo.head})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatalf("findings = %+v", res.Findings)
	}
}

func TestServeReviewParallelRuns(t *testing.T) {
	llm := &serveFakeLLM{}
	startServeFakeLLM(t, llm, vietnameseConfig)
	repos := []serveRepo{newServeRepo(t), newServeRepo(t)}

	type outcome struct {
		res review.Result
		err error
	}
	results := make([]outcome, len(repos))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := serveReviewer{}.Review(context.Background(), review.Request{RepoDir: repo.dir, From: repo.base, To: repo.head})
			results[i] = outcome{res, err}
		}()
	}
	wg.Wait()
	for i, o := range results {
		if o.err != nil {
			t.Fatalf("review %d: %v", i, o.err)
		}
		if len(o.res.Findings) == 0 || o.res.Terminal != review.TerminalComplete {
			t.Fatalf("review %d = %+v", i, o.res)
		}
		assertNoSessionFiles(t, repos[i])
	}
}

func TestServeReviewRefusesMCPServers(t *testing.T) {
	llm := &serveFakeLLM{}
	startServeFakeLLM(t, llm, `{"mcp_servers":{"probe":{"command":"true","setup":"touch setup-ran"}}}`)
	repo := newServeRepo(t)
	_, err := serveReviewer{}.Review(context.Background(), review.Request{RepoDir: repo.dir, From: repo.base, To: repo.head})
	if err == nil || !strings.Contains(err.Error(), "mcp_servers") {
		t.Fatalf("err = %v, want mcp_servers refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(repo.dir, "setup-ran")); statErr == nil {
		t.Fatal("MCP setup ran inside the checkout")
	}
	if llm.requests != 0 {
		t.Fatalf("LLM called %d times", llm.requests)
	}
}

func TestServeReviewRejectsBadRequests(t *testing.T) {
	llm := &serveFakeLLM{}
	startServeFakeLLM(t, llm, "")
	repo := newServeRepo(t)
	missing := strings.Repeat("0", 39) + "1"
	cases := []struct {
		name string
		req  review.Request
		want string
	}{
		{"no repo dir", review.Request{Commit: repo.head}, "repo dir"},
		{"no mode", review.Request{RepoDir: repo.dir}, "commit or a from and to"},
		{"from only", review.Request{RepoDir: repo.dir, From: repo.base}, "commit or a from and to"},
		{"two modes", review.Request{RepoDir: repo.dir, From: repo.base, To: repo.head, Commit: repo.head}, "only one review mode"},
		{"bad effort", review.Request{RepoDir: repo.dir, Commit: repo.head, Effort: "max"}, "--effort"},
		{"not a repo", review.Request{RepoDir: t.TempDir(), Commit: repo.head}, "not a git repository"},
		{"missing commit", review.Request{RepoDir: repo.dir, Commit: missing}, "not a valid commit ref"},
		{"option ref", review.Request{RepoDir: repo.dir, From: "--output=x", To: repo.head}, "must not start with '-'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := serveReviewer{}.Review(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if res.Terminal != review.TerminalFailed {
				t.Fatalf("terminal = %q, want failed", res.Terminal)
			}
		})
	}
	if llm.requests != 0 {
		t.Fatalf("LLM called %d times", llm.requests)
	}
}

func TestServeReviewSetupErrors(t *testing.T) {
	t.Run("unreadable app config", func(t *testing.T) {
		startServeFakeLLM(t, &serveFakeLLM{}, "{not json")
		repo := newServeRepo(t)
		if _, err := (serveReviewer{}).Review(context.Background(), review.Request{RepoDir: repo.dir, Commit: repo.head}); err == nil {
			t.Fatal("broken config.json accepted")
		}
	})
	t.Run("no llm endpoint", func(t *testing.T) {
		startServeFakeLLM(t, &serveFakeLLM{}, "")
		for _, key := range []string{"OCR_LLM_URL", "OCR_LLM_TOKEN", "OCR_LLM_MODEL", "OCR_LLM_PROTOCOL", "OCR_LLM_AUTH_HEADER"} {
			t.Setenv(key, "")
		}
		repo := newServeRepo(t)
		if _, err := (serveReviewer{}).Review(context.Background(), review.Request{RepoDir: repo.dir, Commit: repo.head}); err == nil {
			t.Fatal("review ran without an LLM endpoint")
		}
	})
}

func TestTerminalState(t *testing.T) {
	if got := terminalState(&session.RunManifest{TerminalState: session.StatePartial}, nil); got != review.TerminalPartial {
		t.Errorf("manifest = %q", got)
	}
	if got := terminalState(nil, errors.New("boom")); got != review.TerminalFailed {
		t.Errorf("no manifest, error = %q", got)
	}
	if got := terminalState(nil, nil); got != review.TerminalComplete {
		t.Errorf("no manifest, no error = %q", got)
	}
}

func TestWarningTexts(t *testing.T) {
	if got := warningTexts(nil); got != nil {
		t.Errorf("nil = %v", got)
	}
	got := warningTexts([]agent.AgentWarning{{File: "a.go", Message: "too large"}, {Message: "review limit"}})
	if strings.Join(got, "|") != "a.go: too large|review limit" {
		t.Errorf("texts = %v", got)
	}
}

func TestDeleteServeSession(t *testing.T) {
	startServeFakeLLM(t, &serveFakeLLM{}, "")
	repo := newServeRepo(t)
	if err := deleteServeSession(repo.top, ""); err != nil {
		t.Errorf("empty id: %v", err)
	}
	if err := deleteServeSession(repo.top, "absent-session"); err != nil {
		t.Errorf("not found must be ignored: %v", err)
	}
	if err := deleteServeSession(repo.top, "a/b"); err == nil {
		t.Error("invalid id accepted")
	}
}
