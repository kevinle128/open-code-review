// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func TestGetPRFork(t *testing.T) {
	cases := []struct {
		source, dest int64
		want         bool
	}{{42, 42, false}, {77, 42, true}, {0, 42, false}}
	for _, tc := range cases {
		f := newFake(t)
		mr := mrJSON("opened", true)
		mr["source_project_id"], mr["target_project_id"] = tc.source, tc.dest
		f.json("GET "+mrBase, 200, mr)
		pr, err := newTestForge(t, f, nil).GetPR(context.Background(), testRef)
		if err != nil {
			t.Fatal(err)
		}
		if pr.Fork != tc.want {
			t.Fatalf("source %d dest %d: Fork = %v", tc.source, tc.dest, pr.Fork)
		}
	}
}

func TestPushSpec(t *testing.T) {
	f := newFake(t)
	f.handle("GET /projects/42", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"id": 42, "http_url_to_repo": f.srv.URL + "/team/svc.git"})
	})
	fg := newTestForge(t, f, nil)
	pr := forge.PR{Ref: testRef, SourceBranch: "feat/x", TargetBranch: "main"}
	spec, err := fg.PushSpec(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if spec.HeadRef != "refs/heads/feat/x" || spec.BaseRef != "refs/heads/main" || spec.URL != f.srv.URL+"/team/svc.git" ||
		!strings.HasPrefix(spec.AuthHeader, "Authorization: Basic ") {
		t.Fatalf("spec = %+v", spec)
	}
	pr.Fork = true
	if _, err := fg.PushSpec(context.Background(), pr); !errors.Is(err, forge.ErrFork) {
		t.Fatalf("fork: %v", err)
	}
	pr.Fork, pr.SourceBranch = false, "-x"
	if _, err := fg.PushSpec(context.Background(), pr); err == nil {
		t.Fatal("bad source branch accepted")
	}
}

func TestCommitIdentity(t *testing.T) {
	cases := []struct {
		user apiUser
		want forge.Identity
	}{
		{apiUser{ID: testBotID, Username: "pr-review", Name: "PR Review", Email: "a@x.test", CommitEmail: "c@x.test"},
			forge.Identity{Name: "PR Review", Email: "c@x.test"}},
		{apiUser{ID: testBotID, Username: "pr-review", Email: "a@x.test", PublicEmail: "p@x.test"},
			forge.Identity{Name: "pr-review", Email: "a@x.test"}},
		{apiUser{ID: testBotID, Username: "pr-review", PublicEmail: "p@x.test"},
			forge.Identity{Name: "pr-review", Email: "p@x.test"}},
	}
	for _, tc := range cases {
		f := newFake(t)
		f.json("GET /user", 200, tc.user)
		got, err := newTestForge(t, f, nil).CommitIdentity(context.Background(), testRef)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: got %+v err %v", tc.user, got, err)
		}
	}
	noMail := commitIdentity(apiUser{Username: "pr-review"}, "https://git.example.test")
	if noMail.Email != "pr-review@noreply.git.example.test" {
		t.Fatalf("fallback email %q", noMail.Email)
	}
	if got := commitIdentity(apiUser{Username: "pr-review"}, "::"); got.Email != "pr-review@noreply.gitlab.invalid" {
		t.Fatalf("fallback email %q", got.Email)
	}
	down := newFake(t)
	down.json("GET /user", 500, map[string]string{})
	if _, err := newTestForge(t, down, nil).CommitIdentity(context.Background(), testRef); err == nil {
		t.Fatal("user read failure ignored")
	}
}

func TestReplyAndResolveThread(t *testing.T) {
	f := newFake(t)
	f.json("POST "+mrBase+"/discussions/abc123/notes", 201, map[string]any{"id": 1})
	f.json("PUT "+mrBase+"/discussions/abc123", 200, map[string]any{"id": "abc123"})
	fg := newTestForge(t, f, nil)
	ctx := context.Background()
	if err := fg.ReplyThread(ctx, testRef, "abc123", "skipped: needs a decision"); err != nil {
		t.Fatal(err)
	}
	if err := fg.ResolveThread(ctx, testRef, "abc123"); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 2 {
		t.Fatalf("writes %+v", w)
	}
	var body struct{ Body string }
	_ = json.Unmarshal(w[0].Body, &body)
	if w[0].Path != mrBase+"/discussions/abc123/notes" || body.Body != "skipped: needs a decision" {
		t.Fatalf("reply %+v", w[0])
	}
	if w[1].Method != http.MethodPut || w[1].Query.Get("resolved") != "true" {
		t.Fatalf("resolve %+v", w[1])
	}
	for _, bad := range []string{"", "../x", "a b"} {
		if err := fg.ReplyThread(ctx, testRef, bad, "x"); err == nil {
			t.Fatalf("thread id %q accepted", bad)
		}
		if err := fg.ResolveThread(ctx, testRef, bad); err == nil {
			t.Fatalf("thread id %q accepted", bad)
		}
	}
	if err := fg.ReplyThread(ctx, testRef, "abc123", ""); err == nil {
		t.Fatal("empty reply accepted")
	}
	if err := fg.ResolveThread(ctx, forge.PRRef{}, "abc123"); err == nil {
		t.Fatal("empty ref accepted")
	}
	f.fail("PUT "+mrBase+"/discussions/abc123", 403)
	if err := fg.ResolveThread(ctx, testRef, "abc123"); !isStatus(err, 403) {
		t.Fatalf("403: %v", err)
	}
}

// TestListOCRThreadsPlaceAndReplies reads where a thread sits and keeps the
// replies of people, not the bot's own or system notes.
func TestListOCRThreadsPlaceAndReplies(t *testing.T) {
	m := newMRFake(t)
	human := apiUser{ID: 7, Username: "dev1"}
	m.addDiscussion(botUser, forge.FindingTag("r1-aaaa-01")+"\nbody")
	line, old := 12, 9
	d := &m.discussions[0]
	d.Notes[0].Position = &notePosition{NewPath: "a.go", NewLine: &line}
	d.Notes = append(d.Notes,
		apiNote{ID: 2, Body: "This is intended.", Author: human},
		apiNote{ID: 3, Body: "Skipped by the fix run.", Author: botUser},
		apiNote{ID: 4, Body: "resolved the thread", Author: human, System: true})
	m.addDiscussion(botUser, forge.FindingTag("r1-aaaa-02")+"\nbody")
	m.discussions[1].Notes[0].Position = &notePosition{OldPath: "gone.go", OldLine: &old}
	fg := newTestForge(t, m.fakeGitLab, nil)
	threads, err := fg.ListOCRThreads(context.Background(), testRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 || threads[0].Path != "a.go" || threads[0].Line != 12 || threads[1].Path != "gone.go" || threads[1].Line != 9 {
		t.Fatalf("threads %+v", threads)
	}
	if fmt.Sprint(threads[0].Replies) != fmt.Sprint([]forge.Reply{{Author: "dev1", Body: "This is intended."}}) || threads[1].Replies != nil {
		t.Fatalf("replies %+v / %+v", threads[0].Replies, threads[1].Replies)
	}
	if threads[0].Body != forge.FindingTag("r1-aaaa-01")+"\nbody" {
		t.Fatalf("body %q", threads[0].Body)
	}
}

// TestBotPushIsBotSender proves the bot's own push to the source branch,
// which GitLab reports as a merge request update with oldrev, is filtered.
func TestBotPushIsBotSender(t *testing.T) {
	f := offlineForge(t, serveconfig.GitLabConfig{BotUsername: "pr-review"})
	var p map[string]any
	if err := json.Unmarshal(fixture(t, "mr_push.json"), &p); err != nil {
		t.Fatal(err)
	}
	p["user"] = map[string]any{"id": 99, "username": "pr-review", "name": "PR Review"}
	body, _ := json.Marshal(p)
	ev, err := f.ParseEvent(nil, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != forge.EventPush || !ev.SenderIsBot {
		t.Fatalf("bot push %+v", ev)
	}
}
