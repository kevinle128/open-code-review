// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// fakeFixer records requests and answers with fn.
type fakeFixer struct {
	mu   sync.Mutex
	reqs []review.FixRequest
	fn   func(context.Context, review.FixRequest) (review.FixResult, error)
}

func (x *fakeFixer) Fix(ctx context.Context, req review.FixRequest) (review.FixResult, error) {
	x.mu.Lock()
	x.reqs = append(x.reqs, req)
	fn := x.fn
	x.mu.Unlock()
	if fn == nil {
		return review.FixResult{}, nil
	}
	return fn(ctx, req)
}

func (x *fakeFixer) requests() []review.FixRequest {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.reqs)
}

func fixAnswer(res review.FixResult) func(context.Context, review.FixRequest) (review.FixResult, error) {
	return func(context.Context, review.FixRequest) (review.FixResult, error) { return res, nil }
}

// findingThread is a thread whose root is rendered as the service posts it.
func (h *harness) findingThread(id, path string, line int, title string, resolved bool, replies ...forge.Reply) forge.Thread {
	fd := forge.Finding{ID: id, Path: path, StartLine: line, EndLine: line, Category: "bug", Severity: "high",
		Content: "**" + title + "**\n\nExplain " + title + "."}
	body := state.RenderFinding(fd, forge.Placement{Kind: forge.Line}, forge.GitHub, h.loc)
	return forge.Thread{ID: "t-" + id, FindingID: id, Resolved: resolved, Path: path, Line: line, Body: body, Replies: replies}
}

func (h *harness) setThreads(ref forge.PRRef, ts ...forge.Thread) {
	f := h.forgeFor(ref.Platform)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threads[ref.Key()] = ts
}

func (f *fakeForge) fixLists() (replies, resolved []string, pushSpecs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.replies), slices.Clone(f.resolved), f.pushSpecs
}

func (ws *fakeWorkspaces) fixLists() (branchFetched, commits, pushed []string, acquired, released int) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return slices.Clone(ws.branchFetched), slices.Clone(ws.commits), slices.Clone(ws.pushed), ws.acquired, ws.released
}

func TestParseFixCommand(t *testing.T) {
	cmd, ok, err := ParseCommand("/fix\nplease")
	if !ok || err != nil || cmd.Kind != CommandFix {
		t.Fatalf("cmd %+v ok %v err %v", cmd, ok, err)
	}
	if _, ok, err := ParseCommand("/fix --effort=high"); !ok || err == nil {
		t.Fatalf("argument accepted: ok %v err %v", ok, err)
	}
	if _, ok, err := ParseCommand(`/fix "x`); !ok || err == nil {
		t.Fatalf("bad quote accepted: ok %v err %v", ok, err)
	}
	if _, unknown := unknownSlashCommand("/fix"); unknown {
		t.Fatal("/fix reported as unknown")
	}
}

// TestFixHappyPath commits and pushes the fix as the bot, resolves the fixed
// thread, replies in the skipped one, posts the note and then reviews the
// fix commit without counting a round.
func TestFixHappyPath(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	h.auto(ref, state.TriggerOpened)
	before := h.gh.marker(t, ref)
	h.setThreads(ref,
		h.findingThread("f1", "a.go", 3, "Missing return", false),
		h.findingThread("f2", "b.go", 7, "Unused import", false, forge.Reply{Author: "alice", Body: "Intended."}),
		h.findingThread("f0", "c.go", 1, "Old", true))
	h.ws.set(func(ws *fakeWorkspaces) { ws.dirty = true })
	h.fix.fn = func(_ context.Context, req review.FixRequest) (review.FixResult, error) {
		// The push lands on the forge, so the follow-up review sees it.
		h.gh.push(ref, fixSHA)
		return review.FixResult{Fixed: []string{"f1"}, Skipped: []review.FixSkip{{ID: "f2", Reason: "The author disputed it."}},
			Summary: "Return the error.", CommitMessage: "fix: return the error", Warnings: []string{"w"}}, nil
	}
	h.command(ref, "/fix", "c-1")
	h.drain()

	reqs := h.fix.requests()
	if len(reqs) != 1 || len(reqs[0].Findings) != 2 || reqs[0].RepoDir != "/work/slot-0" || !slices.Equal(reqs[0].Files, []string{"a.go"}) {
		t.Fatalf("fix requests %+v", reqs)
	}
	f1, f2 := reqs[0].Findings[0], reqs[0].Findings[1]
	if f1.ID != "f1" || f1.Path != "a.go" || f1.Line != 3 || f1.Severity != "high" || f1.Title != "Missing return" ||
		f1.Body != "Explain Missing return." || f1.Replies != nil {
		t.Fatalf("finding %+v", f1)
	}
	if len(f2.Replies) != 1 || f2.Replies[0] != (review.FixReply{Author: "alice", Body: "Intended."}) {
		t.Fatalf("replies %+v", f2.Replies)
	}
	fetched, commits, pushed, acquired, released := h.ws.fixLists()
	if !slices.Equal(fetched, []string{"refs/heads/feature@" + sha1 + ".." + shaBase}) ||
		!slices.Equal(commits, []string{"ocr[bot]:fix: return the error"}) ||
		!slices.Equal(pushed, []string{"refs/heads/feature@" + fixSHA}) || acquired != released {
		t.Fatalf("fetched %v commits %v pushed %v acquired %d released %d", fetched, commits, pushed, acquired, released)
	}
	replies, resolved, pushSpecs := h.gh.fixLists()
	if !slices.Equal(resolved, []string{"t-f1"}) || len(replies) != 1 || !strings.HasPrefix(replies[0], "t-f2:") ||
		!strings.Contains(replies[0], "The author disputed it.") || pushSpecs != 2 {
		t.Fatalf("resolved %v replies %v push specs %d", resolved, replies, pushSpecs)
	}
	notes := h.gh.noteList()
	done := ""
	for _, n := range notes {
		if strings.Contains(n, "ffffff") {
			done = n
		}
	}
	mustContain(t, done, h.loc.Text("fix.done", "sha", "`fffffff`", "fixed", "1", "skipped", "1"))
	mustContain(t, done, "Return the error.")
	mustContain(t, done, "Unused import (`b.go` L7): The author disputed it.")
	if r := h.gh.reactionList(); !slices.Contains(r, "c-1:success") || slices.Contains(r, "c-1:failure") {
		t.Fatalf("reactions %v", r)
	}
	// The follow-up reviews only the fix commit and does not count.
	revs := h.rev.requests()
	last := revs[len(revs)-1]
	if len(revs) != 2 || last.Commit != fixSHA || last.From != "" {
		t.Fatalf("reviews %+v", revs)
	}
	after := h.gh.marker(t, ref)
	if after.Auto != before.Auto || after.Runs[len(after.Runs)-1].Kind != state.KindManualCommit {
		t.Fatalf("marker before %+v after %+v", before, after)
	}
	if !h.logs.has("fix warnings") || !h.logs.has("fix pushed") {
		t.Fatalf("logs\n%s", h.logs.String())
	}
}

// TestFixFollowUpRunsOnDraft reviews the fix commit even on a draft PR,
// unlike a command the platform raises.
func TestFixFollowUpRunsOnDraft(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	pr := openPR(ref, sha1)
	pr.Draft = true
	h.gh.setPR(pr)
	h.setThreads(ref, h.findingThread("f1", "a.go", 3, "Missing return", false))
	h.ws.set(func(ws *fakeWorkspaces) { ws.dirty = true })
	h.fix.fn = fixAnswer(review.FixResult{Fixed: []string{"f1"}, CommitMessage: "fix: x"})
	h.command(ref, "/fix", "c-1")
	h.drain()
	if revs := h.rev.requests(); len(revs) != 1 || revs[0].Commit != sha1 {
		t.Fatalf("reviews %+v", revs)
	}
}

func TestFixForkRefused(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	pr := openPR(ref, sha1)
	pr.Fork = true
	h.gh.setPR(pr)
	h.setThreads(ref, h.findingThread("f1", "a.go", 3, "Missing return", false))
	h.command(ref, "/fix", "c-1")
	if len(h.fix.requests()) != 0 {
		t.Fatal("fix ran on a fork PR")
	}
	if _, _, _, acquired, _ := h.ws.fixLists(); acquired != 0 {
		t.Fatal("checkout acquired for a fork PR")
	}
	notes := h.gh.noteList()
	if len(notes) != 1 || notes[0] != state.RenderFixCannotPush("feature", reasonFork, h.loc) {
		t.Fatalf("notes %q", notes)
	}
	if r := h.gh.reactionList(); !slices.Equal(r, []string{"c-1:failure"}) {
		t.Fatalf("reactions %v", r)
	}
}

func TestFixPushSpecRefusesFork(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	h.setThreads(ref, h.findingThread("f1", "a.go", 3, "Missing return", false))
	h.gh.setErr("PushSpec", forge.ErrFork)
	h.command(ref, "/fix", "c-1")
	if notes := h.gh.noteList(); len(notes) != 1 || !strings.Contains(notes[0], reasonFork) {
		t.Fatalf("notes %q", notes)
	}
}

func TestFixPushRejected(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	h.setThreads(ref, h.findingThread("f1", "a.go", 3, "Missing return", false))
	h.ws.set(func(ws *fakeWorkspaces) {
		ws.dirty = true
		ws.pushErr = errors.New("git push: exit status 1: remote: Permission to acme/app.git denied to ocr[bot].")
	})
	h.fix.fn = fixAnswer(review.FixResult{Fixed: []string{"f1"}, CommitMessage: "fix: x"})
	h.command(ref, "/fix", "c-1")
	h.drain()
	notes := h.gh.noteList()
	want := state.RenderFixCannotPush("feature", "remote: Permission to acme/app.git denied to ocr[bot].", h.loc)
	if len(notes) != 1 || notes[0] != want {
		t.Fatalf("notes %q\nwant %q", notes, want)
	}
	if _, resolved, _ := h.gh.fixLists(); len(resolved) != 0 {
		t.Fatal("threads resolved after a rejected push")
	}
	if len(h.rev.requests()) != 0 {
		t.Fatal("review ran after a rejected push")
	}
	if r := h.gh.reactionList(); !slices.Equal(r, []string{"c-1:failure"}) {
		t.Fatalf("reactions %v", r)
	}
}

func TestFixNothingOpen(t *testing.T) {
	h := newHarness(t)
	ref := glRef(1)
	h.gl.setPR(openPR(ref, sha1))
	h.setThreads(ref, h.findingThread("f0", "a.go", 3, "Old", true))
	h.command(ref, "/fix", "n-1")
	if len(h.fix.requests()) != 0 {
		t.Fatal("fix ran without open threads")
	}
	if notes := h.gl.noteList(); len(notes) != 1 || notes[0] != state.RenderFixNothing(h.loc) {
		t.Fatalf("notes %q", notes)
	}
	if r := h.gl.reactionList(); !slices.Equal(r, []string{"n-1:success"}) {
		t.Fatalf("reactions %v", r)
	}
}

func TestFixNoChanges(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	h.setThreads(ref, h.findingThread("f1", "a.go", 3, "Missing return", false))
	h.fix.fn = fixAnswer(review.FixResult{Skipped: []review.FixSkip{{ID: "f1", Reason: "Needs a product decision."}},
		Summary: "Nothing safe to change."})
	h.command(ref, "/fix", "c-1")
	h.drain()
	notes := h.gh.noteList()
	if len(notes) != 1 {
		t.Fatalf("notes %q", notes)
	}
	mustContain(t, notes[0], h.loc.Text("fix.no_changes"))
	mustContain(t, notes[0], "Nothing safe to change.")
	mustContain(t, notes[0], "Missing return (`a.go` L3): Needs a product decision.")
	replies, resolved, pushSpecs := h.gh.fixLists()
	_, _, pushed, _, _ := h.ws.fixLists()
	if len(replies) != 0 || len(resolved) != 0 || len(pushed) != 0 || pushSpecs != 1 || len(h.rev.requests()) != 0 {
		t.Fatalf("replies %v resolved %v pushed %v specs %d", replies, resolved, pushed, pushSpecs)
	}
}

func TestFixUnavailableWithoutFixer(t *testing.T) {
	h := newHarness(t)
	h.srv.fixer = nil
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	h.command(ref, "/fix", "c-1")
	if notes := h.gh.noteList(); len(notes) != 1 || notes[0] != state.RenderCommandError(reasonFixUnavailable, h.loc) {
		t.Fatalf("notes %q", notes)
	}
}

func TestFixFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*harness)
		stage string
	}{
		{"read", func(h *harness) { h.gh.setErr("GetPR", errors.New("boom")) }, "read"},
		{"threads", func(h *harness) { h.gh.setErr("ListOCRThreads", errors.New("boom")) }, "read"},
		{"push spec", func(h *harness) { h.gh.setErr("PushSpec", errors.New("boom")) }, "checkout"},
		{"workspace", func(h *harness) { h.ws.set(func(ws *fakeWorkspaces) { ws.acquireErr = errors.New("boom") }) }, "workspace"},
		{"fetch", func(h *harness) { h.ws.set(func(ws *fakeWorkspaces) { ws.branchErr = errors.New("boom") }) }, "checkout"},
		{"fix", func(h *harness) {
			h.fix.fn = func(context.Context, review.FixRequest) (review.FixResult, error) {
				return review.FixResult{}, errors.New("boom")
			}
		}, "fix"},
		{"identity", func(h *harness) { h.gh.setErr("CommitIdentity", errors.New("boom")) }, "commit"},
		{"commit", func(h *harness) { h.ws.set(func(ws *fakeWorkspaces) { ws.commitErr = errors.New("config changed") }) }, "commit"},
		{"fresh push spec", func(h *harness) {
			h.ws.set(func(ws *fakeWorkspaces) { ws.dirty = true })
			h.fix.fn = func(context.Context, review.FixRequest) (review.FixResult, error) {
				h.gh.setErr("PushSpec", errors.New("boom"))
				return review.FixResult{}, nil
			}
		}, "push"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ref := ghRef(1)
			h.gh.setPR(openPR(ref, sha1))
			h.setThreads(ref, h.findingThread("f1", "a.go", 3, "Missing return", false))
			tc.setup(h)
			h.command(ref, "/fix", "c-1")
			h.drain()
			want := state.RenderCommandError(h.srv.fixFailureReason(tc.stage, "req-cmd"), h.loc)
			if notes := h.gh.noteList(); len(notes) != 1 || notes[0] != want {
				t.Fatalf("notes %q\nwant %q", notes, want)
			}
			if _, _, _, acquired, released := h.ws.fixLists(); acquired != released {
				t.Fatalf("slot not released: %d/%d", acquired, released)
			}
		})
	}
}

func TestFixClosedPR(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	pr := openPR(ref, sha1)
	pr.State = forge.StateMerged
	h.gh.setPR(pr)
	h.command(ref, "/fix", "c-1")
	if notes := h.gh.noteList(); len(notes) != 1 || notes[0] != state.RenderCommandError("pr not open", h.loc) {
		t.Fatalf("notes %q", notes)
	}
}

// TestFixHeadMovedRunsAgain asks the queue to run the fix again when a push
// landed while the branch was fetched.
func TestFixHeadMovedRunsAgain(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	h.setThreads(ref, h.findingThread("f1", "a.go", 3, "Missing return", false))
	h.ws.set(func(ws *fakeWorkspaces) { ws.branchErr = errors.New("head moved") })
	h.gh.mu.Lock()
	h.gh.headAfter[ref.Key()] = sha2
	h.gh.mu.Unlock()
	if !h.command(ref, "/fix", "c-1") {
		t.Fatal("head move did not ask for a rerun")
	}
	if len(h.gh.noteList()) != 0 {
		t.Fatalf("notes %q", h.gh.noteList())
	}
}

// TestFixTimeoutReleasesTheSlot stops a fix that runs past the limit and
// answers the command.
func TestFixTimeoutReleasesTheSlot(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.ReviewTimeout = 50 * time.Millisecond })
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	h.setThreads(ref, h.findingThread("f1", "a.go", 3, "Missing return", false))
	h.fix.fn = func(ctx context.Context, _ review.FixRequest) (review.FixResult, error) {
		<-ctx.Done()
		return review.FixResult{}, ctx.Err()
	}
	h.command(ref, "/fix", "c-1")
	if _, _, _, acquired, released := h.ws.fixLists(); acquired != 1 || released != 1 {
		t.Fatalf("slot %d/%d", acquired, released)
	}
	if len(h.srv.workers) != 0 {
		t.Fatal("worker slot held")
	}
	mustContain(t, strings.Join(h.gh.noteList(), "\n"), h.srv.fixFailureReason("fix", "req-cmd"))
}

func TestPushReason(t *testing.T) {
	if got := pushReason(errors.New("git push: exit status 1: ! [rejected] feature -> feature (fetch first)")); got != "! [rejected] feature -> feature (fetch first)" {
		t.Fatalf("got %q", got)
	}
	if got := pushReason(errors.New("dial tcp: timeout")); got != "dial tcp: timeout" {
		t.Fatalf("got %q", got)
	}
}

// TestReviewPassesDismissedFindings gives the reviewer the resolved threads
// a person replied to, and not the ones resolved without a word.
func TestReviewPassesDismissedFindings(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	h.setThreads(ref,
		h.findingThread("f1", "a.go", 3, "Missing return", true,
			forge.Reply{Author: "alice", Body: " Intended. "}, forge.Reply{Author: "bob", Body: "Agreed."}),
		h.findingThread("f2", "b.go", 4, "Fixed one", true),
		h.findingThread("f3", "c.go", 5, "Still open", false, forge.Reply{Author: "alice", Body: "Working on it."}))
	h.command(ref, "/review", "c-1")
	reqs := h.rev.requests()
	if len(reqs) != 1 || len(reqs[0].Dismissed) != 1 {
		t.Fatalf("requests %+v", reqs)
	}
	want := review.Dismissed{Path: "a.go", Line: 3, Title: "Missing return", Body: "Explain Missing return.",
		Reason: "alice: Intended.\n\nbob: Agreed."}
	if reqs[0].Dismissed[0] != want {
		t.Fatalf("dismissed %+v", reqs[0].Dismissed[0])
	}
}
