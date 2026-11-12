// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package checkout

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.email=ocr@example.test",
		"-c", "user.name=ocr",
		"-c", "commit.gpgsign=false",
		"-C", dir,
	}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remote is a bare repository laid out like a forge: a main branch, a PR
// head under the GitHub ref and an MR head under the GitLab ref.
type remote struct {
	dir     string
	url     string
	base    string // main tip
	first   string // first PR commit
	head    string // PR head
	sideTip string // commit only on an unrelated branch
}

func newRemote(t *testing.T) remote {
	t.Helper()
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	write(t, work, "a.txt", "one\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "base")
	base := git(t, work, "rev-parse", "HEAD")

	git(t, work, "checkout", "-q", "-b", "feature")
	write(t, work, "a.txt", "two\n")
	git(t, work, "commit", "-q", "-am", "first")
	first := git(t, work, "rev-parse", "HEAD")
	write(t, work, "b.txt", "new\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "second")
	head := git(t, work, "rev-parse", "HEAD")

	git(t, work, "checkout", "-q", "-b", "side", "main")
	write(t, work, "c.txt", "side\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "side")
	side := git(t, work, "rev-parse", "HEAD")

	bare := filepath.Join(t.TempDir(), "repo.git")
	git(t, work, "clone", "-q", "--bare", work, bare)
	git(t, bare, "update-ref", "refs/pull/1/head", head)
	git(t, bare, "update-ref", "refs/merge-requests/2/head", head)
	git(t, bare, "update-ref", "-d", "refs/heads/side")
	return remote{dir: bare, url: "file://" + filepath.ToSlash(bare), base: base, first: first, head: head, sideTip: side}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func acquire(t *testing.T, m *Manager) (*workspace, func()) {
	t.Helper()
	ws, release, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(release)
	return ws.(*workspace), release
}

func newManager(t *testing.T, slots int) *Manager {
	t.Helper()
	m, err := NewManager(filepath.Join(t.TempDir(), "work"), slots)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list))
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestNewManagerRejectsBadInput(t *testing.T) {
	if _, err := NewManager(t.TempDir(), 0); err == nil {
		t.Error("zero slots accepted")
	}
	if _, err := NewManager("  ", 1); err == nil {
		t.Error("empty work dir accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	write(t, filepath.Dir(file), "file", "x")
	if _, err := NewManager(file, 1); err == nil {
		t.Error("work dir that is a file accepted")
	}
	if _, err := NewManager(filepath.Join(file, "sub"), 1); err == nil {
		t.Error("work dir under a file accepted")
	}
}

func TestNewManagerEmptiesLeftoverSlots(t *testing.T) {
	work := t.TempDir()
	slot := filepath.Join(work, "slot-0")
	if err := os.MkdirAll(filepath.Join(slot, "old"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, slot, "stale.txt", "x")
	m, err := NewManager(work, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := entries(t, slot); len(got) != 0 {
		t.Fatalf("slot-0 not emptied: %v", got)
	}
	if got := entries(t, m.workDir); strings.Join(got, ",") != "slot-0,slot-1" {
		t.Fatalf("work dir = %v", got)
	}
}

func TestAcquireBoundsSlotsAndReleaseEmpties(t *testing.T) {
	m := newManager(t, 1)
	ws, release := acquire(t, m)
	if want := filepath.Join(m.workDir, "slot-0"); ws.Dir() != want {
		t.Fatalf("Dir = %q, want %q", ws.Dir(), want)
	}
	write(t, ws.Dir(), "x.txt", "x")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := m.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Acquire err = %v, want deadline", err)
	}

	release()
	release() // a second call must not return the slot twice
	if got := entries(t, ws.Dir()); len(got) != 0 {
		t.Fatalf("slot not emptied on release: %v", got)
	}
	ws2, release2 := acquire(t, m)
	if ws2.Dir() != ws.Dir() {
		t.Fatalf("slot dir changed: %q vs %q", ws2.Dir(), ws.Dir())
	}
	release2()
	if len(m.free) != 1 {
		t.Fatalf("free slots = %d, want 1", len(m.free))
	}
}

func TestAcquireCancelledContext(t *testing.T) {
	m := newManager(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := m.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	if len(m.free) != 1 {
		t.Fatal("cancelled Acquire took a slot")
	}
}

func TestAcquireReportsSlotThatCannotBeEmptied(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced for a non-root user")
	}
	m := newManager(t, 1)
	write(t, m.slotDir(0), "x.txt", "x")
	if err := os.Chmod(m.slotDir(0), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(m.slotDir(0), 0o700) })
	if _, _, err := m.Acquire(context.Background()); err == nil || !strings.Contains(err.Error(), "empty checkout slot") {
		t.Fatalf("err = %v, want empty slot error", err)
	}
	if len(m.free) != 1 {
		t.Fatal("slot was not returned after a failed reset")
	}
	if err := os.Chmod(m.slotDir(0), 0o700); err != nil {
		t.Fatal(err)
	}
	acquire(t, m)
}

func TestFetchGitHubAndGitLabRefs(t *testing.T) {
	r := newRemote(t)
	m := newManager(t, 2)
	for _, ref := range []string{"refs/pull/1/head", "refs/merge-requests/2/head"} {
		t.Run(ref, func(t *testing.T) {
			ws, release := acquire(t, m)
			spec := forge.CloneSpec{URL: r.url, HeadRef: ref, BaseRef: "refs/heads/main"}
			if err := ws.Fetch(context.Background(), spec, r.head, r.base); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if got := git(t, ws.Dir(), "rev-parse", "HEAD"); got != r.head {
				t.Fatalf("HEAD = %s, want %s", got, r.head)
			}
			if got := git(t, ws.Dir(), "rev-parse", BaseRef); got != r.base {
				t.Fatalf("%s = %s, want %s", BaseRef, got, r.base)
			}
			if _, err := os.Stat(filepath.Join(ws.Dir(), "b.txt")); err != nil {
				t.Fatalf("head file missing: %v", err)
			}
			release()
			if got := entries(t, ws.Dir()); len(got) != 0 {
				t.Fatalf("slot reuse leaves files: %v", got)
			}
		})
	}
}

func TestFetchWithoutBase(t *testing.T) {
	r := newRemote(t)
	ws, _ := acquire(t, newManager(t, 1))
	spec := forge.CloneSpec{URL: r.url, HeadRef: "refs/pull/1/head"}
	if err := ws.Fetch(context.Background(), spec, r.head, ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resolves(context.Background(), ws.Dir(), BaseRef) {
		t.Fatal("base ref fetched without a BaseRef")
	}
}

func TestFetchHeadMoved(t *testing.T) {
	r := newRemote(t)
	ws, _ := acquire(t, newManager(t, 1))
	spec := forge.CloneSpec{URL: r.url, HeadRef: "refs/pull/1/head", BaseRef: "refs/heads/main"}
	err := ws.Fetch(context.Background(), spec, r.first, r.base)
	if !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("err = %v, want ErrHeadMoved", err)
	}
	if !strings.Contains(err.Error(), "head moved") {
		t.Fatalf("message %q lacks 'head moved'", err)
	}
}

func TestFetchFailures(t *testing.T) {
	r := newRemote(t)
	m := newManager(t, 1)
	missing := "file://" + filepath.ToSlash(filepath.Join(t.TempDir(), "none.git"))
	cases := []struct {
		name string
		spec forge.CloneSpec
		want string
	}{
		{"missing repo", forge.CloneSpec{URL: missing, HeadRef: "refs/pull/1/head"}, "git fetch"},
		{"missing ref", forge.CloneSpec{URL: r.url, HeadRef: "refs/pull/9/head"}, "git fetch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws, release := acquire(t, m)
			defer release()
			err := ws.Fetch(context.Background(), tc.spec, r.head, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFetchInitFailure(t *testing.T) {
	r := newRemote(t)
	ws := &workspace{dir: filepath.Join(t.TempDir(), "absent")}
	err := ws.Fetch(context.Background(), forge.CloneSpec{URL: r.url, HeadRef: "refs/pull/1/head"}, r.head, "")
	if err == nil || !strings.Contains(err.Error(), "git init") {
		t.Fatalf("err = %v, want git init error", err)
	}
}

func TestFetchValidatesInput(t *testing.T) {
	sha := strings.Repeat("a", 40)
	good := forge.CloneSpec{URL: "https://example.test/o/r.git", HeadRef: "refs/pull/1/head", BaseRef: "refs/heads/main"}
	cases := []struct {
		name       string
		spec       forge.CloneSpec
		head, base string
	}{
		{"short head", good, "abc", ""},
		{"upper head", good, strings.Repeat("A", 40), ""},
		{"bad base", good, sha, "--upload-pack=x"},
		{"empty url", forge.CloneSpec{HeadRef: "refs/pull/1/head"}, sha, ""},
		{"option url", forge.CloneSpec{URL: "--upload-pack=x", HeadRef: "refs/pull/1/head"}, sha, ""},
		{"unparsable url", forge.CloneSpec{URL: "https://exa mple.test/%zz", HeadRef: "refs/pull/1/head"}, sha, ""},
		{"ext transport", forge.CloneSpec{URL: "ext::sh -c touch% /tmp/x", HeadRef: "refs/pull/1/head"}, sha, ""},
		{"ssh url", forge.CloneSpec{URL: "ssh://git@example.test/o/r.git", HeadRef: "refs/pull/1/head"}, sha, ""},
		{"auth over http", forge.CloneSpec{URL: "http://example.test/o/r.git", HeadRef: "refs/pull/1/head", AuthHeader: "Authorization: Bearer x"}, sha, ""},
		{"credentials in url", forge.CloneSpec{URL: "https://x:token@example.test/o/r.git", HeadRef: "refs/pull/1/head"}, sha, ""},
		{"no head ref", forge.CloneSpec{URL: good.URL}, sha, ""},
		{"short head ref", forge.CloneSpec{URL: good.URL, HeadRef: "pull/1/head"}, sha, ""},
		{"refspec in ref", forge.CloneSpec{URL: good.URL, HeadRef: "refs/pull/1/head:refs/heads/x"}, sha, ""},
		{"space in base ref", forge.CloneSpec{URL: good.URL, HeadRef: good.HeadRef, BaseRef: "refs/heads/a b"}, sha, ""},
		{"pattern ref", forge.CloneSpec{URL: good.URL, HeadRef: "refs/pull/*"}, sha, ""},
		{"dotdot ref", forge.CloneSpec{URL: good.URL, HeadRef: "refs/../x"}, sha, ""},
		{"reflog ref", forge.CloneSpec{URL: good.URL, HeadRef: "refs/heads/x@{1}"}, sha, ""},
		{"lock ref", forge.CloneSpec{URL: good.URL, HeadRef: "refs/heads/x.lock"}, sha, ""},
		{"trailing slash ref", forge.CloneSpec{URL: good.URL, HeadRef: "refs/heads/"}, sha, ""},
		{"double slash ref", forge.CloneSpec{URL: good.URL, HeadRef: "refs//x"}, sha, ""},
		{"multi-line header", forge.CloneSpec{URL: good.URL, HeadRef: good.HeadRef, AuthHeader: "Authorization: x\r\nX-Other: y"}, sha, ""},
	}
	ws := &workspace{dir: t.TempDir()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ws.Fetch(context.Background(), tc.spec, tc.head, tc.base); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if got := entries(t, ws.dir); len(got) != 0 {
		t.Fatalf("validation ran git: %v", got)
	}
}

func TestIsAncestor(t *testing.T) {
	r := newRemote(t)
	ws, _ := acquire(t, newManager(t, 1))
	spec := forge.CloneSpec{URL: r.url, HeadRef: "refs/pull/1/head", BaseRef: "refs/heads/main"}
	if err := ws.Fetch(context.Background(), spec, r.head, r.base); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := ws.IsAncestor(ctx, r.first, r.head); got != 0 {
		t.Errorf("ancestor = %d, want 0", got)
	}
	if got := ws.IsAncestor(ctx, r.head, r.first); got != 1 {
		t.Errorf("not ancestor = %d, want 1", got)
	}
	if got := ws.IsAncestor(ctx, r.sideTip, r.head); got != 128 {
		t.Errorf("missing object = %d, want 128", got)
	}
	if got := ws.IsAncestor(ctx, "HEAD", r.head); got != -1 {
		t.Errorf("non-sha input = %d, want -1", got)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := ws.IsAncestor(cancelled, r.first, r.head); got != -1 {
		t.Errorf("cancelled = %d, want -1", got)
	}
}

func TestRangeStart(t *testing.T) {
	r := newRemote(t)
	ws, _ := acquire(t, newManager(t, 1))
	spec := forge.CloneSpec{URL: r.url, HeadRef: "refs/pull/1/head", BaseRef: "refs/heads/main"}
	if err := ws.Fetch(context.Background(), spec, r.head, r.base); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := RangeStart(ctx, ws.Dir(), ""); got != "" {
		t.Errorf("empty from = %q", got)
	}
	if got := RangeStart(ctx, ws.Dir(), r.base); got != r.base {
		t.Errorf("present from = %q, want %q", got, r.base)
	}
	if got := RangeStart(ctx, ws.Dir(), r.sideTip); got != BaseRef {
		t.Errorf("absent from = %q, want %q", got, BaseRef)
	}
	if got := RangeStart(ctx, ws.Dir(), "-x"); got != "-x" {
		t.Errorf("option-like from = %q, want it unchanged", got)
	}
	plain := t.TempDir()
	git(t, plain, "init", "-q")
	if got := RangeStart(ctx, plain, r.sideTip); got != r.sideTip {
		t.Errorf("no fetched base = %q, want from unchanged", got)
	}
}

// authServer serves the bare repository over smart HTTP through
// git-http-backend and records the headers of every request.
type authServer struct {
	mu      sync.Mutex
	headers []http.Header
}

func (s *authServer) seen() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

func startAuthServer(t *testing.T, bareDir string) (*authServer, string) {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available: %v", err)
	}
	s := &authServer{}
	handler := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(bareDir),
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
	return s, srv.URL + "/" + filepath.Base(bareDir)
}

func TestFetchSendsAuthHeaderOnlyThroughEnv(t *testing.T) {
	r := newRemote(t)
	srv, url := startAuthServer(t, r.dir)
	// Inherited git config injection must not reach the fetch.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "X-Leak: inherited")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'http.extraheader'='X-Leak: parameters'")
	// So must the global config of the host.
	global := filepath.Join(t.TempDir(), "gitconfig")
	write(t, filepath.Dir(global), "gitconfig", "[http]\n\textraHeader = X-Leak: global\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	ws, _ := acquire(t, newManager(t, 1))
	spec := forge.CloneSpec{
		URL:        url,
		AuthHeader: "Authorization: Bearer secret-token",
		HeadRef:    "refs/pull/1/head",
		BaseRef:    "refs/heads/main",
	}
	if err := ws.Fetch(context.Background(), spec, r.head, r.base); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	headers := srv.seen()
	if len(headers) == 0 {
		t.Fatal("server saw no request")
	}
	for _, h := range headers {
		if h.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("request without the auth header: %v", h)
		}
		if h.Get("X-Leak") != "" {
			t.Errorf("inherited git config reached the fetch: %v", h)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(ws.Dir(), ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"secret-token", "Authorization", "extraHeader", "extraheader"} {
		if strings.Contains(string(cfg), needle) {
			t.Errorf(".git/config contains %q:\n%s", needle, cfg)
		}
	}

	ws2, _ := acquire(t, newManager(t, 1))
	spec.AuthHeader = ""
	if err := ws2.Fetch(context.Background(), spec, r.head, r.base); err == nil {
		t.Fatal("fetch without the auth header succeeded against a protected remote")
	}
}
