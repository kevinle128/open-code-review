// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"bytes"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// retryHintBodyLimit bounds how much of an error body is read to look for a
// wait hint. Rate-limit bodies are short; the rest stays readable for the SDK.
const retryHintBodyLimit = 64 << 10

// retryHintPattern finds a wait the server states in prose, such as
// "reset after 9s", "reset after 1m 4s", "Please try again in 1m30s" or
// "retry in 6.5 seconds".
var retryHintPattern = regexp.MustCompile(
	`(?i)\b(?:retry|try again|reset|resets|wait)\s+(?:after|in)\s+` +
		`((?:\d+(?:\.\d+)?(?:ms|h|m|s)\s*)+|\d+(?:\.\d+)?\s*(?:milliseconds?|seconds?|secs?|minutes?|mins?))\b`,
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
