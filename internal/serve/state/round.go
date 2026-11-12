// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// Trigger is what caused an evaluation.
type Trigger string

const (
	TriggerOpened          Trigger = "opened"
	TriggerReady           Trigger = "ready"
	TriggerReopened        Trigger = "reopened"
	TriggerPush            Trigger = "push"
	TriggerThreadResolved  Trigger = "thread-resolved"
	TriggerNote            Trigger = "note"
	TriggerPoll            Trigger = "poll"
	TriggerCmdReview       Trigger = "cmd-review"
	TriggerCmdReviewCommit Trigger = "cmd-review-commit"
)

// Input is everything Decide needs. Marker is nil when no summary exists or
// the summary marker could not be read; MarkerErr is the Decode error.
type Input struct {
	Trigger        Trigger
	PR             forge.PR
	SummaryPresent bool
	Marker         *Marker
	MarkerErr      error
	OpenThreads    int
	Repo           serveconfig.RepoConfig
	Ignored        bool
}

// ActionKind is what the orchestrator should do.
type ActionKind int

const (
	None ActionKind = iota
	Wait
	RunAuto
	RunManualFull
	RunManualCommit
	NoteLimit
)

func (k ActionKind) String() string {
	switch k {
	case Wait:
		return "wait"
	case RunAuto:
		return "run-auto"
	case RunManualFull:
		return "run-manual-full"
	case RunManualCommit:
		return "run-manual-commit"
	case NoteLimit:
		return "note-limit"
	default:
		return "none"
	}
}

// Review modes.
const (
	ModeFull        = "full"
	ModeIncremental = "incremental"
	ModeCommit      = "commit"
)

// Reasons Decide gives for None when the PR, its repository or its summary
// stops automatic review until something about it changes.
const (
	ReasonDraft            = "draft"
	ReasonDisabled         = "disabled for repository"
	ReasonIgnored          = "ignore rule"
	ReasonMarkerUnreadable = "summary marker unreadable"
)

// Action is the outcome of Decide. Round is the automatic round number the
// run will become (0 for other actions). From is the reviewed SHA an
// incremental round starts at. Reason is a short text for logs.
type Action struct {
	Kind   ActionKind
	Round  int
	Mode   string
	From   string
	Reason string
}

// Decide applies the round rules top to bottom; the first match wins.
//
// Manual commands run even on a draft or ignored PR and never count toward
// the limit. A summary whose marker cannot be read blocks automatic runs
// instead of starting over, so a damaged marker never resets the round count.
// The limit note goes out the first time a round past the limit would start.
func Decide(in Input) Action {
	m := in.Marker
	switch {
	case in.PR.State != forge.StateOpen:
		return Action{Kind: None, Reason: "pr not open"}
	case in.Trigger == TriggerCmdReview:
		return Action{Kind: RunManualFull, Mode: ModeFull, Reason: "manual review"}
	case in.Trigger == TriggerCmdReviewCommit:
		return Action{Kind: RunManualCommit, Mode: ModeCommit, Reason: "manual commit review"}
	case in.PR.Draft:
		return Action{Kind: None, Reason: ReasonDraft}
	case !in.Repo.Enabled:
		return Action{Kind: None, Reason: ReasonDisabled}
	case in.Ignored:
		return Action{Kind: None, Reason: ReasonIgnored}
	case in.SummaryPresent && (in.MarkerErr != nil || m == nil):
		return Action{Kind: None, Reason: ReasonMarkerUnreadable}
	}

	// A manual /review before any automatic round sets Reviewed. Round 1 then
	// goes through the same gate as later rounds and reviews only what came
	// after it, so the commits the manual review covered are not reviewed again.
	if m == nil || (m.Auto == 0 && m.Reviewed == "") {
		switch in.Trigger {
		case TriggerOpened, TriggerReady, TriggerReopened, TriggerPush:
			return Action{Kind: RunAuto, Round: 1, Mode: ModeFull, Reason: "first round"}
		default:
			return Action{Kind: None, Reason: "no round yet"}
		}
	}

	switch {
	case in.PR.HeadSHA == m.Reviewed:
		return Action{Kind: None, Reason: "head already reviewed"}
	case in.OpenThreads > 0:
		return Action{Kind: Wait, Reason: fmt.Sprintf("%d open threads", in.OpenThreads)}
	case m.Auto >= in.Repo.MaxRounds:
		if m.LimitNoted {
			return Action{Kind: None, Reason: "round limit reached"}
		}
		return Action{Kind: NoteLimit, Reason: "round limit reached"}
	case m.Reviewed == "":
		return Action{Kind: RunAuto, Round: m.Auto + 1, Mode: ModeFull, Reason: "next round, no reviewed sha"}
	default:
		return Action{Kind: RunAuto, Round: m.Auto + 1, Mode: ModeIncremental, From: m.Reviewed, Reason: "next round"}
	}
}

// Ancestry results of `git merge-base --is-ancestor`.
const (
	AncestryYes     = 0
	AncestryNo      = 1
	AncestryMissing = 128
	AncestryError   = -1
)

// ResolveRange turns an action into the commit range to review once the
// checkout can answer whether a.From is an ancestor of HEAD. An incremental
// round falls back to a full review, still counted as a round, when history
// was rewritten (1), the old SHA is gone (128), or git failed (-1). Full
// ranges start at baseSHA; the reviewer takes the merge base. A manual commit
// run returns an empty from because it reviews HEAD against its first parent.
// Actions that run nothing return empty strings.
func ResolveRange(a Action, baseSHA, headSHA string, ancestry int) (from, to, kind string) {
	switch a.Kind {
	case RunManualFull:
		return baseSHA, headSHA, KindManualFull
	case RunManualCommit:
		return "", headSHA, KindManualCommit
	case RunAuto:
		if a.Mode == ModeIncremental && a.From != "" && ancestry == AncestryYes {
			return a.From, headSHA, KindIncremental
		}
		return baseSHA, headSHA, KindFull
	default:
		return "", "", ""
	}
}

// Apply returns the marker after a completed action; m may be nil. from, to
// and base are the values ResolveRange returned and the PR base SHA, ids are
// the finding ids posted, and limit is the round limit in force.
//
// An automatic round counts and moves Reviewed. Its run kind is incremental
// only when from is still the action's From, which is exactly when
// ResolveRange kept the incremental range. A manual full review moves
// Reviewed and Base but leaves the count, so the next automatic round does
// not review the same commits again. A manual commit review changes neither.
// The run history keeps the newest 30 entries.
func Apply(m *Marker, a Action, from, to, base string, ids []string, limit int) Marker {
	out := carry(m, limit)
	ids = slices.Clone(ids)
	if ids == nil {
		ids = []string{}
	}
	switch a.Kind {
	case RunAuto:
		kind := KindFull
		if a.Mode == ModeIncremental && a.From != "" && from == a.From {
			kind = KindIncremental
		}
		out.Auto = a.Round
		out.Reviewed, out.Base = to, base
		out.LimitNoted = false
		out.Runs = append(out.Runs, Run{N: a.Round, Kind: kind, From: from, To: to, IDs: ids})
	case RunManualFull:
		out.Reviewed, out.Base = to, base
		out.Runs = append(out.Runs, Run{Kind: KindManualFull, From: from, To: to, IDs: ids})
	case RunManualCommit:
		out.Runs = append(out.Runs, Run{Kind: KindManualCommit, From: from, To: to, IDs: ids})
	case NoteLimit:
		out.LimitNoted = true
	}
	if len(out.Runs) > maxRuns {
		out.Runs = out.Runs[len(out.Runs)-maxRuns:]
	}
	return out
}

// PruneIDs bounds the marker, which must fit in one comment. Thread tags,
// not the marker, keep posting idempotent, so an id is kept only while the
// summary shows it: on the newest runs, whose counts the summary lists, and
// on older runs while its thread is still open.
func PruneIDs(m Marker, open map[string]bool) Marker {
	m.Runs = slices.Clone(m.Runs)
	for i := range len(m.Runs) - summaryRunsShown {
		r := &m.Runs[i]
		r.IDs = slices.DeleteFunc(slices.Clone(r.IDs), func(id string) bool { return !open[id] })
	}
	return m
}

// ApplySkipped returns the marker after an automatic round that found nothing
// to review, for example because every changed file is excluded. Such a round
// does not count toward the limit and adds no run entry, but it moves Reviewed
// and Base to the commits it looked at, so the same commits are not looked at
// again and the next round starts after them.
func ApplySkipped(m *Marker, to, base string, limit int) Marker {
	out := carry(m, limit)
	out.Reviewed, out.Base = to, base
	return out
}

// carry copies m for an update, or starts from the initial marker.
func carry(m *Marker, limit int) Marker {
	if m == nil {
		return Initial(limit)
	}
	out := *m
	out.V = markerVersion
	out.Max = limit
	out.Runs = slices.Clone(m.Runs)
	return out
}

// FindingID builds the id stored in a finding thread so a retried post can
// skip what is already there. Automatic rounds use r<round>-<sha8>-<h8>;
// manual runs add the time, m-<sha8>-<unix>-<h8>, because the same HEAD can be
// reviewed manually more than once. h8 hashes the location and category, not
// the LLM text, so it does not depend on wording or order.
func FindingID(a Action, headSHA string, f forge.Finding, now time.Time) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s", f.Path, f.StartLine, f.EndLine, f.Category))
	h8 := hex.EncodeToString(sum[:4])
	sha8 := headSHA[:min(8, len(headSHA))]
	if a.Kind == RunAuto {
		return fmt.Sprintf("r%d-%s-%s", a.Round, sha8, h8)
	}
	return fmt.Sprintf("m-%s-%d-%s", sha8, now.Unix(), h8)
}
