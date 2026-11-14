// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

var (
	shaBase = strings.Repeat("a", 40)
	sha1    = strings.Repeat("1", 40)
	sha2    = strings.Repeat("2", 40)
	sha3    = strings.Repeat("3", 40)
	sha4    = strings.Repeat("4", 40)
	sha5    = strings.Repeat("5", 40)
)

// fakeForge keeps one platform's PR state in memory. API methods count as
// calls; Verify and ParseEvent are pure and do not.
type fakeForge struct {
	platform forge.Platform

	mu        sync.Mutex
	prs       map[string]forge.PR
	summaries map[string]string
	threads   map[string][]forge.Thread
	files     map[string][]byte // sha + ":" + path
	notes     []string
	reactions []string // commentID + ":" + reaction
	posted    [][]forge.Finding
	rendered  []string
	errs      map[string]error // method name -> error to return
	errOnce   map[string]error // method name -> error for the next call only
	headAfter map[string]string
	replies   []string // threadID + ":" + body
	resolved  []string
	pushSpecs int

	calls     atomic.Int64
	gate      chan struct{} // when set, API calls wait for it to close
	verifyErr error
}

func newFakeForge(p forge.Platform) *fakeForge {
	return &fakeForge{
		platform: p, prs: map[string]forge.PR{}, summaries: map[string]string{},
		threads: map[string][]forge.Thread{}, files: map[string][]byte{},
		errs: map[string]error{}, errOnce: map[string]error{}, headAfter: map[string]string{},
	}
}

func (f *fakeForge) api(method string) error {
	f.calls.Add(1)
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errOnce[method]; ok {
		delete(f.errOnce, method)
		return err
	}
	return f.errs[method]
}

func (f *fakeForge) Platform() forge.Platform { return f.platform }

func (f *fakeForge) Verify(_ http.Header, _ []byte) error { return f.verifyErr }

// ParseEvent decodes the test body as a forge.Event.
func (f *fakeForge) ParseEvent(_ http.Header, body []byte) (forge.Event, error) {
	var ev forge.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return forge.Event{}, err
	}
	return ev, nil
}

func (f *fakeForge) GetPR(_ context.Context, ref forge.PRRef) (forge.PR, error) {
	if err := f.api("GetPR"); err != nil {
		return forge.PR{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[ref.Key()]
	if !ok {
		return forge.PR{}, errors.New("not found")
	}
	if next, ok := f.headAfter[ref.Key()]; ok {
		// The next read sees a new push, as if it landed during the fetch.
		delete(f.headAfter, ref.Key())
		cur := pr
		cur.HeadSHA = next
		f.prs[ref.Key()] = cur
	}
	return pr, nil
}

func (f *fakeForge) GetSummary(_ context.Context, ref forge.PRRef) (*forge.Comment, error) {
	if err := f.api("GetSummary"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.summaries[ref.Key()]
	if !ok {
		return nil, nil
	}
	return &forge.Comment{ID: "summary", Body: body}, nil
}

func (f *fakeForge) UpsertSummary(_ context.Context, ref forge.PRRef, body string) error {
	if err := f.api("UpsertSummary"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaries[ref.Key()] = body
	return nil
}

func (f *fakeForge) ListOCRThreads(_ context.Context, ref forge.PRRef) ([]forge.Thread, error) {
	if err := f.api("ListOCRThreads"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.threads[ref.Key()]), nil
}

func (f *fakeForge) PostFindings(_ context.Context, pr forge.PR, fs []forge.Finding, render func(forge.Finding, forge.Placement) string) (forge.PostResult, error) {
	if err := f.api("PostFindings"); err != nil {
		return forge.PostResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := pr.Ref.Key()
	var res forge.PostResult
	for _, fd := range fs {
		if slices.ContainsFunc(f.threads[key], func(t forge.Thread) bool { return t.FindingID == fd.ID }) {
			res.Skipped++
			continue
		}
		f.rendered = append(f.rendered, render(fd, forge.Placement{Kind: forge.Line, Path: fd.Path, StartLine: fd.StartLine, EndLine: fd.EndLine}))
		f.threads[key] = append(f.threads[key], forge.Thread{ID: "t-" + fd.ID, FindingID: fd.ID})
		res.Posted++
	}
	f.posted = append(f.posted, slices.Clone(fs))
	return res, nil
}

func (f *fakeForge) PostNote(_ context.Context, _ forge.PRRef, body string) error {
	if err := f.api("PostNote"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, body)
	return nil
}

func (f *fakeForge) React(_ context.Context, _ forge.PRRef, commentID string, r forge.Reaction) error {
	if err := f.api("React"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions = append(f.reactions, commentID+":"+string(r))
	return nil
}

func (f *fakeForge) Clone(_ context.Context, pr forge.PR) (forge.CloneSpec, error) {
	if err := f.api("Clone"); err != nil {
		return forge.CloneSpec{}, err
	}
	return forge.CloneSpec{URL: "file:///" + pr.Ref.Repo, HeadRef: "refs/head", BaseRef: "refs/base"}, nil
}

func (f *fakeForge) ReadFile(_ context.Context, _ forge.PRRef, sha, path string, maxBytes int) ([]byte, bool, error) {
	if err := f.api("ReadFile"); err != nil {
		return nil, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ReadFile:"+path]; err != nil {
		return nil, false, err
	}
	raw, ok := f.files[sha+":"+path]
	if len(raw) > maxBytes {
		return nil, false, fmt.Errorf("%s exceeds %d bytes", path, maxBytes)
	}
	return raw, ok, nil
}

func (f *fakeForge) PushSpec(_ context.Context, pr forge.PR) (forge.CloneSpec, error) {
	if err := f.api("PushSpec"); err != nil {
		return forge.CloneSpec{}, err
	}
	if pr.Fork {
		return forge.CloneSpec{}, forge.ErrFork
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushSpecs++
	return forge.CloneSpec{URL: "file:///" + pr.Ref.Repo, HeadRef: "refs/heads/" + pr.SourceBranch, BaseRef: "refs/heads/" + pr.TargetBranch}, nil
}

func (f *fakeForge) CommitIdentity(context.Context, forge.PRRef) (forge.Identity, error) {
	if err := f.api("CommitIdentity"); err != nil {
		return forge.Identity{}, err
	}
	return forge.Identity{Name: "ocr[bot]", Email: "1+ocr[bot]@users.noreply.github.com"}, nil
}

func (f *fakeForge) ReplyThread(_ context.Context, _ forge.PRRef, threadID, body string) error {
	if err := f.api("ReplyThread"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, threadID+":"+body)
	return nil
}

func (f *fakeForge) ResolveThread(_ context.Context, ref forge.PRRef, threadID string) error {
	if err := f.api("ResolveThread"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, threadID)
	for i := range f.threads[ref.Key()] {
		if f.threads[ref.Key()][i].ID == threadID {
			f.threads[ref.Key()][i].Resolved = true
		}
	}
	return nil
}

// Test helpers on the fake.

func (f *fakeForge) setPR(pr forge.PR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[pr.Ref.Key()] = pr
}

func (f *fakeForge) push(ref forge.PRRef, head string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := f.prs[ref.Key()]
	pr.HeadSHA = head
	f.prs[ref.Key()] = pr
}

func (f *fakeForge) resolveAll(ref forge.PRRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.threads[ref.Key()] {
		f.threads[ref.Key()][i].Resolved = true
	}
}

func (f *fakeForge) setErr(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errs, method)
		return
	}
	f.errs[method] = err
}

func (f *fakeForge) failOnce(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errOnce[method] = err
}

func (f *fakeForge) summary(ref forge.PRRef) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summaries[ref.Key()]
}

func (f *fakeForge) marker(t *testing.T, ref forge.PRRef) state.Marker {
	t.Helper()
	m, err := state.Decode(f.summary(ref))
	if err != nil || m == nil {
		t.Fatalf("summary marker: %v %v\n%s", m, err, f.summary(ref))
	}
	return *m
}

func (f *fakeForge) noteList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.notes)
}

func (f *fakeForge) reactionList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reactions)
}

func (f *fakeForge) postedList() [][]forge.Finding {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posted)
}

// fakeReviewer records requests and answers with fn.
type fakeReviewer struct {
	mu   sync.Mutex
	reqs []review.Request
	fn   func(context.Context, review.Request) (review.Result, error)
}

func (r *fakeReviewer) Review(ctx context.Context, req review.Request) (review.Result, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	fn := r.fn
	r.mu.Unlock()
	if fn == nil {
		return review.Result{Terminal: review.TerminalComplete}, nil
	}
	return fn(ctx, req)
}

func (r *fakeReviewer) requests() []review.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reqs)
}

func (r *fakeReviewer) set(fn func(context.Context, review.Request) (review.Result, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fn = fn
}

func findingsResult(cs ...model.LlmComment) func(context.Context, review.Request) (review.Result, error) {
	return func(context.Context, review.Request) (review.Result, error) {
		return review.Result{Findings: cs, Terminal: review.TerminalComplete}, nil
	}
}

func comment(path string, line int) model.LlmComment {
	return model.LlmComment{Path: path, StartLine: line, EndLine: line, Category: "bug", Severity: "high", Content: "problem at " + path}
}

// fakeWorkspaces hands out in-memory slots.
type fakeWorkspaces struct {
	mu         sync.Mutex
	acquireErr error
	fetchErr   error
	ancestry   int
	acquired   int
	released   int
	fetched    []string

	branchFetched []string
	branchErr     error
	dirty         bool // Commit finds changes
	commitErr     error
	pushErr       error
	commits       []string // identity name + ":" + message
	pushed        []string // spec.HeadRef + "@" + sha
}

const fixSHA = "ffffffffffffffffffffffffffffffffffffffff"

func (w fakeWorkspace) FetchBranch(_ context.Context, spec forge.CloneSpec, head, base string) error {
	w.w.mu.Lock()
	defer w.w.mu.Unlock()
	w.w.branchFetched = append(w.w.branchFetched, spec.HeadRef+"@"+head+".."+base)
	return w.w.branchErr
}

func (w fakeWorkspace) ChangedFiles(context.Context, string) ([]string, error) {
	return []string{"a.go"}, nil
}

func (w fakeWorkspace) Commit(_ context.Context, ident forge.Identity, message string) (string, bool, error) {
	w.w.mu.Lock()
	defer w.w.mu.Unlock()
	if w.w.commitErr != nil {
		return "", false, w.w.commitErr
	}
	if !w.w.dirty {
		return "", false, nil
	}
	w.w.commits = append(w.w.commits, ident.Name+":"+message)
	return fixSHA, true, nil
}

func (w fakeWorkspace) Push(_ context.Context, spec forge.CloneSpec, sha string) error {
	w.w.mu.Lock()
	defer w.w.mu.Unlock()
	if w.w.pushErr != nil {
		return w.w.pushErr
	}
	w.w.pushed = append(w.w.pushed, spec.HeadRef+"@"+sha)
	return nil
}

type fakeWorkspace struct{ w *fakeWorkspaces }

func (w fakeWorkspace) Dir() string { return "/work/slot-0" }

func (w fakeWorkspace) Fetch(_ context.Context, spec forge.CloneSpec, head, base string) error {
	w.w.mu.Lock()
	defer w.w.mu.Unlock()
	w.w.fetched = append(w.w.fetched, spec.URL+"@"+head+".."+base)
	return w.w.fetchErr
}

func (w fakeWorkspace) IsAncestor(context.Context, string, string) int {
	w.w.mu.Lock()
	defer w.w.mu.Unlock()
	return w.w.ancestry
}

func (ws *fakeWorkspaces) Acquire(context.Context) (review.Workspace, func(), error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.acquireErr != nil {
		return nil, nil, ws.acquireErr
	}
	ws.acquired++
	return fakeWorkspace{ws}, func() {
		ws.mu.Lock()
		defer ws.mu.Unlock()
		ws.released++
	}, nil
}

func (ws *fakeWorkspaces) set(fn func(*fakeWorkspaces)) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	fn(ws)
}

// syncBuffer is a log sink safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines decodes every JSON log line.
func (b *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(b.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func (b *syncBuffer) has(msg string) bool {
	return strings.Contains(b.String(), `"msg":"`+msg+`"`)
}

type harness struct {
	t     *testing.T
	srv   *Server
	gh    *fakeForge
	gl    *fakeForge
	rev   *fakeReviewer
	ws    *fakeWorkspaces
	logs  *syncBuffer
	cfg   *serveconfig.Config
	loc   state.Locale
	clock time.Time
}

func testConfig(t *testing.T) *serveconfig.Config {
	return &serveconfig.Config{
		MaxRounds: 3, PollInterval: time.Hour, ReviewTimeout: time.Minute, Workers: 2,
		MaxBodyBytes: 1 << 10, DataDir: t.TempDir(), WorkDir: t.TempDir(),
	}
}

func newHarness(t *testing.T, tweak ...func(*serveconfig.Config)) *harness {
	t.Helper()
	cfg := testConfig(t)
	for _, fn := range tweak {
		fn(cfg)
	}
	loc, err := state.LoadLocale("en")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t: t, gh: newFakeForge(forge.GitHub), gl: newFakeForge(forge.GitLab),
		rev: &fakeReviewer{}, ws: &fakeWorkspaces{}, logs: &syncBuffer{}, cfg: cfg, loc: loc,
		clock: time.Unix(1_800_000_000, 0),
	}
	srv, err := New(Deps{
		Config: cfg, Forges: map[forge.Platform]forge.Forge{forge.GitHub: h.gh, forge.GitLab: h.gl},
		Reviewer: h.rev, Workspaces: h.ws, Locale: loc, Logger: NewLogger(h.logs),
		Now: func() time.Time { return h.clock }, Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return h
}

func (h *harness) forgeFor(p forge.Platform) *fakeForge {
	if p == forge.GitLab {
		return h.gl
	}
	return h.gh
}

func ghRef(n int) forge.PRRef {
	return forge.PRRef{Platform: forge.GitHub, Repo: "acme/app", InstallationID: 7, Number: n}
}

func glRef(n int) forge.PRRef {
	return forge.PRRef{Platform: forge.GitLab, Repo: "group/app", ProjectID: 9, Number: n}
}

func openPR(ref forge.PRRef, head string) forge.PR {
	return forge.PR{Ref: ref, Title: "Add feature", Author: "alice", SourceBranch: "feature", TargetBranch: "main",
		HeadSHA: head, BaseSHA: shaBase, State: forge.StateOpen}
}

// auto runs an automatic evaluation synchronously.
func (h *harness) auto(ref forge.PRRef, trigger state.Trigger) bool {
	return h.srv.process(ref, job{trigger: trigger, requestID: "req-" + string(trigger)})
}

// command runs a command synchronously.
func (h *harness) command(ref forge.PRRef, body, commentID string) bool {
	cmd, ok, err := ParseCommand(body)
	if !ok || err != nil {
		h.t.Fatalf("bad command %q: %v", body, err)
	}
	return h.srv.process(ref, job{cmd: &cmdJob{Command: cmd, commentID: commentID}, requestID: "req-cmd"})
}

// drain waits until queued work and background calls finish.
func (h *harness) drain() {
	h.t.Helper()
	done := make(chan struct{})
	go func() {
		h.srv.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("work did not finish")
	}
}

func eventBody(t *testing.T, ev forge.Event) []byte {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("missing %q in:\n%s", sub, s)
	}
}
