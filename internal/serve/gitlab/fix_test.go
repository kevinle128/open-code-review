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
	if ev.Kind != forge.EventPush || !ev.SenderIsBot || !ev.SenderIsSelf {
		t.Fatalf("bot push %+v", ev)
	}
}

func TestCommitURL(t *testing.T) {
	fg := newTestForge(t, newFake(t), nil)
	sha := strings.Repeat("b", 40)
	ref := forge.PRRef{Platform: forge.GitLab, Repo: "group/sub/svc", ProjectID: 42, Number: 3}
	if got := fg.CommitURL(ref, sha); got != fg.cfg.URL+"/group/sub/svc/-/commit/"+sha {
		t.Fatalf("commit url %q", got)
	}
	ref.Repo = ""
	if got := fg.CommitURL(ref, sha); got != "" {
		t.Fatalf("no repository path gave %q", got)
	}
}

func TestLineURL(t *testing.T) {
	fg := newTestForge(t, newFake(t), nil)
	sha := strings.Repeat("b", 40)
	ref := forge.PRRef{Platform: forge.GitLab, Repo: "group/sub/svc", ProjectID: 42, Number: 3}
	if got := fg.LineURL(ref, sha, "internal/job/stall.go", 88); got != fg.cfg.URL+"/group/sub/svc/-/blob/"+sha+"/internal/job/stall.go#L88" {
		t.Fatalf("line url %q", got)
	}
}
