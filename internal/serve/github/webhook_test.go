// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerify(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	body := []byte(`{"action":"opened"}`)
	cases := []struct {
		name string
		sig  string
		want error
	}{
		{"valid", sign(body, testSecret), nil},
		{"missing", "", errMissingSignature},
		{"wrong secret", sign(body, "other"), errBadSignature},
		{"no prefix", strings.TrimPrefix(sign(body, testSecret), "sha256="), errBadSignature},
		{"sha1", "sha1=" + strings.TrimPrefix(sign(body, testSecret), "sha256="), errBadSignature},
		{"not hex", "sha256=zz", errBadSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.sig != "" {
				h.Set("X-Hub-Signature-256", tc.sig)
			}
			if err := f.Verify(h, body); err != tc.want {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
	h := http.Header{}
	h.Set("X-Hub-Signature-256", sign(body, testSecret))
	if err := f.Verify(h, append(body, ' ')); err != errBadSignature {
		t.Fatalf("tampered body: %v", err)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withAction rewrites the action of a fixture so one file covers each action.
func withAction(t *testing.T, raw []byte, action string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["action"] = action
	b, _ := json.Marshal(m)
	return b
}

func parse(t *testing.T, f *Forge, event string, body []byte) (forge.Event, error) {
	t.Helper()
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	h.Set("X-GitHub-Delivery", "d-1")
	return f.ParseEvent(h, body)
}

func TestParsePullRequestEvents(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	opened := fixture(t, "pull_request_opened.json")
	cases := map[string]forge.EventKind{
		"opened":             forge.EventOpened,
		"ready_for_review":   forge.EventReady,
		"reopened":           forge.EventReopened,
		"synchronize":        forge.EventPush,
		"closed":             forge.EventClosed,
		"edited":             forge.EventIgnored,
		"labeled":            forge.EventIgnored,
		"converted_to_draft": forge.EventIgnored,
	}
	for action, want := range cases {
		t.Run(action, func(t *testing.T) {
			ev, err := parse(t, f, "pull_request", withAction(t, opened, action))
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != want || ev.DeliveryID != "d-1" || ev.Sender != "alice" || ev.SenderIsBot {
				t.Fatalf("event = %+v", ev)
			}
			if want == forge.EventIgnored {
				if ev.PR != nil {
					t.Fatal("ignored event carries a PR")
				}
				return
			}
			if ev.Ref != testRef || ev.PR == nil {
				t.Fatalf("ref = %+v pr = %v", ev.Ref, ev.PR)
			}
			got := *ev.PR
			if got.Ref != testRef || strings.Join(got.Labels, ",") != "backend,needs-review" || got.HeadSHA != headSHA ||
				got.BaseSHA != baseSHA || got.Title != "Add retry to the importer" || got.Author != "alice" ||
				got.SourceBranch != "feat/retry" || got.TargetBranch != "main" || got.State != forge.StateOpen || got.Draft {
				t.Fatalf("pr = %+v", got)
			}
		})
	}
}

func TestParseFixtures(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	cases := []struct {
		file, event string
		check       func(t *testing.T, ev forge.Event)
	}{
		{"pull_request_closed_merged.json", "pull_request", func(t *testing.T, ev forge.Event) {
			if ev.Kind != forge.EventClosed || ev.PR.State != forge.StateMerged {
				t.Fatalf("event = %+v pr = %+v", ev, ev.PR)
			}
		}},
		{"pull_request_review_thread_resolved.json", "pull_request_review_thread", func(t *testing.T, ev forge.Event) {
			if ev.Kind != forge.EventThreadResolved || ev.Ref != testRef || !ev.PR.Draft || !ev.PR.AuthorIsBot {
				t.Fatalf("event = %+v pr = %+v", ev, ev.PR)
			}
		}},
		{"issue_comment_created.json", "issue_comment", func(t *testing.T, ev forge.Event) {
			if ev.Kind != forge.EventCommand || ev.Ref != testRef || ev.CommentID != "9001" || ev.PR != nil ||
				ev.CommentBody != "\n  /review --effort=high\nplease look again" || ev.PrevCommentBody != "" {
				t.Fatalf("event = %+v", ev)
			}
		}},
		{"issue_comment_edited.json", "issue_comment", func(t *testing.T, ev forge.Event) {
			if ev.Kind != forge.EventCommand || ev.CommentID != "9002" || ev.CommentBody != "/review-commit\nplease review" ||
				ev.PrevCommentBody != "please review" {
				t.Fatalf("event = %+v", ev)
			}
		}},
		{"issue_comment_on_issue.json", "issue_comment", func(t *testing.T, ev forge.Event) {
			if ev.Kind != forge.EventIgnored {
				t.Fatalf("comment on a plain issue: %+v", ev)
			}
		}},
		{"issue_comment_by_bot.json", "issue_comment", func(t *testing.T, ev forge.Event) {
			if ev.Kind != forge.EventCommand || !ev.SenderIsBot || ev.Sender != testBot {
				t.Fatalf("bot comment: %+v", ev)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			ev, err := parse(t, f, tc.event, fixture(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, ev)
		})
	}
}

func TestParseCommentVariants(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	base := fixture(t, "issue_comment_created.json")
	mutate := func(edit func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(base, &m)
		edit(m)
		b, _ := json.Marshal(m)
		return b
	}
	setBody := func(body string) func(map[string]any) {
		return func(m map[string]any) { m["comment"].(map[string]any)["body"] = body }
	}
	cases := []struct {
		name string
		body []byte
		want forge.EventKind
	}{
		{"unknown slash command", mutate(setBody("/describe")), forge.EventCommand},
		{"plain comment", mutate(setBody("thanks, will fix")), forge.EventIgnored},
		{"slash later in body", mutate(setBody("see\n/review")), forge.EventIgnored},
		{"empty body", mutate(setBody("  \n ")), forge.EventIgnored},
		{"deleted", withAction(t, base, "deleted"), forge.EventIgnored},
		{"edited without body change", withAction(t, base, "edited"), forge.EventIgnored},
		{"missing comment", mutate(func(m map[string]any) { delete(m, "comment") }), forge.EventIgnored},
		{"missing pull_request key", mutate(func(m map[string]any) { delete(m["issue"].(map[string]any), "pull_request") }), forge.EventIgnored},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := parse(t, f, "issue_comment", tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != tc.want {
				t.Fatalf("kind = %s, want %s", ev.Kind, tc.want)
			}
		})
	}
}

func TestParseEventIgnoredAndErrors(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	opened := fixture(t, "pull_request_opened.json")
	thread := fixture(t, "pull_request_review_thread_resolved.json")

	ignored := []struct {
		event string
		body  []byte
	}{
		{"ping", []byte(`{"zen":"x","hook_id":1}`)},
		{"pull_request_review_comment", withAction(t, opened, "created")},
		{"pull_request_review_thread", withAction(t, thread, "unresolved")},
		{"", opened},
	}
	for _, tc := range ignored {
		ev, err := parse(t, f, tc.event, tc.body)
		if err != nil || ev.Kind != forge.EventIgnored {
			t.Errorf("%q: kind=%s err=%v", tc.event, ev.Kind, err)
		}
	}

	strip := func(raw []byte, edit func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		edit(m)
		b, _ := json.Marshal(m)
		return b
	}
	bad := []struct {
		name, event string
		body        []byte
		want        string
	}{
		{"invalid json", "pull_request", []byte("{"), "decode webhook"},
		{"no installation", "pull_request", strip(opened, func(m map[string]any) { delete(m, "installation") }), "installation"},
		{"no pull_request", "pull_request", strip(opened, func(m map[string]any) { delete(m, "pull_request") }), "no pull_request"},
		{"bad repo", "pull_request", strip(opened, func(m map[string]any) { m["repository"] = map[string]any{"full_name": "nope"} }), "invalid repository"},
		{"comment without installation", "issue_comment", strip(fixture(t, "issue_comment_created.json"), func(m map[string]any) {
			delete(m, "installation")
		}), "installation"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parse(t, f, tc.event, tc.body); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
