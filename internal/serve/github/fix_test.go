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

func TestBotPushIsBotSender(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	var p map[string]any
	if err := json.Unmarshal(withAction(t, fixture(t, "pull_request_opened.json"), "synchronize"), &p); err != nil {
		t.Fatal(err)
	}
	push := func(login, typ string) forge.Event {
		t.Helper()
		p["sender"] = map[string]any{"login": login, "type": typ}
		body, _ := json.Marshal(p)
		ev, err := parse(t, f, "pull_request", body)
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	// Before BotLogin reads the slug, every bot counts as the App.
	if ev := push("renovate[bot]", "Bot"); !ev.SenderIsBot || !ev.SenderIsSelf {
		t.Fatalf("bot before slug %+v", ev)
	}
	if _, err := f.BotLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev := push(testBot, "Bot")
	if ev.Kind != forge.EventPush || !ev.SenderIsBot || !ev.SenderIsSelf || ev.Sender != testBot {
		t.Fatalf("own push %+v", ev)
	}
	if ev := push("renovate[bot]", "Bot"); !ev.SenderIsBot || ev.SenderIsSelf {
		t.Fatalf("other bot push %+v", ev)
	}
	if ev := push(testBot, "User"); ev.SenderIsBot || ev.SenderIsSelf {
		t.Fatalf("user push %+v", ev)
	}
}

func TestCommitURL(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	sha := strings.Repeat("a", 40)
	if got := f.CommitURL(testRef, sha); got != f.webURL+"/acme/widgets/commit/"+sha {
		t.Fatalf("commit url %q", got)
	}
	if got := f.CommitURL(testRef, "main"); got != "" {
		t.Fatalf("a ref name gave %q", got)
	}
}

func TestLineURL(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	sha := strings.Repeat("a", 40)
	if got := f.LineURL(testRef, sha, "cmd/a b.go", 88); got != f.webURL+"/acme/widgets/blob/"+sha+"/cmd/a%20b.go#L88" {
		t.Fatalf("line url %q", got)
	}
}
