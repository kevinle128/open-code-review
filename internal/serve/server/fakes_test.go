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
	names    []string

	mu        sync.Mutex
	prs       map[string]forge.PR
	summaries map[string]string
	files     map[string][]byte // sha + ":" + path
	notes     []string
	reactions []string         // commentID + ":" + reaction
	statuses  []string         // sha + ":" + state
	errs      map[string]error // method name -> error to return
	errOnce   map[string]error // method name -> error for the next call only
	headAfter map[string]string
	pushSpecs int

	calls     atomic.Int64
	gate      chan struct{} // when set, API calls wait for it to close
	verifyErr error
}

func newFakeForge(p forge.Platform) *fakeForge {
	return &fakeForge{
		platform: p, prs: map[string]forge.PR{}, summaries: map[string]string{},
		files: map[string][]byte{},
		errs:  map[string]error{}, errOnce: map[string]error{}, headAfter: map[string]string{},
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

func (f *fakeForge) CommandNames() []string { return f.names }

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

func (f *fakeForge) SetStatus(_ context.Context, pr forge.PR, sha string, st forge.CommitState, _ string) error {
	if err := f.api("SetStatus"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, sha[:1]+":"+string(st))
	return nil
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

// Test helpers on the fake.

func (f *fakeForge) CommitURL(ref forge.PRRef, sha string) string {
	return forge.CommitLink("https://forge.test", ref.Repo, "/commit/", sha)
}

func (f *fakeForge) LineURL(ref forge.PRRef, sha, path string, line int) string {
	return forge.LineLink("https://forge.test", ref.Repo, "/blob/", sha, path, line)
}

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

// statusList lists the statuses set, each as the first SHA character and
// the state, in order.
func (f *fakeForge) statusList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.statuses)
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
	// pushErrs are returned by the next pushes, one each, before pushErr.
	pushErrs []error
	// rebaseErr answers Rebase; a rebase that succeeds moves the branch to
	// movedHead and replays the fix as rebasedFix.
	rebaseErr error
	// rebaseWaits makes Rebase wait for its context to end, as a cut
	// cherry-pick that looks like a conflict.
	rebaseWaits bool
	movedHead   string
	rebasedFix  string
	rebases     []string // fixed sha + "@" + new head
	leasePushed []string // spec.HeadRef + "@" + sha + " lease " + lease
	leaseErr    error
	commits     []string // identity name + ":" + message
	pushed      []string // spec.HeadRef + "@" + sha
	// onPush runs after a push, so a test can move the PR head to the fix.
	onPush func(sha string)
	// diff and diffErr answer CommitDiff; commitDiffs, when it holds the
	// commit, answers before diff.
	diff        string
	diffErr     error
	commitDiffs map[string]string
}

// fixSHA is the commit the n-th fix makes: f, e, d, ... repeated.
func fixSHA(n int) string {
	return strings.Repeat(string(rune('f'-n+1)), 40)
}

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
	return fixSHA(len(w.w.commits)), true, nil
}

func (w fakeWorkspace) Rebase(ctx context.Context, _ forge.Identity, _ forge.CloneSpec, fix string) (review.Rebased, error) {
	w.w.mu.Lock()
	defer w.w.mu.Unlock()
	w.w.rebases = append(w.w.rebases, fix+"@"+w.w.movedHead)
	if w.w.rebaseWaits {
		<-ctx.Done()
		return review.Rebased{Head: w.w.movedHead}, fmt.Errorf("%w: %v", review.ErrRebaseConflict, ctx.Err())
	}
	if w.w.rebaseErr != nil {
		return review.Rebased{Head: w.w.movedHead}, w.w.rebaseErr
	}
	return review.Rebased{Head: w.w.movedHead, Fix: w.w.rebasedFix}, nil
}

func (w fakeWorkspace) PushLease(_ context.Context, spec forge.CloneSpec, sha, lease string) error {
	w.w.mu.Lock()
	if w.w.leaseErr != nil {
		defer w.w.mu.Unlock()
		return w.w.leaseErr
	}
	w.w.leasePushed = append(w.w.leasePushed, spec.HeadRef+"@"+sha+" lease "+lease)
	onPush := w.w.onPush
	w.w.mu.Unlock()
	if onPush != nil {
		onPush(sha)
	}
	return nil
}

func (w fakeWorkspace) Push(_ context.Context, spec forge.CloneSpec, sha string) error {
	w.w.mu.Lock()
	if len(w.w.pushErrs) > 0 {
		defer w.w.mu.Unlock()
		err := w.w.pushErrs[0]
		w.w.pushErrs = w.w.pushErrs[1:]
		return err
	}
	if w.w.pushErr != nil {
		defer w.w.mu.Unlock()
		return w.w.pushErr
	}
	w.w.pushed = append(w.w.pushed, spec.HeadRef+"@"+sha)
	onPush := w.w.onPush
	w.w.mu.Unlock()
	if onPush != nil {
		onPush(sha)
	}
	return nil
}

type fakeWorkspace struct{ w *fakeWorkspaces }

func (w fakeWorkspace) CommitDiff(_ context.Context, sha string) (string, error) {
	w.w.mu.Lock()
	defer w.w.mu.Unlock()
	if d, ok := w.w.commitDiffs[sha]; ok {
		return d, nil
	}
	return w.w.diff, w.w.diffErr
}

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
	fix   *fakeFixer
	ws    *fakeWorkspaces
	logs  *syncBuffer
	cfg   *serveconfig.Config
	loc   state.Locale
	clock time.Time
}

func testConfig(t *testing.T) *serveconfig.Config {
	return &serveconfig.Config{
		MaxRounds: 3, ReviewTimeout: time.Minute, FixTimeout: time.Minute, Workers: 2,
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
		rev: &fakeReviewer{}, fix: &fakeFixer{}, ws: &fakeWorkspaces{}, logs: &syncBuffer{}, cfg: cfg, loc: loc,
		clock: time.Unix(1_800_000_000, 0),
	}
	srv, err := New(Deps{
		Config: cfg, Forges: map[forge.Platform]forge.Forge{forge.GitHub: h.gh, forge.GitLab: h.gl},
		Reviewer: h.rev, Fixer: h.fix, Workspaces: h.ws, Locales: testLocales(loc), Languages: testLanguages, Logger: NewLogger(h.logs),
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

// testLanguages are the output languages the harness gives each platform.
var testLanguages = map[forge.Platform]string{forge.GitHub: "English", forge.GitLab: "Vietnamese"}

func testLocales(loc state.Locale) map[forge.Platform]state.Locale {
	return map[forge.Platform]state.Locale{forge.GitHub: loc, forge.GitLab: loc}
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

// opened runs the evaluation of an opened PR synchronously; the rounds it
// queues run in the background, see drain.
func (h *harness) opened(ref forge.PRRef) bool {
	return h.srv.process(ref, job{trigger: triggerOpened, requestID: "req-opened"})
}

// command runs a command synchronously, as dispatch would queue it.
func (h *harness) command(ref forge.PRRef, body, commentID string) bool {
	cmd, ok, err := ParseCommand(body)
	if !ok || err != nil {
		h.t.Fatalf("bad command %q: %v", body, err)
	}
	gen := h.srv.restartChain(ref)
	return h.srv.process(ref, job{cmd: &cmdJob{Command: cmd, commentID: commentID, gen: gen}, requestID: "req-cmd"})
}

// followPushes moves the PR head to each fix the bot pushes, as the forge
// does.
func (h *harness) followPushes(ref forge.PRRef) {
	f := h.forgeFor(ref.Platform)
	h.ws.set(func(w *fakeWorkspaces) { w.onPush = func(sha string) { f.push(ref, sha) } })
}

// fakeFixer records requests and answers with fn; without fn it fixes every
// finding and its tests pass.
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
	if fn != nil {
		return fn(ctx, req)
	}
	return fixAll(req), nil
}

// fixAll fixes every finding of req, or reports each in a report-only run.
func fixAll(req review.FixRequest) review.FixResult {
	res := review.FixResult{Tests: review.Tests{Command: "go test ./...", Passed: true}, CommitMessage: "fix: review findings"}
	for _, fd := range req.Findings {
		if req.ReportOnly {
			res.Reported = append(res.Reported, review.Reported{ID: fd.ID, Path: fd.Path, Line: fd.Line, Problem: fd.Content,
				Scenario: "it fails", ProposedFix: "change it"})
			continue
		}
		res.Fixed = append(res.Fixed, review.Fixed{ID: fd.ID, Path: fd.Path, Line: fd.Line, Problem: fd.Content, Fix: "fixed it"})
	}
	if req.ReportOnly {
		res.Tests = review.Tests{}
	}
	return res
}

func (x *fakeFixer) requests() []review.FixRequest {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.reqs)
}

func (x *fakeFixer) set(fn func(context.Context, review.FixRequest) (review.FixResult, error)) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.fn = fn
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
