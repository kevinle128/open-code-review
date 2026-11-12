// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/serve/state"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		body    string
		want    Command
		isCmd   bool
		wantErr string
	}{
		{"/review", Command{Kind: CommandReview}, true, ""},
		{"  \n\t/review-commit\nplease", Command{Kind: CommandReviewCommit}, true, ""},
		{"/review --effort=high", Command{Kind: CommandReview, Effort: "high"}, true, ""},
		{"/review\t--effort=\"low\"", Command{Kind: CommandReview, Effort: "low"}, true, ""},
		{"/review-commit --effort=medium", Command{Kind: CommandReviewCommit, Effort: "medium"}, true, ""},
		{"/review --max_rounds=9", Command{Kind: CommandReview}, true, "unsupported option"},
		{"/review --effort=extreme", Command{Kind: CommandReview}, true, "invalid effort"},
		{`/review "--effort=high`, Command{Kind: CommandReview}, true, "unterminated quote"},
		{"/reviewer", Command{}, false, ""},
		{"please /review", Command{}, false, ""},
		{"", Command{}, false, ""},
		{"/review it's #1", Command{Kind: CommandReview}, true, `unsupported option "it's"`},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			got, ok, err := ParseCommand(tc.body)
			if ok != tc.isCmd || got != tc.want {
				t.Fatalf("got %+v %v", got, ok)
			}
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err %v want %q", err, tc.wantErr)
			}
		})
	}
	if (Command{Kind: CommandReviewCommit}).trigger() != state.TriggerCmdReviewCommit || (Command{Kind: CommandReview}).trigger() != state.TriggerCmdReview {
		t.Fatal("trigger mapping")
	}
}

func TestSplitArgs(t *testing.T) {
	got, err := splitArgs(`  a "b c"  d"e f"  ""`)
	if err != nil || strings.Join(got, "|") != "a|b c|de f|" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestUnknownSlashCommand(t *testing.T) {
	cases := map[string]string{
		"/describe":        "/describe",
		"/ask what is #1":  "/ask",
		"/reviewer please": "/reviewer",
		"/review":          "",
		"/etc/hosts":       "",
		"see /describe":    "",
		"/":                "",
	}
	for body, want := range cases {
		got, ok := unknownSlashCommand(body)
		if got != want || ok != (want != "") {
			t.Fatalf("%q: got %q %v", body, got, ok)
		}
	}
}

func TestCommandAdded(t *testing.T) {
	cases := []struct {
		body, prev string
		want       bool
	}{
		{"/review", "", true},
		{"/review", "/review", false},
		{"/review   --effort=high", "note\n/review --effort=high", false},
		{"/review --effort=high", "/review", true},
		{"/review-commit", "/review", true},
	}
	for _, tc := range cases {
		if got := commandAdded(tc.body, tc.prev); got != tc.want {
			t.Fatalf("%q after %q: %v", tc.body, tc.prev, got)
		}
	}
}

func TestDedupe(t *testing.T) {
	now := time.Unix(0, 0)
	d := newDedupe(func() time.Time { return now }, time.Minute, 2)
	if d.Seen("a") || !d.Seen("a") {
		t.Fatal("second sight must hit")
	}
	now = now.Add(2 * time.Minute)
	if d.Seen("a") {
		t.Fatal("expired key must miss")
	}
	if d.Seen("b") || d.Seen("c") {
		t.Fatal("new keys must miss")
	}
	if d.Seen("a") {
		t.Fatal("oldest key must be evicted at capacity")
	}
	if len(d.keys) != 2 || d.order.Len() != 2 {
		t.Fatalf("size %d %d", len(d.keys), d.order.Len())
	}
}

func TestLogger(t *testing.T) {
	var buf bytes.Buffer
	withPR(withRequest(NewLogger(&buf), "r1"), ghRef(3)).Info("hello")
	if s := buf.String(); !strings.Contains(s, `"request_id":"r1"`) || !strings.Contains(s, `"pr":"github:acme/app#3"`) {
		t.Fatal(s)
	}
	if a, b := newRequestID(), newRequestID(); len(a) != 16 || a == b {
		t.Fatalf("ids %q %q", a, b)
	}
}
