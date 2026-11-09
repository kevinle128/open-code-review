// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testGate() (*rateLimitGate, *[]string) {
	g := newRateLimitGate()
	g.jitter = func(time.Duration) time.Duration { return 0 }
	var logs []string
	g.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	return g, &logs
}

func mustAcquire(t *testing.T, g *rateLimitGate) func() {
	t.Helper()
	release, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return release
}

func TestRateLimitGate_HoldsUntilWindowEnds(t *testing.T) {
	g, _ := testGate()
	g.rateLimited(200*time.Millisecond, time.Now())
	g.rateLimited(10*time.Millisecond, time.Now()) // a shorter wait must not reopen the window
	start := time.Now()
	mustAcquire(t, g)()
	if elapsed := time.Since(start); elapsed < 190*time.Millisecond {
		t.Errorf("acquire returned after %v, want >= 190ms", elapsed)
	}
}

// A window extended while a request sleeps must hold that request too.
func TestRateLimitGate_RechecksExtendedWindow(t *testing.T) {
	g, _ := testGate()
	g.rateLimited(100*time.Millisecond, time.Now())
	done := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		release, _ := g.acquire(context.Background())
		release()
		done <- time.Since(start)
	}()
	time.Sleep(50 * time.Millisecond)
	g.rateLimited(250*time.Millisecond, time.Now()) // now ends about 300ms after start
	if elapsed := <-done; elapsed < 290*time.Millisecond {
		t.Errorf("acquire returned after %v, want >= 290ms", elapsed)
	}
}

func TestRateLimitGate_SpreadsRelease(t *testing.T) {
	g, _ := testGate()
	var got time.Duration
	g.jitter = func(n time.Duration) time.Duration { got = n; return n - 1 }
	g.rateLimited(time.Millisecond, time.Now())
	mustAcquire(t, g)()
	if got != minReleaseSpread {
		t.Errorf("spread for a short wait = %v, want %v", got, minReleaseSpread)
	}
	g.rateLimited(time.Minute, time.Now())
	g.mu.Lock()
	spread := g.spread
	g.until = time.Time{}
	g.mu.Unlock()
	if spread != maxReleaseSpread {
		t.Errorf("spread for a long wait = %v, want %v", spread, maxReleaseSpread)
	}
	if n := newRateLimitGate().jitter(time.Second); n < 0 || n >= time.Second {
		t.Errorf("default jitter = %v, want [0, 1s)", n)
	}
}

func TestRateLimitGate_HalvesLimitOncePerLoad(t *testing.T) {
	g, logs := testGate()
	var releases []func()
	for range 8 {
		releases = append(releases, mustAcquire(t, g))
	}
	sentBefore := time.Now()
	g.rateLimited(0, sentBefore)
	g.rateLimited(0, sentBefore) // sent under the same load: no second decrease
	if g.limit != 4 || g.ceiling != 8 {
		t.Fatalf("limit/ceiling = %d/%d, want 4/8", g.limit, g.ceiling)
	}
	g.rateLimited(0, time.Now().Add(time.Millisecond)) // sent after the decrease
	if g.limit != 2 {
		t.Fatalf("limit = %d, want 2", g.limit)
	}
	for range 3 {
		g.rateLimited(0, time.Now().Add(time.Hour))
	}
	if g.limit != 1 {
		t.Fatalf("limit = %d, want it to stop at 1", g.limit)
	}
	for _, r := range releases {
		r()
	}
	want := []string{
		"rate limited by the provider: allowing 4 concurrent LLM request(s)",
		"rate limited by the provider: allowing 2 concurrent LLM request(s)",
		"rate limited by the provider: allowing 1 concurrent LLM request(s)",
	}
	if strings.Join(*logs, "\n") != strings.Join(want, "\n") {
		t.Errorf("logs = %q, want %q", *logs, want)
	}
}

func TestRateLimitGate_LimitBlocksUntilRelease(t *testing.T) {
	g, _ := testGate()
	g.limit, g.ceiling = 1, 4
	first := mustAcquire(t, g)
	acquired := make(chan struct{})
	go func() {
		mustAcquire(t, g)()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second request passed a full limit")
	case <-time.After(50 * time.Millisecond):
	}
	first()
	first() // freeing twice must not free a second slot
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second request not admitted after the slot freed")
	}
	if g.inFlight != 0 {
		t.Errorf("inFlight = %d, want 0", g.inFlight)
	}
}

func TestRateLimitGate_SuccessesRaiseLimitToCeiling(t *testing.T) {
	g, logs := testGate()
	g.succeeded() // no limit yet: nothing to raise
	g.limit, g.ceiling = 2, 3
	g.succeeded()
	if g.limit != 2 {
		t.Fatalf("limit = %d after one success, want 2", g.limit)
	}
	g.succeeded()
	if g.limit != 3 {
		t.Fatalf("limit = %d after two successes, want 3", g.limit)
	}
	for range 5 {
		g.succeeded()
	}
	if g.limit != 3 {
		t.Errorf("limit = %d, want it capped at the ceiling 3", g.limit)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "allowing 3") {
		t.Errorf("logs = %q", *logs)
	}
}

func TestRateLimitGate_AcquireHonorsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g, _ := testGate()
	g.rateLimited(time.Minute, time.Now())
	if _, err := g.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("acquire in a closed window = %v, want context.Canceled", err)
	}

	g, _ = testGate()
	g.limit, g.ceiling = 1, 2
	defer mustAcquire(t, g)()
	if _, err := g.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("acquire at a full limit = %v, want context.Canceled", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://example.test/", nil).WithContext(ctx)
	called := false
	resp, err := g.middleware(req, func(*http.Request) (*http.Response, error) { called = true; return nil, nil })
	if called || resp != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("middleware = (%v, %v, called=%v), want canceled without sending", resp, err, called)
	}
}

func TestRateLimitGate_MiddlewareFreesSlot(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.test/", nil)

	g, _ := testGate()
	_, _ = g.middleware(req, func(*http.Request) (*http.Response, error) { return nil, errors.New("reset") })
	_, _ = g.middleware(req, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}, nil
	})
	if g.inFlight != 0 {
		t.Fatalf("inFlight = %d after an error and a body-less response, want 0", g.inFlight)
	}

	resp, _ := g.middleware(req, hintResponse(http.StatusOK, "ok", nil))
	if g.inFlight != 1 {
		t.Fatalf("inFlight = %d while the body is unread, want 1", g.inFlight)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != "ok" || g.inFlight != 0 {
		t.Fatalf("body %q, inFlight = %d after EOF, want ok and 0", b, g.inFlight)
	}
	_ = resp.Body.Close()
	if g.inFlight != 0 {
		t.Errorf("inFlight = %d after Close, want 0", g.inFlight)
	}

	resp, _ = g.middleware(req, hintResponse(http.StatusOK, "ok", nil))
	_ = resp.Body.Close()
	if g.inFlight != 0 {
		t.Errorf("inFlight = %d after Close without reading, want 0", g.inFlight)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, _ = g.middleware(req.WithContext(ctx), hintResponse(http.StatusOK, "ok", nil))
	cancel()
	deadline := time.Now().Add(time.Second)
	for g.inFlightNow() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := g.inFlightNow(); n != 0 {
		t.Errorf("inFlight = %d after the request context ended, want 0", n)
	}
}

func (g *rateLimitGate) inFlightNow() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight
}

func TestRateLimitGate_MiddlewareReactsToStatus(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.test/", nil)
	g, _ := testGate()
	resp, _ := g.middleware(req, hintResponse(http.StatusTooManyRequests, "slow down", nil))
	_ = resp.Body.Close()
	if g.limit != 1 {
		t.Errorf("limit = %d after a 429 without a stated wait, want 1", g.limit)
	}

	g, _ = testGate()
	resp, _ = g.middleware(req, hintResponse(http.StatusTooManyRequests, "", http.Header{"Retry-After": {"30"}}))
	_ = resp.Body.Close()
	if left := time.Until(g.until); left < 29*time.Second {
		t.Errorf("gate open for %v after Retry-After: 30, want about 30s", left)
	}
	g.until = time.Time{}

	g, _ = testGate()
	resp, _ = g.middleware(req, hintResponse(http.StatusInternalServerError, "boom", nil))
	_ = resp.Body.Close()
	if g.limit != 0 || !g.until.IsZero() {
		t.Errorf("a 500 without a stated wait changed the gate: limit %d, until %v", g.limit, g.until)
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
		_, _ = w.Write([]byte(chatOKBody))
	}))
	defer server.Close()

	client := NewOpenAIClient(ClientConfig{URL: server.URL + "/v1", APIKey: "k", Model: "m"})
	errs := make(chan error, 2)
	go func() { errs <- pingClient(client) }()
	<-limited
	time.Sleep(50 * time.Millisecond) // the rate-limit response has reached the client
	go func() { errs <- pingClient(client) }()
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

// A provider that can serve only two requests at once rejects the rest with a
// short stated wait. Waiting alone leaves eight requests competing for two
// slots until they run out of retries; lowering the load lets all succeed.
func TestOpenAIClient_RateLimitLowersConcurrency(t *testing.T) {
	const capacity = 2
	var inFlight, rejected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if inFlight.Add(1) > capacity {
			inFlight.Add(-1)
			rejected.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"[429]: Rate limit exceeded (reset after 50ms)"}`))
			return
		}
		time.Sleep(300 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = w.Write([]byte(chatOKBody))
	}))
	defer server.Close()

	client := NewOpenAIClient(ClientConfig{URL: server.URL + "/v1", APIKey: "k", Model: "m"})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pingClient(client)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("CompletionsWithCtx: %v (rejected attempts: %d)", err, rejected.Load())
		}
	}
}

const chatOKBody = `{"id":"c","object":"chat.completion","model":"m",
	"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
	"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func pingClient(client *OpenAIClient) error {
	_, err := client.CompletionsWithCtx(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "ping"}},
		MaxTokens: 16,
	})
	return err
}

func TestSlotBody_ReadErrorKeepsSlotUntilClose(t *testing.T) {
	released := 0
	b := &slotBody{ReadCloser: failingBody{errors.New("broken")}, release: sync.OnceFunc(func() { released++ })}
	if _, err := b.Read(make([]byte, 8)); err == nil {
		t.Fatal("Read: want error")
	}
	if released != 0 {
		t.Fatalf("released after a read error, want only on EOF or Close")
	}
	_ = b.Close()
	_ = b.Close()
	if released != 1 {
		t.Errorf("released %d times after Close, want 1", released)
	}
}
