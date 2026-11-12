// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/gitlab"
	"github.com/spf13/pflag"
)

var (
	serveTestKeyOnce sync.Once
	serveTestKeyPEM  []byte
)

func serveTestKey(t *testing.T) []byte {
	t.Helper()
	serveTestKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		serveTestKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	})
	return serveTestKeyPEM
}

// writeServeAppConfig points HOME at a fresh directory holding config.json
// with body, or no config file when body is empty.
func writeServeAppConfig(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if body == "" {
		return
	}
	dir := filepath.Join(home, ".opencodereview")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// serveTestEnv is a GitHub-only service environment on a free local port.
func serveTestEnv(t *testing.T) map[string]string {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, serveTestKey(t), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		serveconfig.EnvAddr:                       "127.0.0.1:0",
		serveconfig.EnvDataDir:                    t.TempDir(),
		serveconfig.EnvWorkDir:                    t.TempDir(),
		serveconfig.EnvGitHubAppID:                "42",
		serveconfig.EnvGitHubPrivateKey + "_FILE": keyPath,
		serveconfig.EnvGitHubWebhookSecret:        "gh-secret",
	}
}

func envFrom(m map[string]string) serveEnv {
	return serveEnv{
		getenv:   func(k string) string { return m[k] },
		readFile: os.ReadFile,
		logOut:   io.Discard,
	}
}

// fakeGitLabUser answers GET /user with status and counts the calls.
func fakeGitLabUser(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("PRIVATE-TOKEN") != "gl-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"id": 5, "username": "ocr-bot"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServeMain_StartupErrors(t *testing.T) {
	const okConfig = `{"language": "Vietnamese"}`
	gitlabDown := fakeGitLabUser(t, http.StatusUnauthorized)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		appConfig string
		edit      func(map[string]string)
		want      string
	}{
		{"no platform", okConfig, func(m map[string]string) {
			delete(m, serveconfig.EnvGitHubAppID)
		}, "no platform enabled"},
		{"missing github key file", okConfig, func(m map[string]string) {
			m[serveconfig.EnvGitHubPrivateKey+"_FILE"] = filepath.Join(t.TempDir(), "missing.pem")
		}, serveconfig.EnvGitHubPrivateKey + "_FILE"},
		{"gitlab without token", okConfig, func(m map[string]string) {
			m[serveconfig.EnvGitLabURL] = gitlabDown.URL
		}, serveconfig.EnvGitLabToken + " is required"},
		{"gitlab token rejected", okConfig, func(m map[string]string) {
			m[serveconfig.EnvGitLabURL] = gitlabDown.URL
			m[serveconfig.EnvGitLabToken] = "wrong"
			m[serveconfig.EnvGitLabWebhookSecret] = "gl-secret"
		}, "gitlab: read the bot user"},
		{"mcp servers configured", `{"language": "Vietnamese", "mcp_servers": {"x": {"type": "stdio", "command": "true"}}}`, nil, "mcp_servers is set"},
		{"empty language", `{"language": " "}`, nil, "language is not set"},
		{"no app config", "", nil, "language is not set"},
		{"unreadable app config", `{`, nil, "parse app config"},
		{"work dir unusable", okConfig, func(m map[string]string) {
			m[serveconfig.EnvWorkDir] = filepath.Join(notADir, "work")
		}, "checkout work dir"},
		{"data dir unusable", okConfig, func(m map[string]string) {
			m[serveconfig.EnvDataDir] = filepath.Join(notADir, "data")
		}, "store directory"},
		{"address in use", okConfig, func(m map[string]string) {
			m[serveconfig.EnvAddr] = busy.Addr().String()
		}, "listen on"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeServeAppConfig(t, tt.appConfig)
			m := serveTestEnv(t)
			if tt.edit != nil {
				tt.edit(m)
			}
			env := envFrom(m)
			env.ready = func(string) { t.Error("the service must not start listening") }
			err := serveMain(context.Background(), env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestServeMain_ServesAndShutsDown(t *testing.T) {
	writeServeAppConfig(t, `{"language": "Vietnamese"}`)
	gl := fakeGitLabUser(t, http.StatusOK)
	m := serveTestEnv(t)
	m[serveconfig.EnvGitLabURL] = gl.URL
	m[serveconfig.EnvGitLabToken] = "gl-token"
	m[serveconfig.EnvGitLabWebhookSecret] = "gl-secret"
	m[serveconfig.EnvPollInterval] = "1h"

	var logs bytes.Buffer
	var logMu sync.Mutex
	env := envFrom(m)
	env.logOut = writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	})
	addrCh := make(chan string, 1)
	env.ready = func(addr string) { addrCh <- addr }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveMain(ctx, env) }()

	var addr string
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("service stopped before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("service did not start")
	}
	base := "http://" + addr

	for _, path := range []string{"/healthz", "/ocr/healthz"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || body["status"] != "ok" || body["version"] != Version {
			t.Fatalf("GET %s = %d %v", path, resp.StatusCode, body)
		}
	}

	webhooks := []struct {
		path   string
		header string
		want   int
	}{
		{"/github/webhook", "X-Hub-Signature-256", http.StatusForbidden},
		{"/ocr/github/webhook", "X-Hub-Signature-256", http.StatusForbidden},
		{"/gitlab/webhook", "X-Gitlab-Token", http.StatusUnauthorized},
		{"/ocr/gitlab/webhook", "X-Gitlab-Token", http.StatusUnauthorized},
	}
	for _, wh := range webhooks {
		req, _ := http.NewRequest(http.MethodPost, base+wh.path, strings.NewReader(`{}`))
		req.Header.Set(wh.header, "wrong")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != wh.want {
			t.Fatalf("POST %s = %d, want %d", wh.path, resp.StatusCode, wh.want)
		}
	}
	if resp, err := http.Get(base + "/nope"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path: %v %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveMain: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("service did not shut down")
	}
	logMu.Lock()
	out := logs.String()
	logMu.Unlock()
	for _, want := range []string{`"msg":"listening"`, `"platforms":["github","gitlab"]`, `"msg":"stopped"`} {
		if !strings.Contains(out, want) {
			t.Errorf("logs miss %s:\n%s", want, out)
		}
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Error("listener still open after shutdown")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestRunServe_ReportsConfigError(t *testing.T) {
	t.Setenv(serveconfig.EnvGitHubAppID, "")
	t.Setenv(serveconfig.EnvGitLabURL, "")
	rootCmd.SetArgs([]string{"serve"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no platform enabled") {
		t.Fatalf("error = %v", err)
	}
}

func TestServeRoutes(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	})
	h := serveRoutes(inner)
	for path, want := range map[string]int{"/healthz": 200, "/ocr/healthz": 200, "/ocr/ocr/healthz": 404, "/ocrx/healthz": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
		if want == 200 && rec.Body.String() != "/healthz" {
			t.Errorf("GET %s reached the handler as %q", path, rec.Body.String())
		}
	}
}

// fakeGitLabHooks serves one project without hooks and records writes.
func fakeGitLabHooks(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
			return
		}
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fapp":
			_, _ = io.WriteString(w, `{"id": 9, "path_with_namespace": "group/app", "only_allow_merge_if_all_discussions_are_resolved": false}`)
		case "/api/v4/projects/9/hooks":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

func TestServeHooksGitLab(t *testing.T) {
	gl, writes := fakeGitLabHooks(t)
	full := map[string]string{
		serveconfig.EnvGitLabURL:        gl.URL,
		serveconfig.EnvGitLabAdminToken: "admin-token",
		serveconfig.EnvGitLabHookURL:    "https://hooks.example/ocr/gitlab/webhook",
		// An incomplete GitHub setting must not block the GitLab command.
		serveconfig.EnvGitHubAppID: "42",
	}
	without := func(keys ...string) map[string]string {
		m := map[string]string{}
		for k, v := range full {
			m[k] = v
		}
		for _, k := range keys {
			delete(m, k)
		}
		return m
	}

	errCases := []struct {
		name string
		env  map[string]string
		mode gitlab.HookMode
		want string
	}{
		{"no url", without(serveconfig.EnvGitLabURL), gitlab.HookDryRun, serveconfig.EnvGitLabURL + " is required"},
		{"no admin token", without(serveconfig.EnvGitLabAdminToken), gitlab.HookDryRun, serveconfig.EnvGitLabAdminToken + " is required"},
		{"no hook url", without(serveconfig.EnvGitLabHookURL), gitlab.HookDryRun, "hook URL"},
		{"apply without secret", full, gitlab.HookApply, "webhook secret is required"},
		{"bad url", map[string]string{serveconfig.EnvGitLabURL: "ftp://x"}, gitlab.HookDryRun, serveconfig.EnvGitLabURL},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			err := serveHooksGitLab(context.Background(), envFrom(tc.env), tc.mode, "group/app", io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	t.Run("dry run", func(t *testing.T) {
		var out bytes.Buffer
		if err := serveHooksGitLab(context.Background(), envFrom(full), gitlab.HookDryRun, "group/app", &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "dry run, nothing changed: 2 change(s)") || len(*writes) != 0 {
			t.Fatalf("output %q writes %v", out.String(), *writes)
		}
	})

	t.Run("api error", func(t *testing.T) {
		env := without()
		env[serveconfig.EnvGitLabAdminToken] = "wrong"
		if err := serveHooksGitLab(context.Background(), envFrom(env), gitlab.HookDryRun, "group/app", io.Discard); err == nil {
			t.Fatal("a rejected admin token must fail the command")
		}
	})
}

// resetServeHookFlags clears flag state that earlier Execute calls left on
// the shared command tree, such as --help, now and after the test.
func resetServeHookFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		serveHooksGitLabCmd.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
	reset()
	t.Cleanup(reset)
}

func TestServeHooksGitLabCommand(t *testing.T) {
	gl, writes := fakeGitLabHooks(t)
	t.Setenv(serveconfig.EnvGitHubAppID, "")
	t.Setenv(serveconfig.EnvGitLabURL, gl.URL)
	t.Setenv(serveconfig.EnvGitLabAdminToken, "admin-token")
	t.Setenv(serveconfig.EnvGitLabHookURL, "https://hooks.example/ocr/gitlab/webhook")
	t.Setenv(serveconfig.EnvGitLabWebhookSecret, "gl-secret")

	t.Run("two modes", func(t *testing.T) {
		resetServeHookFlags(t)
		rootCmd.SetArgs([]string{"serve", "hooks", "gitlab", "--apply", "--remove", "--project", "group/app"})
		err := rootCmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "none of the others can be") {
			t.Fatalf("error = %v", err)
		}
		if len(*writes) != 0 {
			t.Fatalf("writes %v", *writes)
		}
	})

	modes := []struct {
		flag   string
		output string
		writes int
	}{
		{"", "dry run, nothing changed: 2 change(s)", 0},
		{"--dry-run", "dry run, nothing changed: 2 change(s)", 0},
		{"--apply", "done: 2 change(s)", 2},
		{"--remove", "done: 0 change(s)", 0},
	}
	for _, mc := range modes {
		t.Run("mode "+mc.flag, func(t *testing.T) {
			resetServeHookFlags(t)
			*writes = nil
			args := []string{"serve", "hooks", "gitlab", "--project", "group/app"}
			if mc.flag != "" {
				args = append(args, mc.flag)
			}
			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetArgs(args)
			if err := rootCmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), mc.output) || len(*writes) != mc.writes {
				t.Fatalf("output %q writes %v", out.String(), *writes)
			}
		})
	}
}
