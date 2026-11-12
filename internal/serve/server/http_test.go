// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

func post(h http.Handler, path string, body io.Reader, length int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.ContentLength = length
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookStatusCodes(t *testing.T) {
	h := newHarness(t)
	handler := h.srv.Handler()
	push := eventBody(t, forge.Event{Kind: forge.EventPush, Ref: ghRef(1), DeliveryID: "d-1"})

	big := strings.Repeat("x", 2<<10)
	if rec := post(handler, "/github/webhook", strings.NewReader(big), int64(len(big))); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared length: %d", rec.Code)
	}
	if rec := post(handler, "/github/webhook", strings.NewReader(big), -1); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("streamed length: %d", rec.Code)
	}
	if rec := post(handler, "/github/webhook", errReader{}, -1); rec.Code != http.StatusBadRequest {
		t.Fatalf("read error: %d", rec.Code)
	}
	if rec := post(handler, "/github/webhook", strings.NewReader("{"), 1); rec.Code != http.StatusBadRequest {
		t.Fatalf("parse error: %d", rec.Code)
	}

	h.gh.verifyErr = errors.New("bad signature")
	h.gl.verifyErr = errors.New("bad token")
	if rec := post(handler, "/github/webhook", bytes.NewReader(push), int64(len(push))); rec.Code != http.StatusForbidden {
		t.Fatalf("github verify: %d", rec.Code)
	}
	if rec := post(handler, "/gitlab/webhook", bytes.NewReader(push), int64(len(push))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("gitlab verify: %d", rec.Code)
	}
	if rec := post(handler, "/gitlab/webhook", bytes.NewReader(push), int64(len(push))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("gitlab verify: %d", rec.Code)
	}
	if h.gh.calls.Load()+h.gl.calls.Load() != 0 {
		t.Fatal("rejected deliveries must make no forge call")
	}
	if strings.Contains(h.logs.String(), `"x`) {
		t.Fatal("body must not be logged")
	}
}

func TestWebhookDisabledPlatform(t *testing.T) {
	h := newHarness(t)
	srv, err := New(Deps{Config: h.cfg, Forges: map[forge.Platform]forge.Forge{forge.GitHub: h.gh, forge.GitLab: nil},
		Reviewer: h.rev, Workspaces: h.ws, Locale: h.loc, Logger: NewLogger(h.logs)})
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(srv.Handler(), "/gitlab/webhook", strings.NewReader("{}"), 2); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled platform: %d", rec.Code)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestWebhookMakesNoForgeCall blocks every forge API call until the response
// is out, so a handler that called the forge would never answer.
func TestWebhookMakesNoForgeCall(t *testing.T) {
	h := newHarness(t)
	h.gh.gate = make(chan struct{})
	h.gh.setPR(openPR(ghRef(1), sha1))
	handler := h.srv.Handler()
	body := eventBody(t, forge.Event{Kind: forge.EventCommand, Ref: ghRef(1), DeliveryID: "d-2", CommentID: "c1", CommentBody: "/review"})

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- post(handler, "/github/webhook", bytes.NewReader(body), int64(len(body))) }()
	select {
	case rec := <-done:
		if rec.Code != http.StatusAccepted || rec.Header().Get("X-Request-Id") != "d-2" {
			t.Fatalf("status %d id %q", rec.Code, rec.Header().Get("X-Request-Id"))
		}
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["request_id"] != "d-2" {
			t.Fatalf("body %q", rec.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler waited on a forge call")
	}
	close(h.gh.gate)
	h.drain()
	if got := h.gh.reactionList(); len(got) < 1 || got[0] != "c1:eyes" {
		t.Fatalf("eyes reaction %v", got)
	}

	var access map[string]any
	for _, l := range h.logs.lines(t) {
		if l["msg"] == "webhook" {
			access = l
		}
	}
	if access == nil || access["request_id"] != "d-2" || access["platform"] != "github" || access["event"] != "command" ||
		access["status"] != float64(http.StatusAccepted) || access["outcome"] != "queued command review" {
		t.Fatalf("access log %v", access)
	}
	for _, l := range h.logs.lines(t) {
		if l["msg"] == "decided" && l["request_id"] != "d-2" {
			t.Fatalf("runner log lacks the request id: %v", l)
		}
	}
}

func TestWebhookGeneratesRequestID(t *testing.T) {
	h := newHarness(t)
	body := eventBody(t, forge.Event{Kind: forge.EventIgnored, Ref: glRef(1)})
	rec := post(h.srv.Handler(), "/gitlab/webhook", bytes.NewReader(body), int64(len(body)))
	if rec.Code != http.StatusAccepted || len(rec.Header().Get("X-Request-Id")) != 16 {
		t.Fatalf("status %d id %q", rec.Code, rec.Header().Get("X-Request-Id"))
	}
	rec = post(h.srv.Handler(), "/gitlab/webhook", strings.NewReader("{"), 1)
	if len(rec.Header().Get("X-Request-Id")) != 16 {
		t.Fatal("a rejected delivery still gets a request id")
	}
}

func TestHealthAndShutdown(t *testing.T) {
	h := newHarness(t)
	handler := h.srv.Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"test"`) || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz %d %s", rec.Code, rec.Body.String())
	}
	if err := h.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Start(context.Background()); err == nil {
		t.Fatal("second start must fail")
	}
	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Start(context.Background()); err == nil {
		t.Fatal("start after shutdown must fail")
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz during shutdown %d", rec.Code)
	}
	body := eventBody(t, forge.Event{Kind: forge.EventPush, Ref: ghRef(1)})
	if rec := post(handler, "/github/webhook", bytes.NewReader(body), int64(len(body))); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("webhook during shutdown %d", rec.Code)
	}
	if h.srv.intake(func() {}) {
		t.Fatal("intake must refuse after shutdown")
	}
}

func TestShutdownDeadline(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	started := make(chan struct{})
	h.rev.set(func(ctx context.Context, _ review.Request) (review.Result, error) {
		close(started)
		<-ctx.Done()
		return review.Result{}, ctx.Err()
	})
	h.srv.queue.submit(ref, job{trigger: state.TriggerOpened, requestID: "r"})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error %v", err)
	}
	h.drain()
	mustContain(t, h.gh.summary(ref), h.loc.Text("pending.failed"))
}

func TestNewValidation(t *testing.T) {
	h := newHarness(t)
	base := Deps{Config: h.cfg, Forges: map[forge.Platform]forge.Forge{forge.GitHub: h.gh}, Reviewer: h.rev, Workspaces: h.ws, Locale: h.loc}
	cases := map[string]func(d *Deps){
		"no config":    func(d *Deps) { d.Config = nil },
		"no reviewer":  func(d *Deps) { d.Reviewer = nil },
		"no locale":    func(d *Deps) { d.Locale = nil },
		"no forge":     func(d *Deps) { d.Forges = map[forge.Platform]forge.Forge{forge.GitHub: nil} },
		"zero workers": func(d *Deps) { c := *h.cfg; c.Workers = 0; d.Config = &c },
		"bad data dir": func(d *Deps) { c := *h.cfg; c.DataDir = "/dev/null/x"; d.Config = &c },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			d := base
			mut(&d)
			if _, err := New(d); err == nil {
				t.Fatal("want error")
			}
		})
	}
	srv, err := New(base)
	if err != nil || srv.log == nil || srv.now == nil {
		t.Fatalf("defaults: %v", err)
	}
	_ = srv.Shutdown(context.Background())
}

func TestNewResetsCorruptStore(t *testing.T) {
	cfg := testConfig(t)
	if err := writeFile(cfg.DataDir+"/"+waitingFile, "not json"); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	loc, _ := state.LoadLocale("en")
	srv, err := New(Deps{Config: cfg, Forges: map[forge.Platform]forge.Forge{forge.GitLab: newFakeForge(forge.GitLab)},
		Reviewer: &fakeReviewer{}, Workspaces: &fakeWorkspaces{}, Locale: loc, Logger: NewLogger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	if !logs.has("waiting store reset") || len(srv.store.List()) != 0 {
		t.Fatal("corrupt store must start empty and log")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken") }
