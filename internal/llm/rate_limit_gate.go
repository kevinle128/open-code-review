// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/alibaba/open-code-review/internal/stdout"
)

const (
	minReleaseSpread = 100 * time.Millisecond
	maxReleaseSpread = 2 * time.Second
)

// rateLimitGate controls when the requests of one client reach the provider.
// A review sends many requests at once, and a gateway that shares one account
// across them answers with rate limits once their total load exceeds what the
// account allows. Waiting the stated time does not help on its own: the same
// load returns when the window ends, so the requests keep failing until they
// run out of retries. The gate therefore does two things:
//
//   - While a stated rate-limit wait is open, it holds every request of the
//     client, then releases them at slightly different times.
//   - On each rate limit it halves the number of requests allowed in flight,
//     and after enough successes it raises that number by one again, up to
//     the load at which the first rate limit arrived.
//
// It is admission, not a retry loop: the SDK still decides whether and how
// often to retry. A request that waits here counts against the SDK's
// per-attempt timeout, so it cannot wait forever.
type rateLimitGate struct {
	mu     sync.Mutex
	until  time.Time
	spread time.Duration

	inFlight int
	// limit is the number of requests allowed in flight; 0 means no limit.
	limit int
	// ceiling is the number of requests in flight when the first rate limit
	// arrived. The limit never grows past it.
	ceiling   int
	successes int
	// lastDecrease is when the limit was last lowered. Rate limits on requests
	// sent before it were caused by the old load, so they do not lower it again.
	lastDecrease time.Time
	// changed is closed and replaced whenever a slot frees or the limit grows.
	changed chan struct{}

	// jitter returns a random duration in [0, n); replaced in tests.
	jitter func(n time.Duration) time.Duration
	logf   func(format string, args ...any)
}

func newRateLimitGate() *rateLimitGate {
	return &rateLimitGate{
		changed: make(chan struct{}),
		jitter:  func(n time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(n))) },
		logf: func(format string, args ...any) {
			fmt.Fprintf(stdout.Writer(), "[ocr] "+format+"\n", args...)
		},
	}
}

// middleware is structurally compatible with both option.Middleware
// (Anthropic SDK) and openaiopt.Middleware (OpenAI SDK).
func (g *rateLimitGate) middleware(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	release, err := g.acquire(req.Context())
	if err != nil {
		return nil, err
	}
	sentAt := time.Now()
	resp, err := next(req)
	if err == nil && resp != nil {
		wait, stated := applyRetryHint(resp, err)
		switch {
		case stated || resp.StatusCode == http.StatusTooManyRequests:
			g.rateLimited(wait, sentAt)
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			g.succeeded()
		}
	}
	if err != nil || resp == nil || resp.Body == nil {
		release()
		return resp, err
	}
	// A streamed response keeps the provider busy until its body is read, so
	// the slot is held until the body ends or closes. The request context is
	// a backstop for a body that is dropped without being closed.
	stop := context.AfterFunc(req.Context(), release)
	resp.Body = &slotBody{ReadCloser: resp.Body, release: func() { stop(); release() }}
	return resp, err
}

// acquire waits until the gate is open and a slot is free, then takes the
// slot. The returned function frees it and is safe to call more than once.
func (g *rateLimitGate) acquire(ctx context.Context) (func(), error) {
	for {
		g.mu.Lock()
		if delay := time.Until(g.until); delay > 0 {
			// Requests that waited leave at random points inside a short
			// spread after the window, never before it, so they do not reach
			// the server on the same instant.
			delay += g.jitter(g.spread)
			g.mu.Unlock()
			if err := sleepContext(ctx, delay); err != nil {
				return nil, err
			}
			continue // a response may have extended the window meanwhile
		}
		if g.limit == 0 || g.inFlight < g.limit {
			g.inFlight++
			g.mu.Unlock()
			return sync.OnceFunc(g.release), nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (g *rateLimitGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight--
	g.notifyLocked()
}

// rateLimited closes the gate for the stated wait and halves the number of
// requests allowed in flight, once per load: only a request sent after the
// last decrease shows that the lower limit is still too high.
func (g *rateLimitGate) rateLimited(wait time.Duration, sentAt time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if until := now.Add(wait); until.After(g.until) {
		g.until = until
		g.spread = min(max(wait/4, minReleaseSpread), maxReleaseSpread)
	}
	if !sentAt.After(g.lastDecrease) {
		return
	}
	g.lastDecrease = now
	current := g.limit
	if current == 0 {
		current = g.inFlight
		g.ceiling = max(g.inFlight, 1)
	}
	g.successes = 0
	if next := max(current/2, 1); next != g.limit {
		g.limit = next
		g.logf("rate limited by the provider: allowing %d concurrent LLM request(s)", next)
	}
}

// succeeded raises the limit by one after as many successes as the limit
// allows at once, up to the ceiling.
func (g *rateLimitGate) succeeded() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limit == 0 || g.limit >= g.ceiling {
		return
	}
	g.successes++
	if g.successes < g.limit {
		return
	}
	g.successes = 0
	g.limit++
	g.logf("provider accepting requests again: allowing %d concurrent LLM request(s)", g.limit)
	g.notifyLocked()
}

func (g *rateLimitGate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// slotBody frees the gate slot when the body ends or closes. A read error
// does not free it: the SDK closes the body after any error.
type slotBody struct {
	io.ReadCloser
	release func()
}

func (b *slotBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.release()
	}
	return n, err
}

func (b *slotBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}
