// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeConnectionLLM is an Anthropic endpoint for `ocr llm test`. callTool
// makes the first answer call the self-test tool; failAt fails that request
// number (1-based, 0 never).
type fakeConnectionLLM struct {
	callTool bool
	failAt   int

	mu       sync.Mutex
	requests []string
}

func (f *fakeConnectionLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, string(raw))
	n := len(f.requests)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case n == f.failAt:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"rejected"}}`)
	case f.callTool && !bytes.Contains(raw, []byte(`"tool_result"`)):
		_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","model":"served-model",
			"content":[{"type":"tool_use","id":"tu1","name":"ocr_selftest","input":{"note":"hi"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	default:
		_, _ = io.WriteString(w, `{"id":"m2","type":"message","role":"assistant","model":"served-model",
			"content":[{"type":"text","text":"I am open-code-review."}],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}
}

func setupConnectionLLM(t *testing.T, f *fakeConnectionLLM, appConfig string) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("OCR_LLM_URL", srv.URL+"/v1/messages")
	t.Setenv("OCR_LLM_TOKEN", "test-token")
	t.Setenv("OCR_LLM_MODEL", "claude-test")
	t.Setenv("OCR_LLM_PROTOCOL", "anthropic")
	t.Setenv("OCR_LLM_AUTH_HEADER", "x-api-key")
	path := filepath.Join(t.TempDir(), "config.json")
	if appConfig != "" {
		if err := os.WriteFile(path, []byte(appConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestRunLLMTestWithConfigPath(t *testing.T) {
	t.Run("tool round trip", func(t *testing.T) {
		f := &fakeConnectionLLM{callTool: true}
		path := setupConnectionLLM(t, f, `{"language":"Vietnamese"}`)
		var err error
		out := captureStdout(t, func() { err = runLLMTestWithConfigPath(path) })
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Source: OCR environment", "Model:  served-model", "I am open-code-review.", "Connection test successful"} {
			if !strings.Contains(out, want) {
				t.Errorf("output misses %q:\n%s", want, out)
			}
		}
		if len(f.requests) != 2 || !strings.Contains(f.requests[1], "ocr_selftest ok") {
			t.Fatalf("second turn must answer the tool call: %d requests", len(f.requests))
		}
		if !strings.Contains(f.requests[0], "Vietnamese") {
			t.Error("the configured language must reach the request")
		}
	})

	t.Run("no tool call", func(t *testing.T) {
		f := &fakeConnectionLLM{}
		path := setupConnectionLLM(t, f, "")
		var err error
		out := captureStdout(t, func() { err = runLLMTestWithConfigPath(path) })
		if err != nil {
			t.Fatal(err)
		}
		if len(f.requests) != 1 || !strings.Contains(out, "Connection test successful") {
			t.Fatalf("requests %d output %q", len(f.requests), out)
		}
	})

	t.Run("openai protocol with empty answer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test",
				"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
		}))
		t.Cleanup(srv.Close)
		path := setupConnectionLLM(t, &fakeConnectionLLM{}, "")
		t.Setenv("OCR_LLM_URL", srv.URL+"/v1")
		t.Setenv("OCR_LLM_PROTOCOL", "openai")
		var err error
		out := captureStdout(t, func() { err = runLLMTestWithConfigPath(path) })
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"URL:    " + srv.URL + "/v1", "Model:  gpt-test", "(empty response)", "Tool-call round trip unverified"} {
			if !strings.Contains(out, want) {
				t.Errorf("output misses %q:\n%s", want, out)
			}
		}
	})

	t.Run("endpoint cannot be resolved", func(t *testing.T) {
		path := setupConnectionLLM(t, &fakeConnectionLLM{}, "")
		t.Setenv("OCR_LLM_PROTOCOL", "carrier-pigeon")
		err := runLLMTestWithConfigPath(path)
		if err == nil || !strings.Contains(err.Error(), "resolve LLM endpoint") {
			t.Fatalf("error = %v", err)
		}
	})

	failures := []struct {
		name   string
		llm    *fakeConnectionLLM
		config string
		want   string
	}{
		{"first request rejected", &fakeConnectionLLM{failAt: 1}, "", "llm request failed"},
		{"tool turn rejected", &fakeConnectionLLM{callTool: true, failAt: 2}, "", "llm request after tool call failed"},
		{"unreadable config", &fakeConnectionLLM{}, "{", "load config"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			path := setupConnectionLLM(t, tc.llm, tc.config)
			var err error
			captureStdout(t, func() { err = runLLMTestWithConfigPath(path) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunConfigModel_NeedsAConfiguredProvider(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{"no provider", `{}`, "no provider configured"},
		{"unknown custom provider", `{"provider": "my-gateway"}`, `provider "my-gateway" is not configured in custom_providers`},
		{"unreadable config", `{`, "load config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeServeAppConfig(t, tt.config)
			err := runConfigModel()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRunConfigUnset_Effort(t *testing.T) {
	writeServeAppConfig(t, `{"effort": "high", "language": "Vietnamese"}`)
	var err error
	out := captureStdout(t, func() { err = runConfigUnset("effort") })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Cleared effort") {
		t.Fatalf("output %q", out)
	}
	path, _ := defaultConfigPath()
	cfg, err := LoadAppConfig(path)
	if err != nil || cfg.Effort != "" || cfg.Language != "Vietnamese" {
		t.Fatalf("config after unset: %+v %v", cfg, err)
	}
}

func TestDescribeTarget(t *testing.T) {
	tests := []struct{ provider, model, want string }{
		{"", "", "-"},
		{"", "m", "m"},
		{"p", "", "p"},
		{"p", "m", "p/m"},
	}
	for _, tt := range tests {
		if got := describeTarget(tt.provider, tt.model); got != tt.want {
			t.Errorf("describeTarget(%q, %q) = %q, want %q", tt.provider, tt.model, got, tt.want)
		}
	}
}

// A failed connection test after a provider change keeps the saved settings
// and still succeeds, so the user can fix the endpoint and run `ocr llm test`.
func TestApplyProviderConfig_ConnectionTestFailureKeepsConfig(t *testing.T) {
	original := runLLMTestPath
	t.Cleanup(func() { runLLMTestPath = original })
	runLLMTestPath = func(string) error { return errors.New("connection refused") }

	tests := []struct {
		name  string
		apply func(path string) error
		check func(*Config) bool
	}{
		{"manual", func(path string) error {
			return applyManualConfig(path, &Config{}, providerTUIResult{isManual: true, url: "https://llm.example/v1", model: "m1", protocol: "openai"})
		}, func(c *Config) bool { return c.Llm.URL == "https://llm.example/v1" && c.Llm.Model == "m1" }},
		{"custom", func(path string) error {
			return applyCustomProviderConfig(path, &Config{}, providerTUIResult{isCustom: true, provider: "gateway", model: "m2", url: "https://gw.example/v1", protocol: "openai"})
		}, func(c *Config) bool { return c.Provider == "gateway" && c.CustomProviders["gateway"].Model == "m2" }},
		{"official", func(path string) error {
			return applyOfficialProviderConfig(path, &Config{}, providerTUIResult{provider: "openai", model: "m3", apiKey: "sk-test"})
		}, func(c *Config) bool { return c.Provider == "openai" && c.Model == "m3" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			var err error
			stderr := captureStderr(t, func() {
				captureStdout(t, func() { err = tt.apply(path) })
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr, "Connection test failed: connection refused") {
				t.Fatalf("stderr %q", stderr)
			}
			cfg, err := LoadAppConfig(path)
			if err != nil || cfg == nil || !tt.check(cfg) {
				t.Fatalf("saved config %+v %v", cfg, err)
			}
		})
	}
}

func TestConfigCommands_UnreadableConfig(t *testing.T) {
	writeServeAppConfig(t, `{`)
	for name, run := range map[string]func() error{
		"unset max_tokens": func() error { return runConfigUnset("max_tokens") },
		"unset effort":     func() error { return runConfigUnset("effort") },
		"provider":         runConfigProvider,
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "load config") {
			t.Errorf("%s: error = %v, want a load config error", name, err)
		}
	}
}
