// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func commentNode(login, typename, body string) map[string]any {
	c := map[string]any{"body": body}
	if login != "" {
		c["author"] = map[string]any{"login": login, "__typename": typename}
	}
	return c
}

func thread(id string, resolved bool, login, typename, body string, replies ...map[string]any) map[string]any {
	nodes := append([]any{commentNode(login, typename, body)}, toAny(replies)...)
	return map[string]any{"id": id, "isResolved": resolved, "comments": map[string]any{"nodes": nodes}}
}

func toAny(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func TestListOCRThreads(t *testing.T) {
	fa := newFakeAPI(t)
	fa.threadPage = 2
	tag := forge.FindingTag
	fa.threads = []map[string]any{
		// GraphQL reports the App by its bare slug.
		thread("T1", false, testSlug, "Bot", tag("r1-aaaa-01")+"\nbody"),
		thread("T2", true, testSlug, "Bot", tag("r1-aaaa-02")),
		thread("T3", false, "mallory", "User", tag("r1-aaaa-03")),
		thread("T4", false, "other-app", "Bot", tag("r1-aaaa-04")),
		thread("T5", false, testSlug, "Bot", "a bot thread with no finding tag"),
		thread("T6", false, "", "", tag("r1-aaaa-06")),
		{"id": "T7", "isResolved": false, "comments": map[string]any{"nodes": []any{}}},
		thread("T8", false, testBot, "Bot", tag("m-bbbb-08")),
	}
	f, _ := fa.forge(t)
	got, err := f.ListOCRThreads(context.Background(), testRef)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.Thread{
		{ID: "T1", FindingID: "r1-aaaa-01", Body: tag("r1-aaaa-01") + "\nbody"},
		{ID: "T2", FindingID: "r1-aaaa-02", Resolved: true, Body: tag("r1-aaaa-02")},
		{ID: "T8", FindingID: "m-bbbb-08", Body: tag("m-bbbb-08")},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("threads = %+v, want %+v", got, want)
	}
	if len(fa.graphqlVars) != 4 {
		t.Fatalf("pages = %d, want 4", len(fa.graphqlVars))
	}
	v := fa.graphqlVars[0]
	if v["owner"] != "acme" || v["repo"] != "widgets" || v["number"] != float64(7) || v["cursor"] != nil {
		t.Fatalf("first page vars = %v", v)
	}
	if fa.graphqlVars[1]["cursor"] != "2" {
		t.Fatalf("second page cursor = %v", fa.graphqlVars[1]["cursor"])
	}
}

// TestListOCRThreadsPlaceAndReplies reads where a thread sits, falls back to
// the original line of an outdated thread, and keeps only replies that the
// App did not write.
func TestListOCRThreadsPlaceAndReplies(t *testing.T) {
	fa := newFakeAPI(t)
	tag := forge.FindingTag
	live := thread("T1", true, testSlug, "Bot", tag("r1-aaaa-01"),
		commentNode("alice", "User", "This is intended."),
		commentNode(testSlug, "Bot", "Skipped by the fix run."),
		commentNode("", "", "ghost reply"))
	live["path"], live["line"], live["originalLine"] = "a.go", 12, 10
	outdated := thread("T2", false, testSlug, "Bot", tag("r1-aaaa-02"))
	outdated["path"], outdated["line"], outdated["originalLine"] = "b.go", nil, 7
	fa.threads = []map[string]any{live, outdated}
	f, _ := fa.forge(t)
	got, err := f.ListOCRThreads(context.Background(), testRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != "a.go" || got[0].Line != 12 || got[1].Path != "b.go" || got[1].Line != 7 {
		t.Fatalf("threads = %+v", got)
	}
	want := []forge.Reply{{Author: "alice", Body: "This is intended."}, {Author: "", Body: "ghost reply"}}
	if fmt.Sprint(got[0].Replies) != fmt.Sprint(want) || got[1].Replies != nil {
		t.Fatalf("replies = %+v / %+v", got[0].Replies, got[1].Replies)
	}
}

func TestListOCRThreadsFailsClosed(t *testing.T) {
	gql := "POST /graphql"
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"graphql errors", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"errors": []any{map[string]any{"message": "Something went wrong"}}})
		}, "Something went wrong"},
		{"no pull request", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": nil}}})
		}, "not found"},
		{"decode", func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 200, "{") }, "decode"},
		{"http error", func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 403, "{}") }, "HTTP 403"},
		{"endless pages", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
				"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "x"}, "nodes": []any{}},
			}}}})
		}, "exceed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAPI(t)
			f, _ := fa.forge(t)
			fa.override[gql] = func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+fa.token {
					t.Errorf("GraphQL call without the installation token")
				}
				tc.handler(w, r)
			}
			threads, err := f.ListOCRThreads(context.Background(), testRef)
			if err == nil || threads != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("threads=%v err=%v, want %q", threads, err, tc.want)
			}
		})
	}
}

func TestListOCRThreadsBadInput(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	if _, err := f.ListOCRThreads(context.Background(), forge.PRRef{Repo: "bad"}); err == nil {
		t.Error("bad ref accepted")
	}
	fa.fail["GET /app"] = []failure{{status: 404, body: "{}"}}
	if _, err := f.ListOCRThreads(context.Background(), testRef); err == nil {
		t.Error("bot login failure ignored")
	}
}

func TestGraphqlUser(t *testing.T) {
	cases := []struct{ login, typename, want string }{
		{"ocr-test", "Bot", "ocr-test[bot]"},
		{"ocr-test[bot]", "Bot", "ocr-test[bot]"},
		{"alice", "User", "alice"},
	}
	for _, tc := range cases {
		if got := graphqlUser(tc.login, tc.typename).Login; got != tc.want {
			t.Errorf("graphqlUser(%s, %s) = %s", tc.login, tc.typename, got)
		}
	}
}
