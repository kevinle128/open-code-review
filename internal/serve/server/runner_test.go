// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

func TestRoundScenarioGitLab(t *testing.T) {
	h := newHarness(t)
	ref := glRef(1)
	f := h.gl
	f.setPR(openPR(ref, sha1))

	h.rev.set(findingsResult(comment("a.go", 3)))
	h.auto(ref, state.TriggerOpened)
	reqs := h.rev.requests()
	if len(reqs) != 1 || reqs[0].From != shaBase || reqs[0].To != sha1 || reqs[0].RepoDir != "/work/slot-0" {
		t.Fatalf("round one request %+v", reqs)
	}
	m := f.marker(t, ref)
	if m.Auto != 1 || m.Reviewed != sha1 || m.Runs[0].Kind != state.KindFull || len(m.Runs[0].IDs) != 1 {
		t.Fatalf("after round one %+v", m)
	}
	mustContain(t, f.summary(ref), h.loc.Text("summary.open", "count", "1"))
	if got := h.srv.store.List(); len(got) != 1 || got[0].Ref.Key() != ref.Key() {
		t.Fatalf("waiting store %+v", got)
	}

	f.push(ref, sha2)
	h.auto(ref, state.TriggerPush)
	if len(h.rev.requests()) != 1 {
		t.Fatal("push with open threads must not review")
	}
	if len(h.srv.store.List()) != 1 {
		t.Fatal("waiting entry must stay")
	}

	f.resolveAll(ref)
	h.rev.set(findingsResult())
	h.auto(ref, state.TriggerPoll)
	reqs = h.rev.requests()
	if len(reqs) != 2 || reqs[1].From != sha1 || reqs[1].To != sha2 {
		t.Fatalf("round two request %+v", reqs[1])
	}
	m = f.marker(t, ref)
	if m.Auto != 2 || m.Runs[1].Kind != state.KindIncremental {
		t.Fatalf("after round two %+v", m)
	}
	if len(h.srv.store.List()) != 0 {
		t.Fatal("no open threads must clear the waiting entry")
	}

	// A round with no findings lets the next push review at once.
	f.push(ref, sha3)
	h.ws.set(func(w *fakeWorkspaces) { w.ancestry = state.AncestryNo })
	h.rev.set(findingsResult(comment("b.go", 1)))
	h.auto(ref, state.TriggerPush)
	reqs = h.rev.requests()
	if len(reqs) != 3 || reqs[2].From != shaBase {
		t.Fatalf("force push must review in full: %+v", reqs[2])
	}
	m = f.marker(t, ref)
	if m.Auto != 3 || m.Runs[2].Kind != state.KindFull {
		t.Fatalf("force push round %+v", m)
	}

	f.resolveAll(ref)
	f.push(ref, sha4)
	h.auto(ref, state.TriggerPush)
	if len(h.rev.requests()) != 3 {
		t.Fatal("limit reached must not review")
	}
	notes := f.noteList()
	if len(notes) != 1 || notes[0] != state.RenderLimitNote(3, h.loc) {
		t.Fatalf("limit note %q", notes)
	}
	if !f.marker(t, ref).LimitNoted {
		t.Fatal("limit note not recorded")
	}
	f.push(ref, sha5)
	h.auto(ref, state.TriggerPush)
	if len(f.noteList()) != 1 {
		t.Fatal("limit note must be posted once")
	}

	h.command(ref, "/review", "c1")
	h.command(ref, "/review-commit", "c2")
	reqs = h.rev.requests()
	if len(reqs) != 5 || reqs[3].From != shaBase || reqs[3].To != sha5 ||
		reqs[4].Commit != sha5 || reqs[4].From != "" || reqs[4].To != "" {
		t.Fatalf("manual requests %+v", reqs[3:])
	}
	m = f.marker(t, ref)
	if m.Auto != 3 || m.Reviewed != sha5 {
		t.Fatalf("manual runs must keep the count %+v", m)
	}
	mustContain(t, f.summary(ref), h.loc.Text("summary.round", "round", "3", "max", "3"))
	if got := f.reactionList(); strings.Join(got, ",") != "c1:success,c2:success" {
		t.Fatalf("reactions %v", got)
	}
	done := "✅ " + h.loc.Text("command.done", "count", "1")
	if notes := f.noteList(); len(notes) != 3 || notes[1] != done || notes[2] != done {
		t.Fatalf("command replies %q", notes)
	}

	f.mu.Lock()
	pr := f.prs[ref.Key()]
	pr.State = forge.StateMerged
	f.prs[ref.Key()] = pr
	f.mu.Unlock()
	if len(h.srv.store.List()) != 1 {
		t.Fatal("manual findings are open and must be waiting")
	}
	h.auto(ref, state.TriggerPoll)
	if len(h.srv.store.List()) != 0 {
		t.Fatal("merged MR must leave the waiting store")
	}
}

func TestGitHubWaitUsesNoStore(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(1)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(findingsResult(comment("a.go", 1)))
	h.auto(ref, state.TriggerOpened)
	h.gh.push(ref, sha2)
	h.auto(ref, state.TriggerPush)
	if len(h.srv.store.List()) != 0 || len(h.rev.requests()) != 1 {
		t.Fatal("GitHub must not use the waiting store")
	}
	h.gh.resolveAll(ref)
	h.auto(ref, state.TriggerThreadResolved)
	if r := h.rev.requests(); len(r) != 2 || r[1].From != sha1 {
		t.Fatalf("thread resolved must start round two: %+v", r)
	}
}

func TestRunUsesEventRef(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(19)
	pr := openPR(ref, sha1)
	pr.Ref = forge.PRRef{}
	h.gh.prs[ref.Key()] = pr
	h.rev.set(findingsResult(comment("a.go", 1)))
	h.auto(ref, state.TriggerOpened)
	if got := h.gh.postedList(); len(got) != 1 || len(h.gh.threads[ref.Key()]) != 1 {
		t.Fatalf("findings not posted under the event ref: %+v", got)
	}
	if !strings.Contains(h.ws.fetched[0], "file:///acme/app@") {
		t.Fatalf("clone spec %q", h.ws.fetched)
	}
}

func TestRoundOneFailureKeepsInitialMarker(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(2)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{}, errors.New("llm down")
	})
	h.auto(ref, state.TriggerOpened)
	m := h.gh.marker(t, ref)
	if m.Auto != 0 || m.Reviewed != "" {
		t.Fatalf("failed round one must keep the initial marker %+v", m)
	}
	mustContain(t, h.gh.summary(ref), h.loc.Text("pending.failed"))
	if h.ws.released != 1 {
		t.Fatalf("workspace released %d times", h.ws.released)
	}

	h.rev.set(findingsResult())
	h.auto(ref, state.TriggerPush)
	if m := h.gh.marker(t, ref); m.Auto != 1 || len(h.rev.requests()) != 2 {
		t.Fatalf("push after failure must run round one: %+v", m)
	}
	if strings.Contains(h.gh.summary(ref), h.loc.Text("pending.failed")) {
		t.Fatal("success must clear the failure line")
	}
}

func TestReviewErrorKeepsMarkerAndAnswersCommand(t *testing.T) {
	cases := []struct {
		name string
		fn   func(context.Context, review.Request) (review.Result, error)
	}{
		{"error", func(context.Context, review.Request) (review.Result, error) {
			return review.Result{}, errors.New("boom")
		}},
		{"failed terminal", func(context.Context, review.Request) (review.Result, error) {
			return review.Result{Terminal: review.TerminalFailed}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ref := ghRef(3)
			h.gh.setPR(openPR(ref, sha2))
			before := state.Marker{V: 1, Auto: 1, Max: 3, Reviewed: sha1, Base: shaBase}
			h.gh.summaries[ref.Key()] = state.RenderSummary(before, 0, h.loc, "")
			h.rev.set(tc.fn)

			h.auto(ref, state.TriggerPush)
			if m := h.gh.marker(t, ref); m.Auto != 1 || m.Reviewed != sha1 {
				t.Fatalf("marker changed %+v", m)
			}
			h.command(ref, "/review", "c9")
			if m := h.gh.marker(t, ref); m.Reviewed != sha1 {
				t.Fatalf("failed manual run changed marker %+v", m)
			}
			notes := h.gh.noteList()
			if len(notes) != 1 || !strings.HasPrefix(notes[0], "❌") || !strings.Contains(notes[0], "req-cmd") || strings.Contains(notes[0], "boom") {
				t.Fatalf("command failure reply %q", notes)
			}
			if got := h.gh.reactionList(); len(got) != 1 || got[0] != "c9:failure" {
				t.Fatalf("reactions %v", got)
			}
			if strings.Contains(h.gh.summary(ref), h.loc.Text("pending.manual")) {
				t.Fatal("failed manual run left its pending line")
			}
		})
	}
}

func TestHeadMovedDuringFetch(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(4)
	h.gh.setPR(openPR(ref, sha1))
	h.ws.set(func(w *fakeWorkspaces) { w.fetchErr = errors.New("head moved") })
	h.gh.headAfter[ref.Key()] = sha2

	if !h.auto(ref, state.TriggerOpened) {
		t.Fatal("head moved must ask for a new evaluation")
	}
	if len(h.rev.requests()) != 0 {
		t.Fatal("must not review the old head")
	}
	if strings.Contains(h.gh.summary(ref), h.loc.Text("pending.round", "round", "1", "max", "3")) {
		t.Fatal("pending line must be cleared")
	}

	h.ws.set(func(w *fakeWorkspaces) { w.fetchErr = nil })
	if h.auto(ref, state.TriggerOpened) {
		t.Fatal("second run must finish")
	}
	if r := h.rev.requests(); len(r) != 1 || r[0].To != sha2 {
		t.Fatalf("re-evaluation must review the new head: %+v", r)
	}
}

func TestFetchFailureWithSameHead(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(5)
	h.gh.setPR(openPR(ref, sha1))
	h.ws.set(func(w *fakeWorkspaces) { w.fetchErr = errors.New("network") })
	if h.auto(ref, state.TriggerOpened) {
		t.Fatal("a real fetch failure must not loop")
	}
	if !h.logs.has("review run failed") {
		t.Fatal("failure not logged")
	}
}

func TestRepoConfigReadAtBase(t *testing.T) {
	h := newHarness(t)
	ref := glRef(6)
	pr := openPR(ref, sha1)
	h.gl.setPR(pr)
	h.gl.files[shaBase+":.opencodereview/serve.json"] = []byte(`{"max_rounds":1,"effort":"low","background":"ctx","exclude":["gen/**"],"owner":"x"}`)
	h.gl.files[sha1+":.opencodereview/serve.json"] = []byte(`{"max_rounds":9}`)

	h.auto(ref, state.TriggerOpened)
	r := h.rev.requests()[0]
	if r.Effort != "low" || r.Background != "ctx" || len(r.Excludes) != 1 {
		t.Fatalf("repo config not applied: %+v", r)
	}
	if !h.logs.has("repository config keys ignored") {
		t.Fatal("ignored keys not logged")
	}
	h.gl.push(ref, sha2)
	h.auto(ref, state.TriggerPush)
	if len(h.rev.requests()) != 1 || len(h.gl.noteList()) != 1 {
		t.Fatalf("max_rounds=1 from base must stop after one round, notes %q", h.gl.noteList())
	}
	mustContain(t, h.gl.noteList()[0], state.RenderLimitNote(1, h.loc))

	h.command(ref, "/review --effort=high", "c1")
	if r := h.rev.requests(); r[1].Effort != "high" {
		t.Fatalf("command effort must win: %+v", r[1])
	}
}

func TestRepoConfigProblemsFallBack(t *testing.T) {
	t.Run("invalid json", func(t *testing.T) {
		h := newHarness(t)
		ref := ghRef(7)
		h.gh.setPR(openPR(ref, sha1))
		h.gh.files[shaBase+":.opencodereview/serve.json"] = []byte(`{`)
		h.auto(ref, state.TriggerOpened)
		if len(h.rev.requests()) != 1 || !h.logs.has("repository config invalid; using defaults") {
			t.Fatal("invalid config must fall back to defaults")
		}
	})
	t.Run("read error", func(t *testing.T) {
		h := newHarness(t)
		ref := ghRef(7)
		h.gh.setPR(openPR(ref, sha1))
		h.gh.setErr("ReadFile", errors.New("503"))
		h.auto(ref, state.TriggerOpened)
		if len(h.rev.requests()) != 1 || !h.logs.has("read repository config") {
			t.Fatal("read error must fall back to defaults")
		}
	})
	t.Run("disabled", func(t *testing.T) {
		h := newHarness(t)
		ref := ghRef(7)
		h.gh.setPR(openPR(ref, sha1))
		h.gh.files[shaBase+":.opencodereview/serve.json"] = []byte(`{"enabled":false}`)
		h.auto(ref, state.TriggerOpened)
		if len(h.rev.requests()) != 0 {
			t.Fatal("disabled repo must not review")
		}
	})
}

func TestManualReviewBeforeRoundOne(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(8)
	pr := openPR(ref, sha1)
	pr.Draft = true
	h.gh.setPR(pr)
	h.command(ref, "/review", "c1")
	h.gh.resolveAll(ref)

	pr.Draft = false
	h.gh.setPR(pr)
	h.auto(ref, state.TriggerReady)
	if len(h.rev.requests()) != 1 {
		t.Fatal("head already reviewed by /review must not run round one")
	}

	h.gh.push(ref, sha2)
	h.auto(ref, state.TriggerPush)
	r := h.rev.requests()
	if len(r) != 2 || r[1].From != sha1 || r[1].To != sha2 {
		t.Fatalf("round one must start at the manual review: %+v", r)
	}
	if m := h.gh.marker(t, ref); m.Auto != 1 || m.Runs[1].Kind != state.KindIncremental {
		t.Fatalf("marker %+v", m)
	}
}

func TestSkippedReviewConsumesNoRound(t *testing.T) {
	skipped := func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Terminal: review.TerminalSkipped}, nil
	}

	t.Run("round one", func(t *testing.T) {
		h := newHarness(t)
		ref := glRef(20)
		h.gl.setPR(openPR(ref, sha1))
		h.rev.set(skipped)
		h.auto(ref, state.TriggerOpened)
		m := h.gl.marker(t, ref)
		if m.Auto != 0 || m.Reviewed != sha1 || m.Base != shaBase || len(m.Runs) != 0 {
			t.Fatalf("skipped round one must not count: %+v", m)
		}
		mustContain(t, h.gl.summary(ref), h.loc.Text("summary.skipped"))
		if len(h.srv.store.List()) != 0 {
			t.Fatal("a skipped review leaves nothing to wait for")
		}

		h.auto(ref, state.TriggerPush)
		if len(h.rev.requests()) != 1 {
			t.Fatal("the skipped head must not be reviewed again")
		}

		h.gl.push(ref, sha2)
		h.rev.set(findingsResult(comment("a.go", 1)))
		h.auto(ref, state.TriggerPush)
		r := h.rev.requests()
		if len(r) != 2 || r[1].From != sha1 || r[1].To != sha2 {
			t.Fatalf("round one must start after the skipped commits: %+v", r)
		}
		m = h.gl.marker(t, ref)
		if m.Auto != 1 || m.Reviewed != sha2 || len(m.Runs) != 1 || m.Runs[0].N != 1 {
			t.Fatalf("after round one %+v", m)
		}
		if strings.Contains(h.gl.summary(ref), h.loc.Text("summary.skipped")) {
			t.Fatal("a counted round must clear the skipped line")
		}
	})

	t.Run("later round", func(t *testing.T) {
		h := newHarness(t)
		ref := ghRef(21)
		h.gh.setPR(openPR(ref, sha2))
		before := state.Marker{V: 1, Auto: 2, Max: 3, Reviewed: sha1, Base: shaBase,
			Runs: []state.Run{{N: 2, Kind: state.KindFull, From: shaBase, To: sha1, IDs: []string{}}}}
		h.gh.summaries[ref.Key()] = state.RenderSummary(before, 0, h.loc, "")
		h.rev.set(skipped)
		h.auto(ref, state.TriggerPush)
		m := h.gh.marker(t, ref)
		if m.Auto != 2 || m.Reviewed != sha2 || len(m.Runs) != 1 {
			t.Fatalf("skipped round three must not count: %+v", m)
		}
		mustContain(t, h.gh.summary(ref), h.loc.Text("summary.round", "round", "2", "max", "3"))

		h.gh.push(ref, sha3)
		h.rev.set(findingsResult())
		h.auto(ref, state.TriggerPush)
		if m := h.gh.marker(t, ref); m.Auto != 3 || m.Runs[1].From != sha2 {
			t.Fatalf("round three must still run after the skipped one: %+v", m)
		}
	})

	t.Run("manual command", func(t *testing.T) {
		h := newHarness(t)
		ref := ghRef(22)
		h.gh.setPR(openPR(ref, sha1))
		h.rev.set(skipped)
		h.command(ref, "/review", "c1")
		m := h.gh.marker(t, ref)
		if m.Auto != 0 || m.Reviewed != sha1 {
			t.Fatalf("marker %+v", m)
		}
		mustContain(t, h.gh.summary(ref), h.loc.Text("summary.skipped"))
		if got := h.gh.reactionList(); len(got) != 1 || got[0] != "c1:success" {
			t.Fatalf("reactions %v", got)
		}
	})
}

func TestUnreadableMarker(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(9)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.summaries[ref.Key()] = forge.SummaryTag + "\n<!-- ocr-serve:v1 !!! -->"
	h.auto(ref, state.TriggerPush)
	if len(h.rev.requests()) != 0 || !h.logs.has("summary marker unreadable; automatic rounds stop until a manual review rewrites it") {
		t.Fatal("damaged marker must block automatic rounds")
	}
	h.gh.summaries[ref.Key()] = forge.SummaryTag
	h.auto(ref, state.TriggerPush)
	if len(h.rev.requests()) != 0 || !h.logs.has("summary has no marker; automatic rounds stop until a manual review rewrites it") {
		t.Fatal("summary without marker must block automatic rounds")
	}
	h.command(ref, "/review", "c1")
	if m := h.gh.marker(t, ref); m.Reviewed != sha1 || m.Auto != 0 {
		t.Fatalf("manual review must rewrite a valid marker %+v", m)
	}
}

func TestDuplicateFindingIDsGetSuffix(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(10)
	h.gh.setPR(openPR(ref, sha1))
	same := comment("a.go", 4)
	other := same
	other.Content = "second wording"
	h.rev.set(findingsResult(same, other, same, comment("b.go", 1)))
	h.auto(ref, state.TriggerOpened)

	posted := h.gh.postedList()[0]
	ids := map[string]bool{}
	for _, fd := range posted {
		ids[fd.ID] = true
	}
	if len(ids) != 4 {
		t.Fatalf("ids not distinct: %+v", posted)
	}
	if posted[1].ID != posted[0].ID+"-2" || posted[2].ID != posted[0].ID+"-3" {
		t.Fatalf("suffixes %q %q %q", posted[0].ID, posted[1].ID, posted[2].ID)
	}
	if got, ok := forge.ParseFindingID(forge.FindingTag(posted[1].ID)); !ok || got != posted[1].ID {
		t.Fatalf("suffixed id does not survive the tag: %q", got)
	}
	if m := h.gh.marker(t, ref); len(m.Runs[0].IDs) != 4 || m.Runs[0].IDs[1] != posted[1].ID {
		t.Fatalf("marker ids %+v", m.Runs[0].IDs)
	}
	mustContain(t, h.gh.rendered[0], "**["+h.loc.Text("category.bug")+" · "+h.loc.Text("severity.high")+"]**")
}

func TestCommandEdgeCases(t *testing.T) {
	t.Run("closed pr", func(t *testing.T) {
		h := newHarness(t)
		ref := ghRef(11)
		pr := openPR(ref, sha1)
		pr.State = forge.StateClosed
		h.gh.setPR(pr)
		h.command(ref, "/review", "c1")
		if notes := h.gh.noteList(); len(notes) != 1 || !strings.Contains(notes[0], "pr not open") {
			t.Fatalf("closed PR reply %q", notes)
		}
	})
	t.Run("rejected override", func(t *testing.T) {
		h := newHarness(t)
		ref := ghRef(11)
		h.srv.process(ref, job{cmd: &cmdJob{commentID: "c2", reject: "unsupported option"}, requestID: "r"})
		if notes := h.gh.noteList(); len(notes) != 1 || !strings.Contains(notes[0], "unsupported option") ||
			!strings.Contains(notes[0], h.loc.Text("command.help")) {
			t.Fatalf("reject reply %q", notes)
		}
		if h.gh.calls.Load() != 2 {
			t.Fatalf("reject must only react and reply, calls %d", h.gh.calls.Load())
		}
	})
	t.Run("platform command has no comment", func(t *testing.T) {
		h := newHarness(t)
		ref := glRef(11)
		h.gl.setPR(openPR(ref, sha1))
		h.srv.process(ref, job{cmd: &cmdJob{Command: Command{Kind: CommandReview}}, requestID: "r"})
		if len(h.rev.requests()) != 1 || len(h.gl.noteList()) != 0 || len(h.gl.reactionList()) != 0 {
			t.Fatal("reviewer assignment must review without reply or reaction")
		}
		h.srv.process(ref, job{cmd: &cmdJob{reject: "x"}, requestID: "r"})
		if len(h.gl.noteList()) != 0 {
			t.Fatal("no reply without a comment")
		}
	})
	t.Run("platform command skips draft", func(t *testing.T) {
		h := newHarness(t)
		ref := glRef(11)
		pr := openPR(ref, sha1)
		pr.Draft = true
		h.gl.setPR(pr)
		h.srv.process(ref, job{cmd: &cmdJob{Command: Command{Kind: CommandReview}}, requestID: "r"})
		if len(h.rev.requests()) != 0 || len(h.gl.noteList()) != 0 {
			t.Fatal("reviewer assignment on a draft must not review")
		}
		h.command(ref, "/review", "c1")
		if len(h.rev.requests()) != 1 {
			t.Fatal("a typed command must still run on a draft")
		}
	})
	t.Run("ignore rule on full pr", func(t *testing.T) {
		h := newHarness(t, func(c *serveconfig.Config) {
			c.Ignore.Authors = []*regexp.Regexp{regexp.MustCompile(`^renovate$`)}
		})
		ref := glRef(12)
		pr := openPR(ref, sha1)
		pr.Author = "renovate"
		h.gl.setPR(pr)
		// The webhook carried no author, so only the runner can apply the rule.
		h.srv.dispatch(forge.Event{Kind: forge.EventOpened, Ref: ref, DeliveryID: "x", PR: &forge.PR{Ref: ref, HeadSHA: sha1, State: forge.StateOpen}}, nil, "r")
		h.drain()
		if len(h.rev.requests()) != 0 {
			t.Fatal("author rule must apply to the PR from GetPR")
		}
	})
	t.Run("read failure answers command", func(t *testing.T) {
		h := newHarness(t)
		ref := ghRef(11)
		h.gh.setPR(openPR(ref, sha1))
		h.gh.setErr("GetPR", errors.New("502"))
		h.command(ref, "/review-commit", "c3")
		if notes := h.gh.noteList(); len(notes) != 1 || !strings.Contains(notes[0], "review failed at read") {
			t.Fatalf("reply %q", notes)
		}
	})
	t.Run("reply errors are logged", func(t *testing.T) {
		h := newHarness(t)
		ref := ghRef(11)
		h.gh.setPR(openPR(ref, sha1))
		h.gh.setErr("React", errors.New("x"))
		h.gh.setErr("PostNote", errors.New("x"))
		h.command(ref, "/review", "c4")
		h.srv.process(ref, job{cmd: &cmdJob{commentID: "c5", reject: "bad"}, requestID: "r"})
		if !h.logs.has("post command reply") || !h.logs.has("reaction failed") {
			t.Fatal("reply failures not logged")
		}
	})
}

func TestReadFailures(t *testing.T) {
	for _, method := range []string{"GetPR", "GetSummary", "ListOCRThreads"} {
		t.Run(method, func(t *testing.T) {
			h := newHarness(t)
			ref := ghRef(12)
			h.gh.setPR(openPR(ref, sha1))
			h.gh.setErr(method, errors.New("down"))
			h.auto(ref, state.TriggerOpened)
			if len(h.rev.requests()) != 0 || !h.logs.has("read pull request state") {
				t.Fatal("read failure must stop the evaluation")
			}
		})
	}
}

func TestRunStageFailures(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(h *harness)
		stage   string
		pending bool
	}{
		{"pending summary", func(h *harness) { h.gh.failOnce("UpsertSummary", errors.New("x")) }, "summary", false},
		{"workspace", func(h *harness) { h.ws.set(func(w *fakeWorkspaces) { w.acquireErr = errors.New("x") }) }, "workspace", true},
		{"clone", func(h *harness) { h.gh.setErr("Clone", errors.New("x")) }, "checkout", true},
		{"post", func(h *harness) { h.gh.setErr("PostFindings", errors.New("x")) }, "post findings", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ref := ghRef(13)
			h.gh.setPR(openPR(ref, sha1))
			tc.setup(h)
			h.command(ref, "/review", "c1")
			if notes := h.gh.noteList(); len(notes) != 1 || !strings.Contains(notes[0], "review failed at "+tc.stage) {
				t.Fatalf("reply %q", notes)
			}
			if tc.pending {
				if m := h.gh.marker(t, ref); m.Reviewed != "" {
					t.Fatalf("marker moved %+v", m)
				}
			}
		})
	}
}

func TestCommitSummaryFailure(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(14)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(findingsResult(comment("a.go", 1)))
	calls := 0
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		calls++
		if calls == 1 {
			h.gh.setErr("UpsertSummary", errors.New("x"))
		}
		return review.Result{Findings: []model.LlmComment{comment("a.go", 1)}, Terminal: review.TerminalComplete}, nil
	})
	h.auto(ref, state.TriggerOpened)
	if !h.logs.has("review run failed") || !h.logs.has("replace pending summary line") {
		t.Fatal("commit failure not logged")
	}
	h.gh.setErr("UpsertSummary", nil)
	h.auto(ref, state.TriggerPush)
	if m := h.gh.marker(t, ref); m.Auto != 1 {
		t.Fatalf("retry must commit round one %+v", m)
	}
	if posted := h.gh.postedList(); len(posted) != 2 || posted[0][0].ID != posted[1][0].ID {
		t.Fatal("retry must reuse the finding id so the forge can skip it")
	}
	if h.gh.threads[ref.Key()][0].Resolved || len(h.gh.threads[ref.Key()]) != 1 {
		t.Fatal("retry must not duplicate the thread")
	}
}

func TestThreadRecountFailureUsesEstimate(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(15)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		h.gh.setErr("ListOCRThreads", errors.New("x"))
		return review.Result{Findings: []model.LlmComment{comment("a.go", 1)}, Terminal: review.TerminalPartial, Warnings: []string{"w"}}, nil
	})
	h.auto(ref, state.TriggerOpened)
	mustContain(t, h.gh.summary(ref), h.loc.Text("summary.open", "count", "1"))
	if !h.logs.has("count open threads after posting; using an estimate") || !h.logs.has("review warnings") {
		t.Fatal("estimate or warnings not logged")
	}
}

func TestNoteLimitFailures(t *testing.T) {
	setup := func(t *testing.T) (*harness, forge.PRRef) {
		h := newHarness(t)
		ref := ghRef(16)
		h.gh.setPR(openPR(ref, sha2))
		h.gh.summaries[ref.Key()] = state.RenderSummary(state.Marker{V: 1, Auto: 3, Max: 3, Reviewed: sha1, Base: shaBase}, 0, h.loc, "")
		return h, ref
	}
	t.Run("post note", func(t *testing.T) {
		h, ref := setup(t)
		h.gh.setErr("PostNote", errors.New("x"))
		h.auto(ref, state.TriggerPush)
		if h.gh.marker(t, ref).LimitNoted || !h.logs.has("post limit note") {
			t.Fatal("failed note must not be recorded")
		}
	})
	t.Run("record", func(t *testing.T) {
		h, ref := setup(t)
		h.gh.setErr("UpsertSummary", errors.New("x"))
		h.auto(ref, state.TriggerPush)
		if !h.logs.has("record limit note") {
			t.Fatal("record failure not logged")
		}
	})
}

func TestWorkerWaitCancelled(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.Workers = 1 })
	ref := ghRef(17)
	h.gh.setPR(openPR(ref, sha1))
	h.srv.workers <- struct{}{}
	h.srv.cancelRun()
	h.command(ref, "/review", "c1")
	<-h.srv.workers
	if !h.logs.has("review run failed") || len(h.gh.noteList()) != 1 {
		t.Fatal("cancelled wait must fail the run and answer")
	}
}

func TestStoreWriteFailureIsLogged(t *testing.T) {
	h := newHarness(t)
	ref := glRef(18)
	h.gl.setPR(openPR(ref, sha1))
	h.rev.set(findingsResult(comment("a.go", 1)))
	h.srv.store.path = t.TempDir() + "/missing/waiting.json"
	h.auto(ref, state.TriggerOpened)
	h.srv.store.entries[ref.Key()] = Waiting{Ref: ref}
	h.gl.resolveAll(ref)
	h.gl.push(ref, sha2)
	h.rev.set(findingsResult())
	h.auto(ref, state.TriggerPush)
	if strings.Count(h.logs.String(), `"msg":"update waiting store"`) != 2 {
		t.Fatalf("store errors not logged:\n%s", h.logs.String())
	}
}
