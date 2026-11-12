// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"regexp"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

func prEvent(kind forge.EventKind, ref forge.PRRef, id string) forge.Event {
	pr := openPR(ref, sha1)
	return forge.Event{Kind: kind, Ref: ref, DeliveryID: id, Sender: "alice", PR: &pr}
}

func TestDispatchFilters(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) {
		c.Ignore = serveconfig.IgnoreRules{
			Repos:          []*regexp.Regexp{regexp.MustCompile(`^skip/`)},
			Authors:        []*regexp.Regexp{regexp.MustCompile(`^renovate`)},
			Titles:         []*regexp.Regexp{regexp.MustCompile(`(?i)^wip`)},
			SourceBranches: []*regexp.Regexp{regexp.MustCompile(`^release/`)},
			TargetBranches: []*regexp.Regexp{regexp.MustCompile(`^legacy$`)},
			Labels:         []string{"no-review"},
		}
	})
	withPR := func(ref forge.PRRef, id string, mut func(*forge.PR)) forge.Event {
		ev := prEvent(forge.EventOpened, ref, id)
		mut(ev.PR)
		return ev
	}
	skipRef := forge.PRRef{Platform: forge.GitHub, Repo: "skip/app", Number: 1}
	cases := []struct {
		name string
		ev   forge.Event
		want string
	}{
		{"ignored kind", forge.Event{Kind: forge.EventIgnored, Ref: ghRef(1)}, "ignored event"},
		{"bot sender", forge.Event{Kind: forge.EventPush, Ref: ghRef(1), SenderIsBot: true}, "bot sender"},
		{"unsupported", forge.Event{Kind: "label", Ref: ghRef(1), DeliveryID: "u"}, "unsupported event"},
		{"draft", withPR(ghRef(1), "d", func(p *forge.PR) { p.Draft = true }), "draft"},
		{"repo rule", withPR(skipRef, "r1", func(*forge.PR) {}), "ignore rule repo:^skip/"},
		{"author rule", withPR(ghRef(1), "r2", func(p *forge.PR) { p.Author = "renovate[bot]" }), "ignore rule author:^renovate"},
		{"title rule", withPR(ghRef(1), "r3", func(p *forge.PR) { p.Title = "WIP: x" }), "ignore rule title:(?i)^wip"},
		{"source rule", withPR(ghRef(1), "r4", func(p *forge.PR) { p.SourceBranch = "release/1" }), "ignore rule source_branch:^release/"},
		{"target rule", withPR(ghRef(1), "r5", func(p *forge.PR) { p.TargetBranch = "legacy" }), "ignore rule target_branch:^legacy$"},
		{"label rule", withPR(ghRef(1), "r6", func(p *forge.PR) { p.Labels = []string{"x", "no-review"} }), "ignore rule label:no-review"},
		{"command on ignored repo", forge.Event{Kind: forge.EventCommand, Ref: skipRef, DeliveryID: "c0", CommentID: "1", CommentBody: "/review"}, "ignore rule repo:^skip/"},
		{"command bypasses author rule", forge.Event{Kind: forge.EventCommand, Ref: ghRef(2), DeliveryID: "c1", CommentID: "2", CommentBody: "/review",
			PR: &forge.PR{Author: "renovate"}}, "queued command review"},
		{"not a command", forge.Event{Kind: forge.EventCommand, Ref: ghRef(2), DeliveryID: "c2", CommentID: "3", CommentBody: "looks good"}, "not a command"},
		{"push queued", prEvent(forge.EventPush, ghRef(3), "p1"), "queued push"},
		{"push without pr", forge.Event{Kind: forge.EventPush, Ref: ghRef(3), DeliveryID: "p2"}, "queued push"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.srv.dispatch(tc.ev, []byte(tc.name), "rid"); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	h.drain()
}

func TestDispatchDedupe(t *testing.T) {
	h := newHarness(t)
	ev := prEvent(forge.EventPush, ghRef(1), "same")
	if got := h.srv.dispatch(ev, []byte("a"), "r"); got != "queued push" {
		t.Fatal(got)
	}
	if got := h.srv.dispatch(ev, []byte("b"), "r"); got != "duplicate delivery" {
		t.Fatalf("redelivery: %q", got)
	}
	noID := prEvent(forge.EventPush, glRef(1), "")
	if got := h.srv.dispatch(noID, []byte("body"), "r"); got != "queued push" {
		t.Fatal(got)
	}
	if got := h.srv.dispatch(noID, []byte("body"), "r"); got != "duplicate delivery" {
		t.Fatalf("body hash: %q", got)
	}
	if got := h.srv.dispatch(noID, []byte("other"), "r"); got != "queued push" {
		t.Fatalf("different body: %q", got)
	}
	h.drain()
}

func TestDispatchClosedRemovesWaiting(t *testing.T) {
	h := newHarness(t)
	ref := glRef(1)
	if err := h.srv.store.Put(Waiting{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if got := h.srv.dispatch(forge.Event{Kind: forge.EventClosed, Ref: ref}, nil, "r"); got != "closed" {
		t.Fatal(got)
	}
	if len(h.srv.store.List()) != 0 {
		t.Fatal("closed MR must leave the store")
	}
}

func TestDispatchCommands(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(5)
	h.gh.setPR(openPR(ref, sha1))
	cmd := func(id, commentID, body, prev string) forge.Event {
		return forge.Event{Kind: forge.EventCommand, Ref: ref, DeliveryID: id, CommentID: commentID, CommentBody: body, PrevCommentBody: prev}
	}
	steps := []struct {
		ev   forge.Event
		want string
	}{
		{cmd("1", "c1", "/review --effort=high", ""), "queued command review"},
		{cmd("2", "c1", "/review   --effort=high\n\nthanks", ""), "duplicate command"},
		{cmd("3", "c1", "/review --effort=high\nmore text", "/review --effort=high"), "edit added no command"},
		{cmd("4", "c1", "/review-commit", "/review --effort=high"), "queued command review-commit"},
		{cmd("5", "c2", "/review --max_rounds=9", ""), "queued command reply"},
		{cmd("6", "c3", "/describe", ""), "queued command reply"},
		{cmd("7", "", "/describe", ""), "unknown platform command"},
		{cmd("8", "", "/review", ""), "queued command review"},
	}
	for i, s := range steps {
		if got := h.srv.dispatch(s.ev, nil, "r"); got != s.want {
			t.Fatalf("step %d: got %q want %q", i, got, s.want)
		}
	}
	h.drain()

	reqs := h.rev.requests()
	if len(reqs) != 3 || reqs[0].Effort != "high" || reqs[1].Commit != sha1 || reqs[2].Commit != "" {
		t.Fatalf("command order or effort: %+v", reqs)
	}
	notes := strings.Join(h.gh.noteList(), "\n---\n")
	mustContain(t, notes, "max_rounds")
	mustContain(t, notes, "unknown command /describe")
	reactions := strings.Join(h.gh.reactionList(), ",")
	for _, want := range []string{"c1:eyes", "c2:eyes", "c3:eyes", "c2:failure", "c3:failure", "c1:success"} {
		if !strings.Contains(reactions, want) {
			t.Fatalf("missing reaction %s in %s", want, reactions)
		}
	}
}

func TestDispatchNotes(t *testing.T) {
	h := newHarness(t)
	ref := glRef(6)
	h.gl.setPR(openPR(ref, sha1))
	note := func(id, body string) forge.Event {
		return forge.Event{Kind: forge.EventNote, Ref: ref, DeliveryID: id, CommentID: "n" + id, CommentBody: body}
	}
	if got := h.srv.dispatch(note("1", "please check /etc/hosts"), nil, "r"); got != "queued note" {
		t.Fatal(got)
	}
	if got := h.srv.dispatch(note("2", "/ask why"), nil, "r"); got != "queued command reply" {
		t.Fatal(got)
	}
	h.drain()
	if notes := h.gl.noteList(); len(notes) != 1 || !strings.Contains(notes[0], "/ask") {
		t.Fatalf("help reply %q", notes)
	}
	decided := 0
	for _, l := range h.logs.lines(t) {
		if l["msg"] == "decided" && l["trigger"] == string(state.TriggerNote) {
			decided++
		}
	}
	if decided == 0 {
		t.Fatal("a note with an unknown command must still re-check the MR")
	}
}

func TestReactAsyncFailureIsLogged(t *testing.T) {
	h := newHarness(t)
	h.gh.setErr("React", errTest)
	h.srv.reactAsync(ghRef(1), "c1", forge.ReactionEyes, "r")
	h.drain()
	if !h.logs.has("reaction failed") {
		t.Fatal("not logged")
	}
}
