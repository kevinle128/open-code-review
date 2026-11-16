// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"regexp"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
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
		{"own push", forge.Event{Kind: forge.EventPush, Ref: ghRef(1), SenderIsBot: true, SenderIsSelf: true}, "bot sender"},
		{"other bot comment", forge.Event{Kind: forge.EventCommand, Ref: ghRef(1), SenderIsBot: true}, "bot sender"},
		{"other bot opened", forge.Event{Kind: forge.EventOpened, Ref: ghRef(5), DeliveryID: "b1", SenderIsBot: true}, "queued opened"},
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
		{"push copies status", prEvent(forge.EventPush, ghRef(3), "p1"), "status copy"},
		{"reopened copies status", forge.Event{Kind: forge.EventReopened, Ref: ghRef(3), DeliveryID: "p2"}, "status copy"},
		{"ready queued", prEvent(forge.EventReady, ghRef(4), "p3"), "queued ready"},
		{"opened queued", prEvent(forge.EventOpened, ghRef(4), "p4"), "queued opened"},
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
	if got := h.srv.dispatch(ev, []byte("a"), "r"); got != "status copy" {
		t.Fatal(got)
	}
	if got := h.srv.dispatch(ev, []byte("b"), "r"); got != "duplicate delivery" {
		t.Fatalf("redelivery: %q", got)
	}
	noID := prEvent(forge.EventPush, glRef(1), "")
	if got := h.srv.dispatch(noID, []byte("body"), "r"); got != "status copy" {
		t.Fatal(got)
	}
	if got := h.srv.dispatch(noID, []byte("body"), "r"); got != "duplicate delivery" {
		t.Fatalf("body hash: %q", got)
	}
	if got := h.srv.dispatch(noID, []byte("other"), "r"); got != "status copy" {
		t.Fatalf("different body: %q", got)
	}
	h.drain()
}

func TestDispatchClosedForgetsTheChain(t *testing.T) {
	h := newHarness(t)
	ref := glRef(1)
	gen := h.srv.restartChain(ref)
	cancelled := false
	if !h.srv.begin(ref, gen, func() { cancelled = true }) {
		t.Fatal("begin refused the current chain")
	}
	if got := h.srv.dispatch(forge.Event{Kind: forge.EventClosed, Ref: ref}, nil, "r"); got != "closed" {
		t.Fatal(got)
	}
	if !cancelled || h.srv.current(ref, gen) {
		t.Fatalf("closed MR keeps its chain: cancelled=%v", cancelled)
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
		h.drain()
	}

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

func TestDispatchMentionCommands(t *testing.T) {
	h := newHarness(t)
	h.gh.names = []string{"kevinle128-ocr"}
	ref := ghRef(5)
	h.gh.setPR(openPR(ref, sha1))
	cmd := func(id, commentID, body, prev string) forge.Event {
		return forge.Event{Kind: forge.EventCommand, Ref: ref, DeliveryID: id, CommentID: commentID, CommentBody: body, PrevCommentBody: prev}
	}
	steps := []struct {
		ev   forge.Event
		want string
	}{
		{cmd("1", "c1", "@kevinle128-ocr review --effort=high", ""), "queued command review"},
		{cmd("2", "c1", "/review --effort=high", "@kevinle128-ocr review --effort=high"), "edit added no command"},
		{cmd("3", "c2", "@Kevinle128-OCR: review-commit", ""), "queued command review-commit"},
		{cmd("4", "c2", "/review-commit", ""), "duplicate command"},
		{cmd("5", "c3", "@kevinle128-ocr hello", ""), "queued command reply"},
		{cmd("6", "c4", "@kevinle128-ocr", ""), "queued command reply"},
		{cmd("7", "c5", "@someone review", ""), "not a command"},
		{cmd("8", "c6", "thanks @kevinle128-ocr review", ""), "not a command"},
	}
	for i, s := range steps {
		if got := h.srv.dispatch(s.ev, nil, "r"); got != s.want {
			t.Fatalf("step %d: got %q want %q", i, got, s.want)
		}
		h.drain()
	}

	reqs := h.rev.requests()
	if len(reqs) != 2 || reqs[0].Effort != "high" || reqs[1].Commit != sha1 {
		t.Fatalf("command order or effort: %+v", reqs)
	}
	notes := strings.Join(h.gh.noteList(), "\n---\n")
	mustContain(t, notes, "unknown command /hello")
	mustContain(t, notes, "no command given")
	mustContain(t, notes, h.loc.Text("command.help_mention", "bot", "kevinle128-ocr"))
	mustContain(t, h.gh.summaries[ref.Key()], h.loc.Text("summary.hint_mention", "bot", "kevinle128-ocr"))
	reactions := strings.Join(h.gh.reactionList(), ",")
	for _, unwanted := range []string{"c5:", "c6:"} {
		if strings.Contains(reactions, unwanted) {
			t.Fatalf("reaction on a comment not meant for the bot: %s", reactions)
		}
	}
}

// TestDispatchFixIsUnknown answers the removed fix command with the help.
func TestDispatchFixIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.gh.names = []string{"kevinle128-ocr"}
	ref := ghRef(7)
	h.gh.setPR(openPR(ref, sha1))
	ev := forge.Event{Kind: forge.EventCommand, Ref: ref, DeliveryID: "1", CommentID: "c1", CommentBody: "@kevinle128-ocr, fix"}
	if got := h.srv.dispatch(ev, nil, "r"); got != "queued command reply" {
		t.Fatal(got)
	}
	h.drain()
	notes := strings.Join(h.gh.noteList(), "\n")
	mustContain(t, notes, "unknown command /fix")
	if strings.Contains(notes, "@kevinle128-ocr fix") || len(h.rev.requests()) != 0 {
		t.Fatalf("fix still offered or run:\n%s", notes)
	}
}

func TestDispatchNotes(t *testing.T) {
	h := newHarness(t)
	h.gl.names = []string{"pr-review"}
	ref := glRef(6)
	h.gl.setPR(openPR(ref, sha1))
	note := func(id, body string) forge.Event {
		return forge.Event{Kind: forge.EventNote, Ref: ref, DeliveryID: id, CommentID: "n" + id, CommentBody: body}
	}
	steps := []struct {
		ev   forge.Event
		want string
	}{
		{note("1", "@someone review"), "not a command"},
		{note("2", "please check /etc/hosts"), "not a command"},
		{note("3", "/ask why"), "queued command reply"},
		{note("4", "@pr-review review"), "queued command review"},
	}
	for i, st := range steps {
		if got := h.srv.dispatch(st.ev, nil, "r"); got != st.want {
			t.Fatalf("step %d: got %q want %q", i, got, st.want)
		}
		h.drain()
	}
	edited := note("5", "/review")
	edited.CommentID = "n4"
	if got := h.srv.dispatch(edited, nil, "r"); got != "duplicate command" {
		t.Fatal(got)
	}
	h.drain()
	if reqs := h.rev.requests(); len(reqs) != 1 {
		t.Fatalf("reviews %+v", reqs)
	}
	mustContain(t, strings.Join(h.gl.noteList(), "\n"), "/ask")
	if h.srv.mention(forge.GitLab) != "pr-review" || h.srv.mention("other") != "" {
		t.Fatal("mention per platform")
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
