// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func offlineForge(t *testing.T, cfg serveconfig.GitLabConfig) *Forge {
	t.Helper()
	if cfg.URL == "" {
		cfg.URL = "https://git.example.test"
	}
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	f, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestVerify(t *testing.T) {
	f := offlineForge(t, serveconfig.GitLabConfig{WebhookSecret: "s3cret"})
	h := http.Header{}
	if err := f.Verify(h, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("missing token: %v", err)
	}
	h.Set("X-Gitlab-Token", "wrong")
	if err := f.Verify(h, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	h.Set("X-Gitlab-Token", "s3cret")
	if err := f.Verify(h, nil); err != nil {
		t.Fatalf("good token: %v", err)
	}
	empty := offlineForge(t, serveconfig.GitLabConfig{})
	h.Set("X-Gitlab-Token", "")
	if err := empty.Verify(h, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty secret must fail closed: %v", err)
	}
}

func TestParseEventFixtures(t *testing.T) {
	f := offlineForge(t, serveconfig.GitLabConfig{BotUsername: "pr-review"})
	head := strings.Repeat("a", 40)
	cases := []struct {
		file     string
		kind     forge.EventKind
		body     string
		comment  string
		author   string
		draft    bool
		state    string
		noPR     bool
		labelled bool
	}{
		{file: "mr_open.json", kind: forge.EventOpened, author: "dev1", state: forge.StateOpen, labelled: true},
		{file: "mr_reopen.json", kind: forge.EventReopened, author: "dev1", state: forge.StateOpen, labelled: true},
		{file: "mr_close.json", kind: forge.EventClosed, author: "dev1", state: forge.StateClosed, labelled: true},
		{file: "mr_merge.json", kind: forge.EventClosed, author: "dev1", state: forge.StateMerged, labelled: true},
		{file: "mr_push.json", kind: forge.EventPush, author: "dev1", state: forge.StateOpen, labelled: true},
		{file: "mr_ready.json", kind: forge.EventReady, author: "dev1", state: forge.StateOpen, labelled: true},
		{file: "mr_ready_title.json", kind: forge.EventReady, author: "dev1", state: forge.StateOpen, labelled: true},
		{file: "mr_reviewer.json", kind: forge.EventCommand, body: "/review", author: "dev1", state: forge.StateOpen, labelled: true},
		{file: "mr_approved.json", kind: forge.EventIgnored, author: "dev1", state: forge.StateOpen, labelled: true},
		{file: "note_command.json", kind: forge.EventCommand, body: "\n  /review --effort=high\nplease", comment: "3001", state: forge.StateOpen, labelled: true},
		{file: "note_plain.json", kind: forge.EventNote, body: "Looks good, thanks", comment: "3001", state: forge.StateOpen, labelled: true},
		{file: "note_issue.json", kind: forge.EventIgnored, noPR: true},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			h := http.Header{}
			h.Set("X-Gitlab-Event-UUID", "uuid-1")
			ev, err := f.ParseEvent(h, fixture(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != tc.kind || ev.CommentBody != tc.body || ev.CommentID != tc.comment || ev.DeliveryID != "uuid-1" {
				t.Fatalf("event %+v", ev)
			}
			if ev.SenderIsBot {
				t.Fatal("human sender marked as bot")
			}
			if tc.noPR {
				if ev.PR != nil {
					t.Fatal("unexpected PR")
				}
				return
			}
			want := forge.PRRef{Platform: forge.GitLab, Repo: "team/svc", ProjectID: 42, Number: 5}
			if ev.Ref != want || ev.PR == nil || ev.PR.Ref != want {
				t.Fatalf("ref %+v pr %+v", ev.Ref, ev.PR)
			}
			pr := ev.PR
			if pr.HeadSHA != head || pr.SourceBranch != "feat/x" || pr.TargetBranch != "main" || pr.Title != "Add feature" ||
				pr.Author != tc.author || pr.Draft != tc.draft || pr.State != tc.state {
				t.Fatalf("pr %+v", pr)
			}
			if tc.labelled && (len(pr.Labels) != 1 || pr.Labels[0] != "backend") {
				t.Fatalf("labels %v", pr.Labels)
			}
		})
	}
}

func TestParseEventEdgeCases(t *testing.T) {
	f := offlineForge(t, serveconfig.GitLabConfig{})
	if _, err := f.ParseEvent(nil, []byte("{")); err == nil {
		t.Fatal("bad JSON: want error")
	}
	for _, body := range []string{
		`{}`,
		`{"object_kind":" "}`,
		`{"object_kind":"merge_request"}`,
		`{"object_kind":"merge_request","object_attributes":{"iid":0},"project":{"id":1}}`,
		`{"object_kind":"note"}`,
		`{"object_kind":"note","object_attributes":{"noteable_type":"MergeRequest"},"merge_request":{"iid":2},"project":{"id":0}}`,
	} {
		if _, err := f.ParseEvent(nil, []byte(body)); err == nil {
			t.Errorf("%s: want error", body)
		}
	}
	ev, err := f.ParseEvent(nil, []byte(`{"object_kind":"push"}`))
	if err != nil || ev.Kind != forge.EventIgnored {
		t.Fatalf("push hook: %+v %v", ev, err)
	}
	h := http.Header{}
	h.Set("Idempotency-Key", "idem-1")
	ev, _ = f.ParseEvent(h, []byte(`{"object_kind":"note","object_attributes":{"noteable_type":"MergeRequest","system":true,"note":"/review"},"merge_request":{"iid":2},"project":{"id":3}}`))
	if ev.Kind != forge.EventIgnored || ev.DeliveryID != "idem-1" {
		t.Fatalf("system note: %+v", ev)
	}
	ev, _ = f.ParseEvent(nil, []byte(`{"object_kind":"note","object_attributes":{"noteable_type":"MergeRequest","note":"  \n\n"},"merge_request":{"iid":2,"labels":[]},"labels":[{"title":"top"}],"project":{"id":3}}`))
	if ev.Kind != forge.EventNote || ev.DeliveryID != "" || len(ev.PR.Labels) != 1 || ev.PR.Labels[0] != "top" {
		t.Fatalf("blank note: %+v", ev)
	}
	ev, _ = f.ParseEvent(nil, []byte(`{"object_kind":"note","object_attributes":{"noteable_type":"MergeRequest","note":"/describe"},"merge_request":{"iid":2},"project":{"id":3}}`))
	if ev.Kind != forge.EventCommand {
		t.Fatalf("unknown slash command must reach the orchestrator: %+v", ev)
	}
}

func TestParseEventDraftAndReviewer(t *testing.T) {
	f := offlineForge(t, serveconfig.GitLabConfig{BotUsername: "pr-review"})
	base := `{"object_kind":"merge_request","project":{"id":1},"user":{"id":7,"username":"dev"},"object_attributes":{"iid":3,"action":"update",%s},"changes":{%s}}`
	cases := []struct {
		name, attrs, changes string
		kind                 forge.EventKind
		draft                bool
	}{
		{"string draft flags", `"draft":false`, `"draft":{"previous":"true","current":"false"}`, forge.EventReady, false},
		{"draft became draft", `"draft":true`, `"draft":{"previous":false,"current":true}`, forge.EventIgnored, true},
		{"unreadable draft flags", `"draft":false`, `"draft":{"previous":"yes","current":1}`, forge.EventIgnored, false},
		{"title still draft", `"title":"Draft: x"`, `"title":{"previous":"Draft: x","current":"Draft: y"}`, forge.EventIgnored, true},
		{"title not strings", `"title":"x"`, `"title":{"previous":1,"current":2}`, forge.EventIgnored, false},
		{"wip flag", `"work_in_progress":true`, `"labels":{}`, forge.EventIgnored, true},
		{"bot reviewer on draft", `"draft":true`, `"reviewers":{"previous":[],"current":[{"id":99,"username":"pr-review"}]}`, forge.EventCommand, true},
		{"inner labels", `"draft":false,"labels":[{"title":"inner"}]`, `"labels":{}`, forge.EventIgnored, false},
		{"bot already reviewer", `"draft":false`, `"reviewers":{"previous":[{"id":99,"username":"PR-Review"}],"current":[{"id":99,"username":"pr-review"},{"id":5,"username":"x"}]}`, forge.EventIgnored, false},
		{"other reviewer", `"draft":false`, `"reviewers":{"previous":null,"current":[{"id":5,"username":"x"}]}`, forge.EventIgnored, false},
		{"bad reviewers", `"draft":false`, `"reviewers":{"previous":[],"current":"x"}`, forge.EventIgnored, false},
		{"bot reviewer, null previous", `"draft":false`, `"reviewers":{"current":[{"id":99,"username":"pr-review"}]}`, forge.EventCommand, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := f.ParseEvent(nil, []byte(fmt.Sprintf(base, tc.attrs, tc.changes)))
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != tc.kind || ev.PR.Draft != tc.draft {
				t.Fatalf("kind %s draft %v", ev.Kind, ev.PR.Draft)
			}
			if tc.name == "inner labels" && (len(ev.PR.Labels) != 1 || ev.PR.Labels[0] != "inner") {
				t.Fatalf("labels %v", ev.PR.Labels)
			}
			if ev.Kind == forge.EventCommand && (ev.CommentID != "" || ev.CommentBody != "/review") {
				t.Fatalf("reviewer command %+v", ev)
			}
		})
	}
}

func TestSenderIsBot(t *testing.T) {
	f := offlineForge(t, serveconfig.GitLabConfig{BotUsername: "pr-review"})
	body := func(id int, name string) []byte {
		return []byte(fmt.Sprintf(`{"object_kind":"merge_request","project":{"id":1},"user":{"id":%d,"username":%q},"object_attributes":{"iid":3,"action":"open"}}`, id, name))
	}
	check := func(id int, name string, want bool) {
		t.Helper()
		ev, err := f.ParseEvent(nil, body(id, name))
		if err != nil || ev.SenderIsBot != want || ev.Sender != name {
			t.Fatalf("%d %q: bot=%v err=%v", id, name, ev.SenderIsBot, err)
		}
	}
	check(7, "dev", false)
	check(7, "PR-Review", true)
	check(7, "renovate-bot", true)
	check(7, "", false)
	f.mu.Lock()
	f.botID, f.botName = 55, "svc-reviewer"
	f.mu.Unlock()
	check(55, "renamed", true)
	check(8, "svc-reviewer", true)
	f.SetBotIndicators([]string{" Robot ", ""})
	check(7, "renovate-bot", false)
	check(7, "my-robot", true)
}
