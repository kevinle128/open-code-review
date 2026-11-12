// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func openPR(head string) forge.PR {
	return forge.PR{State: forge.StateOpen, HeadSHA: head, BaseSHA: shaA}
}

func repo(maxRounds int) serveconfig.RepoConfig {
	return serveconfig.RepoConfig{Enabled: true, MaxRounds: maxRounds}
}

func markerAt(auto int, reviewed string, limitNoted bool) *Marker {
	m := Marker{V: 1, Auto: auto, Max: 3, Reviewed: reviewed, Base: shaA, LimitNoted: limitNoted}
	return &m
}

func TestDecide(t *testing.T) {
	initial := Initial(3)
	draft := openPR(shaC)
	draft.Draft = true
	closed := openPR(shaC)
	closed.State = forge.StateClosed
	merged := openPR(shaC)
	merged.State = forge.StateMerged
	disabled := repo(3)
	disabled.Enabled = false

	cases := []struct {
		name string
		in   Input
		want Action
	}{
		{"closed pr", Input{Trigger: TriggerPush, PR: closed, Repo: repo(3)}, Action{Kind: None}},
		{"merged pr ignores command", Input{Trigger: TriggerCmdReview, PR: merged, Repo: repo(3)}, Action{Kind: None}},
		{"manual review on open pr", Input{Trigger: TriggerCmdReview, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(3, shaB, true), Repo: repo(3)},
			Action{Kind: RunManualFull, Mode: ModeFull}},
		{"manual review on draft", Input{Trigger: TriggerCmdReview, PR: draft, Repo: repo(3)}, Action{Kind: RunManualFull, Mode: ModeFull}},
		{"manual review with unreadable marker", Input{Trigger: TriggerCmdReview, PR: openPR(shaC), SummaryPresent: true, MarkerErr: errors.New("x"), Repo: repo(3)},
			Action{Kind: RunManualFull, Mode: ModeFull}},
		{"manual commit on ignored pr", Input{Trigger: TriggerCmdReviewCommit, PR: openPR(shaC), Ignored: true, Repo: repo(3)},
			Action{Kind: RunManualCommit, Mode: ModeCommit}},
		{"manual commit on disabled repo", Input{Trigger: TriggerCmdReviewCommit, PR: openPR(shaC), Repo: disabled},
			Action{Kind: RunManualCommit, Mode: ModeCommit}},
		{"draft push", Input{Trigger: TriggerPush, PR: draft, Repo: repo(3)}, Action{Kind: None}},
		{"repo disabled", Input{Trigger: TriggerOpened, PR: openPR(shaC), Repo: disabled}, Action{Kind: None}},
		{"ignore rule", Input{Trigger: TriggerOpened, PR: openPR(shaC), Ignored: true, Repo: repo(3)}, Action{Kind: None}},
		{"two markers block auto", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, MarkerErr: errors.New("found 2"), Repo: repo(3)}, Action{Kind: None}},
		{"summary without marker blocks auto", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Repo: repo(3)}, Action{Kind: None}},
		{"opened starts round one", Input{Trigger: TriggerOpened, PR: openPR(shaC), Repo: repo(3)}, Action{Kind: RunAuto, Round: 1, Mode: ModeFull}},
		{"ready starts round one", Input{Trigger: TriggerReady, PR: openPR(shaC), Repo: repo(3)}, Action{Kind: RunAuto, Round: 1, Mode: ModeFull}},
		{"reopened starts round one", Input{Trigger: TriggerReopened, PR: openPR(shaC), Repo: repo(3)}, Action{Kind: RunAuto, Round: 1, Mode: ModeFull}},
		{"no marker and push starts round one", Input{Trigger: TriggerPush, PR: openPR(shaC), Repo: repo(3)}, Action{Kind: RunAuto, Round: 1, Mode: ModeFull}},
		{"initial marker after failed round one and push", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: &initial, Repo: repo(3)},
			Action{Kind: RunAuto, Round: 1, Mode: ModeFull}},
		{"manual review before round one and head not moved", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(0, shaC, false), Repo: repo(3)},
			Action{Kind: None}},
		{"manual review before round one and push", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(0, shaB, false), Repo: repo(3)},
			Action{Kind: RunAuto, Round: 1, Mode: ModeIncremental, From: shaB}},
		{"manual review before round one and thread resolved", Input{Trigger: TriggerThreadResolved, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(0, shaB, false), Repo: repo(3)},
			Action{Kind: RunAuto, Round: 1, Mode: ModeIncremental, From: shaB}},
		{"manual review before round one and poll", Input{Trigger: TriggerPoll, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(0, shaB, false), Repo: repo(3)},
			Action{Kind: RunAuto, Round: 1, Mode: ModeIncremental, From: shaB}},
		{"manual review before round one with open threads waits", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(0, shaB, false), OpenThreads: 1, Repo: repo(3)},
			Action{Kind: Wait}},
		{"no marker and note", Input{Trigger: TriggerNote, PR: openPR(shaC), Repo: repo(3)}, Action{Kind: None}},
		{"no marker and thread resolved", Input{Trigger: TriggerThreadResolved, PR: openPR(shaC), Repo: repo(3)}, Action{Kind: None}},
		{"no marker and poll", Input{Trigger: TriggerPoll, PR: openPR(shaC), SummaryPresent: true, Marker: &initial, Repo: repo(3)}, Action{Kind: None}},
		{"head already reviewed", Input{Trigger: TriggerThreadResolved, PR: openPR(shaB), SummaryPresent: true, Marker: markerAt(1, shaB, false), Repo: repo(3)}, Action{Kind: None}},
		{"push with open threads waits", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(1, shaB, false), OpenThreads: 2, Repo: repo(3)}, Action{Kind: Wait}},
		{"open threads beat the limit", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(3, shaB, false), OpenThreads: 1, Repo: repo(3)}, Action{Kind: Wait}},
		{"limit reached notes once", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(3, shaB, false), Repo: repo(3)}, Action{Kind: NoteLimit}},
		{"limit noted twice is quiet", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(3, shaB, true), Repo: repo(3)}, Action{Kind: None}},
		{"repo limit is authoritative", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(3, shaB, true), Repo: repo(5)},
			Action{Kind: RunAuto, Round: 4, Mode: ModeIncremental, From: shaB}},
		{"resolved threads start next round", Input{Trigger: TriggerThreadResolved, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(1, shaB, false), Repo: repo(3)},
			Action{Kind: RunAuto, Round: 2, Mode: ModeIncremental, From: shaB}},
		{"poll starts next round", Input{Trigger: TriggerPoll, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(2, shaB, false), Repo: repo(3)},
			Action{Kind: RunAuto, Round: 3, Mode: ModeIncremental, From: shaB}},
		{"counted round without reviewed sha runs full", Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: markerAt(1, "", false), Repo: repo(3)},
			Action{Kind: RunAuto, Round: 2, Mode: ModeFull}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.in)
			if got.Reason == "" {
				t.Fatal("empty reason")
			}
			got.Reason = ""
			if got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestActionKindString(t *testing.T) {
	want := map[ActionKind]string{None: "none", Wait: "wait", RunAuto: "run-auto", RunManualFull: "run-manual-full",
		RunManualCommit: "run-manual-commit", NoteLimit: "note-limit", ActionKind(99): "none"}
	for k, s := range want {
		if k.String() != s {
			t.Fatalf("%d: %q", k, k.String())
		}
	}
}

func TestResolveRange(t *testing.T) {
	inc := Action{Kind: RunAuto, Round: 2, Mode: ModeIncremental, From: shaB}
	cases := []struct {
		name               string
		a                  Action
		ancestry           int
		from, to, wantKind string
	}{
		{"incremental kept", inc, AncestryYes, shaB, shaC, KindIncremental},
		{"rewritten history", inc, AncestryNo, shaA, shaC, KindFull},
		{"old sha missing", inc, AncestryMissing, shaA, shaC, KindFull},
		{"git failed", inc, AncestryError, shaA, shaC, KindFull},
		{"round one full", Action{Kind: RunAuto, Round: 1, Mode: ModeFull}, AncestryYes, shaA, shaC, KindFull},
		{"manual full", Action{Kind: RunManualFull, Mode: ModeFull}, AncestryNo, shaA, shaC, KindManualFull},
		{"manual commit", Action{Kind: RunManualCommit, Mode: ModeCommit}, AncestryYes, "", shaC, KindManualCommit},
		{"wait runs nothing", Action{Kind: Wait}, AncestryYes, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, kind := ResolveRange(tc.a, shaA, shaC, tc.ancestry)
			if from != tc.from || to != tc.to || kind != tc.wantKind {
				t.Fatalf("got %q %q %q", from, to, kind)
			}
		})
	}
}

func TestApply(t *testing.T) {
	inc := Action{Kind: RunAuto, Round: 2, Mode: ModeIncremental, From: shaB}

	t.Run("round one from nothing", func(t *testing.T) {
		got := Apply(nil, Action{Kind: RunAuto, Round: 1, Mode: ModeFull}, shaA, shaB, shaA, []string{"r1-x"}, 3)
		want := Marker{V: 1, Auto: 1, Max: 3, Reviewed: shaB, Base: shaA,
			Runs: []Run{{N: 1, Kind: KindFull, From: shaA, To: shaB, IDs: []string{"r1-x"}}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("incremental round", func(t *testing.T) {
		m := markerAt(1, shaB, true)
		got := Apply(m, inc, shaB, shaC, shaA, nil, 4)
		if got.Auto != 2 || got.Reviewed != shaC || got.Max != 4 || got.LimitNoted ||
			!reflect.DeepEqual(got.Runs, []Run{{N: 2, Kind: KindIncremental, From: shaB, To: shaC, IDs: []string{}}}) {
			t.Fatalf("got %+v", got)
		}
		if m.Auto != 1 || m.Runs != nil {
			t.Fatalf("input mutated: %+v", m)
		}
	})

	t.Run("round one after manual review", func(t *testing.T) {
		first := Action{Kind: RunAuto, Round: 1, Mode: ModeIncremental, From: shaB}
		got := Apply(markerAt(0, shaB, false), first, shaB, shaC, shaA, nil, 3)
		if got.Auto != 1 || got.Reviewed != shaC || got.Runs[0].Kind != KindIncremental {
			t.Fatalf("got %+v", got)
		}
		from, _, kind := ResolveRange(first, shaA, shaC, AncestryNo)
		if got := Apply(markerAt(0, shaB, false), first, from, shaC, shaA, nil, 3); kind != KindFull || got.Runs[0].Kind != KindFull || got.Auto != 1 {
			t.Fatalf("rewritten history: kind %q marker %+v", kind, got)
		}
	})

	t.Run("incremental upgraded to full", func(t *testing.T) {
		got := Apply(markerAt(1, shaB, false), inc, shaA, shaC, shaA, nil, 3)
		if got.Runs[0].Kind != KindFull || got.Auto != 2 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("manual full moves reviewed and keeps count", func(t *testing.T) {
		got := Apply(markerAt(3, shaB, true), Action{Kind: RunManualFull, Mode: ModeFull}, shaD, shaC, shaD, []string{"m-1"}, 3)
		if got.Auto != 3 || got.Reviewed != shaC || got.Base != shaD || !got.LimitNoted ||
			!reflect.DeepEqual(got.Runs, []Run{{Kind: KindManualFull, From: shaD, To: shaC, IDs: []string{"m-1"}}}) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("manual commit changes neither count nor reviewed", func(t *testing.T) {
		got := Apply(markerAt(2, shaB, false), Action{Kind: RunManualCommit, Mode: ModeCommit}, "", shaC, shaD, []string{"m-2"}, 3)
		if got.Auto != 2 || got.Reviewed != shaB || got.Base != shaA || len(got.Runs) != 1 || got.Runs[0].Kind != KindManualCommit {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("limit note", func(t *testing.T) {
		got := Apply(markerAt(3, shaB, false), Action{Kind: NoteLimit}, "", "", "", nil, 3)
		if !got.LimitNoted || got.Auto != 3 || len(got.Runs) != 0 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("run history capped", func(t *testing.T) {
		m := markerAt(1, shaB, false)
		for i := range 35 {
			m.Runs = append(m.Runs, Run{N: 0, Kind: KindManualCommit, To: shaB, IDs: []string{strings.Repeat("x", i+1)}})
		}
		got := Apply(m, inc, shaB, shaC, shaA, nil, 3)
		if len(got.Runs) != 30 || got.Runs[29].N != 2 || len(got.Runs[0].IDs[0]) != 7 {
			t.Fatalf("len=%d last=%+v first=%+v", len(got.Runs), got.Runs[29], got.Runs[0])
		}
		if _, err := Decode(Encode(got)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestApplySkipped(t *testing.T) {
	t.Run("before any round", func(t *testing.T) {
		got := ApplySkipped(nil, shaB, shaA, 3)
		want := Marker{V: 1, Auto: 0, Max: 3, Reviewed: shaB, Base: shaA, Runs: []Run{}}
		if got.Auto != want.Auto || got.Reviewed != want.Reviewed || got.Base != want.Base || got.Max != 3 || len(got.Runs) != 0 {
			t.Fatalf("got %+v", got)
		}
		// The next push goes through the round-one gate and reviews only
		// the commits after the skipped ones.
		a := Decide(Input{Trigger: TriggerPush, PR: openPR(shaC), SummaryPresent: true, Marker: &got, Repo: repo(3)})
		if a.Kind != RunAuto || a.Round != 1 || a.Mode != ModeIncremental || a.From != shaB {
			t.Fatalf("next action %+v", a)
		}
	})

	t.Run("keeps count, runs and limit note", func(t *testing.T) {
		m := markerAt(2, shaB, true)
		m.Runs = []Run{{N: 2, Kind: KindIncremental, From: shaA, To: shaB, IDs: []string{"r2-x"}}}
		got := ApplySkipped(m, shaC, shaD, 4)
		if got.Auto != 2 || got.Reviewed != shaC || got.Base != shaD || got.Max != 4 || !got.LimitNoted ||
			!reflect.DeepEqual(got.Runs, m.Runs) {
			t.Fatalf("got %+v", got)
		}
		got.Runs[0].N = 9
		if m.Runs[0].N != 2 || m.Reviewed != shaB {
			t.Fatalf("input mutated: %+v", m)
		}
		if _, err := Decode(Encode(got)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFindingID(t *testing.T) {
	f := forge.Finding{Path: "a.go", StartLine: 3, EndLine: 5, Category: "bug", Content: "one wording"}
	g := f
	g.Content = "another wording"
	now := time.Unix(1700000000, 0)
	auto := Action{Kind: RunAuto, Round: 2}

	id := FindingID(auto, shaC, f, now)
	if !strings.HasPrefix(id, "r2-cccccccc-") || len(id) != len("r2-cccccccc-")+8 {
		t.Fatalf("auto id %q", id)
	}
	if FindingID(auto, shaC, g, now.Add(time.Hour)) != id {
		t.Fatal("auto id must not depend on wording or time")
	}
	h := f
	h.EndLine = 6
	if FindingID(auto, shaC, h, now) == id {
		t.Fatal("different span must change the id")
	}

	manual := FindingID(Action{Kind: RunManualCommit}, shaC, f, now)
	if !strings.HasPrefix(manual, "m-cccccccc-1700000000-") || !strings.HasSuffix(manual, id[len(id)-8:]) {
		t.Fatalf("manual id %q", manual)
	}
	if got, ok := forge.ParseFindingID(forge.FindingTag(manual)); !ok || got != manual {
		t.Fatalf("tag round trip %q", got)
	}
	if short := FindingID(auto, "abc", f, now); !strings.HasPrefix(short, "r2-abc-") {
		t.Fatalf("short sha %q", short)
	}
}
