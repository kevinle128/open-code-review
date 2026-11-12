// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package forge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func resp(status int, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h}
}

func fixedJitter(ms int) func() int { return func() int { return ms } }

func TestRetryDelay(t *testing.T) {
	reset := strconv.FormatInt(fixedNow.Add(90*time.Second).Unix(), 10)
	cases := []struct {
		name    string
		resp    *http.Response
		body    string
		attempt int
		jitter  int
		want    time.Duration
		retry   bool
	}{
		{"nil response", nil, "", 0, 0, 0, false},
		{"not retryable", resp(404, nil), "", 0, 0, 0, false},
		{"forbidden without rate limit text", resp(403, nil), "Resource not accessible", 0, 0, 0, false},
		{"retry-after seconds", resp(429, map[string]string{"Retry-After": "7"}), "", 3, 500, 7 * time.Second, true},
		{"retry-after http date", resp(429, map[string]string{"Retry-After": fixedNow.Add(42 * time.Second).Format(http.TimeFormat)}), "", 0, 0, 42 * time.Second, true},
		{"retry-after date in the past", resp(429, map[string]string{"Retry-After": fixedNow.Add(-time.Minute).Format(http.TimeFormat)}), "", 0, 0, 0, true},
		{"retry-after capped", resp(429, map[string]string{"Retry-After": "100000"}), "", 0, 0, 300 * time.Second, true},
		{"github reset", resp(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}), "API rate limit exceeded", 0, 0, 90 * time.Second, true},
		{"gitlab reset", resp(429, map[string]string{"RateLimit-Remaining": "0", "RateLimit-Reset": reset}), "", 0, 0, 90 * time.Second, true},
		{"reset with quota left falls back", resp(429, map[string]string{"X-RateLimit-Remaining": "5", "X-RateLimit-Reset": reset}), "", 0, 250, 60*time.Second + 250*time.Millisecond, true},
		{"bad reset falls back", resp(429, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "soon"}), "", 0, 0, 60 * time.Second, true},
		{"bad retry-after falls back", resp(429, map[string]string{"Retry-After": "later"}), "", 1, 0, 120 * time.Second, true},
		{"secondary limit backoff", resp(403, nil), "You have exceeded a secondary rate limit", 1, 999, 120*time.Second + 999*time.Millisecond, true},
		{"secondary limit capped", resp(403, nil), "abuse detection", 5, 999, 300 * time.Second, true},
		{"server error", resp(502, nil), "", 0, 100, 2*time.Second + 100*time.Millisecond, true},
		{"server error grows", resp(503, nil), "", 3, 0, 16 * time.Second, true},
		{"request timeout", resp(408, nil), "", -1, -1, 2*time.Second + 999*time.Millisecond, true},
		{"jitter folded", resp(500, nil), "", 0, 1234, 2*time.Second + 234*time.Millisecond, true},
		{"server error capped", resp(500, nil), "", 20, 0, 300 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, retry := RetryDelay(tc.resp, []byte(tc.body), tc.attempt, fixedNow, fixedJitter(tc.jitter))
			if got != tc.want || retry != tc.retry {
				t.Fatalf("got %v %v, want %v %v", got, retry, tc.want, tc.retry)
			}
		})
	}
	if d, ok := RetryDelay(resp(500, nil), nil, 0, fixedNow, nil); !ok || d != 2*time.Second {
		t.Fatalf("nil jitter: %v %v", d, ok)
	}
}

type sleepLog struct{ calls []time.Duration }

func (s *sleepLog) sleep(_ context.Context, d time.Duration) error {
	s.calls = append(s.calls, d)
	return nil
}

func statusServer(t *testing.T, statuses ...int) (*httptest.Server, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		status := statuses[min(i, len(statuses)-1)]
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte("attempt " + strconv.Itoa(i) + " " + string(body)))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func postBuilder(url string) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, url, strings.NewReader("payload"))
	}
}

func TestDo_RetriesThenSucceeds(t *testing.T) {
	srv, n := statusServer(t, 502, 429, 200)
	s := &sleepLog{}
	r, body, err := Do(context.Background(), srv.Client(), postBuilder(srv.URL), s.sleep, 5)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("err=%v status=%v", err, r)
	}
	if string(body) != "attempt 2 payload" || atomic.LoadInt32(n) != 3 || len(s.calls) != 2 {
		t.Fatalf("body=%q sends=%d sleeps=%v", body, *n, s.calls)
	}
}

func TestDo_GivesUpAfterMaxAttempts(t *testing.T) {
	srv, n := statusServer(t, 503)
	s := &sleepLog{}
	r, body, err := Do(context.Background(), srv.Client(), postBuilder(srv.URL), s.sleep, 3)
	if err != nil || r.StatusCode != 503 || string(body) != "attempt 2 payload" {
		t.Fatalf("err=%v resp=%v body=%q", err, r, body)
	}
	if atomic.LoadInt32(n) != 3 || len(s.calls) != 2 {
		t.Fatalf("sends=%d sleeps=%d", *n, len(s.calls))
	}
}

func TestDo_NonRetryableReturnsAtOnce(t *testing.T) {
	srv, n := statusServer(t, 404)
	r, _, err := Do(context.Background(), nil, postBuilder(srv.URL), nil, 0)
	if err != nil || r.StatusCode != 404 || atomic.LoadInt32(n) != 1 {
		t.Fatalf("err=%v resp=%v sends=%d", err, r, *n)
	}
}

func TestDo_HonorsContextCancel(t *testing.T) {
	srv, n := statusServer(t, 500)
	ctx, cancel := context.WithCancel(context.Background())
	sleep := func(ctx context.Context, d time.Duration) error {
		cancel()
		return SleepContext(ctx, d)
	}
	_, _, err := Do(ctx, srv.Client(), postBuilder(srv.URL), sleep, 5)
	if !errors.Is(err, context.Canceled) || atomic.LoadInt32(n) != 1 {
		t.Fatalf("err=%v sends=%d", err, *n)
	}
	if _, _, err := Do(ctx, srv.Client(), postBuilder(srv.URL), nil, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled before send: %v", err)
	}
}

func TestDo_BuildAndTransportErrors(t *testing.T) {
	boom := errors.New("boom")
	_, _, err := Do(context.Background(), nil, func() (*http.Request, error) { return nil, boom }, nil, 2)
	if !errors.Is(err, boom) {
		t.Fatalf("build error: %v", err)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, _, err := Do(context.Background(), nil, postBuilder(url), nil, 3); err == nil {
		t.Fatal("expected transport error")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errReader) Close() error             { return nil }

type fixedTransport struct{ body io.ReadCloser }

func (f fixedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: f.body}, nil
}

func TestDo_BodyErrors(t *testing.T) {
	c := &http.Client{Transport: fixedTransport{body: errReader{}}}
	if _, _, err := Do(context.Background(), c, postBuilder("http://example.invalid"), nil, 1); err == nil {
		t.Fatal("expected read error")
	}
	big := io.NopCloser(strings.NewReader(strings.Repeat("x", maxResponseBytes+1)))
	c = &http.Client{Transport: fixedTransport{body: big}}
	if _, _, err := Do(context.Background(), c, postBuilder("http://example.invalid"), nil, 1); !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("expected size error, got %v", err)
	}
}

func TestSleepContext(t *testing.T) {
	if err := SleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRandomJitterRange(t *testing.T) {
	for range 100 {
		if v := randomJitter(); v < 0 || v >= maxJitterMillis {
			t.Fatalf("jitter %d out of range", v)
		}
	}
}
