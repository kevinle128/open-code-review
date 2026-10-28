// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseRetryHint(t *testing.T) {
	cases := []struct {
		body string
		want time.Duration
		ok   bool
	}{
		{`{"message":"[codex/gpt] [429]: {\"detail\":\"Rate limit exceeded\"} (reset after 9s)"}`, 9 * time.Second, true},
		{"Rate limit reached. Please try again in 1m30s.", 90 * time.Second, true},
		{"Please try again in 6.5s.", 6500 * time.Millisecond, true},
		{"Please try again in 120ms.", 120 * time.Millisecond, true},
		{"Too many requests, retry after 3 seconds", 3 * time.Second, true},
		{"Too many requests, retry in 1 second", time.Second, true},
		{"quota resets in 2 minutes", 2 * time.Minute, true},
		{"wait in 5min", 5 * time.Minute, true},
		{"retry after 1.5 secs", 1500 * time.Millisecond, true},
		{"retry after 250 milliseconds", 250 * time.Millisecond, true},
		{"RETRY AFTER 4S", 4 * time.Second, true},
		{"retry after 0s", 0, false},
		{"rate limit exceeded", 0, false},
		{"retry after soon", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseRetryHint(c.body)
		if ok != c.ok || got != c.want {
			t.Errorf("parseRetryHint(%q) = (%v, %v), want (%v, %v)", c.body, got, ok, c.want, c.ok)
		}
	}
}

func hintResponse(status int, body string, header http.Header) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		if header == nil {
			header = http.Header{}
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func runHintMiddleware(t *testing.T, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://example.test/v1/chat/completions", nil)
	return newRateLimitGate().middleware(req, next)
}

func TestRetryHintMiddleware_SetsRetryAfterMsAndKeepsBody(t *testing.T) {
	const body = `{"message":"[429]: Rate limit exceeded (reset after 9s)"}`
	resp, err := runHintMiddleware(t, hintResponse(http.StatusServiceUnavailable, body, nil))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := resp.Header.Get("Retry-After-Ms"); got != "9000" {
		t.Errorf("Retry-After-Ms = %q, want 9000", got)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestRetryHintMiddleware_RetryableStatuses(t *testing.T) {
	const body = "retry after 2s"
	for _, status := range []int{408, 409, 429, 500, 503} {
		resp, _ := runHintMiddleware(t, hintResponse(status, body, nil))
		if got := resp.Header.Get("Retry-After-Ms"); got != "2000" {
			t.Errorf("status %d: Retry-After-Ms = %q, want 2000", status, got)
		}
	}
	resp, _ := runHintMiddleware(t, hintResponse(http.StatusForbidden, body, http.Header{"X-Should-Retry": {"true"}}))
	if got := resp.Header.Get("Retry-After-Ms"); got != "2000" {
		t.Errorf("x-should-retry: Retry-After-Ms = %q, want 2000", got)
	}
}

func TestRetryHintMiddleware_LeavesResponseUntouched(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
	}{
		{"non-retryable status", http.StatusBadRequest, nil},
		{"success", http.StatusOK, nil},
		{"server Retry-After wins", http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}}},
		{"server Retry-After-Ms wins", http.StatusTooManyRequests, http.Header{"Retry-After-Ms": {"500"}}},
	}
	for _, c := range cases {
		resp, _ := runHintMiddleware(t, hintResponse(c.status, "retry after 9s", c.header.Clone()))
		if c.header == nil && resp.Header.Get("Retry-After-Ms") != "" {
			t.Errorf("%s: Retry-After-Ms = %q, want empty", c.name, resp.Header.Get("Retry-After-Ms"))
		}
		if c.header != nil && resp.Header.Get("Retry-After-Ms") != c.header.Get("Retry-After-Ms") {
			t.Errorf("%s: Retry-After-Ms changed to %q", c.name, resp.Header.Get("Retry-After-Ms"))
		}
	}
}

func TestRetryHintMiddleware_NoHintNoHeader(t *testing.T) {
	resp, _ := runHintMiddleware(t, hintResponse(http.StatusTooManyRequests, "rate limited", nil))
	if got := resp.Header.Get("Retry-After-Ms"); got != "" {
		t.Errorf("Retry-After-Ms = %q, want empty", got)
	}
}

func TestRetryHintMiddleware_NilHeaderMap(t *testing.T) {
	next := func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("retry after 1s"))}, nil
	}
	resp, _ := runHintMiddleware(t, next)
	if got := resp.Header.Get("Retry-After-Ms"); got != "1000" {
		t.Errorf("Retry-After-Ms = %q, want 1000", got)
	}
}

func TestRetryHintMiddleware_PassesThroughTransportError(t *testing.T) {
	wantErr := errors.New("connection reset")
	resp, err := runHintMiddleware(t, func(*http.Request) (*http.Response, error) { return nil, wantErr })
	if !errors.Is(err, wantErr) || resp != nil {
		t.Fatalf("got (%v, %v), want (nil, %v)", resp, err, wantErr)
	}
}

type failingBody struct{ err error }

func (f failingBody) Read([]byte) (int, error) { return 0, f.err }
func (failingBody) Close() error               { return nil }

func TestRetryHintMiddleware_KeepsBodyReadError(t *testing.T) {
	readErr := errors.New("stream broke")
	next := func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: failingBody{readErr}}, nil
	}
	resp, err := runHintMiddleware(t, next)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, readErr) {
		t.Errorf("body read error = %v, want %v", err, readErr)
	}
}

// hintServer answers the first request with status and body, then with ok,
// and records when each request arrived.
func hintServer(t *testing.T, status int, body, ok string) (*httptest.Server, func() []time.Time) {
	t.Helper()
	var mu sync.Mutex
	var arrivals []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		n := len(arrivals)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(ok))
	}))
	t.Cleanup(server.Close)
	return server, func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), arrivals...)
	}
}

// The SDKs' own first backoff is at most 0.5s, so a gap of at least the
// stated 900ms shows the retry waited for the hint.
const hintWait = 900 * time.Millisecond

func assertWaitedForHint(t *testing.T, arrivals []time.Time) {
	t.Helper()
	if len(arrivals) != 2 {
		t.Fatalf("requests = %d, want 2", len(arrivals))
	}
	if gap := arrivals[1].Sub(arrivals[0]); gap < hintWait {
		t.Errorf("retry gap = %v, want >= %v", gap, hintWait)
	}
}

func TestOpenAIClient_RetryWaitsForBodyHint(t *testing.T) {
	server, arrivals := hintServer(t, http.StatusServiceUnavailable,
		`{"message":"[codex] [429]: {\"detail\":\"Rate limit exceeded\"} (reset after 900ms)"}`,
		`{"id":"c1","object":"chat.completion","model":"gpt-test",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)

	client := NewOpenAIClient(ClientConfig{URL: server.URL + "/v1", APIKey: "k", Model: "gpt-test"})
	resp, err := client.CompletionsWithCtx(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "ping"}},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("CompletionsWithCtx: %v", err)
	}
	if got := resp.Content(); got != "ok" {
		t.Errorf("Content() = %q, want ok", got)
	}
	assertWaitedForHint(t, arrivals())
}

func TestAnthropicClient_RetryWaitsForBodyHint(t *testing.T) {
	server, arrivals := hintServer(t, http.StatusTooManyRequests,
		`{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited, retry after 900ms"}}`,
		`{"id":"msg-1","type":"message","role":"assistant","model":"claude-test",
		  "content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		  "usage":{"input_tokens":1,"output_tokens":1}}`)

	client := NewAnthropicClient(ClientConfig{URL: server.URL, APIKey: "k", Model: "claude-test"})
	resp, err := client.CompletionsWithCtx(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "ping"}},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("CompletionsWithCtx: %v", err)
	}
	if got := resp.Content(); got != "ok" {
		t.Errorf("Content() = %q, want ok", got)
	}
	assertWaitedForHint(t, arrivals())
}

func TestHeaderRetryWait(t *testing.T) {
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	cases := []struct {
		name   string
		header http.Header
		min    time.Duration
		max    time.Duration
		ok     bool
	}{
		{"retry-after-ms", http.Header{"Retry-After-Ms": {"1500"}}, 1500 * time.Millisecond, 1500 * time.Millisecond, true},
		{"retry-after seconds", http.Header{"Retry-After": {"2"}}, 2 * time.Second, 2 * time.Second, true},
		{"retry-after date", http.Header{"Retry-After": {future}}, 28 * time.Second, 31 * time.Second, true},
		{"ms preferred", http.Header{"Retry-After-Ms": {"100"}, "Retry-After": {"9"}}, 100 * time.Millisecond, 100 * time.Millisecond, true},
		{"bad ms falls back", http.Header{"Retry-After-Ms": {"soon"}, "Retry-After": {"3"}}, 3 * time.Second, 3 * time.Second, true},
		{"bad value", http.Header{"Retry-After": {"later"}}, 0, 0, false},
		{"zero", http.Header{"Retry-After": {"0"}}, 0, 0, false},
		{"none", http.Header{}, 0, 0, false},
	}
	for _, c := range cases {
		got, ok := headerRetryWait(c.header)
		if ok != c.ok || got < c.min || got > c.max {
			t.Errorf("%s: headerRetryWait = (%v, %v), want [%v, %v] %v", c.name, got, ok, c.min, c.max, c.ok)
		}
	}
}

func TestRateLimitGate_HoldsUntilWindowEnds(t *testing.T) {
	g := newRateLimitGate()
	g.jitter = func(time.Duration) time.Duration { return 0 }
	g.hold(200 * time.Millisecond)
	g.hold(10 * time.Millisecond) // a shorter wait must not reopen the window
	start := time.Now()
	if err := g.wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 190*time.Millisecond {
		t.Errorf("wait returned after %v, want >= 190ms", elapsed)
	}
	start = time.Now()
	if err := g.wait(context.Background()); err != nil || time.Since(start) > 20*time.Millisecond {
		t.Errorf("open gate: wait took %v, err %v", time.Since(start), err)
	}
}

func TestRateLimitGate_SpreadsRelease(t *testing.T) {
	g := newRateLimitGate()
	var got time.Duration
	g.jitter = func(n time.Duration) time.Duration { got = n; return n - 1 }
	g.hold(time.Millisecond)
	_ = g.wait(context.Background())
	if got != minReleaseSpread {
		t.Errorf("spread for a short wait = %v, want %v", got, minReleaseSpread)
	}
	g.until = time.Time{}
	g.hold(time.Minute)
	g.mu.Lock()
	spread := g.spread
	g.until = time.Now() // keep the test fast
	g.mu.Unlock()
	if spread != maxReleaseSpread {
		t.Errorf("spread for a long wait = %v, want %v", spread, maxReleaseSpread)
	}
	if n := newRateLimitGate().jitter(time.Second); n < 0 || n >= time.Second {
		t.Errorf("default jitter = %v, want [0, 1s)", n)
	}
}

func TestRateLimitGate_WaitHonorsCancel(t *testing.T) {
	g := newRateLimitGate()
	g.hold(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait = %v, want context.Canceled", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://example.test/", nil).WithContext(ctx)
	called := false
	resp, err := g.middleware(req, func(*http.Request) (*http.Response, error) { called = true; return nil, nil })
	if called || resp != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("middleware = (%v, %v, called=%v), want canceled without sending", resp, err, called)
	}
}

func TestRateLimitGate_HeaderWaitClosesGate(t *testing.T) {
	g := newRateLimitGate()
	req := httptest.NewRequest(http.MethodPost, "http://example.test/", nil)
	_, _ = g.middleware(req, hintResponse(http.StatusTooManyRequests, "", http.Header{"Retry-After": {"30"}}))
	g.mu.Lock()
	left := time.Until(g.until)
	g.mu.Unlock()
	if left < 29*time.Second {
		t.Errorf("gate open for %v, want about 30s", left)
	}
}

// While one request has a stated rate-limit wait open, a second request of the
// same client must not reach the server before the window ends.
func TestOpenAIClient_RateLimitHoldsConcurrentRequests(t *testing.T) {
	const window = 600 * time.Millisecond
	limited := make(chan struct{})
	var mu sync.Mutex
	var arrivals []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		n := len(arrivals)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"[429]: Rate limit exceeded (reset after 600ms)"}`))
			close(limited)
			return
		}
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(ClientConfig{URL: server.URL + "/v1", APIKey: "k", Model: "m"})
	call := func() error {
		_, err := client.CompletionsWithCtx(context.Background(), ChatRequest{
			Messages:  []Message{{Role: "user", Content: "ping"}},
			MaxTokens: 16,
		})
		return err
	}
	errs := make(chan error, 2)
	go func() { errs <- call() }()
	<-limited
	time.Sleep(50 * time.Millisecond) // the rate-limit response has reached the client
	go func() { errs <- call() }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("CompletionsWithCtx: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 3 {
		t.Fatalf("requests = %d, want 3", len(arrivals))
	}
	for i, at := range arrivals[1:] {
		if gap := at.Sub(arrivals[0]); gap < window {
			t.Errorf("request %d reached the server %v after the rate limit, want >= %v", i+2, gap, window)
		}
	}
}

func TestPositiveDuration_RejectsUnusableValues(t *testing.T) {
	for _, h := range []http.Header{
		{"Retry-After-Ms": {"1e30"}},
		{"Retry-After-Ms": {"NaN"}},
		{"Retry-After": {"+Inf"}},
		{"Retry-After": {"-5"}},
	} {
		if d, ok := headerRetryWait(h); ok {
			t.Errorf("headerRetryWait(%v) = %v, want no wait", h, d)
		}
	}
	if d, ok := parseRetryHint("retry after 99999999999999999999 minutes"); ok {
		t.Errorf("parseRetryHint overflow = %v, want no wait", d)
	}
}

// A window extended while a request sleeps must hold that request too.
func TestRateLimitGate_RechecksExtendedWindow(t *testing.T) {
	g := newRateLimitGate()
	g.jitter = func(time.Duration) time.Duration { return 0 }
	g.hold(100 * time.Millisecond)
	done := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		_ = g.wait(context.Background())
		done <- time.Since(start)
	}()
	time.Sleep(50 * time.Millisecond)
	g.hold(250 * time.Millisecond) // now ends about 300ms after start
	if elapsed := <-done; elapsed < 290*time.Millisecond {
		t.Errorf("wait returned after %v, want >= 290ms", elapsed)
	}
}
