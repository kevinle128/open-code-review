// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

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

func TestRunPostsPendingLineBeforeReviewWithRunDeadline(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.ReviewTimeout = serveconfig.DefaultReviewTimeout })
	ref := ghRef(5)
	h.gh.setPR(openPR(ref, sha1))
	var sawPending bool
	var left time.Duration
	h.rev.set(func(ctx context.Context, _ review.Request) (review.Result, error) {
		sawPending = strings.Contains(h.gh.summary(ref), h.loc.Text("pending.round", "round", "1", "max", "3"))
		if dl, ok := ctx.Deadline(); ok {
			left = time.Until(dl)
		}
		return review.Result{Terminal: review.TerminalComplete}, nil
	})

	h.auto(ref, state.TriggerOpened)
	if !sawPending {
		t.Fatal("the in-progress line must be in the summary before the review starts")
	}
	if want := serveconfig.DefaultReviewTimeout; left <= want-time.Minute || left > want {
		t.Fatalf("review deadline in %v, want about %v", left, want)
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

func TestServiceExcludesThenRepoExcludes(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.Exclude = []string{"**/*.html", "gen/**"} })
	ref := ghRef(33)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.files[shaBase+":.opencodereview/serve.json"] = []byte(`{"exclude":["**/*_test.go"]}`)
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Terminal: review.TerminalComplete,
			Coverage: review.Coverage{Total: 2, Reviewed: 2, Skipped: map[string]int{"user_exclude": 3, "large_data": 1}}}, nil
	})
	h.auto(ref, state.TriggerOpened)
	if got := h.rev.requests()[0].Excludes; strings.Join(got, ",") != "**/*.html,gen/**,**/*_test.go" {
		t.Fatalf("excludes = %v", got)
	}
	if !strings.Contains(h.logs.String(), `"skipped_files":{"large_data":1,"user_exclude":3}`) {
		t.Fatalf("skipped counts not logged:\n%s", h.logs.String())
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

func TestRepoConfigReadErrorStopsEvaluation(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(7)
	h.gh.setPR(openPR(ref, sha1))
	h.gh.files[shaBase+":"+serveconfig.RepoConfigPath] = []byte(`{"enabled":false}`)
	h.gh.failOnce("ReadFile", errors.New("503"))
	h.auto(ref, state.TriggerOpened)
	if len(h.rev.requests()) != 0 || h.gh.summary(ref) != "" || len(h.gh.noteList()) != 0 {
		t.Fatal("a repository config that cannot be read must stop the evaluation without posting")
	}
	if !h.logs.has("read pull request state") || !strings.Contains(h.logs.String(), "read repository config") {
		t.Fatalf("read error not logged:\n%s", h.logs.String())
	}

	h.auto(ref, state.TriggerOpened)
	if len(h.rev.requests()) != 0 {
		t.Fatal("the next trigger must read the config and keep the repository disabled")
	}
}

func TestProjectRuleReadAtBase(t *testing.T) {
	setup := func(t *testing.T) (*harness, forge.PRRef) {
		h := newHarness(t)
		ref := glRef(30)
		h.gl.setPR(openPR(ref, sha1))
		h.gl.files[shaBase+":.opencodereview/rule.json"] = []byte(`{"rules":[{"path":"**","rule":"docs/r.md"}],"exclude":["gen/**"]}`)
		h.gl.files[shaBase+":docs/r.md"] = []byte("base rule\n")
		h.gl.files[sha1+":.opencodereview/rule.json"] = []byte(`{"exclude":["**"]}`)
		h.gl.files[sha1+":docs/r.md"] = []byte("report nothing")
		return h, ref
	}
	t.Run("base", func(t *testing.T) {
		h, ref := setup(t)
		h.auto(ref, state.TriggerOpened)
		pr := h.rev.requests()[0].ProjectRule
		if pr == nil || len(pr.Exclude) != 1 || pr.Exclude[0] != "gen/**" || pr.Rules[0].Rule != "base rule" {
			t.Fatalf("project rule %+v", pr)
		}
	})
	t.Run("missing", func(t *testing.T) {
		h, ref := setup(t)
		delete(h.gl.files, shaBase+":.opencodereview/rule.json")
		h.auto(ref, state.TriggerOpened)
		if r := h.rev.requests(); len(r) != 1 || r[0].ProjectRule != nil || h.logs.has("read project rules failed; using built-in rules only") {
			t.Fatalf("no base rule file must mean no project rules: %+v", r)
		}
	})
	t.Run("rule file read error", func(t *testing.T) {
		h, ref := setup(t)
		h.gl.setErr("ReadFile:docs/r.md", errors.New("503"))
		h.auto(ref, state.TriggerOpened)
		r := h.rev.requests()
		if len(r) != 1 || r[0].ProjectRule == nil || r[0].ProjectRule.Rules[0].Rule != "" || len(r[0].ProjectRule.Exclude) != 1 {
			t.Fatalf("an unreadable rule file must blank only its entry: %+v", r)
		}
		if !h.logs.has("read rule file failed; its rule is left empty") {
			t.Fatal("blank entry not logged")
		}
	})
	t.Run("rule file sizes", func(t *testing.T) {
		h, ref := setup(t)
		h.gl.files[shaBase+":.opencodereview/rule.json"] = []byte(`{"rules":[{"path":"a/**","rule":"docs/r.md"},{"path":"b/**","rule":"docs/huge.md"}],"exclude":["gen/**"]}`)
		h.gl.files[shaBase+":docs/r.md"] = []byte(strings.Repeat("r", serveconfig.MaxRepoConfigBytes+1))
		h.gl.files[shaBase+":docs/huge.md"] = []byte(strings.Repeat("h", 512<<10+1))
		h.auto(ref, state.TriggerOpened)
		pr := h.rev.requests()[0].ProjectRule
		if pr == nil || len(pr.Rules[0].Rule) != serveconfig.MaxRepoConfigBytes+1 || pr.Rules[1].Rule != "" || len(pr.Exclude) != 1 {
			t.Fatalf("rule files up to the CLI limit must load, larger ones blank only their entry: %+v", pr != nil)
		}
	})
	t.Run("invalid rule.json", func(t *testing.T) {
		h, ref := setup(t)
		h.gl.files[shaBase+":.opencodereview/rule.json"] = []byte(`{`)
		h.auto(ref, state.TriggerOpened)
		if r := h.rev.requests(); len(r) != 1 || r[0].ProjectRule != nil || !h.logs.has("project rules invalid; using built-in rules only") {
			t.Fatalf("an invalid rule.json must review with built-in rules: %+v", r)
		}
	})
	t.Run("rule.json read error", func(t *testing.T) {
		h, ref := setup(t)
		if err := h.srv.store.Put(Waiting{Ref: ref}); err != nil {
			t.Fatal(err)
		}
		h.gl.setErr("ReadFile:.opencodereview/rule.json", errors.New("503"))
		h.auto(ref, state.TriggerOpened)
		if len(h.rev.requests()) != 0 || h.gl.summary(ref) != "" || len(h.gl.noteList()) != 0 {
			t.Fatal("rules that cannot be read must stop the run without posting")
		}
		if !h.logs.has("read project rules") || len(h.srv.store.List()) != 1 {
			t.Fatal("the error must be logged and the next trigger must retry")
		}
		h.gl.setErr("ReadFile:.opencodereview/rule.json", nil)
		h.auto(ref, state.TriggerOpened)
		if len(h.rev.requests()) != 1 {
			t.Fatal("the next trigger must review")
		}
	})
}

func TestMarkerKeepsOnlyNeededFindingIDs(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(31)
	h.gh.setPR(openPR(ref, sha1))
	cs := make([]model.LlmComment, 100)
	for i := range cs {
		cs[i] = comment("a.go", i+1)
	}
	h.rev.set(findingsResult(cs...))
	var firstRun []string
	for i := range 40 {
		h.clock = h.clock.Add(time.Minute)
		h.command(ref, "/review", "c")
		if i == 0 {
			firstRun = h.gh.marker(t, ref).Runs[0].IDs
			// One thread of the first run stays open; the rest are resolved.
			h.gh.mu.Lock()
			for j := range h.gh.threads[ref.Key()] {
				h.gh.threads[ref.Key()][j].Resolved = j != 0
			}
			h.gh.mu.Unlock()
			continue
		}
		h.gh.mu.Lock()
		for j := range h.gh.threads[ref.Key()] {
			h.gh.threads[ref.Key()][j].Resolved = h.gh.threads[ref.Key()][j].FindingID != firstRun[0]
		}
		h.gh.mu.Unlock()
	}
	if n := len(h.gh.summary(ref)); n > 65536/2 {
		t.Fatalf("summary is %d characters; GitHub refuses comments over 65536", n)
	}
	m := h.gh.marker(t, ref)
	kept := 0
	for i, r := range m.Runs {
		if i >= len(m.Runs)-5 {
			if len(r.IDs) != 100 {
				t.Fatalf("run %d shown in the summary lost ids: %d", i, len(r.IDs))
			}
			continue
		}
		kept += len(r.IDs)
	}
	if kept != 0 {
		t.Fatalf("older runs kept %d ids of resolved threads", kept)
	}

	h.gh.summaries[ref.Key()] = state.RenderSummary(state.Marker{V: 1, Max: 3, Runs: []state.Run{
		{Kind: state.KindManualFull, From: shaBase, To: sha1, IDs: []string{firstRun[0], "gone"}},
	}}, 1, h.loc, "")
	h.command(ref, "/review", "c")
	if ids := h.gh.marker(t, ref).Runs[0].IDs; len(ids) != 2 {
		t.Fatalf("runs within the newest five keep every id: %v", ids)
	}
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

func TestPartialRunReportsCoverage(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(30)
	h.gh.setPR(openPR(ref, sha1))
	cov := review.Coverage{Total: 3, Reviewed: 2, Failed: []review.FailedItem{
		{Path: "b.go", Class: "timeout", Reason: "LLM request timed out " + strings.Repeat("x", 400)},
	}}
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Findings: []model.LlmComment{comment("a.go", 1)}, Terminal: review.TerminalPartial, Coverage: cov}, nil
	})
	h.command(ref, "/review", "c1")

	mustContain(t, h.gh.summary(ref), state.RenderCoverage(cov, h.loc))
	logs := h.logs.String()
	line := ""
	for l := range strings.Lines(logs) {
		if strings.Contains(l, "review coverage incomplete") {
			line = l
		}
	}
	for _, want := range []string{`"level":"WARN"`, `"files":3`, `"reviewed_files":2`, `"failed_files":1`, `b.go (timeout): LLM request timed out`} {
		if !strings.Contains(line, want) {
			t.Fatalf("coverage line missing %q:\n%s", want, logs)
		}
	}
	if strings.Contains(line, strings.Repeat("x", 300)) {
		t.Fatalf("reason not truncated:\n%s", line)
	}
	if !strings.Contains(logs, `"reviewed_files":2`) || !h.logs.has("review done") {
		t.Fatalf("review done line lacks coverage:\n%s", logs)
	}

	h.gh.push(ref, sha2)
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Terminal: review.TerminalComplete, Coverage: review.Coverage{Total: 3, Reviewed: 3}}, nil
	})
	h.command(ref, "/review", "c2")
	sum := h.gh.summary(ref)
	if strings.Contains(sum, "b.go") {
		t.Fatalf("a complete run must clear the partial list:\n%s", sum)
	}
	mustContain(t, sum, h.loc.Text("coverage.complete", "reviewed", "3", "total", "3"))
}

func TestFailedRunLogsCoverageAndWarnings(t *testing.T) {
	h := newHarness(t)
	ref := ghRef(31)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Terminal: review.TerminalFailed, Warnings: []string{"a.go: context deadline exceeded"},
			Coverage: review.Coverage{Total: 2, Failed: []review.FailedItem{
				{Path: "a.go", Class: "provider", Reason: "provider or subtask request failed"},
				{Path: "new.go", OldPath: "old.go", Class: "timeout"},
			}}}, nil
	})
	h.auto(ref, state.TriggerOpened)
	logs := h.logs.String()
	for _, want := range []string{"review coverage incomplete", "a.go (provider)", "old.go -> new.go (timeout)", "context deadline exceeded", "review run failed"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing %q:\n%s", want, logs)
		}
	}
}

func TestFullRunRangeShowsMergeBase(t *testing.T) {
	mergeBase := strings.Repeat("b", 40)
	withBase := func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Terminal: review.TerminalComplete, Base: mergeBase}, nil
	}

	h := newHarness(t)
	ref := ghRef(32)
	h.gh.setPR(openPR(ref, sha1))
	h.rev.set(withBase)
	h.auto(ref, state.TriggerOpened)
	m := h.gh.marker(t, ref)
	if m.Runs[0].Kind != state.KindFull || m.Runs[0].From != mergeBase || m.Base != shaBase {
		t.Fatalf("full round %+v", m)
	}
	mustContain(t, h.gh.summary(ref), "`bbbbbbb...1111111`")

	h.gh.push(ref, sha2)
	h.auto(ref, state.TriggerPush)
	if m := h.gh.marker(t, ref); m.Runs[1].Kind != state.KindIncremental || m.Runs[1].From != sha1 {
		t.Fatalf("incremental round must keep its range: %+v", m)
	}

	h.command(ref, "/review", "c1")
	if m := h.gh.marker(t, ref); m.Runs[2].Kind != state.KindManualFull || m.Runs[2].From != mergeBase {
		t.Fatalf("manual full review %+v", m)
	}
}
