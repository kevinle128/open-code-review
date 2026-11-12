// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// appliedThen502 lets the first POST to key take effect and then answers
// 502, as GitHub can when it times out after the write.
func appliedThen502(fa *fakeAPI, key string, h http.HandlerFunc) {
	first := true
	fa.override[key] = func(w http.ResponseWriter, r *http.Request) {
		if first {
			first = false
			fa.installation(h)(httptest.NewRecorder(), r)
			writeRaw(w, http.StatusBadGateway, "{}")
			return
		}
		fa.installation(h)(w, r)
	}
}

func TestPostFindingsNotRepeatedAfterAppliedServerError(t *testing.T) {
	ctx := context.Background()
	t.Run("review", func(t *testing.T) {
		fa := newFakeAPI(t)
		fa.files = standardFiles()
		appliedThen502(fa, "POST /repos/acme/widgets/pulls/7/reviews", fa.handleReview)
		f, _ := fa.forge(t)
		res, err := f.PostFindings(ctx, testPR(), []forge.Finding{finding("a", "a.go", 2, 2), finding("b", "a.go", 12, 12)}, render)
		if err != nil || res.Posted != 2 {
			t.Fatalf("PostFindings = %+v, %v", res, err)
		}
		if n := fa.count("POST /repos/acme/widgets/pulls/7/reviews"); n != 1 || len(fa.reviewComments) != 2 {
			t.Fatalf("review posts %d, comments %d: an applied review was posted again", n, len(fa.reviewComments))
		}
	})
	t.Run("file comment", func(t *testing.T) {
		fa := newFakeAPI(t)
		fa.files = standardFiles()
		appliedThen502(fa, "POST /repos/acme/widgets/pulls/7/comments", fa.handleSingle)
		f, _ := fa.forge(t)
		res, err := f.PostFindings(ctx, testPR(), []forge.Finding{finding("b", "gone.go", 1, 1)}, render)
		if err != nil || res.Posted != 1 {
			t.Fatalf("PostFindings = %+v, %v", res, err)
		}
		if n := fa.count("POST /repos/acme/widgets/pulls/7/comments"); n != 1 || len(fa.reviewComments) != 1 {
			t.Fatalf("comment posts %d, comments %d", n, len(fa.reviewComments))
		}
	})
	t.Run("general comment", func(t *testing.T) {
		fa := newFakeAPI(t)
		appliedThen502(fa, "POST /repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
			payload := decodeBody(t, r)
			writeJSON(w, 201, fa.addComment(&fa.issueComments, payload["body"].(string), testBot, "Bot"))
		})
		f, _ := fa.forge(t)
		res, err := f.PostFindings(ctx, testPR(), []forge.Finding{finding("g", "x.go", 0, 0)}, render)
		if err != nil || res.Posted != 1 {
			t.Fatalf("PostFindings = %+v, %v", res, err)
		}
		if n := fa.count("POST /repos/acme/widgets/issues/7/comments"); n != 1 || len(fa.issueComments) != 1 {
			t.Fatalf("comment posts %d, comments %d", n, len(fa.issueComments))
		}
	})
}

func TestPostFindingsRetriesServerErrorThatAppliedNothing(t *testing.T) {
	fa := newFakeAPI(t)
	fa.files = standardFiles()
	fa.fail["POST /repos/acme/widgets/pulls/7/reviews"] = []failure{{status: 502}}
	f, slept := fa.forge(t)
	res, err := f.PostFindings(context.Background(), testPR(), []forge.Finding{finding("a", "a.go", 2, 2)}, render)
	if err != nil || res.Posted != 1 {
		t.Fatalf("PostFindings = %+v, %v", res, err)
	}
	if n := fa.count("POST /repos/acme/widgets/pulls/7/reviews"); n != 2 || len(fa.reviewComments) != 1 || len(*slept) != 1 {
		t.Fatalf("review posts %d, comments %d, sleeps %v", n, len(fa.reviewComments), *slept)
	}
}

func TestPostFindingsStopsWhenDuplicateCheckFails(t *testing.T) {
	fa := newFakeAPI(t)
	fa.files = standardFiles()
	fa.fail["POST /repos/acme/widgets/pulls/7/reviews"] = []failure{{status: 503}}
	f, _ := fa.forge(t)
	// The first list is the pre-post duplicate check; the second runs after the 503.
	calls := 0
	fa.override["GET /repos/acme/widgets/pulls/7/comments"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			writeRaw(w, http.StatusNotFound, "{}")
			return
		}
		fa.installation(func(w http.ResponseWriter, r *http.Request) { fa.page(w, r, fa.reviewComments) })(w, r)
	}
	_, err := f.PostFindings(context.Background(), testPR(), []forge.Finding{finding("a", "a.go", 2, 2)}, render)
	if err == nil || !strings.Contains(err.Error(), "duplicate check failed") {
		t.Fatalf("err = %v", err)
	}
	if n := fa.count("POST /repos/acme/widgets/pulls/7/reviews"); n != 1 {
		t.Fatalf("review posts %d, want no retry", n)
	}
}

func TestPostNoteServerErrorIsNotRepeated(t *testing.T) {
	fa := newFakeAPI(t)
	fa.fail["POST /repos/acme/widgets/issues/7/comments"] = []failure{{status: 502}}
	f, _ := fa.forge(t)
	if err := f.PostNote(context.Background(), testRef, "hello"); err == nil {
		t.Fatal("a 502 must be returned, not retried")
	}
	if n := fa.count("POST /repos/acme/widgets/issues/7/comments"); n != 1 {
		t.Fatalf("posts %d, want 1", n)
	}

	fa.fail["POST /repos/acme/widgets/issues/7/comments"] = []failure{{status: 429, header: map[string]string{"Retry-After": "3"}}}
	if err := f.PostNote(context.Background(), testRef, "hello"); err != nil {
		t.Fatalf("a rate limit must still be retried: %v", err)
	}
	if n := fa.count("POST /repos/acme/widgets/issues/7/comments"); n != 3 || len(fa.issueComments) != 1 {
		t.Fatalf("posts %d, comments %d", n, len(fa.issueComments))
	}
}

func TestSummaryCreateRetriedWithoutDuplicate(t *testing.T) {
	issueHandler := func(fa *fakeAPI) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			payload := decodeBody(t, r)
			writeJSON(w, 201, fa.addComment(&fa.issueComments, payload["body"].(string), testBot, "Bot"))
		}
	}
	t.Run("applied", func(t *testing.T) {
		fa := newFakeAPI(t)
		appliedThen502(fa, "POST /repos/acme/widgets/issues/7/comments", issueHandler(fa))
		f, _ := fa.forge(t)
		if err := f.UpsertSummary(context.Background(), testRef, forge.SummaryTag+"\nreviewing"); err != nil {
			t.Fatalf("an applied summary must count as written: %v", err)
		}
		if n := fa.count("POST /repos/acme/widgets/issues/7/comments"); n != 1 || len(fa.issueComments) != 1 {
			t.Fatalf("posts %d, comments %d", n, len(fa.issueComments))
		}
	})
	t.Run("not applied", func(t *testing.T) {
		fa := newFakeAPI(t)
		fa.fail["POST /repos/acme/widgets/issues/7/comments"] = []failure{{status: 502}}
		f, _ := fa.forge(t)
		if err := f.UpsertSummary(context.Background(), testRef, forge.SummaryTag+"\nreviewing"); err != nil {
			t.Fatalf("a 502 that wrote nothing must be retried: %v", err)
		}
		if n := fa.count("POST /repos/acme/widgets/issues/7/comments"); n != 2 || len(fa.issueComments) != 1 {
			t.Fatalf("posts %d, comments %d", n, len(fa.issueComments))
		}
	})
}
