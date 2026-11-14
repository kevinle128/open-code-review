// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// srcPatch: new lines 10 (context, old 10), 11-12 added, 13 (context, old
// 11), 14 added, 15 (context, old 13).
const srcPatch = "@@ -10,4 +10,6 @@ func x() {\n ctx10\n+add11\n+add12\n ctx13\n-removed\n+add14\n ctx15\n"

var testDiffs = []apiDiff{
	{OldPath: "src/a.go", NewPath: "src/a.go", Diff: srcPatch},
	{OldPath: "gone.go", NewPath: "gone.go", Diff: "@@ -1,2 +0,0 @@\n-a\n-b\n"},
	{OldPath: "old/name.go", NewPath: "new/name.go", Diff: "@@ -1 +1 @@\n-x\n+y\n"},
}

var testVersions = []apiVersion{
	{HeadCommitSHA: strings.Repeat("e", 40), BaseCommitSHA: baseSHA, StartCommitSHA: startSHA, CreatedAt: "2026-11-12T11:00:00Z"},
	{HeadCommitSHA: headSHA, BaseCommitSHA: baseSHA, StartCommitSHA: startSHA, CreatedAt: "2026-11-12T10:00:00Z"},
}

var testPR = forge.PR{Ref: testRef, HeadSHA: headSHA, BaseSHA: baseSHA, TargetBranch: "main", State: forge.StateOpen}

func renderKind(f forge.Finding, p forge.Placement) string {
	return forge.FindingTag(f.ID) + fmt.Sprintf("\nkind=%d", p.Kind)
}

func postFake(t *testing.T) *mrFake {
	t.Helper()
	m := newMRFake(t)
	m.json("GET "+mrBase+"/diffs", 200, testDiffs)
	m.json("GET "+mrBase+"/versions", 200, testVersions)
	return m
}

func lineCode(path string, old, line int) string {
	sum := sha1.Sum([]byte(path))
	return fmt.Sprintf("%s_%d_%d", hex.EncodeToString(sum[:]), old, line)
}

func TestPostFindingsPlacement(t *testing.T) {
	m := postFake(t)
	f := newTestForge(t, m.fakeGitLab, nil)
	fs := []forge.Finding{
		{ID: "single", Path: "src/a.go", StartLine: 11, EndLine: 11},
		{ID: "multi", Path: "src/a.go", StartLine: 10, EndLine: 12},
		{ID: "context", Path: "src/a.go", StartLine: 13, EndLine: 13},
		{ID: "outside", Path: "src/a.go", StartLine: 40, EndLine: 42},
		{ID: "notindiff", Path: "other.go", StartLine: 3, EndLine: 3},
		{ID: "deleted", Path: "gone.go", StartLine: 1, EndLine: 1},
		{ID: "renamed", Path: "new/name.go", StartLine: 1, EndLine: 1},
		{ID: "prlevel"},
	}
	res, err := f.PostFindings(context.Background(), testPR, fs, renderKind)
	if err != nil {
		t.Fatal(err)
	}
	if res != (forge.PostResult{Posted: 8, Relocated: 1, General: 3}) {
		t.Fatalf("result %+v", res)
	}
	byID := map[string]discussionPayload{}
	for _, p := range m.posted {
		id, _ := forge.ParseFindingID(p.Body)
		byID[id] = p
	}

	single := byID["single"].Position
	if single == nil || single.NewLine != 11 || single.OldLine != 0 || single.LineRange != nil || single.PositionType != "text" ||
		single.HeadSHA != headSHA || single.BaseSHA != baseSHA || single.StartSHA != startSHA || single.OldPath != "src/a.go" {
		t.Fatalf("single %+v", single)
	}
	multi := byID["multi"].Position
	if multi == nil || multi.NewLine != 12 || multi.LineRange == nil ||
		multi.LineRange.Start != (apiLineBound{LineCode: lineCode("src/a.go", 10, 10), OldLine: 10, NewLine: 10}) ||
		multi.LineRange.End != (apiLineBound{LineCode: lineCode("src/a.go", 0, 12), Type: "new", NewLine: 12}) {
		t.Fatalf("multi %+v", multi)
	}
	if ctx := byID["context"].Position; ctx == nil || ctx.NewLine != 13 || ctx.OldLine != 11 {
		t.Fatalf("context %+v", ctx)
	}
	if out := byID["outside"]; out.Position == nil || out.Position.NewLine != 15 || !strings.Contains(out.Body, "kind=1") {
		t.Fatalf("outside %+v", out)
	}
	for _, id := range []string{"notindiff", "deleted", "prlevel"} {
		if p := byID[id]; p.Position != nil || !strings.Contains(p.Body, "kind=3") {
			t.Fatalf("%s %+v", id, p)
		}
	}
	if r := byID["renamed"].Position; r == nil || r.OldPath != "old/name.go" || r.NewPath != "new/name.go" {
		t.Fatalf("renamed %+v", r)
	}
}

func TestPostFindingsIdempotent(t *testing.T) {
	m := postFake(t)
	m.addDiscussion(apiUser{ID: 7, Username: "dev1"}, "quoting "+forge.FindingTag("b"))
	f := newTestForge(t, m.fakeGitLab, nil)
	fs := []forge.Finding{
		{ID: "a", Path: "src/a.go", StartLine: 11, EndLine: 11},
		{ID: "b", Path: "src/a.go", StartLine: 12, EndLine: 12},
		{ID: "a", Path: "src/a.go", StartLine: 11, EndLine: 11},
	}
	res, err := f.PostFindings(context.Background(), testPR, fs, renderKind)
	if err != nil || res.Posted != 2 || res.Skipped != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	res, err = f.PostFindings(context.Background(), testPR, fs, renderKind)
	if err != nil || res.Posted != 0 || res.Skipped != 3 || len(m.posted) != 2 {
		t.Fatalf("second: %+v %v posted=%d", res, err, len(m.posted))
	}
}

func TestPostFindingsLineFallback(t *testing.T) {
	m := postFake(t)
	m.postScript = []int{400}
	f := newTestForge(t, m.fakeGitLab, nil)
	res, err := f.PostFindings(context.Background(), testPR, []forge.Finding{{ID: "x", Path: "src/a.go", StartLine: 11, EndLine: 11}}, renderKind)
	if err != nil || res != (forge.PostResult{Posted: 1, General: 1}) {
		t.Fatalf("%+v %v", res, err)
	}
	if len(m.posted) != 1 || m.posted[0].Position != nil || !strings.Contains(m.posted[0].Body, "kind=3") {
		t.Fatalf("fallback %+v", m.posted)
	}
}

func TestPostFindingsRetries(t *testing.T) {
	cases := []struct {
		name      string
		script    []int
		posted    int
		sleeps    []time.Duration
		discLists int
	}{
		{"rate limit", []int{429}, 1, []time.Duration{3 * time.Second}, 1},
		{"server error, not created", []int{500}, 1, []time.Duration{2 * time.Second}, 2},
		{"gateway error, created", []int{-502}, 1, []time.Duration{2 * time.Second}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := postFake(t)
			m.postScript = tc.script
			sl := &sleeps{}
			f := newTestForge(t, m.fakeGitLab, sl)
			res, err := f.PostFindings(context.Background(), testPR, []forge.Finding{{ID: "x", Path: "src/a.go", StartLine: 11, EndLine: 11}}, renderKind)
			if err != nil || res.Posted != 1 || len(m.posted) != tc.posted {
				t.Fatalf("%+v %v posted=%d", res, err, len(m.posted))
			}
			got := sl.all()
			if len(got) != len(tc.sleeps) || got[0] < tc.sleeps[0] || got[0] > tc.sleeps[0]+time.Second {
				t.Fatalf("sleeps %v", got)
			}
			lists := 0
			for _, r := range m.requests(http.MethodGet) {
				if strings.HasSuffix(r.Path, "/discussions") {
					lists++
				}
			}
			if lists != tc.discLists {
				t.Fatalf("discussion lists %d", lists)
			}
		})
	}
}

func TestPostFindingsFailures(t *testing.T) {
	ctx := context.Background()
	one := []forge.Finding{{ID: "x", Path: "src/a.go", StartLine: 11, EndLine: 11}}

	m := postFake(t)
	m.postScript = []int{403, 201}
	f := newTestForge(t, m.fakeGitLab, nil)
	two := append(one, forge.Finding{ID: "y", Path: "src/a.go", StartLine: 12, EndLine: 12})
	res, err := f.PostFindings(ctx, testPR, two, renderKind)
	if err == nil || !isStatus(err, 403) || res.Posted != 1 {
		t.Fatalf("one failure must not stop the rest: %+v %v", res, err)
	}

	m = postFake(t)
	m.postScript = []int{500, 500, 500, 500}
	res, err = newTestForge(t, m.fakeGitLab, nil).PostFindings(ctx, testPR, one, renderKind)
	if !isStatus(err, 500) || res.Posted != 0 {
		t.Fatalf("attempts exhausted: %+v %v", res, err)
	}

	// The duplicate check itself fails after an ambiguous error: no retry.
	lists := 0
	fake := newFake(t)
	fake.json("GET /user", 200, botUser)
	fake.json("GET "+mrBase+"/diffs", 200, testDiffs)
	fake.json("GET "+mrBase+"/versions", 200, testVersions)
	fake.handle("GET "+mrBase+"/discussions", func(w http.ResponseWriter, _ *http.Request) {
		lists++
		if lists > 1 {
			writeJSON(w, 403, map[string]string{"message": "403"})
			return
		}
		writeJSON(w, 200, []apiDiscussion{})
	})
	posts := 0
	fake.handle("POST "+mrBase+"/discussions", func(w http.ResponseWriter, _ *http.Request) {
		posts++
		writeJSON(w, 502, map[string]string{})
	})
	_, err = newTestForge(t, fake, nil).PostFindings(ctx, testPR, one, renderKind)
	if err == nil || !strings.Contains(err.Error(), "duplicate check failed") || posts != 1 {
		t.Fatalf("check failure: %v posts=%d", err, posts)
	}

	// A dropped connection is ambiguous too.
	drop := newFake(t)
	drop.json("GET /user", 200, botUser)
	drop.json("GET "+mrBase+"/discussions", 200, []apiDiscussion{})
	drop.json("GET "+mrBase+"/diffs", 404, map[string]string{})
	drop.json("GET "+mrBase+"/versions", 200, testVersions)
	dropped := 0
	drop.handle("POST "+mrBase+"/discussions", func(w http.ResponseWriter, _ *http.Request) {
		dropped++
		if dropped == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		writeJSON(w, 201, map[string]string{})
	})
	sl := &sleeps{}
	res, err = newTestForge(t, drop, sl).PostFindings(ctx, testPR, one, renderKind)
	if err != nil || res.Posted != 1 || dropped != 2 || len(sl.all()) != 1 || sl.all()[0] != transientRetryBase {
		t.Fatalf("dropped: %+v %v posts=%d sleeps=%v", res, err, dropped, sl.all())
	}

	// A cancelled context ends the loop.
	cctx, cancel := context.WithCancel(ctx)
	cancelFake := newFake(t)
	cancelFake.json("GET /user", 200, botUser)
	cancelFake.json("GET "+mrBase+"/discussions", 200, []apiDiscussion{})
	cancelFake.json("GET "+mrBase+"/diffs", 200, testDiffs)
	cancelFake.json("GET "+mrBase+"/versions", 200, testVersions)
	cancelFake.handle("POST "+mrBase+"/discussions", func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		writeJSON(w, 500, map[string]string{})
	})
	if _, err := newTestForge(t, cancelFake, nil).PostFindings(cctx, testPR, one, renderKind); err == nil {
		t.Fatal("cancelled: want error")
	}

	// A sleeper that fails stops the retry.
	m = postFake(t)
	m.postScript = []int{429}
	sf := newTestForge(t, m.fakeGitLab, nil)
	sf.c.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	if _, err := sf.PostFindings(ctx, testPR, one, renderKind); err == nil {
		t.Fatal("sleep failure: want error")
	}
}

func TestPostFindingsGuards(t *testing.T) {
	ctx := context.Background()
	m := postFake(t)
	f := newTestForge(t, m.fakeGitLab, nil)
	if res, err := f.PostFindings(ctx, testPR, nil, renderKind); err != nil || res != (forge.PostResult{}) {
		t.Fatalf("empty: %+v %v", res, err)
	}
	one := []forge.Finding{{ID: "x", Path: "src/a.go", StartLine: 11, EndLine: 11}}
	if _, err := f.PostFindings(ctx, testPR, one, nil); err == nil {
		t.Fatal("nil render: want error")
	}
	if _, err := f.PostFindings(ctx, forge.PR{}, one, renderKind); err == nil {
		t.Fatal("empty ref: want error")
	}
	broken := newFake(t)
	broken.json("GET /user", 200, botUser)
	broken.json("GET "+mrBase+"/discussions", 500, map[string]string{})
	if _, err := newTestForge(t, broken, nil).PostFindings(ctx, testPR, one, renderKind); err == nil {
		t.Fatal("unreadable discussions: want error")
	}
}

func TestPostFindingsWithoutDiffData(t *testing.T) {
	ctx := context.Background()
	one := []forge.Finding{{ID: "x", Path: "unknown.go", StartLine: 3, EndLine: 4}}

	m := newMRFake(t)
	m.json("GET "+mrBase+"/diffs", 500, map[string]string{})
	m.json("GET "+mrBase+"/versions", 200, testVersions)
	res, err := newTestForge(t, m.fakeGitLab, nil).PostFindings(ctx, testPR, one, renderKind)
	if err != nil || res.Posted != 1 || res.General != 0 || m.posted[0].Position == nil || m.posted[0].Position.NewLine != 4 {
		t.Fatalf("incomplete inventory keeps the line: %+v %v %+v", res, err, m.posted)
	}

	m = newMRFake(t)
	m.json("GET "+mrBase+"/diffs", 200, testDiffs)
	m.json("GET "+mrBase+"/versions", 500, map[string]string{})
	res, err = newTestForge(t, m.fakeGitLab, nil).PostFindings(ctx, testPR, []forge.Finding{{ID: "y", Path: "src/a.go", StartLine: 11, EndLine: 11}}, renderKind)
	if err != nil || res.General != 1 || m.posted[0].Position != nil {
		t.Fatalf("no versions -> positionless: %+v %v", res, err)
	}
}

func TestDiffRefsChoice(t *testing.T) {
	m := newFake(t)
	m.json("GET "+mrBase+"/versions", 200, []apiVersion{
		{HeadCommitSHA: "old", BaseCommitSHA: "x", CreatedAt: "2026-11-12T09:00:00Z"},
		{HeadCommitSHA: headSHA, BaseCommitSHA: "other", StartCommitSHA: "s1", CreatedAt: "2026-11-12T10:00:00Z"},
		{HeadCommitSHA: headSHA, BaseCommitSHA: "other2", StartCommitSHA: "s2", CreatedAt: "2026-11-12T10:30:00Z"},
		{HeadCommitSHA: "newest", BaseCommitSHA: "y", CreatedAt: "2026-11-12T11:00:00Z"},
	})
	m.json("GET /projects/1/merge_requests/1/versions", 200, []apiVersion{})
	f := newTestForge(t, m, nil)
	ctx := context.Background()
	if v := f.diffRefs(ctx, mrBase, testPR); v == nil || v.StartCommitSHA != "s1" {
		t.Fatalf("head-only match %+v", v)
	}
	if v := f.diffRefs(ctx, mrBase, forge.PR{HeadSHA: headSHA, BaseSHA: "other2"}); v == nil || v.StartCommitSHA != "s2" {
		t.Fatalf("head+base match %+v", v)
	}
	if v := f.diffRefs(ctx, mrBase, forge.PR{HeadSHA: "zzz"}); v == nil || v.HeadCommitSHA != "newest" {
		t.Fatalf("newest fallback %+v", v)
	}
	if v := f.diffRefs(ctx, "/projects/1/merge_requests/1", testPR); v != nil {
		t.Fatalf("no versions %+v", v)
	}
}

func TestBuildLineRange(t *testing.T) {
	_, positions, _ := forge.ParsePatch(srcPatch)
	if r := buildLineRange("src/a.go", 11, 99, positions); r != nil {
		t.Fatalf("unresolvable end %+v", r)
	}
	if r := buildLineRange("src/a.go", 1, 11, positions); r != nil {
		t.Fatalf("unresolvable start %+v", r)
	}
	if r := buildLineRange("src/a.go", 13, 15, positions); r == nil || r.Start.OldLine != 11 || r.End.OldLine != 13 || r.Start.Type != "" {
		t.Fatalf("context range %+v", r)
	}
}

func TestDiffViewSkipsEmptyPaths(t *testing.T) {
	m := newFake(t)
	m.json("GET "+mrBase+"/diffs", 200, []apiDiff{{}, {OldPath: "only-old.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"}, {NewPath: "clipped.go", Diff: "@@ -1,5 +1,5 @@\n a\n"}})
	v := newTestForge(t, m, nil).diffView(context.Background(), mrBase)
	if !v.inv.Complete || !v.inv.Known["only-old.go"] || len(v.inv.Known) != 2 || v.inv.Ranges["clipped.go"] != nil {
		t.Fatalf("view %+v", v.inv)
	}
}
