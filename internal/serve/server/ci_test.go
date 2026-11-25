// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

const ciToken = "ci-secret"

func (h *harness) enableCI(secret, token string) {
	h.cfg.GitLab = &serveconfig.GitLabConfig{Token: "glpat", WebhookSecret: secret, CIToken: token}
}

func ciJSON(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ciFields(sha string) map[string]any {
	return map[string]any{
		"project_id": int64(9), "mr_iid": 7, "sha": sha, "pipeline_id": int64(42),
		"project_path": "group/app",
	}
}

func postCI(h http.Handler, body []byte, header, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/gitlab/ci-trigger", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if header != "" {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeCI(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestCITriggerDisabled(t *testing.T) {
	h := newHarness(t)
	body := ciJSON(t, ciFields(sha1))
	rec := postCI(h.srv.Handler(), body, "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusNotFound || decodeCI(t, rec)["outcome"] != "gitlab ci trigger disabled" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	h.enableCI("hook", "")
	rec = postCI(h.srv.Handler(), body, "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusNotFound || decodeCI(t, rec)["outcome"] != "gitlab ci trigger disabled" {
		t.Fatalf("empty token %d %s", rec.Code, rec.Body.String())
	}
	ev := eventBody(t, forge.Event{Kind: forge.EventIgnored, Ref: glRef(7)})
	if rec := post(h.srv.Handler(), "/gitlab/webhook", bytes.NewReader(ev), int64(len(ev))); rec.Code != http.StatusAccepted {
		t.Fatalf("webhook with a secret: %d", rec.Code)
	}
}

func TestGitLabWebhookDisabledWithoutSecret(t *testing.T) {
	h := newHarness(t)
	h.enableCI("", ciToken)
	ev := eventBody(t, forge.Event{Kind: forge.EventIgnored, Ref: glRef(7)})
	rec := post(h.srv.Handler(), "/gitlab/webhook", bytes.NewReader(ev), int64(len(ev)))
	if rec.Code != http.StatusNotFound || !strings.Contains(h.logs.String(), "gitlab webhook disabled") {
		t.Fatalf("webhook %d logs %s", rec.Code, h.logs.String())
	}
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	rec = postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ci trigger %d %s", rec.Code, rec.Body.String())
	}
	h.drain()
}

func TestCITriggerAuthAndValidation(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	handler := h.srv.Handler()
	body := ciJSON(t, ciFields(sha1))

	if rec := postCI(handler, body, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token %d", rec.Code)
	}
	if rec := postCI(handler, body, "Authorization", "Bearer wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer %d", rec.Code)
	}
	if rec := postCI(handler, body, "Authorization", "Bearer no"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("short token %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/gitlab/ci-trigger", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("X-OCR-Token", ciToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bearer wins over header %d", rec.Code)
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{"broken", `{`, "invalid json"},
		{"trailing", string(body) + `{"x":1}`, "trailing json"},
		{"project", `{"project_id":0,"mr_iid":7,"sha":"` + sha1 + `","pipeline_id":1}`, "invalid project_id"},
		{"mr", `{"project_id":9,"mr_iid":0,"sha":"` + sha1 + `","pipeline_id":1}`, "invalid mr_iid"},
		{"sha", `{"project_id":9,"mr_iid":7,"sha":"abc","pipeline_id":1}`, "invalid sha"},
		{"sha case", `{"project_id":9,"mr_iid":7,"sha":"` + strings.Repeat("A", 40) + `","pipeline_id":1}`, "invalid sha"},
		{"pipeline", `{"project_id":9,"mr_iid":7,"sha":"` + sha1 + `","pipeline_id":-1}`, "invalid pipeline_id"},
		{"path", `{"project_id":9,"mr_iid":7,"sha":"` + sha1 + `","pipeline_id":1,"project_path":"group/../app"}`, "invalid project_path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postCI(handler, []byte(tc.body), "Authorization", "Bearer "+ciToken)
			if rec.Code != http.StatusBadRequest || decodeCI(t, rec)["outcome"] != tc.want {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
	if len(h.gl.statusList()) != 0 {
		t.Fatal("rejected requests must not set a status")
	}
}

func TestCITriggerBodyLimitAndShutdown(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	big := strings.Repeat("x", 2<<10)
	if rec := postCI(h.srv.Handler(), []byte(big), "Authorization", "Bearer "+ciToken); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body %d", rec.Code)
	}
	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown %d %s", rec.Code, rec.Body.String())
	}
}

func TestCITriggerQueuesAndSetsPending(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	rec := postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	out := decodeCI(t, rec)
	if rec.Code != http.StatusAccepted || out["outcome"] != "queued ci trigger" || out["status"] != "Accepted" ||
		out["request_id"] == "" || rec.Header().Get("X-Request-Id") != out["request_id"] {
		t.Fatalf("%d %s id %q", rec.Code, rec.Body.String(), rec.Header().Get("X-Request-Id"))
	}
	h.gl.mu.Lock()
	first := ""
	if len(h.gl.texts) > 0 {
		first = h.gl.texts[0]
	}
	h.gl.mu.Unlock()
	if first != h.loc.Text("status.waiting") || !strings.Contains(strings.Join(h.gl.statusList(), ","), "1:pending#42") {
		t.Fatalf("status %q list %v", first, h.gl.statusList())
	}
	h.drain()
	if n := len(h.rev.requests()); n != 1 || h.rev.requests()[0].To != sha1 {
		t.Fatalf("reviews %+v", h.rev.requests())
	}
	if h.srv.busy(ref) {
		t.Fatal("chain still busy")
	}
}

func TestCITriggerResolvesProjectPath(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	h.gl.paths = map[int64]string{9: "group/app"}
	fields := ciFields(sha1)
	delete(fields, "project_path")
	rec := postCI(h.srv.Handler(), ciJSON(t, fields), "X-OCR-Token", ciToken)
	if rec.Code != http.StatusAccepted || decodeCI(t, rec)["outcome"] != "queued ci trigger" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	h.drain()

	h.gl.paths = nil
	rec = postCI(h.srv.Handler(), ciJSON(t, fields), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusNotFound || decodeCI(t, rec)["outcome"] != "project not found" {
		t.Fatalf("missing %d %s", rec.Code, rec.Body.String())
	}
	h.gl.pathErr = errors.New("gitlab down")
	rec = postCI(h.srv.Handler(), ciJSON(t, fields), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusBadGateway || !strings.Contains(decodeCI(t, rec)["outcome"], "resolve project") {
		t.Fatalf("error %d %s", rec.Code, rec.Body.String())
	}
}

func TestCITriggerIgnoreSetsNoStatus(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	h.cfg.Ignore.Repos = []*regexp.Regexp{regexp.MustCompile(`^group/app$`)}
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	rec := postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusAccepted || !strings.HasPrefix(decodeCI(t, rec)["outcome"], "ignore rule ") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	h.drain()
	if len(h.gl.statusList()) != 0 || len(h.rev.requests()) != 0 || h.srv.busy(ref) {
		t.Fatalf("status %v reviews %d busy %v", h.gl.statusList(), len(h.rev.requests()), h.srv.busy(ref))
	}
}

func TestCITriggerStatusFailureReleasesChain(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	h.gl.setErr("SetStatus", errors.New("boom"))
	rec := postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusBadGateway || h.srv.busy(ref) {
		t.Fatalf("%d busy %v %s", rec.Code, h.srv.busy(ref), rec.Body.String())
	}
	h.gl.setErr("SetStatus", nil)
	rec = postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusAccepted || decodeCI(t, rec)["outcome"] != "queued ci trigger" {
		t.Fatalf("retry %d %s", rec.Code, rec.Body.String())
	}
	h.drain()
	if len(h.rev.requests()) != 1 || h.srv.busy(ref) {
		t.Fatalf("reviews %d busy %v", len(h.rev.requests()), h.srv.busy(ref))
	}
}

func TestCITriggerChainRunningStillSetsPending(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	release := make(chan struct{})
	var once sync.Once
	entered := make(chan struct{})
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		once.Do(func() { close(entered) })
		<-release
		return review.Result{Terminal: review.TerminalComplete}, nil
	})
	rec := postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusAccepted || decodeCI(t, rec)["outcome"] != "queued ci trigger" {
		t.Fatalf("first %d %s", rec.Code, rec.Body.String())
	}
	<-entered
	fields := ciFields(sha2)
	fields["pipeline_id"] = int64(99)
	rec = postCI(h.srv.Handler(), ciJSON(t, fields), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusAccepted || decodeCI(t, rec)["outcome"] != "chain running" {
		t.Fatalf("second %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.Join(h.gl.statusList(), ","), "2:pending#99") {
		t.Fatalf("status %v", h.gl.statusList())
	}
	close(release)
	h.drain()
	if len(h.rev.requests()) != 1 || h.srv.busy(ref) {
		t.Fatalf("reviews %d busy %v", len(h.rev.requests()), h.srv.busy(ref))
	}
}

func TestCITriggerSkipsReviewedCommit(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	h.gl.summaries[ref.Key()] = state.Encode(state.Marker{
		Status: state.ChainSuccess, Max: 3,
		Rounds: []state.Round{{N: 2, Scope: state.ScopeFull, To: sha1}},
	})
	rec := postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	h.drain()
	if len(h.rev.requests()) != 0 || h.srv.busy(ref) {
		t.Fatalf("reviews %d busy %v", len(h.rev.requests()), h.srv.busy(ref))
	}
	if !strings.Contains(strings.Join(h.gl.statusList(), ","), "1:success#42") || h.gl.lastStatusText() != h.loc.Text("status.success") {
		t.Fatalf("status %v text %q", h.gl.statusList(), h.gl.lastStatusText())
	}

	fields := ciFields(sha1)
	fields["force"] = true
	rec = postCI(h.srv.Handler(), ciJSON(t, fields), "Authorization", "Bearer "+ciToken)
	if decodeCI(t, rec)["outcome"] != "queued ci trigger" {
		t.Fatalf("force %d %s", rec.Code, rec.Body.String())
	}
	h.drain()
	if len(h.rev.requests()) != 1 {
		t.Fatalf("forced reviews %d", len(h.rev.requests()))
	}
}

func TestCITriggerDisabledRepository(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	h.gl.files[shaBase+":"+serveconfig.RepoConfigPath] = []byte(`{"enabled":false}`)
	rec := postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	h.drain()
	if len(h.rev.requests()) != 0 || h.srv.busy(ref) {
		t.Fatalf("reviews %d busy %v", len(h.rev.requests()), h.srv.busy(ref))
	}
	if !strings.Contains(h.gl.lastStatusText(), "not required") || !strings.Contains(strings.Join(h.gl.statusList(), ","), "1:success#42") {
		t.Fatalf("status %v text %q", h.gl.statusList(), h.gl.lastStatusText())
	}
}

func TestCITriggerClosedMergeRequest(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	ref := glRef(7)
	pr := openPR(ref, sha1)
	pr.State = forge.StateClosed
	h.gl.setPR(pr)
	rec := postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusAccepted || decodeCI(t, rec)["outcome"] != "queued ci trigger" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	h.drain()
	if len(h.rev.requests()) != 0 || h.srv.busy(ref) {
		t.Fatalf("reviews %d busy %v", len(h.rev.requests()), h.srv.busy(ref))
	}
}

func TestCITriggerFailedStatusKeepsPipeline(t *testing.T) {
	h := newHarness(t)
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	h.gl.failOnce("GetPR", errTest)
	gen := h.srv.startChain(ref, 1, state.ScopeFull)
	j := job{ci: &ciJob{gen: gen, sha: sha1, pipelineID: 42}, requestID: "r"}
	err := h.srv.round(h.gl, ref, roundJob{n: 1, scope: state.ScopeFull, gen: gen}, j, h.srv.log)
	if err == nil {
		t.Fatal("expected the read to fail once")
	}
	if !strings.Contains(strings.Join(h.gl.statusList(), ","), "1:failure#42") {
		t.Fatalf("status %v", h.gl.statusList())
	}

	h2 := newHarness(t)
	h2.cfg.MaxShutdownCuts = 1
	h2.gl.setPR(openPR(ref, sha1))
	// record() keeps the cut count already stored for this round, so the
	// next run takes the path that reads the PR only to publish a failure.
	h2.srv.journal.entries[ref.Key()] = journalEntry{
		Platform: ref.Platform, Repo: ref.Repo, ProjectID: ref.ProjectID, Number: ref.Number,
		Round: 1, Scope: state.ScopeFull, Cuts: 2, Proc: h2.srv.journal.proc,
	}
	gen = h2.srv.startChain(ref, 1, state.ScopeFull)
	j2 := job{ci: &ciJob{gen: gen, sha: sha1, pipelineID: 42}, requestID: "r"}
	err = h2.srv.round(h2.gl, ref, roundJob{n: 1, scope: state.ScopeFull, gen: gen}, j2, h2.srv.log)
	if !errors.Is(err, errTooManyCuts) {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(strings.Join(h2.gl.statusList(), ","), "1:failure#42") {
		t.Fatalf("status after the cut limit %v", h2.gl.statusList())
	}
}

func TestCITriggerStatusFailureDoesNotBlockPeer(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	h.gl.failOnce("SetStatus", errors.New("boom"))
	body := ciJSON(t, ciFields(sha1))
	type result struct {
		code    int
		outcome string
	}
	out := make(chan result, 2)
	for range 2 {
		go func() {
			rec := postCI(h.srv.Handler(), body, "Authorization", "Bearer "+ciToken)
			out <- result{rec.Code, decodeCI(t, rec)["outcome"]}
		}()
	}
	var got []result
	for range 2 {
		got = append(got, <-out)
	}
	h.drain()
	codes := map[int]int{}
	for _, r := range got {
		codes[r.code]++
		if r.outcome == "chain running" {
			t.Fatalf("a failed status must not look like a running chain: %+v", got)
		}
	}
	if codes[http.StatusBadGateway] != 1 || codes[http.StatusAccepted] != 1 {
		t.Fatalf("results %+v", got)
	}
	if len(h.rev.requests()) != 1 || h.srv.busy(ref) {
		t.Fatalf("reviews %d busy %v", len(h.rev.requests()), h.srv.busy(ref))
	}
}

func TestCITriggerRerunsWhenHeadMoves(t *testing.T) {
	h := newHarness(t)
	h.enableCI("hook", ciToken)
	ref := glRef(7)
	h.gl.setPR(openPR(ref, sha1))
	h.ws.set(func(w *fakeWorkspaces) { w.branchErrs = []error{errTest} })
	// The trigger reads the repository config before the round reads the PR.
	// Move HEAD there, so the round's fetch sees the new commit and the
	// queue runs the job again.
	h.gl.beforeRead = func() {
		h.gl.mu.Lock()
		h.gl.headAfter[ref.Key()] = sha2
		h.gl.mu.Unlock()
	}
	h.rev.set(func(_ context.Context, req review.Request) (review.Result, error) {
		if req.To != sha2 {
			t.Errorf("reviewed %s", req.To)
		}
		return review.Result{Terminal: review.TerminalComplete}, nil
	})
	rec := postCI(h.srv.Handler(), ciJSON(t, ciFields(sha1)), "Authorization", "Bearer "+ciToken)
	if rec.Code != http.StatusAccepted || decodeCI(t, rec)["outcome"] != "queued ci trigger" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	h.drain()
	if n := len(h.rev.requests()); n != 1 || h.rev.requests()[0].To != sha2 || h.srv.busy(ref) {
		t.Fatalf("reviews %+v busy %v\n%s", h.rev.requests(), h.srv.busy(ref), h.logs.String())
	}
	if !h.logs.has("head moved, running again") {
		t.Fatal("queue did not rerun the job")
	}
}
