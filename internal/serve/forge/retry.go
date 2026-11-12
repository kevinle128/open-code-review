// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sleeper waits for d or until ctx is done. Tests inject a fake one.
type Sleeper func(context.Context, time.Duration) error

// SleepContext is the real Sleeper.
func SleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const (
	retryCap           = 300 * time.Second
	rateLimitBase      = 60 * time.Second
	transientBase      = 2 * time.Second
	maxJitterMillis    = 1000
	maxResponseBytes   = 32 << 20
	defaultMaxAttempts = 1
)

var rateLimitTextRE = regexp.MustCompile(`(?i)rate limit|abuse|secondary`)

// RetryDelay decides whether a response is worth retrying and how long to
// wait first. body is the response body, which Do has already read; GitHub
// marks a secondary rate limit on a 403 only in the body text.
//
// Rate limits (429, or 403 with rate-limit text) honor Retry-After (seconds or
// an HTTP date), then a reset time when the remaining quota is 0 (GitHub
// X-RateLimit-*, GitLab RateLimit-*), else back off exponentially from 60s,
// because GitHub asks for at least one minute after a secondary limit.
// Transient errors (5xx, 408) back off from 2s. Every delay is capped at 300s.
// rnd returns the jitter in milliseconds; values outside [0, 1000) are folded
// into that range.
func RetryDelay(resp *http.Response, body []byte, attempt int, now time.Time, rnd func() int) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	status := resp.StatusCode
	isRateLimit := status == http.StatusTooManyRequests ||
		(status == http.StatusForbidden && rateLimitTextRE.Match(body))
	isTransient := (status >= 500 && status < 600) || status == http.StatusRequestTimeout
	if !isRateLimit && !isTransient {
		return 0, false
	}
	if attempt < 0 {
		attempt = 0
	}

	var delay time.Duration
	if isRateLimit {
		var ok bool
		if delay, ok = headerDelay(resp.Header, now); !ok {
			delay = backoff(rateLimitBase, attempt) + jitter(rnd)
		}
	} else {
		delay = backoff(transientBase, attempt) + jitter(rnd)
	}
	return min(delay, retryCap), true
}

func headerDelay(h http.Header, now time.Time) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
			return time.Duration(min(secs, int64(retryCap/time.Second))) * time.Second, true
		}
		if at, err := http.ParseTime(v); err == nil {
			return max(0, at.Sub(now)), true
		}
	}
	for _, prefix := range []string{"X-RateLimit-", "RateLimit-"} {
		remaining := strings.TrimSpace(h.Get(prefix + "Remaining"))
		reset := strings.TrimSpace(h.Get(prefix + "Reset"))
		if remaining != "0" || reset == "" {
			continue
		}
		epoch, err := strconv.ParseInt(reset, 10, 64)
		if err != nil {
			continue
		}
		return max(0, time.Unix(epoch, 0).Sub(now)), true
	}
	return 0, false
}

func backoff(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 0; i < attempt && d < retryCap; i++ {
		d *= 2
	}
	return min(d, retryCap)
}

func jitter(rnd func() int) time.Duration {
	if rnd == nil {
		return 0
	}
	ms := rnd() % maxJitterMillis
	if ms < 0 {
		ms += maxJitterMillis
	}
	return time.Duration(ms) * time.Millisecond
}

// Do sends the request from build and retries the responses RetryDelay
// accepts, up to maxAttempts sends in total (values below 1 mean one send).
// build is called once per attempt so each send gets a fresh body. The
// response body is read (at most 32 MiB), closed, and returned as bytes.
//
// When attempts run out, Do returns the last response and body with a nil
// error, so callers handle every non-2xx status the same way. Transport
// errors are returned at once and not retried: a POST may already have been
// applied. A done ctx stops both the send and the wait. A nil sleep uses
// SleepContext.
func Do(ctx context.Context, c *http.Client, build func() (*http.Request, error), sleep Sleeper, maxAttempts int) (*http.Response, []byte, error) {
	if c == nil {
		c = http.DefaultClient
	}
	if sleep == nil {
		sleep = SleepContext
	}
	if maxAttempts < defaultMaxAttempts {
		maxAttempts = defaultMaxAttempts
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		req, err := build()
		if err != nil {
			return nil, nil, fmt.Errorf("build request: %w", err)
		}
		resp, err := c.Do(req.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		body, err := readBody(resp)
		if err != nil {
			return resp, nil, err
		}
		if attempt+1 >= maxAttempts {
			return resp, body, nil
		}
		delay, retry := RetryDelay(resp, body, attempt, time.Now(), randomJitter)
		if !retry {
			return resp, body, nil
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, nil, err
		}
	}
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, errResponseTooLarge
	}
	return body, nil
}

var errResponseTooLarge = errors.New("response body exceeds 32 MiB")

func randomJitter() int {
	return rand.IntN(maxJitterMillis)
}
