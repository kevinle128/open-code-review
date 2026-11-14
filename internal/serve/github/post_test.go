// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

const lineError = `{"message":"Unprocessable Entity","errors":["Line could not be resolved"]}`

func testPR() forge.PR {
	return forge.PR{Ref: testRef, HeadSHA: headSHA, BaseSHA: baseSHA, TargetBranch: "main", State: forge.StateOpen}
}

func render(f forge.Finding, p forge.Placement) string {
	return fmt.Sprintf("%s kind=%d path=%s lines=%d-%d relocated=%v", forge.FindingTag(f.ID), p.Kind, p.Path, p.StartLine, p.EndLine, p.Relocated)
}

func standardFiles() []map[string]any {
	return []map[string]any{
		{"filename": "a.go", "patch": "@@ -1,3 +1,4 @@\n line1\n+added2\n line3\n line4\n@@ -10,2 +11,3 @@\n x\n+y\n z"},
		{"filename": "gone.go", "patch": "@@ -1,2 +0,0 @@\n-a\n-b"},
		{"filename": "big.bin"},
		{"filename": "clipped.go", "patch": "@@ -1,5 +1,5 @@\n a\n b"},
	}
}

func finding(id, path string, start, end int) forge.Finding {
	return forge.Finding{ID: id, Path: path, StartLine: start, EndLine: end, Category: "bug", Severity: "high", Content: "x"}
}

func TestPostFindingsPlacement(t *testing.T) {
	fa := newFakeAPI(t)
	fa.files = standardFiles()
	f, _ := fa.forge(t)
	fs := []forge.Finding{
		finding("f1", "a.go", 2, 2),
		finding("f2", "a.go", 11, 13),
		finding("f3", "a.go", 7, 7),
		finding("f4", "gone.go", 1, 1),
		finding("f5", "big.bin", 3, 3),
		finding("f6", "clipped.go", 1, 1),
		finding("f7", "other.go", 4, 5),
		finding("f8", "", 0, 0),
	}
	res, err := f.PostFindings(context.Background(), testPR(), fs, render)
	if err != nil {
		t.Fatal(err)
	}
	want := forge.PostResult{Posted: 8, Relocated: 3, FileLevel: 5}
	if res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	if len(fa.reviews) != 1 {
		t.Fatalf("reviews = %d, want 1", len(fa.reviews))
	}
	review := fa.reviews[0]
	if review["commit_id"] != headSHA || review["event"] != "COMMENT" {
		t.Fatalf("review = %v", review)
	}
	if _, ok := review["body"]; ok {
		t.Fatal("review carries a body; findings must be the only content")
	}
	comments := review["comments"].([]any)
	got := make([]string, 0, len(comments))
	for _, c := range comments {
		m := c.(map[string]any)
		got = append(got, fmt.Sprintf("%s:%v-%v:%v:%v", m["path"], m["start_line"], m["line"], m["side"], m["start_side"]))
	}
	wantLines := []string{
		"a.go:<nil>-2:RIGHT:<nil>",
		"a.go:11-13:RIGHT:RIGHT",
		"a.go:<nil>-4:RIGHT:<nil>",
	}
	if strings.Join(got, " ") != strings.Join(wantLines, " ") {
		t.Fatalf("line comments = %v, want %v", got, wantLines)
	}
	if !strings.Contains(comments[2].(map[string]any)["body"].(string), "relocated=true") {
		t.Fatal("nearest-line finding not marked relocated")
	}

	var files []string
	for _, p := range fa.singlePosts {
		if p["subject_type"] != "file" || p["commit_id"] != headSHA {
			t.Fatalf("file comment = %v", p)
		}
		if _, ok := p["line"]; ok {
			t.Fatalf("file comment carries a line: %v", p)
		}
		files = append(files, p["path"].(string))
	}
	// A PR-level finding has no path; GitHub resolves only review threads,
	// which need a file, so it goes file-level on the first changed file.
	if strings.Join(files, ",") != "gone.go,big.bin,clipped.go,a.go,a.go" {
		t.Fatalf("file-level paths = %v", files)
	}
	for _, i := range []int{3, 4} {
		if !strings.Contains(fa.singlePosts[i]["body"].(string), "relocated=true") {
			t.Fatalf("file comment %d not marked relocated", i)
		}
	}
}

func TestPostFindingsBatches(t *testing.T) {
	fa := newFakeAPI(t)
	var patch strings.Builder
	patch.WriteString("@@ -0,0 +1,120 @@\n")
	for i := 0; i < 120; i++ {
		patch.WriteString("+x\n")
	}
	fa.files = []map[string]any{{"filename": "a.go", "patch": patch.String()}}
	f, _ := fa.forge(t)
	var fs []forge.Finding
	for i := 1; i <= 120; i++ {
		fs = append(fs, finding(fmt.Sprintf("id-%d", i), "a.go", i, i))
	}
	res, err := f.PostFindings(context.Background(), testPR(), fs, render)
	if err != nil {
		t.Fatal(err)
	}
	if res.Posted != 120 || len(fa.reviews) != 3 {
		t.Fatalf("posted=%d reviews=%d", res.Posted, len(fa.reviews))
	}
	for i, want := range []int{50, 50, 20} {
		if n := len(fa.reviews[i]["comments"].([]any)); n != want {
			t.Errorf("review %d has %d comments, want %d", i, n, want)
		}
	}
}

func TestPostFindingsIdempotent(t *testing.T) {
	fa := newFakeAPI(t)
	fa.files = standardFiles()
	// A human quoting a finding tag must not suppress the real finding.
	fa.addComment(&fa.reviewComments, forge.FindingTag("f1"), "mallory", "User")
	f, _ := fa.forge(t)
	fs := []forge.Finding{finding("f1", "a.go", 2, 2), finding("f2", "gone.go", 1, 1), finding("f2", "a.go", 3, 3)}
	ctx := context.Background()

	res, err := f.PostFindings(ctx, testPR(), fs, render)
	if err != nil {
		t.Fatal(err)
	}
	if res.Posted != 2 || res.Skipped != 1 {
		t.Fatalf("first post = %+v", res)
	}
	reviews, singles := len(fa.reviews), len(fa.singlePosts)

	res, err = f.PostFindings(ctx, testPR(), fs, render)
	if err != nil {
		t.Fatal(err)
	}
	if res.Posted != 0 || res.Skipped != 3 {
		t.Fatalf("second post = %+v", res)
	}
	if len(fa.reviews) != reviews || len(fa.singlePosts) != singles {
		t.Fatal("second post sent requests")
	}
}

func TestPostFindingsLineFallback(t *testing.T) {
	fa := newFakeAPI(t)
	fa.files = standardFiles()
	bad := func(c map[string]any) bool { return c["line"] == float64(3) }
	fa.reviewReject = func(comments []any) (int, string) {
		for _, c := range comments {
			if bad(c.(map[string]any)) {
				return 422, lineError
			}
		}
		return 0, ""
	}
	fa.singleReject = func(p map[string]any) (int, string) {
		if p["subject_type"] == nil && bad(p) {
			return 422, lineError
		}
		return 0, ""
	}
	f, _ := fa.forge(t)
	fs := []forge.Finding{finding("ok1", "a.go", 1, 2), finding("bad", "a.go", 3, 3), finding("ok2", "a.go", 12, 12)}
	res, err := f.PostFindings(context.Background(), testPR(), fs, render)
	if err != nil {
		t.Fatal(err)
	}
	if want := (forge.PostResult{Posted: 3, FileLevel: 1}); res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	var got []string
	for _, p := range fa.singlePosts {
		got = append(got, fmt.Sprintf("%v:%v:%v:%v", p["path"], p["start_line"], p["line"], p["subject_type"]))
	}
	want := "a.go:1:2:<nil> a.go:<nil>:3:<nil> a.go:<nil>:<nil>:file a.go:<nil>:12:<nil>"
	if strings.Join(got, " ") != want {
		t.Fatalf("single posts = %v\nwant %s", got, want)
	}
	if body := fa.singlePosts[2]["body"].(string); !strings.Contains(body, "kind=2 path=a.go") {
		t.Fatalf("demoted body not re-rendered at file level: %s", body)
	}
}

func TestPostFindingsUnknownFileFallsBack(t *testing.T) {
	t.Run("first changed file", func(t *testing.T) {
		fa := newFakeAPI(t)
		fa.pageSize = 1
		fa.files = standardFiles()
		f, _ := fa.forge(t)
		fa.reviewReject = func([]any) (int, string) { return 422, lineError }
		fa.singleReject = func(p map[string]any) (int, string) {
			if p["subject_type"] == nil {
				return 422, `{"message":"Validation Failed","errors":[{"field":"line","code":"invalid"}]}`
			}
			return 0, ""
		}
		p := &poster{f: f, pr: testPR(), pullPath: "/repos/acme/widgets/pulls/7", render: render}
		if err := p.loadInventory(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !p.inv.Complete || p.first != "a.go" {
			t.Fatalf("paged inventory: complete=%v first=%q", p.inv.Complete, p.first)
		}
		// An incomplete inventory cannot prove the path is outside the diff,
		// so the finding is tried on its own line first.
		p.inv.Complete = false
		it := placed{finding("u", "unknown.go", 5, 5), forge.Place(finding("u", "unknown.go", 5, 5), p.inv, p.first)}
		if it.placement.Kind != forge.Line {
			t.Fatalf("placement = %+v", it.placement)
		}
		if err := p.postBatch(context.Background(), []placed{it}); err != nil {
			t.Fatal(err)
		}
		last := fa.singlePosts[len(fa.singlePosts)-1]
		if last["path"] != "a.go" || last["subject_type"] != "file" || p.result.Relocated != 1 || p.result.FileLevel != 1 {
			t.Fatalf("fallback = %v result = %+v", last, p.result)
		}
	})
	t.Run("no files at all", func(t *testing.T) {
		fa := newFakeAPI(t)
		fa.reviewReject = func([]any) (int, string) { return 422, lineError }
		fa.singleReject = func(map[string]any) (int, string) { return 422, lineError }
		f, _ := fa.forge(t)
		fs := []forge.Finding{finding("g1", "x.go", 4, 4), finding("g2", "y.go", 0, 0)}
		res, err := f.PostFindings(context.Background(), testPR(), fs, render)
		if err != nil {
			t.Fatal(err)
		}
		if want := (forge.PostResult{Posted: 2, Relocated: 2, General: 2}); res != want {
			t.Fatalf("result = %+v, want %+v", res, want)
		}
		if len(fa.issueComments) != 2 {
			t.Fatalf("general comments = %d", len(fa.issueComments))
		}
		res, err = f.PostFindings(context.Background(), testPR(), fs, render)
		if err != nil {
			t.Fatal(err)
		}
		if want := (forge.PostResult{Skipped: 2}); res != want || len(fa.issueComments) != 2 {
			t.Fatalf("second post = %+v, comments = %d", res, len(fa.issueComments))
		}
	})
}

func TestPostFindingsFailures(t *testing.T) {
	ctx := context.Background()
	fs := []forge.Finding{finding("a", "a.go", 2, 2), finding("b", "gone.go", 1, 1)}
	cases := []struct {
		name  string
		setup func(fa *fakeAPI)
		want  string
	}{
		{"review rejected for another reason", func(fa *fakeAPI) {
			fa.reviewReject = func([]any) (int, string) { return 422, `{"message":"was submitted too quickly"}` }
		}, "create review"},
		{"single comment rejected for another reason", func(fa *fakeAPI) {
			fa.reviewReject = func([]any) (int, string) { return 422, lineError }
			fa.singleReject = func(map[string]any) (int, string) { return 403, `{"message":"forbidden"}` }
		}, "create review comment"},
		{"file comment rejected", func(fa *fakeAPI) {
			fa.singleReject = func(map[string]any) (int, string) { return 422, `{"message":"bad"}` }
		}, "create file comment"},
		{"list review comments", func(fa *fakeAPI) {
			fa.fail["GET /repos/acme/widgets/pulls/7/comments"] = []failure{{status: 404, body: "{}"}}
		}, "list review comments"},
		{"decode review comment", func(fa *fakeAPI) {
			fa.reviewComments = []map[string]any{{"id": "x"}}
		}, "decode comment"},
		{"list files", func(fa *fakeAPI) {
			fa.fail["GET /repos/acme/widgets/pulls/7/files"] = []failure{{status: 404, body: "{}"}}
		}, "list pull request files"},
		{"decode file", func(fa *fakeAPI) {
			fa.files = []map[string]any{{"filename": 3}}
		}, "decode pull request file"},
		{"bot login", func(fa *fakeAPI) {
			fa.fail["GET /app"] = []failure{{status: 404, body: "{}"}}
		}, "get app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAPI(t)
			fa.files = append(standardFiles(), map[string]any{"filename": ""})
			tc.setup(fa)
			f, _ := fa.forge(t)
			if _, err := f.PostFindings(ctx, testPR(), fs, render); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("issue comments unreadable", func(t *testing.T) {
		fa := newFakeAPI(t)
		fa.fail["GET /repos/acme/widgets/issues/7/comments"] = []failure{{status: 404, body: "{}"}}
		f, _ := fa.forge(t)
		if _, err := f.PostFindings(ctx, testPR(), []forge.Finding{finding("g", "x.go", 0, 0)}, render); err == nil ||
			!strings.Contains(err.Error(), "list issue comments") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("general comment rejected", func(t *testing.T) {
		fa := newFakeAPI(t)
		fa.fail["POST /repos/acme/widgets/issues/7/comments"] = []failure{{status: 403, body: "{}"}}
		f, _ := fa.forge(t)
		if _, err := f.PostFindings(ctx, testPR(), []forge.Finding{finding("g", "x.go", 0, 0)}, render); err == nil {
			t.Fatal("general comment failure ignored")
		}
	})
}

func TestPostFindingsInputs(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	fs := []forge.Finding{finding("a", "a.go", 1, 1)}
	if _, err := f.PostFindings(ctx, forge.PR{Ref: forge.PRRef{Repo: "bad"}, HeadSHA: headSHA}, fs, render); err == nil {
		t.Error("bad ref accepted")
	}
	if _, err := f.PostFindings(ctx, testPR(), fs, nil); err == nil {
		t.Error("nil render accepted")
	}
	pr := testPR()
	pr.HeadSHA = ""
	if _, err := f.PostFindings(ctx, pr, fs, render); err == nil {
		t.Error("empty head sha accepted")
	}
	res, err := f.PostFindings(ctx, testPR(), nil, render)
	if err != nil || res != (forge.PostResult{}) || len(fa.calls) != 0 {
		t.Fatalf("empty findings: %+v %v calls=%v", res, err, fa.calls)
	}
}

func TestPostFindingsTruncatedCommentList(t *testing.T) {
	fa := newFakeAPI(t)
	fa.files = standardFiles()
	path := "/repos/acme/widgets/pulls/7/comments"
	fa.override["GET "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=2>; rel="next"`, fa.srv.URL, path))
		writeJSON(w, 200, []any{})
	}
	f, _ := fa.forge(t)
	var logs bytes.Buffer
	f.log = slog.New(slog.NewJSONHandler(&logs, nil))
	res, err := f.PostFindings(context.Background(), testPR(), []forge.Finding{finding("a", "a.go", 2, 2)}, render)
	if err != nil || res.Posted != 1 {
		t.Fatalf("PostFindings = %+v, %v", res, err)
	}
	if !strings.Contains(logs.String(), "duplicate check is partial") {
		t.Fatalf("no truncation warning: %s", logs.String())
	}
}
