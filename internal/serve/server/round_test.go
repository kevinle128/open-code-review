// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// chainPR opens a PR whose fixes the bot can commit and push, and moves its
// head to each pushed fix.
func chainPR(h *harness, ref forge.PRRef) {
	h.forgeFor(ref.Platform).setPR(openPR(ref, sha1))
	h.ws.set(func(w *fakeWorkspaces) { w.dirty = true })
	h.followPushes(ref)
}

func lastStatus(f *fakeForge) string {
	st := f.statusList()
	if len(st) == 0 {
		return ""
	}
	return st[len(st)-1]
}

// TestChainRunsThreeFixRoundsAndAReport walks a chain on both forges: round
// 1 reviews the whole PR, rounds 2 and 3 each review the previous fix
// commit, round 4 reviews the round-3 fix and only reports.
func TestChainRunsThreeFixRoundsAndAReport(t *testing.T) {
	for _, ref := range []forge.PRRef{ghRef(1), glRef(1)} {
		t.Run(string(ref.Platform), func(t *testing.T) {
			h := newHarness(t)
			f := h.forgeFor(ref.Platform)
			chainPR(h, ref)
			h.rev.set(findingsResult(comment("a.go", 3)))
			h.opened(ref)
			h.drain()

			reqs := h.rev.requests()
			if len(reqs) != 4 || reqs[0].From != shaBase || reqs[0].To != sha1 || reqs[0].Commit != "" || reqs[0].FixCommit {
				t.Fatalf("reviews %+v", reqs)
			}
			for i, want := range []string{fixSHA(1), fixSHA(2), fixSHA(3)} {
				if r := reqs[i+1]; r.Commit != want || !r.FixCommit || r.From != "" {
					t.Fatalf("round %d review %+v", i+2, r)
				}
			}
			fixes := h.fix.requests()
			if len(fixes) != 4 || fixes[0].ReportOnly || fixes[2].ReportOnly || !fixes[3].ReportOnly ||
				fixes[0].Findings[0].ID != "F1" || len(fixes[0].Files) != 1 || fixes[1].Head != fixSHA(1) || fixes[1].Base != "" {
				t.Fatalf("fix runs %+v", fixes)
			}
			if len(h.ws.pushed) != 3 || h.ws.pushed[0] != "refs/heads/feature@"+fixSHA(1) {
				t.Fatalf("pushed %v", h.ws.pushed)
			}
			notes := f.noteList()
			if len(notes) != 4 {
				t.Fatalf("notes %q", notes)
			}
			mustContain(t, notes[0], "## 🔍 OCR review, round 1/3")
			mustContain(t, notes[0], `commit <a href="https://forge.test/`+ref.Repo+"/commit/"+fixSHA(1)+`"><code>fffffff</code></a>`)
			mustContain(t, notes[0], "<code>go test ./...</code> passed")
			mustContain(t, notes[0], "round 2/3 reviews commit <code>fffffff</code>")
			mustContain(t, notes[0], "(https://forge.test/"+ref.Repo+"/blob/"+fixSHA(1)+"/a.go#L3)")
			mustContain(t, notes[1], "**(Reviewed fix commit `fffffff`)**")
			mustContain(t, notes[2], "round 4 (report only) reviews commit")
			mustContain(t, notes[3], "## 🔍 OCR review, round 4 (report only)")
			mustContain(t, notes[3], "**(Reviewed fix commit `ddddddd`)**")
			mustContain(t, notes[3], h.loc.Text("round.report.limit", "max", "3"))
			mustContain(t, notes[3], "**Failing scenario**: it fails")
			mustContain(t, notes[3], h.loc.Text("round.end_value"))
			m := f.marker(t, ref)
			if m.Status != state.ChainSuccess || len(m.Rounds) != 4 || m.Rounds[0].Fix != fixSHA(1) || m.Rounds[3].Fix != "" || m.Rounds[3].Real != 1 {
				t.Fatalf("marker %+v", m)
			}
			want := "1:pending f:pending f:pending e:pending e:pending d:pending d:pending d:success"
			if got := strings.Join(f.statusList(), " "); got != want {
				t.Fatalf("statuses %s, want %s", got, want)
			}
		})
	}
}

func TestChainEndsWithoutARealFinding(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(2)
	chainPR(h, ref)
	h.rev.set(findingsResult(comment("a.go", 3), comment("b.go", 1)))
	h.fix.set(func(context.Context, review.FixRequest) (review.FixResult, error) {
		return review.FixResult{Tests: review.Tests{Command: "go test ./...", Passed: true}}, nil
	})
	h.opened(ref)
	h.drain()
	if len(h.rev.requests()) != 1 || len(h.ws.pushed) != 0 || len(h.ws.commits) != 0 {
		t.Fatalf("reviews %d pushed %v", len(h.rev.requests()), h.ws.pushed)
	}
	notes := h.gh.noteList()
	if len(notes) != 1 || !strings.Contains(notes[0], "2 issues → 0 real, 2 skipped") || !strings.Contains(notes[0], h.loc.Text("round.end_value")) ||
		strings.Count(notes[0], "➖ "+h.loc.Text("finding.result.dropped")) != 2 {
		t.Fatalf("notes %q", notes)
	}
	if got := lastStatus(h.gh); got != "1:success" {
		t.Fatalf("status %s", got)
	}

	// A review that finds nothing does not start the fix run.
	h2 := newHarness(t)
	chainPR(h2, ref)
	h2.opened(ref)
	h2.drain()
	if len(h2.fix.requests()) != 0 || lastStatus(h2.gh) != "1:success" {
		t.Fatalf("fix runs %d status %s", len(h2.fix.requests()), lastStatus(h2.gh))
	}
}

// TestFailingTestsArePushed pushes a fix whose tests fail and lists the
// failure in the round comment.
func TestFailingTestsArePushed(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(3)
	chainPR(h, ref)
	h.rev.set(func(_ context.Context, req review.Request) (review.Result, error) {
		if req.FixCommit {
			return review.Result{Terminal: review.TerminalComplete}, nil
		}
		return findingsResult(comment("a.go", 3))(context.Background(), req)
	})
	h.fix.set(func(_ context.Context, req review.FixRequest) (review.FixResult, error) {
		res := fixAll(req)
		res.Tests = review.Tests{Command: "go test ./...", OutputTail: "--- FAIL: TestA"}
		return res, nil
	})
	h.opened(ref)
	h.drain()
	if len(h.ws.pushed) != 1 {
		t.Fatalf("pushed %v", h.ws.pushed)
	}
	notes := h.gh.noteList()
	if len(notes) != 2 {
		t.Fatalf("notes %q", notes)
	}
	mustContain(t, notes[0], h.loc.Text("round.tests_failed", "command", "<code>go test ./...</code>"))
	mustContain(t, notes[0], "--- FAIL: TestA")
	mustContain(t, notes[1], h.loc.Text("round.found_zero"))
	if got := lastStatus(h.gh); got != "f:success" {
		t.Fatalf("status %s", got)
	}
}

func TestForkRoundIsReportOnly(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(4)
	pr := openPR(ref, sha1)
	pr.Fork = true
	h.gh.setPR(pr)
	h.rev.set(findingsResult(comment("a.go", 3)))
	h.opened(ref)
	h.drain()
	fixes := h.fix.requests()
	if len(fixes) != 1 || !fixes[0].ReportOnly || len(h.ws.branchFetched) != 0 || len(h.ws.fetched) != 1 || len(h.ws.pushed) != 0 {
		t.Fatalf("fix %+v fetched %v branch %v", fixes, h.ws.fetched, h.ws.branchFetched)
	}
	note := h.gh.noteList()[0]
	mustContain(t, note, h.loc.Text("round.report.fork", "branch", "<code>feature</code>"))
	mustContain(t, note, "**Proposed fix**: change it")
	if got := lastStatus(h.gh); got != "1:success" {
		t.Fatalf("status %s", got)
	}
}

// TestForkFoundAtPushSpec turns a round report-only when the forge refuses
// a push spec for a fork the PR read did not flag.
func TestForkFoundAtPushSpec(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(5)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.setErr("PushSpec", forge.ErrFork)
	h.rev.set(findingsResult(comment("a.go", 3)))
	h.opened(ref)
	h.drain()
	if fixes := h.fix.requests(); len(fixes) != 1 || !fixes[0].ReportOnly {
		t.Fatalf("fix %+v", fixes)
	}
	mustContain(t, h.gh.noteList()[0], "head branch is in a fork")
}

func TestPushRejectedReportsTheFixes(t *testing.T) {
	h := newHarness(t)
	ref := glRef(6)
	chainPR(h, ref)
	h.ws.set(func(w *fakeWorkspaces) {
		w.pushErr = errors.New("git push: exit status 1: ! [remote rejected] feature (protected branch hook declined)")
	})
	h.rev.set(findingsResult(comment("a.go", 3)))
	h.opened(ref)
	h.drain()
	note := h.gl.noteList()[0]
	mustContain(t, note, "The bot cannot push to branch <code>feature</code>: <code>! [remote rejected] feature (protected branch hook declined)</code>")
	mustContain(t, note, "**Proposed fix**: fixed it")
	if strings.Contains(note, "<code>fffffff</code>") || len(h.rev.requests()) != 1 || lastStatus(h.gl) != "1:success" {
		t.Fatalf("note:\n%s\nstatus %s", note, lastStatus(h.gl))
	}
	if m := h.gl.marker(t, ref); m.Rounds[0].Fixed != 0 || m.Rounds[0].Real != 1 || m.Rounds[0].Fix != "" {
		t.Fatalf("marker %+v", m)
	}
}

// roundDiff is a fix commit diff with a hunk in a.go, one in b.go and one
// in c.go.
const roundDiff = `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -3 +3 @@
-func A() {}
+func A() int { return 1 }
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -9 +9 @@
-b := 1
+b := 2
diff --git a/c.go b/c.go
--- a/c.go
+++ b/c.go
@@ -1 +1 @@
-c
+d
`

// fixWithChanges fixes every finding and ties the first one to a.go line 3
// and the second to b.go line 9; the rest claim no hunk.
func fixWithChanges(_ context.Context, req review.FixRequest) (review.FixResult, error) {
	res := fixAll(req)
	ranges := []review.Change{{Path: "a.go", StartLine: 3, EndLine: 3}, {Path: "b.go", StartLine: 9, EndLine: 9}}
	for i := range res.Fixed {
		res.Fixed[i].Scenario = "it fails"
		if i < len(ranges) {
			res.Fixed[i].Changes = []review.Change{ranges[i]}
		}
	}
	return res, nil
}

func TestRoundCommentShowsTheFixDiff(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(30)
	chainPR(h, ref)
	h.ws.set(func(w *fakeWorkspaces) { w.diff = roundDiff })
	h.rev.set(func(_ context.Context, req review.Request) (review.Result, error) {
		if req.FixCommit {
			return review.Result{Terminal: review.TerminalComplete}, nil
		}
		return findingsResult(comment("a.go", 3), comment("b.go", 9), comment("d.go", 1))(context.Background(), req)
	})
	h.fix.set(fixWithChanges)
	h.opened(ref)
	h.drain()
	note := h.gh.noteList()[0]
	mustContain(t, note, "```diff\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3 +3 @@\n-func A() {}\n+func A() int { return 1 }\n```")
	mustContain(t, note, "+b := 2")
	mustContain(t, note, "**"+h.loc.Text("round.other_changes")+"**\n\n```diff\ndiff --git a/c.go b/c.go")
	mustContain(t, note, `<a href="https://forge.test/acme/app/commit/`+fixSHA(1)+`"><code>fffffff</code></a>`)
	if strings.Count(note, "**Fixed**: fixed it") != 3 {
		t.Fatalf("each fixed finding names its fix:\n%s", note)
	}
	if !h.logs.has("fixed findings match no hunk of the fix commit; the round comment shows them as prose") ||
		!strings.Contains(h.logs.String(), `"ids":["F3"]`) {
		t.Fatalf("logs:\n%s", h.logs.String())
	}
}

func TestRoundCommentWithoutTheFixDiff(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(31)
	chainPR(h, ref)
	h.ws.set(func(w *fakeWorkspaces) { w.diffErr = errors.New("git diff: exit status 128") })
	h.rev.set(func(_ context.Context, req review.Request) (review.Result, error) {
		if req.FixCommit {
			return review.Result{Terminal: review.TerminalComplete}, nil
		}
		return findingsResult(comment("a.go", 3))(context.Background(), req)
	})
	h.fix.set(fixWithChanges)
	h.opened(ref)
	h.drain()
	note := h.gh.noteList()[0]
	mustContain(t, note, "**Fixed**: fixed it")
	if strings.Contains(note, "```diff") || !h.logs.has("read the fix commit diff; the round comment shows the fixes as prose") {
		t.Fatalf("note:\n%s\nlogs:\n%s", note, h.logs.String())
	}
	if len(h.ws.pushed) != 1 {
		t.Fatalf("pushed %v", h.ws.pushed)
	}
}

func TestPushRejectedShowsTheUnpushedDiff(t *testing.T) {
	h := newHarness(t)
	ref := glRef(32)
	chainPR(h, ref)
	h.ws.set(func(w *fakeWorkspaces) {
		w.diff = roundDiff
		w.pushErr = errors.New("git push: exit status 1: protected branch")
	})
	h.rev.set(findingsResult(comment("a.go", 3)))
	h.fix.set(fixWithChanges)
	h.opened(ref)
	h.drain()
	note := h.gl.noteList()[0]
	mustContain(t, note, h.loc.Text("round.proposed_diff")+"\n\n```diff\ndiff --git a/a.go b/a.go")
	mustContain(t, note, "**Failing scenario**: it fails")
	if strings.Contains(note, "c.go") || strings.Contains(note, h.loc.Text("round.other_changes")) {
		t.Fatalf("note:\n%s", note)
	}
}

func TestFixWithoutChangesReports(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(7)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(findingsResult(comment("a.go", 3)))
	h.opened(ref)
	h.drain()
	mustContain(t, h.gh.noteList()[0], h.loc.Text("round.report.no-edit"))
	if len(h.ws.pushed) != 0 || lastStatus(h.gh) != "1:success" {
		t.Fatalf("pushed %v", h.ws.pushed)
	}
}

// TestWithoutFixerEveryRoundReports covers the OCR engine, which has no fix
// run: the findings are reported as the review gave them.
func TestWithoutFixerEveryRoundReports(t *testing.T) {
	h := newHarness(t)
	h.srv.fixer = nil
	ref := ghRef(8)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(findingsResult(comment("a.go", 3)))
	h.opened(ref)
	h.drain()
	note := h.gh.noteList()[0]
	mustContain(t, note, "<summary>problem at a.go</summary>")
	mustContain(t, note, "**Problem**: problem at a.go")
	mustContain(t, note, "⚠️ "+h.loc.Text("finding.result.reported"))
	mustContain(t, note, h.loc.Text("round.report.engine"))
	if len(h.ws.branchFetched) != 0 || lastStatus(h.gh) != "1:success" {
		t.Fatal("a report-only round must not check out the branch")
	}
}

// TestUserPushCopiesTheStatus starts no round on a push by a person; the new
// head gets the chain's status.
func TestUserPushCopiesTheStatus(t *testing.T) {
	cases := []struct {
		name, status, want string
	}{
		{"running", state.ChainRunning, "2:pending"},
		{"ended", state.ChainSuccess, "2:success"},
		{"failed", state.ChainFailure, "2:failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ref := glRef(9)
			h.gl.setPR(openPR(ref, sha2))
			h.gl.summaries[ref.Key()] = state.RenderSummary(state.Marker{Status: tc.status, Max: 3,
				Rounds: []state.Round{{N: 2, Scope: state.ScopeCommit, To: sha1}}}, h.loc, "", "")
			if got := h.srv.dispatch(prEvent(forge.EventPush, ref, "p1"), nil, "r"); got != "status copy" {
				t.Fatal(got)
			}
			h.drain()
			if got := strings.Join(h.gl.statusList(), " "); got != tc.want || len(h.rev.requests()) != 0 {
				t.Fatalf("statuses %s reviews %d", got, len(h.rev.requests()))
			}
		})
	}
	h := newHarness(t)
	ref := ghRef(9)
	h.gh.setPR(openPR(ref, sha2))
	h.srv.dispatch(prEvent(forge.EventPush, ref, "p1"), nil, "r")
	h.gh.setErr("GetPR", errTest)
	h.srv.dispatch(prEvent(forge.EventReopened, ref, "p2"), nil, "r")
	h.drain()
	if len(h.gh.statusList()) != 0 || !h.logs.has("read pull request for a status copy") {
		t.Fatal("a PR without a chain gets no status")
	}
}

func TestOpenedWithAChainOnlyCopiesTheStatus(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(10)
	h.gh.setPR(openPR(ref, sha2))
	h.gh.summaries[ref.Key()] = state.RenderSummary(state.Marker{Status: state.ChainSuccess, Max: 3}, h.loc, "", "")
	h.opened(ref)
	if len(h.rev.requests()) != 0 || lastStatus(h.gh) != "2:success" {
		t.Fatalf("reviews %d status %s", len(h.rev.requests()), lastStatus(h.gh))
	}
	// A summary whose marker is unreadable keeps the PR from starting over.
	h.gh.summaries[ref.Key()] = forge.SummaryTag + "\n<!-- ocr-serve:v2 !!! -->"
	h.opened(ref)
	if len(h.rev.requests()) != 0 || !h.logs.has("summary marker unreadable; the next review rewrites it") {
		t.Fatal("unreadable marker started a chain")
	}
}

func TestOpenedDecisions(t *testing.T) {
	cases := map[string]func(*harness, forge.PRRef){
		"draft": func(h *harness, ref forge.PRRef) { pr := openPR(ref, sha1); pr.Draft = true; h.gh.setPR(pr) },
		"closed": func(h *harness, ref forge.PRRef) {
			pr := openPR(ref, sha1)
			pr.State = forge.StateClosed
			h.gh.setPR(pr)
		},
		"disabled": func(h *harness, ref forge.PRRef) {
			h.gh.files[shaBase+":"+serveconfig.RepoConfigPath] = []byte(`{"enabled":false}`)
		},
		"ignored": func(h *harness, ref forge.PRRef) {
			h.cfg.Ignore.Labels = []string{"skip"}
			pr := openPR(ref, sha1)
			pr.Labels = []string{"skip"}
			h.gh.setPR(pr)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ref := ghRef(11)
			h.gh.setPR(openPR(ref, sha1))
			setup(h, ref)
			h.opened(ref)
			if len(h.rev.requests()) != 0 {
				t.Fatal("no chain must start")
			}
			// A PR the service never reviews gets success, so a merge gate
			// on every project does not block it.
			want := 0
			if name == "disabled" || name == "ignored" {
				want = 1
			}
			if st := h.gh.statusList(); len(st) != want || (want == 1 && st[0] != "1:success") {
				t.Fatalf("statuses %v", st)
			}
		})
	}
	h := newHarness(t)
	ref := ghRef(11)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.failOnce("ReadFile", errTest)
	h.opened(ref)
	h.gh.failOnce("GetSummary", errTest)
	h.opened(ref)
	if len(h.rev.requests()) != 0 || !strings.Contains(h.logs.String(), "read repository config") || !h.logs.has("read pull request state") {
		t.Fatalf("read errors:\n%s", h.logs.String())
	}
}

// TestReviewCommandRestartsTheChain runs review after a chain ended: a new
// round 1 on the whole PR, and the marker shows only the new chain.
func TestReviewCommandRestartsTheChain(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(12)
	chainPR(h, ref)
	h.rev.set(func(_ context.Context, req review.Request) (review.Result, error) {
		if req.FixCommit {
			return review.Result{Terminal: review.TerminalComplete}, nil
		}
		return findingsResult(comment("a.go", 3))(context.Background(), req)
	})
	h.opened(ref)
	h.drain()
	if m := h.gh.marker(t, ref); len(m.Rounds) != 2 {
		t.Fatalf("marker %+v", m)
	}
	h.command(ref, "/review --effort=high", "c1")
	h.drain()
	reqs := h.rev.requests()
	if r := reqs[2]; r.From != shaBase || r.To != fixSHA(1) || r.Effort != "high" {
		t.Fatalf("restart review %+v", r)
	}
	if r := reqs[3]; !r.FixCommit || r.Effort != "high" {
		t.Fatalf("the chain keeps the effort: %+v", r)
	}
	if m := h.gh.marker(t, ref); m.Status != state.ChainSuccess || len(m.Rounds) != 2 || m.Rounds[0].From != shaBase {
		t.Fatalf("marker %+v", m)
	}
	if !strings.Contains(strings.Join(h.gh.reactionList(), ","), "c1:success") {
		t.Fatalf("reactions %v", h.gh.reactionList())
	}

	h.rev.set(findingsResult())
	h.command(ref, "/review-commit", "c2")
	h.drain()
	if r := h.rev.requests()[4]; r.Commit != fixSHA(2) || r.FixCommit || r.From != "" {
		t.Fatalf("review-commit %+v", r)
	}
	if m := h.gh.marker(t, ref); len(m.Rounds) != 1 || m.Rounds[0].Scope != state.ScopeCommit {
		t.Fatalf("marker %+v", m)
	}
}

// TestReviewCommandCancelsARunningChain cancels round 2 when a review is
// requested: that round fails quietly and the new chain owns the PR.
func TestReviewCommandCancelsARunningChain(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(13)
	chainPR(h, ref)
	started := make(chan struct{})
	var full atomic.Int32
	h.rev.set(func(ctx context.Context, req review.Request) (review.Result, error) {
		if req.FixCommit {
			close(started)
			<-ctx.Done()
			return review.Result{}, ctx.Err()
		}
		if full.Add(1) == 1 {
			return review.Result{Findings: []model.LlmComment{comment("a.go", 3)}, Terminal: review.TerminalComplete}, nil
		}
		return review.Result{Terminal: review.TerminalComplete}, nil
	})
	h.srv.queue.submit(ref, job{trigger: triggerOpened, requestID: "r-open"})
	<-started
	ev := forge.Event{Kind: forge.EventCommand, Ref: ref, DeliveryID: "c", CommentID: "c1", CommentBody: "/review"}
	if got := h.srv.dispatch(ev, nil, "r-cmd"); got != "queued command review" {
		t.Fatal(got)
	}
	h.drain()
	if len(h.rev.requests()) != 3 || !h.logs.has("round cancelled: a newer review replaced its chain") || h.logs.has("review round failed") {
		t.Fatalf("reviews %d logs:\n%s", len(h.rev.requests()), h.logs.String())
	}
	if st := strings.Join(h.gh.statusList(), " "); strings.Contains(st, "failure") || lastStatus(h.gh) != "f:success" {
		t.Fatalf("statuses %s", st)
	}
	if m := h.gh.marker(t, ref); m.Status != state.ChainSuccess || len(m.Rounds) != 1 {
		t.Fatalf("marker %+v", m)
	}
}

func TestStaleRoundIsSkipped(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(14)
	h.gh.setPR(openPR(ref, sha1))
	gen := h.srv.restartChain(ref)
	h.srv.restartChain(ref)
	h.srv.process(ref, job{round: &roundJob{n: 2, scope: state.ScopeCommit, commit: sha1, gen: gen}, requestID: "r"})
	if len(h.rev.requests()) != 0 || !h.logs.has("round skipped: a newer review replaced its chain") {
		t.Fatal("a round of a replaced chain ran")
	}
	if h.srv.begin(ref, gen, func() {}) {
		t.Fatal("begin accepted a replaced chain")
	}
}

// TestRoundFailureBlocksTheMerge sets failure when a round fails, and a
// later review that succeeds clears it.
func TestRoundFailureBlocksTheMerge(t *testing.T) {
	h := newHarness(t)
	ref := glRef(15)
	chainPR(h, ref)
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Terminal: review.TerminalFailed, Warnings: []string{"a.go: quota"},
			Coverage: review.Coverage{Total: 1, Failed: []review.FailedItem{{Path: "new.go", OldPath: "old.go", Class: "provider"}}}}, nil
	})
	h.command(ref, "/review", "c1")
	if got := lastStatus(h.gl); got != "1:failure" {
		t.Fatalf("status %s", got)
	}
	m := h.gl.marker(t, ref)
	mustContain(t, h.gl.summary(ref), h.loc.Text("pending.failed"))
	if m.Status != state.ChainFailure {
		t.Fatalf("marker %+v", m)
	}
	mustContain(t, strings.Join(h.gl.noteList(), "\n"), h.srv.failureReason("review", "req-cmd"))
	for _, want := range []string{"review coverage incomplete", "old.go -> new.go (provider)", "a.go: quota", "review round failed"} {
		mustContain(t, h.logs.String(), want)
	}

	h.rev.set(findingsResult())
	h.command(ref, "/review", "c2")
	if got := lastStatus(h.gl); got != "1:success" {
		t.Fatalf("status %s", got)
	}
}

// TestChainEndSetsTheStatusOfANewHead covers a person who pushed while the
// last round ran: the new head gets the final status too.
func TestChainEndSetsTheStatusOfANewHead(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(16)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		h.gh.push(ref, sha2)
		return review.Result{Terminal: review.TerminalComplete}, nil
	})
	h.opened(ref)
	if got := strings.Join(h.gh.statusList(), " "); got != "1:pending 1:success 2:success" {
		t.Fatalf("statuses %s", got)
	}
}

func TestRoundStageFailures(t *testing.T) {
	cases := map[string]func(*harness){
		"workspace": func(h *harness) { h.ws.set(func(w *fakeWorkspaces) { w.acquireErr = errTest }) },
		"checkout":  func(h *harness) { h.ws.set(func(w *fakeWorkspaces) { w.branchErr = errTest }) },
		"push spec": func(h *harness) { h.gh.setErr("PushSpec", errTest) },
		"fix": func(h *harness) {
			h.fix.set(func(context.Context, review.FixRequest) (review.FixResult, error) {
				return review.FixResult{Warnings: []string{"w"}}, errTest
			})
		},
		"identity":  func(h *harness) { h.gh.setErr("CommitIdentity", errTest) },
		"commit":    func(h *harness) { h.ws.set(func(w *fakeWorkspaces) { w.commitErr = errTest }) },
		"read rule": func(h *harness) { h.gh.setErr("ReadFile:"+".opencodereview/rule.json", errTest) },
		"fresh push spec": func(h *harness) {
			h.ws.set(func(w *fakeWorkspaces) {
				w.onPush = nil
				w.commitErr = nil
			})
			orig := h.fix.fn
			h.fix.set(func(ctx context.Context, req review.FixRequest) (review.FixResult, error) {
				h.gh.setErr("PushSpec", errTest)
				if orig != nil {
					return orig(ctx, req)
				}
				return fixAll(req), nil
			})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ref := ghRef(17)
			chainPR(h, ref)
			h.rev.set(findingsResult(comment("a.go", 3)))
			setup(h)
			h.command(ref, "/review", "c1")
			if got := lastStatus(h.gh); got != "1:failure" {
				t.Fatalf("status %s\n%s", got, h.logs.String())
			}
			if !h.logs.has("review round failed") || len(h.ws.pushed) != 0 {
				t.Fatal("failure not logged or a fix pushed")
			}
		})
	}
	h := newHarness(t)
	ref := ghRef(17)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.setErr("GetPR", errTest)
	h.command(ref, "/review", "c1")
	if len(h.gh.statusList()) != 0 || !strings.Contains(strings.Join(h.gh.noteList(), ""), "review round failed at read") {
		t.Fatal("a PR that cannot be read gets a reply and no status")
	}
}

// TestForgeWriteFailuresAreLogged keeps the chain going when a comment,
// the summary or a status cannot be written; each failure is logged.
func TestForgeWriteFailuresAreLogged(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(18)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.setErr("PostNote", errTest)
	h.gh.setErr("UpsertSummary", errTest)
	h.gh.setErr("SetStatus", errTest)
	h.opened(ref)
	for _, want := range []string{"post round comment", "write summary", "set commit status"} {
		if !h.logs.has(want) {
			t.Fatalf("%q not logged:\n%s", want, h.logs.String())
		}
	}
	if !h.logs.has("round done") {
		t.Fatal("the round did not finish")
	}
}

func TestRoundOnAClosedPR(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(19)
	pr := openPR(ref, sha1)
	pr.State = forge.StateMerged
	h.gh.setPR(pr)
	h.command(ref, "/review", "c1")
	if len(h.rev.requests()) != 0 || !strings.Contains(strings.Join(h.gh.noteList(), ""), "pr not open") {
		t.Fatal("a closed PR must not be reviewed")
	}
}

func TestHeadMovedDuringFetch(t *testing.T) {
	for _, fork := range []bool{false, true} {
		h := newHarness(t)
		ref := ghRef(20)
		pr := openPR(ref, sha1)
		pr.Fork = fork
		h.gh.setPR(pr)
		h.ws.set(func(w *fakeWorkspaces) { w.fetchErr, w.branchErr = errTest, errTest })
		h.gh.headAfter[ref.Key()] = sha2
		if !h.command(ref, "/review", "c1") {
			t.Fatal("head moved must ask for a new run")
		}
		if len(h.rev.requests()) != 0 {
			t.Fatal("must not review the old head")
		}
		h.ws.set(func(w *fakeWorkspaces) { w.fetchErr, w.branchErr = nil, nil })
		if h.command(ref, "/review", "c1") {
			t.Fatal("second run must finish")
		}
		if r := h.rev.requests(); len(r) != 1 || r[0].To != sha2 {
			t.Fatalf("the new head must be reviewed: %+v", r)
		}
	}
}

func TestRepoConfigReadAtBase(t *testing.T) {
	h := newHarness(t)
	ref := glRef(21)
	chainPR(h, ref)
	h.gl.files[shaBase+":.opencodereview/serve.json"] = []byte(`{"max_rounds":1,"effort":"low","background":"ctx","exclude":["gen/**"],"owner":"x"}`)
	h.gl.files[sha1+":.opencodereview/serve.json"] = []byte(`{"max_rounds":9}`)
	h.rev.set(findingsResult(comment("a.go", 3)))
	h.opened(ref)
	h.drain()
	reqs := h.rev.requests()
	if len(reqs) != 2 || reqs[0].Effort != "low" || reqs[0].Background != "ctx" || len(reqs[0].Excludes) != 1 {
		t.Fatalf("repo config not applied: %+v", reqs)
	}
	if fixes := h.fix.requests(); len(fixes) != 2 || !fixes[1].ReportOnly {
		t.Fatalf("max_rounds=1 must make round 2 report only: %+v", fixes)
	}
	mustContain(t, h.gl.noteList()[1], "round 2 (report only)")
	if !h.logs.has("repository config keys ignored") {
		t.Fatal("ignored keys not logged")
	}
}

func TestProjectRuleReadAtBase(t *testing.T) {
	h := newHarness(t)
	ref := glRef(22)
	h.gl.setPR(openPR(ref, sha1))
	h.gl.files[shaBase+":.opencodereview/rule.json"] = []byte(`{"rules":[{"path":"**","rule":"docs/r.md"}],"exclude":["gen/**"]}`)
	h.gl.files[shaBase+":docs/r.md"] = []byte("base rule\n")
	h.gl.files[sha1+":.opencodereview/rule.json"] = []byte(`{"exclude":["**"]}`)
	h.gl.setErr("ReadFile:docs/missing.md", errTest)
	h.opened(ref)
	pr := h.rev.requests()[0].ProjectRule
	if pr == nil || len(pr.Exclude) != 1 || pr.Exclude[0] != "gen/**" || pr.Rules[0].Rule != "base rule" {
		t.Fatalf("project rule %+v", pr)
	}

	h2 := newHarness(t)
	h2.gl.setPR(openPR(ref, sha1))
	h2.gl.files[shaBase+":.opencodereview/rule.json"] = []byte(`{"rules":[{"path":"**","rule":"docs/r.md"}]}`)
	h2.gl.setErr("ReadFile:docs/r.md", errTest)
	h2.opened(ref)
	if r := h2.rev.requests(); len(r) != 1 || r[0].ProjectRule == nil || r[0].ProjectRule.Rules[0].Rule != "" || !h2.logs.has("read rule file failed; its rule is left empty") {
		t.Fatalf("an unreadable rule file must blank only its entry: %+v", r)
	}

	h3 := newHarness(t)
	h3.gl.setPR(openPR(ref, sha1))
	h3.gl.files[shaBase+":.opencodereview/rule.json"] = []byte(`{`)
	h3.opened(ref)
	if r := h3.rev.requests(); len(r) != 1 || r[0].ProjectRule != nil || !h3.logs.has("project rules invalid; using built-in rules only") {
		t.Fatalf("an invalid rule.json must review with built-in rules: %+v", r)
	}
}

func TestServiceExcludesThenRepoExcludes(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.Exclude = []string{"**/*.html", "gen/**"} })
	ref := ghRef(23)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.files[shaBase+":.opencodereview/serve.json"] = []byte(`{"exclude":["**/*_test.go"]}`)
	h.opened(ref)
	if got := h.rev.requests()[0].Excludes; strings.Join(got, ",") != "**/*.html,gen/**,**/*_test.go" {
		t.Fatalf("excludes = %v", got)
	}
}

// TestRoundCommentShowsMergeBaseAndCoverage shows the merge base the review
// diffed against, a partial review, and a review with nothing to review.
func TestRoundCommentShowsMergeBaseAndCoverage(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(24)
	h.gh.setPR(openPR(ref, sha1))
	cov := review.Coverage{Total: 3, Reviewed: 2, Failed: []review.FailedItem{{Path: "b.go", Class: "timeout", Reason: strings.Repeat("x", 400)}}}
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Terminal: review.TerminalPartial, Base: strings.Repeat("b", 40), Coverage: cov}, nil
	})
	h.opened(ref)
	note := h.gh.noteList()[0]
	mustContain(t, note, "**(Reviewed the whole PR: `bbbbbbb...1111111`)**")
	mustContain(t, note, state.RenderCoverage(cov, h.loc))
	if m := h.gh.marker(t, ref); m.Rounds[0].From != strings.Repeat("b", 40) {
		t.Fatalf("marker %+v", m)
	}
	if strings.Contains(h.logs.String(), strings.Repeat("x", 301)) {
		t.Fatal("failure reason not truncated in the log")
	}

	h2 := newHarness(t)
	h2.gh.setPR(openPR(ref, sha1))
	h2.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Terminal: review.TerminalSkipped}, nil
	})
	h2.opened(ref)
	mustContain(t, h2.gh.noteList()[0], h2.loc.Text("round.skipped"))
	if lastStatus(h2.gh) != "1:success" {
		t.Fatal("a round with nothing to review ends the chain")
	}
}

func TestWorkerWaitCancelled(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.Workers = 1; c.DataDir = "" })
	ref := ghRef(25)
	h.gh.setPR(openPR(ref, sha1))
	h.srv.workers <- struct{}{}
	h.srv.cancelRun()
	h.command(ref, "/review", "c1")
	<-h.srv.workers
	if !h.logs.has("review round failed") || len(h.gh.noteList()) != 1 {
		t.Fatal("cancelled wait must fail the round and answer")
	}
}

func TestPushReason(t *testing.T) {
	err := errors.New("git push: exit status 1: ! [rejected]   main -> main (fetch first)")
	if got := pushReason(err); got != "! [rejected] main -> main (fetch first)" {
		t.Fatalf("reason %q", got)
	}
	if got := pushReason(errors.New(strings.Repeat("y", 400))); len(got) != maxPushReason+3 {
		t.Fatalf("reason length %d", len(got))
	}
}

// TestFixHasItsOwnTimeLimit gives the fix run its own limit: a review that
// uses up most of its limit leaves the fix the full fix limit, and a fix
// that runs past that limit fails the round.
func TestFixHasItsOwnTimeLimit(t *testing.T) {
	const reviewLimit = 300 * time.Millisecond
	h := newHarness(t, func(c *serveconfig.Config) { c.ReviewTimeout, c.FixTimeout = reviewLimit, 400*time.Millisecond })
	ref := ghRef(31)
	chainPR(h, ref)
	h.rev.set(func(ctx context.Context, _ review.Request) (review.Result, error) {
		time.Sleep(reviewLimit - 100*time.Millisecond)
		return findingsResult(comment("a.go", 3))(ctx, review.Request{})
	})
	var left time.Duration
	h.fix.set(func(ctx context.Context, _ review.FixRequest) (review.FixResult, error) {
		if d, ok := ctx.Deadline(); ok {
			left = time.Until(d)
		}
		return review.FixResult{}, nil
	})
	h.opened(ref)
	h.drain()
	if left < 300*time.Millisecond {
		t.Fatalf("fix got %v of its 400ms limit", left)
	}

	h2 := newHarness(t, func(c *serveconfig.Config) { c.FixTimeout = 50 * time.Millisecond })
	chainPR(h2, ref)
	h2.rev.set(findingsResult(comment("a.go", 3)))
	h2.fix.set(func(ctx context.Context, _ review.FixRequest) (review.FixResult, error) {
		<-ctx.Done()
		return review.FixResult{}, ctx.Err()
	})
	h2.opened(ref)
	h2.drain()
	if got := lastStatus(h2.gh); got != "1:failure" {
		t.Fatalf("status %s", got)
	}
}

// TestPushOnIgnoredPRMarksNotRequired: a push to a PR that the service never
// reviews gives the new head success, so the merge gate does not block it.
func TestPushOnIgnoredPRMarksNotRequired(t *testing.T) {
	h := newHarness(t)
	h.cfg.Ignore.Labels = []string{"skip"}
	ref := ghRef(31)
	pr := openPR(ref, sha2)
	pr.Labels = []string{"skip"}
	h.gh.setPR(pr)
	h.srv.copyStatusAsync(ref, "r-push")
	h.drain()
	if got := lastStatus(h.gh); got != "2:success" {
		t.Fatalf("status %q", got)
	}

	h2 := newHarness(t)
	ref2 := ghRef(32)
	h2.gh.setPR(openPR(ref2, sha2))
	h2.srv.copyStatusAsync(ref2, "r-push")
	h2.drain()
	if n := len(h2.gh.statusList()); n != 0 {
		t.Fatalf("a reviewed PR without a chain got %d statuses", n)
	}
}

// TestPlatformLanguageAndLocale gives each platform its own output language
// in the engine requests and its own locale in the posted text.
func TestPlatformLanguageAndLocale(t *testing.T) {
	h := newHarness(t)
	vi, err := state.LoadLocale("vi")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Deps{Config: h.cfg, Forges: map[forge.Platform]forge.Forge{forge.GitHub: h.gh, forge.GitLab: h.gl},
		Reviewer: h.rev, Fixer: h.fix, Workspaces: h.ws, Logger: NewLogger(h.logs), Now: func() time.Time { return h.clock },
		Locales:   map[forge.Platform]state.Locale{forge.GitHub: h.loc, forge.GitLab: vi},
		Languages: map[forge.Platform]string{forge.GitHub: "English", forge.GitLab: "Vietnamese"}})
	if err != nil {
		t.Fatal(err)
	}
	old := h.srv
	h.srv = srv
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	_ = old.Shutdown(context.Background())
	h.rev.set(findingsResult(comment("a.go", 3), comment("b.go", 4)))
	h.fix.set(func(_ context.Context, req review.FixRequest) (review.FixResult, error) {
		return review.FixResult{Reported: []review.Reported{{ID: "F1", Path: "a.go", Line: 3, Title: "t", Problem: "p"}},
			Dropped: []review.Dropped{{ID: "F2", Reason: "not real"}}}, nil
	})
	gh, gl := ghRef(40), glRef(41)
	for _, ref := range []forge.PRRef{gh, gl} {
		pr := openPR(ref, sha1)
		pr.Fork = true
		h.forgeFor(ref.Platform).setPR(pr)
		h.opened(ref)
	}
	h.drain()
	langs := map[string]bool{}
	for _, r := range h.rev.requests() {
		langs[r.Language] = true
	}
	fixes := h.fix.requests()
	if !langs["English"] || !langs["Vietnamese"] || len(fixes) != 2 || fixes[0].Language == fixes[1].Language {
		t.Fatalf("languages %v fixes %+v", langs, fixes)
	}
	for _, x := range fixes {
		if want := map[forge.Platform]string{forge.GitHub: "English", forge.GitLab: "Vietnamese"}[x.PR.Ref.Platform]; x.Language != want {
			t.Fatalf("%s fix language %q", x.PR.Ref.Platform, x.Language)
		}
	}
	ghNote, glNote := h.gh.noteList()[0], h.gl.noteList()[0]
	mustContain(t, ghNote, "<strong>"+h.loc.Text("round.found")+"</strong>")
	mustContain(t, ghNote, "**"+h.loc.Text("finding.why_skipped")+"**: not real")
	mustContain(t, glNote, "<strong>"+vi.Text("round.found")+"</strong>")
	mustContain(t, glNote, "**"+vi.Text("finding.why_skipped")+"**: not real")
	mustContain(t, glNote, vi.Text("round.report.fork", "branch", "<code>feature</code>"))
	mustContain(t, h.gl.summary(gl), vi.Text("summary.title"))
	if strings.Contains(glNote, h.loc.Text("finding.why_skipped")) {
		t.Fatalf("English text in a GitLab note:\n%s", glNote)
	}
}
