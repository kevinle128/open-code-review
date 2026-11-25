// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func TestSetStatus(t *testing.T) {
	f := newFake(t)
	noPipelines(f)
	sha := strings.Repeat("c", 40)
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	fg := newTestForge(t, f, nil)
	pr := forge.PR{Ref: testRef, SourceBranch: "feat/x"}
	if err := fg.SetStatus(context.Background(), pr, sha, forge.StatusFailure, strings.Repeat("x", 300)); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 1 || w[0].Path != "/projects/42/statuses/"+sha {
		t.Fatalf("writes %+v", w)
	}
	var body map[string]string
	if err := json.Unmarshal(w[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["state"] != "failed" || body["name"] != forge.StatusContext || body["ref"] != "feat/x" || len([]rune(body["description"])) != maxStatusDescription {
		t.Fatalf("body %v", body)
	}
}

// TestSetStatusSameState treats GitLab's refusal to set the state a status
// already has as success.
func TestSetStatusSameState(t *testing.T) {
	f := newFake(t)
	noPipelines(f)
	sha := strings.Repeat("c", 40)
	f.json("POST /projects/42/statuses/"+sha, 400, map[string]string{"message": "Cannot transition status via :enqueue from :pending"})
	fg := newTestForge(t, f, nil)
	if err := fg.SetStatus(context.Background(), forge.PR{Ref: testRef}, sha, forge.StatusPending, "x"); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	_ = json.Unmarshal(f.writes()[0].Body, &body)
	if _, ok := body["ref"]; ok {
		t.Fatalf("ref sent without a source branch: %v", body)
	}
}

func TestSetStatusErrors(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	f.json("POST /projects/42/statuses/"+sha, 403, map[string]string{"message": "403 Forbidden"})
	fg := newTestForge(t, f, nil)
	ctx := context.Background()
	if err := fg.SetStatus(ctx, forge.PR{Ref: testRef}, sha, forge.StatusSuccess, "x"); !isStatus(err, 403) {
		t.Fatalf("err %v", err)
	}
	if err := fg.SetStatus(ctx, forge.PR{}, sha, forge.StatusSuccess, "x"); err == nil {
		t.Fatal("empty project accepted")
	}
	if err := fg.SetStatus(ctx, forge.PR{Ref: testRef}, sha, "running", "x"); err == nil {
		t.Fatal("unknown state accepted")
	}
	if err := fg.SetStatus(ctx, forge.PR{Ref: testRef}, "main", forge.StatusSuccess, "x"); err == nil {
		t.Fatal("non-sha accepted")
	}
}

// noPipelines makes the fake answer like a project without pipelines.
func noPipelines(f *fakeGitLab) {
	f.json("GET /projects/42/merge_requests/5/pipelines", 200, []map[string]any{})
	f.json("GET /projects/42/pipelines", 200, []map[string]any{})
}

func mrPipe(id int, sha, source string) map[string]any {
	return map[string]any{"id": id, "sha": sha, "source": source}
}

func statusBody(t *testing.T, r recorded) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// TestSetStatusIntoMRPipeline: the status goes into the merge request
// pipeline of the commit, and a success there starts only the manual jobs
// named in AfterReviewJobs.
func TestSetStatusIntoMRPipeline(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	f.json("GET /projects/42/merge_requests/5/pipelines", 200, []map[string]any{
		mrPipe(72, sha, "external"), mrPipe(71, sha, "push"), mrPipe(70, sha, mrPipelineSource),
	})
	f.handle("GET /projects/42/pipelines/70/jobs", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["scope[]"]; len(got) != 1 || got[0] != "manual" {
			t.Errorf("job scope %v", got)
		}
		writeJSON(w, 200, []map[string]any{{"id": 900, "name": "unit-test"}, {"id": 901, "name": "deploy-review-app"}})
	})
	f.json("POST /projects/42/jobs/900/play", 200, map[string]any{"id": 900})
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	fg := newTestForge(t, f, nil)
	fg.cfg.AfterReviewJobs = []string{"unit-test"}
	pr := forge.PR{Ref: testRef, SourceBranch: "feat/x"}

	if err := fg.SetStatus(context.Background(), pr, sha, forge.StatusPending, "x"); err != nil {
		t.Fatal(err)
	}
	if err := fg.SetStatus(context.Background(), pr, sha, forge.StatusSuccess, "x"); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 3 || w[2].Path != "/projects/42/jobs/900/play" {
		t.Fatalf("writes %+v", w)
	}
	body := statusBody(t, w[0])
	if body["pipeline_id"] != "70" || body["ref"] != "" {
		t.Fatalf("body %v", body)
	}
}

// TestSetStatusPaginatesPipelinesAndJobs: the merge request pipeline and the
// manual jobs are found on a later page of the GitLab lists, behind
// push and external pipelines.
func TestSetStatusPaginatesPipelinesAndJobs(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("d", 40)
	other := strings.Repeat("e", 40)
	f.handle("GET /projects/42/merge_requests/5/pipelines", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "2" {
			w.Header().Set("X-Next-Page", "2")
			writeJSON(w, 200, []map[string]any{mrPipe(82, other, "external"), mrPipe(81, other, "push")})
			return
		}
		writeJSON(w, 200, []map[string]any{mrPipe(80, sha, mrPipelineSource)})
	})
	f.handle("GET /projects/42/pipelines/80/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "2" {
			w.Header().Set("X-Next-Page", "2")
			writeJSON(w, 200, []map[string]any{{"id": 910, "name": "deploy"}})
			return
		}
		writeJSON(w, 200, []map[string]any{{"id": 911, "name": "unit-test"}})
	})
	f.json("POST /projects/42/jobs/911/play", 200, map[string]any{"id": 911})
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	fg := newTestForge(t, f, nil)
	fg.cfg.AfterReviewJobs = []string{"unit-test"}
	pr := forge.PR{Ref: testRef, SourceBranch: "feat/x"}

	if err := fg.SetStatus(context.Background(), pr, sha, forge.StatusSuccess, "x"); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 2 || w[1].Path != "/projects/42/jobs/911/play" {
		t.Fatalf("writes %+v", w)
	}
	if body := statusBody(t, w[0]); body["pipeline_id"] != "80" {
		t.Fatalf("body %v", body)
	}
}

// TestSetStatusOldCommitStartsNothing: a success on a commit that is no
// longer the newest one (the round's commit, after a person pushed) goes
// into its pipeline but does not start the tests there.
func TestSetStatusOldCommitStartsNothing(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	f.json("GET /projects/42/merge_requests/5/pipelines", 200, []map[string]any{
		mrPipe(71, strings.Repeat("d", 40), mrPipelineSource), mrPipe(70, sha, mrPipelineSource),
	})
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	fg := newTestForge(t, f, nil)
	fg.cfg.AfterReviewJobs = []string{"unit-test"}
	if err := fg.SetStatus(context.Background(), forge.PR{Ref: testRef}, sha, forge.StatusSuccess, "x"); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 1 || statusBody(t, w[0])["pipeline_id"] != "70" {
		t.Fatalf("writes %+v", w)
	}
}

// TestSetStatusWaitsForNewPipeline: the pipeline of a fresh push appears a
// moment after the push; the status waits for it instead of going out as a
// separate pipeline that the merge check would ignore.
func TestSetStatusWaitsForNewPipeline(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	calls := 0
	f.handle("GET /projects/42/merge_requests/5/pipelines", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		list := []map[string]any{mrPipe(69, strings.Repeat("d", 40), mrPipelineSource)}
		if calls > 2 {
			list = append([]map[string]any{mrPipe(70, sha, mrPipelineSource)}, list...)
		}
		writeJSON(w, 200, list)
	})
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	sl := &sleeps{}
	fg := newTestForge(t, f, sl)
	if err := fg.SetStatus(context.Background(), forge.PR{Ref: testRef, SourceBranch: "feat/x"}, sha, forge.StatusPending, "x"); err != nil {
		t.Fatal(err)
	}
	if body := statusBody(t, f.writes()[0]); body["pipeline_id"] != "70" || len(sl.all()) != 2 {
		t.Fatalf("body %v sleeps %v", body, sl.all())
	}
}

// TestSetStatusNewMRWaits: a merge request that has no pipeline yet waits
// for its first one when the project runs merge request pipelines, so a
// status set at once (a merge request that is not reviewed) still lands in
// it and starts the tests.
func TestSetStatusNewMRWaits(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	calls := 0
	f.handle("GET /projects/42/merge_requests/5/pipelines", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		list := []map[string]any{}
		if calls > 1 {
			list = append(list, mrPipe(70, sha, mrPipelineSource))
		}
		writeJSON(w, 200, list)
	})
	f.handle("GET /projects/42/pipelines", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("source") != mrPipelineSource {
			t.Errorf("query %v", r.URL.Query())
		}
		writeJSON(w, 200, []map[string]any{mrPipe(12, strings.Repeat("e", 40), mrPipelineSource)})
	})
	f.json("GET /projects/42/pipelines/70/jobs", 200, []map[string]any{{"id": 900, "name": "unit-test"}})
	f.json("POST /projects/42/jobs/900/play", 200, map[string]any{"id": 900})
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	sl := &sleeps{}
	fg := newTestForge(t, f, sl)
	fg.cfg.AfterReviewJobs = []string{"unit-test"}
	if err := fg.SetStatus(context.Background(), forge.PR{Ref: testRef, SourceBranch: "feat/x"}, sha, forge.StatusSuccess, "x"); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 2 || statusBody(t, w[0])["pipeline_id"] != "70" || len(sl.all()) != 1 {
		t.Fatalf("writes %+v sleeps %v", w, sl.all())
	}
}

// TestSetStatusWithoutMRPipelines: a project without merge request
// pipelines keeps the external pipeline on the source branch, without
// waiting, even when the merge request already has external and branch
// pipelines; nothing is started.
func TestSetStatusWithoutMRPipelines(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	f.json("GET /projects/42/merge_requests/5/pipelines", 200, []map[string]any{
		mrPipe(71, strings.Repeat("d", 40), "external"), mrPipe(70, strings.Repeat("d", 40), "push"),
	})
	f.json("GET /projects/42/pipelines", 200, []map[string]any{})
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	sl := &sleeps{}
	fg := newTestForge(t, f, sl)
	fg.cfg.AfterReviewJobs = []string{"unit-test"}
	if err := fg.SetStatus(context.Background(), forge.PR{Ref: testRef, SourceBranch: "feat/x"}, sha, forge.StatusSuccess, "x"); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	body := statusBody(t, w[0])
	if len(w) != 1 || body["ref"] != "feat/x" || body["pipeline_id"] != "" || len(sl.all()) != 0 {
		t.Fatalf("writes %+v body %v sleeps %v", w, body, sl.all())
	}
}

// TestSetStatusLookupError: a failed pipeline lookup still sends the
// status, as an external pipeline, and reports the lookup error.
func TestSetStatusLookupError(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	f.json("GET /projects/42/merge_requests/5/pipelines", 403, map[string]string{"message": "403 Forbidden"})
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	fg := newTestForge(t, f, nil)
	err := fg.SetStatus(context.Background(), forge.PR{Ref: testRef, SourceBranch: "feat/x"}, sha, forge.StatusPending, "x")
	if err == nil || !strings.Contains(err.Error(), "find the merge request pipeline") {
		t.Fatalf("err %v", err)
	}
	if w := f.writes(); len(w) != 1 || statusBody(t, w[0])["ref"] != "feat/x" {
		t.Fatalf("writes %+v", w)
	}
	if err := fg.PostCommitStatus(context.Background(), forge.PR{Ref: testRef, SourceBranch: "feat/x"}, sha, forge.StatusPending, "x"); err != nil {
		t.Fatal(err)
	}
}

func TestSetStatusUsesGivenPipeline(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	other := strings.Repeat("d", 40)
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	f.json("GET /projects/42/pipelines/77/jobs", 200, []map[string]any{{"id": 9, "name": "unit-test"}})
	f.json("POST /projects/42/jobs/9/play", 200, map[string]any{"id": 9})
	fg := newTestForge(t, f, nil)
	fg.cfg.AfterReviewJobs = []string{"unit-test"}
	pr := forge.PR{Ref: testRef, SourceBranch: "feat/x", PipelineID: 77, PipelineSHA: sha}
	if err := fg.SetStatus(context.Background(), pr, sha, forge.StatusSuccess, "x"); err != nil {
		t.Fatal(err)
	}
	for _, g := range f.requests(http.MethodGet) {
		if strings.Contains(g.Path, "/merge_requests/") {
			t.Fatalf("looked up merge request pipelines: %+v", g)
		}
	}
	w := f.writes()
	if len(w) != 2 || statusBody(t, w[0])["pipeline_id"] != "77" || w[1].Path != "/projects/42/jobs/9/play" {
		t.Fatalf("writes %+v", w)
	}

	f.json("GET /projects/42/merge_requests/5/pipelines", 200, []map[string]any{mrPipe(80, other, mrPipelineSource)})
	f.json("POST /projects/42/statuses/"+other, 201, map[string]any{"id": 2})
	if err := fg.SetStatus(context.Background(), pr, other, forge.StatusPending, "x"); err != nil {
		t.Fatal(err)
	}
	writes := f.writes()
	if statusBody(t, writes[len(writes)-1])["pipeline_id"] != "80" {
		t.Fatalf("writes %+v", writes)
	}
}

func TestResolveProjectPath(t *testing.T) {
	f := newFake(t)
	f.json("GET /projects/42", 200, map[string]any{"path_with_namespace": "team/svc"})
	fg := newTestForge(t, f, nil)
	got, err := fg.ResolveProjectPath(context.Background(), 42)
	if err != nil || got != "team/svc" {
		t.Fatalf("path %q err %v", got, err)
	}
	if _, err := fg.ResolveProjectPath(context.Background(), 0); err == nil {
		t.Fatal("project id 0 accepted")
	}
	f.json("GET /projects/7", 404, map[string]string{"message": "404 Project Not Found"})
	if _, err := fg.ResolveProjectPath(context.Background(), 7); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	f.json("GET /projects/8", 200, map[string]any{"path_with_namespace": "team/../svc"})
	if _, err := fg.ResolveProjectPath(context.Background(), 8); err == nil || errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}
