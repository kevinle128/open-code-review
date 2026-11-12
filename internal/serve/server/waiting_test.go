// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

func TestFailedAutoRunStopsPollingUntilNextTrigger(t *testing.T) {
	h := newHarness(t)
	ref := glRef(40)
	h.gl.setPR(openPR(ref, sha1))
	h.rev.set(findingsResult(comment("a.go", 1)))
	h.auto(ref, state.TriggerOpened)
	h.gl.push(ref, sha2)
	h.auto(ref, state.TriggerPush)
	if len(h.srv.store.List()) != 1 {
		t.Fatal("open threads must keep the merge request waiting")
	}

	h.gl.resolveAll(ref)
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		return review.Result{}, errors.New("llm 400")
	})
	h.auto(ref, state.TriggerPoll)
	if n := len(h.rev.requests()); n != 2 {
		t.Fatalf("resolved threads must start round two, got %d reviews", n)
	}
	if got := h.srv.store.List(); len(got) != 0 {
		t.Fatalf("failed automatic run must leave the waiting store: %+v", got)
	}

	h.srv.pollOnce()
	h.drain()
	if n := len(h.rev.requests()); n != 2 {
		t.Fatalf("the poller must not retry a failed review, got %d reviews", n)
	}

	h.rev.set(findingsResult())
	h.gl.push(ref, sha3)
	h.auto(ref, state.TriggerPush)
	if m := h.gl.marker(t, ref); len(h.rev.requests()) != 3 || m.Auto != 2 {
		t.Fatalf("a new trigger must retry the review: %+v", m)
	}
}

func TestShutdownDuringRunKeepsWaiting(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.Workers = 1 })
	ref := glRef(41)
	h.gl.setPR(openPR(ref, sha2))
	before := state.Marker{V: 1, Auto: 1, Max: 3, Reviewed: sha1, Base: shaBase}
	h.gl.summaries[ref.Key()] = state.RenderSummary(before, 0, h.loc, "")
	if err := h.srv.store.Put(Waiting{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	h.srv.workers <- struct{}{}
	h.srv.cancelRun()
	h.auto(ref, state.TriggerPoll)
	<-h.srv.workers
	if !h.logs.has("review run failed") {
		t.Fatal("cancelled wait must fail the run")
	}
	if len(h.srv.store.List()) != 1 {
		t.Fatal("a run stopped by shutdown must stay waiting for the next start")
	}
}

func TestStoppedMergeRequestLeavesWaiting(t *testing.T) {
	cases := []struct {
		name  string
		tweak func(*serveconfig.Config)
		setup func(h *harness, ref forge.PRRef)
	}{
		{"draft", nil, func(h *harness, ref forge.PRRef) {
			pr := openPR(ref, sha2)
			pr.Draft = true
			h.gl.setPR(pr)
		}},
		{"disabled", nil, func(h *harness, ref forge.PRRef) {
			h.gl.files[shaBase+":"+serveconfig.RepoConfigPath] = []byte(`{"enabled":false}`)
		}},
		{"ignored", func(c *serveconfig.Config) {
			c.Ignore.Authors = []*regexp.Regexp{regexp.MustCompile(`^alice$`)}
		}, nil},
		{"unreadable marker", nil, func(h *harness, ref forge.PRRef) {
			h.gl.summaries[ref.Key()] = forge.SummaryTag + "\n<!-- ocr-serve:v1 !!! -->"
		}},
		{"not found", nil, func(h *harness, ref forge.PRRef) {
			h.gl.setErr("GetPR", fmt.Errorf("gitlab: %w", forge.ErrNotFound))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tweaks []func(*serveconfig.Config)
			if tc.tweak != nil {
				tweaks = append(tweaks, tc.tweak)
			}
			h := newHarness(t, tweaks...)
			ref := glRef(42)
			h.gl.setPR(openPR(ref, sha2))
			before := state.Marker{V: 1, Auto: 1, Max: 3, Reviewed: sha1, Base: shaBase}
			h.gl.summaries[ref.Key()] = state.RenderSummary(before, 1, h.loc, "")
			h.gl.threads[ref.Key()] = []forge.Thread{{ID: "t1", FindingID: "r1-11111111-abcd"}}
			if err := h.srv.store.Put(Waiting{Ref: ref}); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(h, ref)
			}
			h.auto(ref, state.TriggerPoll)
			if len(h.rev.requests()) != 0 {
				t.Fatal("must not review")
			}
			if got := h.srv.store.List(); len(got) != 0 {
				t.Fatalf("waiting entry must go: %+v", got)
			}
		})
	}
}

func TestWaitingKeptWhileThreadsOpen(t *testing.T) {
	h := newHarness(t)
	ref := glRef(43)
	h.gl.setPR(openPR(ref, sha1))
	before := state.Marker{V: 1, Auto: 1, Max: 3, Reviewed: sha1, Base: shaBase}
	h.gl.summaries[ref.Key()] = state.RenderSummary(before, 1, h.loc, "")
	h.gl.threads[ref.Key()] = []forge.Thread{{ID: "t1", FindingID: "r1-11111111-abcd"}}
	if err := h.srv.store.Put(Waiting{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	h.gl.setErr("GetSummary", errors.New("503"))
	h.auto(ref, state.TriggerPoll)
	h.gl.setErr("GetSummary", nil)
	h.auto(ref, state.TriggerPoll)
	if len(h.srv.store.List()) != 1 {
		t.Fatal("a transient read error or a reviewed head with open threads must keep waiting")
	}
}

// glRoundTwo leaves a GitLab merge request whose round-one threads are
// resolved and whose head moved, so the next poll starts round two.
func glRoundTwo(t *testing.T, h *harness, n int) forge.PRRef {
	t.Helper()
	ref := glRef(n)
	h.gl.setPR(openPR(ref, sha1))
	h.rev.set(findingsResult(comment("a.go", 1)))
	h.auto(ref, state.TriggerOpened)
	h.gl.push(ref, sha2)
	h.gl.resolveAll(ref)
	return ref
}

func TestFailedCommitWithPostedThreadsKeepsWaiting(t *testing.T) {
	h := newHarness(t)
	ref := glRoundTwo(t, h, 44)
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		// The pending line is written; the commit write after posting fails.
		h.gl.setErr("UpsertSummary", errors.New("502"))
		return review.Result{Findings: []model.LlmComment{comment("b.go", 2)}, Terminal: review.TerminalComplete}, nil
	})
	h.auto(ref, state.TriggerPoll)
	if m := h.gl.marker(t, ref); m.Auto != 1 {
		t.Fatalf("failed commit must not count: %+v", m)
	}
	if len(h.srv.store.List()) != 1 {
		t.Fatal("posted threads are open, so the merge request must stay waiting")
	}

	h.gl.setErr("UpsertSummary", nil)
	h.gl.resolveAll(ref)
	h.rev.set(findingsResult())
	h.srv.pollOnce()
	h.drain()
	if m := h.gl.marker(t, ref); len(h.rev.requests()) != 3 || m.Auto != 2 {
		t.Fatalf("resolving the threads must rerun the round through polling: %+v", m)
	}
}

func TestFailedPostWithNothingPostedLeavesWaiting(t *testing.T) {
	for _, listFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("list fails %v", listFails), func(t *testing.T) {
			h := newHarness(t)
			ref := glRoundTwo(t, h, 45)
			if err := h.srv.store.Put(Waiting{Ref: ref}); err != nil {
				t.Fatal(err)
			}
			h.rev.set(func(context.Context, review.Request) (review.Result, error) {
				h.gl.setErr("PostFindings", errors.New("400 body rejected"))
				if listFails {
					// One thread stays open, but the recount cannot see it.
					h.gl.mu.Lock()
					h.gl.threads[ref.Key()][0].Resolved = false
					h.gl.mu.Unlock()
					h.gl.setErr("ListOCRThreads", errors.New("503"))
				}
				return review.Result{Findings: []model.LlmComment{comment("b.go", 2)}, Terminal: review.TerminalComplete}, nil
			})
			h.auto(ref, state.TriggerPoll)
			if got := h.srv.store.List(); len(got) != 0 {
				t.Fatalf("no open thread is known, so the merge request must leave the store: %+v", got)
			}
			h.srv.pollOnce()
			h.drain()
			if n := len(h.rev.requests()); n != 2 {
				t.Fatalf("the poller must not rerun the failing review, got %d reviews", n)
			}
		})
	}
}
