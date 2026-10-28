// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"bytes"
	"context"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// retryHintBodyLimit bounds how much of an error body is read to look for a
// wait hint. Rate-limit bodies are short; the rest stays readable for the SDK.
const retryHintBodyLimit = 64 << 10

// retryHintPattern finds a wait the server states in prose, such as
// "reset after 9s", "Please try again in 1m30s" or "retry in 6.5 seconds".
var retryHintPattern = regexp.MustCompile(
	`(?i)\b(?:retry|try again|reset|resets|wait)\s+(?:after|in)\s+` +
		`((?:\d+(?:\.\d+)?(?:ms|h|m|s))+|\d+(?:\.\d+)?\s*(?:milliseconds?|seconds?|secs?|minutes?|mins?))\b`,
)

// Both SDKs honor Retry-After-Ms and Retry-After, but some gateways report a
// rate limit as "429 ... (reset after 9s)" or wrap it in a 503 without either
// header, so the SDK falls back to its own short backoff and retries before the
// limit resets. applyRetryHint copies such a stated wait into Retry-After-Ms;
// the SDK stays the owner of the retry decision, the attempt count and the wait.

// applyRetryHint returns the wait a retryable response asks for. A wait stated
// only in the body is also copied into Retry-After-Ms for the SDK.
func applyRetryHint(resp *http.Response, err error) (time.Duration, bool) {
	if err != nil || resp == nil || !retryableStatus(resp) {
		return 0, false
	}
	if wait, ok := headerRetryWait(resp.Header); ok {
		return wait, true
	}
	if resp.Body == nil {
		return 0, false
	}
	head, readErr := io.ReadAll(io.LimitReader(resp.Body, retryHintBodyLimit))
	// Hand the SDK the body it would have read without this middleware,
	// including a read error after the bytes already consumed.
	rest := resp.Body
	if readErr != nil {
		rest = io.NopCloser(errReader{readErr})
	}
	resp.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(head), rest), Closer: resp.Body}
	wait, ok := parseRetryHint(string(head))
	if !ok {
		return 0, false
	}
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	resp.Header.Set("Retry-After-Ms", strconv.FormatInt(wait.Milliseconds(), 10))
	return wait, true
}

// headerRetryWait reads the wait from Retry-After-Ms or Retry-After (seconds
// or an HTTP date), in the order of preference both SDKs use.
func headerRetryWait(h http.Header) (time.Duration, bool) {
	if v := h.Get("Retry-After-Ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil {
			return positiveDuration(ms, time.Millisecond)
		}
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil {
		return positiveDuration(s, time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		return d, d > 0
	}
	return 0, false
}

const (
	minReleaseSpread = 100 * time.Millisecond
	maxReleaseSpread = 2 * time.Second
)

// rateLimitGate holds every request of one client while the server's stated
// rate-limit wait is open. The SDK retry loop only delays the request that
// received the rate limit; the other concurrent review requests would still
// reach the same account inside the closed window, receive a rate limit too
// and spend their own retry attempts, so all of them exhaust their attempts
// together. The gate is admission, not a retry loop: the SDK still decides
// whether and how often to retry.
type rateLimitGate struct {
	mu     sync.Mutex
	until  time.Time
	spread time.Duration
	// jitter returns a random duration in [0, n); replaced in tests.
	jitter func(n time.Duration) time.Duration
}

func newRateLimitGate() *rateLimitGate {
	return &rateLimitGate{jitter: func(n time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(n))) }}
}

// middleware is structurally compatible with both option.Middleware
// (Anthropic SDK) and openaiopt.Middleware (OpenAI SDK).
func (g *rateLimitGate) middleware(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if err := g.wait(req.Context()); err != nil {
		return nil, err
	}
	resp, err := next(req)
	if wait, ok := applyRetryHint(resp, err); ok {
		g.hold(wait)
	}
	return resp, err
}

// hold closes the gate for at least wait. A shorter wait never reopens a
// window that an earlier response closed for longer.
func (g *rateLimitGate) hold(wait time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until := time.Now().Add(wait); until.After(g.until) {
		g.until = until
		g.spread = min(max(wait/4, minReleaseSpread), maxReleaseSpread)
	}
}

// wait blocks while the gate is closed. Requests that waited are released at
// random points inside a short spread after the window, never before it, so
// they do not reach the server on the same instant and close it again.
func (g *rateLimitGate) wait(ctx context.Context) error {
	// A response that arrives while this request sleeps can extend the window,
	// so the gate is checked again after every sleep.
	for {
		g.mu.Lock()
		delay := time.Until(g.until)
		if delay > 0 {
			delay += g.jitter(g.spread)
		}
		g.mu.Unlock()
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// retryableStatus mirrors the status codes both SDKs retry by default, plus an
// explicit x-should-retry: true (set by retryCodesMiddleware for retry_codes).
func retryableStatus(resp *http.Response) bool {
	if resp.Header.Get("x-should-retry") == "true" {
		return true
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return resp.StatusCode >= http.StatusInternalServerError
}

// parseRetryHint returns the first positive wait stated in body.
func parseRetryHint(body string) (time.Duration, bool) {
	m := retryHintPattern.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	value := strings.ToLower(strings.ReplaceAll(m[1], " ", ""))
	if d, err := time.ParseDuration(value); err == nil {
		return d, d > 0
	}
	i := strings.IndexFunc(value, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	n, err := strconv.ParseFloat(value[:i], 64)
	if err != nil {
		return 0, false
	}
	unit := time.Second
	switch {
	case strings.HasPrefix(value[i:], "milli"):
		unit = time.Millisecond
	case strings.HasPrefix(value[i:], "min"):
		unit = time.Minute
	}
	return positiveDuration(n, unit)
}

// positiveDuration converts n units to a Duration. A value that is not
// positive, not finite or too large for a Duration states no usable wait.
func positiveDuration(n float64, unit time.Duration) (time.Duration, bool) {
	if !(n > 0) || n >= float64(math.MaxInt64)/float64(unit) {
		return 0, false
	}
	d := time.Duration(n * float64(unit))
	return d, d > 0
}

type readCloser struct {
	io.Reader
	io.Closer
}
