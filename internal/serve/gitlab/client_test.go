// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

const testToken = "glpat-test-token"

type recorded struct {
	Method string
	Path   string // escaped path, without /api/v4
	Query  url.Values
	Body   []byte
	Token  string
}

// fakeGitLab is an httptest GitLab that records every request. Handlers are
// registered with Go ServeMux patterns relative to /api/v4.
type fakeGitLab struct {
	t   *testing.T
	mux *http.ServeMux
	srv *httptest.Server

	mu       sync.Mutex
	reqs     []recorded
	failures map[string]int
}

func newFake(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{t: t, mux: http.NewServeMux()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4")
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{Method: r.Method, Path: path, Query: r.URL.Query(), Body: body, Token: r.Header.Get("PRIVATE-TOKEN")})
		status := f.failures[r.Method+" "+path]
		f.mu.Unlock()
		if status != 0 {
			writeJSON(w, status, map[string]string{"message": "injected failure"})
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v4")
		r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, "/api/v4")
		f.mux.ServeHTTP(w, r2)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitLab) handle(pattern string, h http.HandlerFunc) { f.mux.HandleFunc(pattern, h) }

// fail makes every request for "METHOD /escaped/path" answer status, taking
// precedence over the registered handlers.
func (f *fakeGitLab) fail(methodPath string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == nil {
		f.failures = map[string]int{}
	}
	f.failures[methodPath] = status
}

func (f *fakeGitLab) json(pattern string, status int, v any) {
	f.handle(pattern, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, v) })
}

func (f *fakeGitLab) requests(method string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.reqs {
		if method == "" || r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGitLab) writes() []recorded {
	var out []recorded
	for _, r := range f.requests("") {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// sleeps records the waits a Sleeper was asked for, without waiting.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

func newTestForge(t *testing.T, f *fakeGitLab, sl *sleeps) *Forge {
	t.Helper()
	if sl == nil {
		sl = &sleeps{}
	}
	fg, err := New(serveconfig.GitLabConfig{URL: f.srv.URL, Token: testToken, WebhookSecret: "s3cret", BotUsername: "pr-review"}, f.srv.Client(), sl.sleep)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return fg
}

func TestNewClientValidation(t *testing.T) {
	for _, tc := range []struct{ url, token string }{
		{"", "t"}, {"ftp://x", "t"}, {"https://", "t"}, {"https://git.example.test", " "}, {"://bad", "t"},
	} {
		if _, err := newClient(tc.url, tc.token, nil, nil); err == nil {
			t.Errorf("newClient(%q, %q): want error", tc.url, tc.token)
		}
	}
	c, err := newClient("https://git.example.test/", "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.api != "https://git.example.test/api/v4" || c.httpc == nil || c.sleep == nil {
		t.Fatalf("unexpected client %+v", c)
	}
	if _, err := New(serveconfig.GitLabConfig{URL: "x"}, nil, nil); err == nil {
		t.Fatal("New with bad config: want error")
	}
}

func TestCallSendsTokenAndDecodes(t *testing.T) {
	f := newFake(t)
	f.handle("POST /things", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		writeJSON(w, 201, map[string]int{"id": 3})
	})
	f.handle("GET /empty", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	f.handle("GET /garbage", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{not json")) })
	f.handle("GET /fail", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(strings.Repeat("x ", 400)))
	})
	c, _ := newClient(f.srv.URL, testToken, f.srv.Client(), (&sleeps{}).sleep)
	ctx := context.Background()

	var out struct{ ID int }
	if _, err := c.call(ctx, http.MethodPost, "/things", nil, map[string]string{"a": "b"}, &out); err != nil || out.ID != 3 {
		t.Fatalf("call: %v %+v", err, out)
	}
	if got := f.requests(http.MethodPost)[0]; got.Token != testToken || string(got.Body) != `{"a":"b"}` {
		t.Fatalf("recorded %+v", got)
	}
	if _, err := c.call(ctx, http.MethodGet, "/empty", nil, nil, &out); err != nil {
		t.Fatalf("empty body: %v", err)
	}
	if _, err := c.call(ctx, http.MethodGet, "/garbage", nil, nil, &out); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("garbage: %v", err)
	}
	_, err := c.call(ctx, http.MethodGet, "/fail", nil, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 403 || len(apiErr.Body) > errBodyCap+3 || !strings.HasSuffix(apiErr.Body, "...") {
		t.Fatalf("fail: %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("error leaks the token")
	}
	if _, err := c.call(ctx, http.MethodPost, "/things", nil, func() {}, nil); err == nil {
		t.Fatal("unencodable payload: want error")
	}
	if _, err := c.call(ctx, "BAD METHOD", "/things", nil, nil, nil); err == nil {
		t.Fatal("bad method: want error")
	}
}

func TestCallRetriesRateLimit(t *testing.T) {
	f := newFake(t)
	n := 0
	f.handle("GET /limited", func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("RateLimit-Remaining", "0")
			w.Header().Set("RateLimit-Reset", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(w, 200, []int{1})
	})
	sl := &sleeps{}
	c, _ := newClient(f.srv.URL, testToken, f.srv.Client(), sl.sleep)
	if _, err := c.call(context.Background(), http.MethodGet, "/limited", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(sl.all()) != 1 {
		t.Fatalf("calls %d sleeps %v", n, sl.all())
	}
}

func TestPostRetriesOnlyRateLimits(t *testing.T) {
	f := newFake(t)
	statuses := map[string][]int{"/limited": {429, 201}, "/gateway": {502, 201}, "/forbidden": {403, 201}, "/always": {429, 429, 429, 429, 429}}
	calls := map[string]int{}
	for path := range statuses {
		f.handle("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			n := calls[r.URL.Path]
			calls[r.URL.Path]++
			if st := statuses[r.URL.Path][n]; st == 429 {
				w.Header().Set("Retry-After", "1")
				writeJSON(w, st, map[string]string{})
			} else {
				writeJSON(w, st, map[string]string{})
			}
		})
	}
	sl := &sleeps{}
	c, _ := newClient(f.srv.URL, testToken, f.srv.Client(), sl.sleep)
	ctx := context.Background()
	if _, err := c.call(ctx, http.MethodPost, "/limited", nil, nil, nil); err != nil || calls["/limited"] != 2 {
		t.Fatalf("429 then 201: %v calls=%d", err, calls["/limited"])
	}
	if _, err := c.call(ctx, http.MethodPost, "/gateway", nil, nil, nil); !isStatus(err, 502) || calls["/gateway"] != 1 {
		t.Fatalf("502 must not be retried: %v calls=%d", err, calls["/gateway"])
	}
	if _, err := c.call(ctx, http.MethodPost, "/forbidden", nil, nil, nil); !isStatus(err, 403) || calls["/forbidden"] != 1 {
		t.Fatalf("403 without rate-limit text: %v calls=%d", err, calls["/forbidden"])
	}
	if _, err := c.call(ctx, http.MethodPost, "/always", nil, nil, nil); !isStatus(err, 429) || calls["/always"] != maxAttempts {
		t.Fatalf("attempts cap: %v calls=%d", err, calls["/always"])
	}
	failing, _ := newClient(f.srv.URL, testToken, f.srv.Client(), func(context.Context, time.Duration) error { return context.Canceled })
	calls["/limited"] = 0
	if _, err := failing.call(ctx, http.MethodPost, "/limited", nil, nil, nil); err == nil {
		t.Fatal("failed sleep: want error")
	}
}

func TestPaginate(t *testing.T) {
	f := newFake(t)
	f.handle("GET /items", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("x") != "y" {
			t.Errorf("query %v", r.URL.Query())
		}
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			writeJSON(w, 200, []int{1, 2})
		default:
			writeJSON(w, 200, []int{3})
		}
	})
	f.handle("GET /loop", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Next-Page", "1")
		writeJSON(w, 200, []int{})
	})
	f.handle("GET /broken", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	c, _ := newClient(f.srv.URL, testToken, f.srv.Client(), (&sleeps{}).sleep)
	ctx := context.Background()
	got, err := paginate[int](ctx, c, "/items", url.Values{"x": {"y"}})
	if err != nil || len(got) != 3 || got[2] != 3 {
		t.Fatalf("items %v %v", got, err)
	}
	if _, err := paginate[int](ctx, c, "/loop", nil); err == nil {
		t.Fatal("loop: want error")
	}
	if _, err := paginate[int](ctx, c, "/broken", nil); err == nil {
		t.Fatal("broken: want error")
	}
	f2 := newFake(t)
	count := 0
	f2.handle("GET /many", func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.Header().Set("X-Next-Page", strconv.Itoa(count+1))
		writeJSON(w, 200, []int{count})
	})
	c2, _ := newClient(f2.srv.URL, testToken, f2.srv.Client(), (&sleeps{}).sleep)
	if _, err := paginate[int](ctx, c2, "/many", nil); err == nil || !strings.Contains(err.Error(), "pages") || count != maxPages {
		t.Fatalf("many: %v after %d calls", err, count)
	}
}

func TestProjectAndMRPath(t *testing.T) {
	if p, _ := projectPath(forge.PRRef{ProjectID: 42, Repo: "a/b"}); p != "/projects/42" {
		t.Fatal(p)
	}
	if p, _ := projectPath(forge.PRRef{Repo: "group/sub/proj"}); p != "/projects/group%2Fsub%2Fproj" {
		t.Fatal(p)
	}
	if _, err := projectPath(forge.PRRef{}); err == nil {
		t.Fatal("empty ref: want error")
	}
	if _, err := mrPath(forge.PRRef{ProjectID: 1}); err == nil {
		t.Fatal("zero iid: want error")
	}
	if _, err := mrPath(forge.PRRef{}); err == nil {
		t.Fatal("empty ref: want error")
	}
	if p, _ := mrPath(forge.PRRef{ProjectID: 1, Number: 9}); p != "/projects/1/merge_requests/9" {
		t.Fatal(p)
	}
}
