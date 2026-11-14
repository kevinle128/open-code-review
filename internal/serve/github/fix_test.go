// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func TestGetPRFork(t *testing.T) {
	cases := []struct {
		name string
		head any
		want bool
	}{
		{"same repository", map[string]any{"full_name": "ACME/widgets"}, false},
		{"fork", map[string]any{"full_name": "mallory/widgets"}, true},
		{"deleted fork", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAPI(t)
			fa.pull["head"].(map[string]any)["repo"] = tc.head
			f, _ := fa.forge(t)
			pr, err := f.GetPR(context.Background(), testRef)
			if err != nil {
				t.Fatal(err)
			}
			if pr.Fork != tc.want {
				t.Fatalf("Fork = %v, want %v", pr.Fork, tc.want)
			}
		})
	}
}

func TestPushSpec(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	pr := forge.PR{Ref: testRef, SourceBranch: "feat/retry", TargetBranch: "main"}
	spec, err := f.PushSpec(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if spec.HeadRef != "refs/heads/feat/retry" || spec.BaseRef != "refs/heads/main" ||
		!strings.HasSuffix(spec.URL, "/acme/widgets.git") || !strings.HasPrefix(spec.AuthHeader, "Authorization: Basic ") {
		t.Fatalf("spec = %+v", spec)
	}
	pr.Fork = true
	if _, err := f.PushSpec(context.Background(), pr); !errors.Is(err, forge.ErrFork) {
		t.Fatalf("fork err = %v", err)
	}
	pr.Fork, pr.SourceBranch = false, ""
	if _, err := f.PushSpec(context.Background(), pr); err == nil {
		t.Fatal("empty source branch accepted")
	}
}

func TestCommitIdentity(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	for range 2 {
		id, err := f.CommitIdentity(context.Background(), testRef)
		if err != nil {
			t.Fatal(err)
		}
		if id.Name != testBot || id.Email != "98765+"+testBot+"@users.noreply.github.com" {
			t.Fatalf("identity = %+v", id)
		}
	}
	if n := fa.count("GET /users/" + testBot); n != 1 {
		t.Fatalf("bot user read %d times, want 1 (cached)", n)
	}
}

func TestCommitIdentityErrors(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	fa.fail["GET /app"] = []failure{{status: 404, body: "{}"}}
	if _, err := f.CommitIdentity(context.Background(), testRef); err == nil {
		t.Fatal("app lookup failure ignored")
	}
	fa.override["GET /users/"+testBot] = func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 200, `{"id": 0}`) }
	if _, err := f.CommitIdentity(context.Background(), testRef); err == nil || !strings.Contains(err.Error(), "no id") {
		t.Fatalf("err = %v", err)
	}
	fa.override["GET /users/"+testBot] = func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 200, `{`) }
	if _, err := f.CommitIdentity(context.Background(), testRef); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v", err)
	}
	fa.override["GET /users/"+testBot] = func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 403, `{}`) }
	if _, err := f.CommitIdentity(context.Background(), testRef); err == nil {
		t.Fatal("HTTP error ignored")
	}
}

func TestReplyAndResolveThread(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	if err := f.ReplyThread(ctx, testRef, "T1", "skipped: needs a decision"); err != nil {
		t.Fatal(err)
	}
	if err := f.ResolveThread(ctx, testRef, "T1"); err != nil {
		t.Fatal(err)
	}
	if len(fa.mutations) != 2 {
		t.Fatalf("mutations = %d", len(fa.mutations))
	}
	reply, resolve := fa.mutations[0], fa.mutations[1]
	if !strings.Contains(reply["query"].(string), "addPullRequestReviewThreadReply") ||
		reply["variables"].(map[string]any)["body"] != "skipped: needs a decision" ||
		reply["variables"].(map[string]any)["thread"] != "T1" {
		t.Fatalf("reply = %v", reply)
	}
	if !strings.Contains(resolve["query"].(string), "resolveReviewThread") || resolve["variables"].(map[string]any)["thread"] != "T1" {
		t.Fatalf("resolve = %v", resolve)
	}
}

func TestReplyAndResolveThreadErrors(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	if err := f.ReplyThread(ctx, testRef, "", "x"); err == nil {
		t.Fatal("empty thread id accepted")
	}
	if err := f.ResolveThread(ctx, testRef, ""); err == nil {
		t.Fatal("empty thread id accepted")
	}
	if err := f.ResolveThread(ctx, forge.PRRef{Repo: "bad"}, "T1"); err == nil {
		t.Fatal("bad ref accepted")
	}
	fa.mutationErr = "Resource not accessible by integration"
	if err := f.ResolveThread(ctx, testRef, "T1"); err == nil || !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("err = %v", err)
	}
	fa.mutationErr = ""
	fa.override["POST /graphql"] = func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 200, `{`) }
	if err := f.ReplyThread(ctx, testRef, "T1", "x"); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v", err)
	}
	fa.override["POST /graphql"] = func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 502, `{}`) }
	if err := f.ReplyThread(ctx, testRef, "T1", "x"); err == nil {
		t.Fatal("HTTP error ignored")
	}
	if n := fa.count("POST /graphql"); n != 3 {
		t.Fatalf("graphql calls = %d, want 3 (a 5xx mutation is not resent)", n)
	}
}

// TestBotPushIsBotSender proves the App's own push to the head branch, which
// GitHub reports as pull_request synchronize sent by the App, is filtered.
func TestBotPushIsBotSender(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	var p map[string]any
	if err := json.Unmarshal(withAction(t, fixture(t, "pull_request_opened.json"), "synchronize"), &p); err != nil {
		t.Fatal(err)
	}
	p["sender"] = map[string]any{"login": testBot, "type": "Bot"}
	body, _ := json.Marshal(p)
	ev, err := parse(t, f, "pull_request", body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != forge.EventPush || !ev.SenderIsBot || ev.Sender != testBot {
		t.Fatalf("bot push %+v", ev)
	}
}
