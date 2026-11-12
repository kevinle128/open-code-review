// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

const (
	testBotID = 99
	mrBase    = "/projects/42/merge_requests/5"
)

var (
	testRef  = forge.PRRef{Platform: forge.GitLab, Repo: "team/svc", ProjectID: 42, Number: 5}
	headSHA  = strings.Repeat("a", 40)
	baseSHA  = strings.Repeat("b", 40)
	startSHA = strings.Repeat("c", 40)
)

var botUser = apiUser{ID: testBotID, Username: "pr-review"}

// mrFake keeps the notes and discussions of MR team/svc!5 in memory.
type mrFake struct {
	*fakeGitLab
	mu          sync.Mutex
	nextID      int64
	notes       []apiNote
	discussions []apiDiscussion
	// postScript holds statuses for the next discussion POSTs; a negative
	// status creates the discussion and then answers with -status.
	postScript []int
	posted     []discussionPayload
}

func newMRFake(t *testing.T) *mrFake {
	t.Helper()
	m := &mrFake{fakeGitLab: newFake(t), nextID: 1000}
	m.json("GET /user", 200, botUser)
	m.handle("GET "+mrBase+"/notes", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		writeJSON(w, 200, m.notes)
	})
	m.handle("POST "+mrBase+"/notes", func(w http.ResponseWriter, r *http.Request) {
		var p struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&p)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.nextID++
		n := apiNote{ID: m.nextID, Body: p.Body, Author: botUser}
		m.notes = append(m.notes, n)
		writeJSON(w, 201, n)
	})
	m.handle("PUT "+mrBase+"/notes/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&p)
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := range m.notes {
			if m.notes[i].ID == id {
				m.notes[i].Body = p.Body
				writeJSON(w, 200, m.notes[i])
				return
			}
		}
		writeJSON(w, 404, map[string]string{"message": "404 Not found"})
	})
	m.handle("GET "+mrBase+"/discussions", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		writeJSON(w, 200, m.discussions)
	})
	m.handle("POST "+mrBase+"/discussions", func(w http.ResponseWriter, r *http.Request) {
		var p discussionPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		m.mu.Lock()
		defer m.mu.Unlock()
		status := 201
		if len(m.postScript) > 0 {
			status, m.postScript = m.postScript[0], m.postScript[1:]
		}
		if status > 299 {
			if status == 400 {
				writeJSON(w, 400, map[string]any{"message": map[string]any{"line_code": []string{"can't be blank", "must be a valid line code"}}})
				return
			}
			if status == 429 {
				w.Header().Set("Retry-After", "3")
			}
			writeJSON(w, status, map[string]string{"message": "error"})
			return
		}
		m.posted = append(m.posted, p)
		m.addDiscussion(botUser, p.Body)
		if status < 0 {
			writeJSON(w, -status, map[string]string{"message": "gateway"})
			return
		}
		writeJSON(w, status, m.discussions[len(m.discussions)-1])
	})
	return m
}

// addDiscussion must be called with m.mu held or before the server is used.
func (m *mrFake) addDiscussion(author apiUser, body string) {
	m.nextID++
	m.discussions = append(m.discussions, apiDiscussion{
		ID:    "d" + strconv.FormatInt(m.nextID, 10),
		Notes: []apiNote{{ID: m.nextID, Body: body, Author: author, Resolvable: true}},
	})
}

func TestBotUserIDCaches(t *testing.T) {
	m := newMRFake(t)
	f := newTestForge(t, m.fakeGitLab, nil)
	for range 2 {
		id, err := f.BotUserID(context.Background())
		if err != nil || id != testBotID {
			t.Fatalf("id %d err %v", id, err)
		}
	}
	if n := len(m.requests(http.MethodGet)); n != 1 {
		t.Fatalf("GET /user called %d times", n)
	}
	if !f.isBotUser(0, "PR-REVIEW") {
		t.Fatal("cached username not used")
	}

	bad := newFake(t)
	bad.json("GET /user", 200, map[string]any{})
	if _, err := newTestForge(t, bad, nil).BotUserID(context.Background()); err == nil {
		t.Fatal("user without id: want error")
	}
	down := newFake(t)
	down.json("GET /user", 401, map[string]string{"message": "401 Unauthorized"})
	if _, err := newTestForge(t, down, nil).BotUserID(context.Background()); !isStatus(err, 401) {
		t.Fatalf("401: %v", err)
	}
}

func mrJSON(state string, refs bool) map[string]any {
	mr := map[string]any{
		"iid": 5, "title": "Draft: Add", "author": map[string]any{"id": 7, "username": "dev1"},
		"labels": []string{"backend"}, "source_branch": "feat/x", "target_branch": "main",
		"sha": headSHA, "draft": true, "state": state,
	}
	if refs {
		mr["diff_refs"] = map[string]string{"base_sha": baseSHA, "head_sha": headSHA, "start_sha": startSHA}
	}
	return mr
}

func TestGetPR(t *testing.T) {
	f := newFake(t)
	calls := 0
	f.handle("GET "+mrBase, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		switch calls {
		case 1:
			writeJSON(w, 404, map[string]string{"message": "404 Not found"})
		case 2:
			writeJSON(w, 200, mrJSON("opened", false))
		default:
			writeJSON(w, 200, mrJSON("opened", true))
		}
	})
	sl := &sleeps{}
	fg := newTestForge(t, f, sl)
	pr, err := fg.GetPR(context.Background(), forge.PRRef{ProjectID: 42, Number: 5})
	if err != nil {
		t.Fatal(err)
	}
	want := forge.PR{
		Ref: forge.PRRef{Platform: forge.GitLab, ProjectID: 42, Number: 5}, Title: "Draft: Add", Author: "dev1",
		SourceBranch: "feat/x", TargetBranch: "main", HeadSHA: headSHA, BaseSHA: baseSHA, Draft: true, State: forge.StateOpen,
	}
	if pr.Ref != want.Ref || pr.Title != want.Title || pr.Author != want.Author || pr.HeadSHA != want.HeadSHA ||
		pr.BaseSHA != want.BaseSHA || !pr.Draft || pr.State != want.State || pr.AuthorIsBot || len(pr.Labels) != 1 {
		t.Fatalf("pr %+v", pr)
	}
	if got := sl.all(); len(got) != 2 || got[0] != time.Second || got[1] != 2*time.Second {
		t.Fatalf("sleeps %v", got)
	}
}

func TestGetPRGivesUp(t *testing.T) {
	notFound := newFake(t)
	notFound.json("GET "+mrBase, 404, map[string]string{"message": "404 Not found"})
	sl := &sleeps{}
	if _, err := newTestForge(t, notFound, sl).GetPR(context.Background(), testRef); !isStatus(err, 404) || !errors.Is(err, forge.ErrNotFound) || len(sl.all()) != 3 {
		t.Fatalf("404: %v after %v", err, sl.all())
	}

	noRefs := newFake(t)
	noRefs.json("GET "+mrBase, 200, mrJSON("opened", false))
	if _, err := newTestForge(t, noRefs, nil).GetPR(context.Background(), testRef); err == nil || !strings.Contains(err.Error(), "no head or base") {
		t.Fatalf("no refs: %v", err)
	}

	denied := newFake(t)
	denied.json("GET "+mrBase, 403, map[string]string{"message": "403 Forbidden"})
	sl = &sleeps{}
	if _, err := newTestForge(t, denied, sl).GetPR(context.Background(), testRef); !isStatus(err, 403) || len(sl.all()) != 0 {
		t.Fatalf("403: %v", err)
	}

	closed := newFake(t)
	closed.json("GET "+mrBase, 200, mrJSON("merged", false))
	pr, err := newTestForge(t, closed, nil).GetPR(context.Background(), testRef)
	if err != nil || pr.State != forge.StateMerged || pr.BaseSHA != "" {
		t.Fatalf("merged without refs: %+v %v", pr, err)
	}

	cancelled := newFake(t)
	cancelled.json("GET "+mrBase, 404, map[string]string{})
	ctx, cancel := context.WithCancel(context.Background())
	fg, _ := New(serveconfig.GitLabConfig{URL: cancelled.srv.URL, Token: testToken}, cancelled.srv.Client(), func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	})
	if _, err := fg.GetPR(ctx, testRef); err == nil {
		t.Fatal("cancelled sleep: want error")
	}
	if _, err := fg.GetPR(ctx, forge.PRRef{}); err == nil {
		t.Fatal("empty ref: want error")
	}
}

func TestGetPRBotAuthor(t *testing.T) {
	f := newFake(t)
	mr := mrJSON("locked", true)
	mr["author"] = map[string]any{"id": 3, "username": "project_42_bot_x", "bot": true}
	f.json("GET "+mrBase, 200, mr)
	pr, err := newTestForge(t, f, nil).GetPR(context.Background(), testRef)
	if err != nil || !pr.AuthorIsBot || pr.State != forge.StateOpen {
		t.Fatalf("%+v %v", pr, err)
	}
}

func TestSummaryLifecycle(t *testing.T) {
	m := newMRFake(t)
	human := apiUser{ID: 7, Username: "dev1"}
	m.notes = []apiNote{
		{ID: 1, Body: forge.SummaryTag + " forged by a human", Author: human},
		{ID: 2, Body: forge.SummaryTag + " system", Author: botUser, System: true},
	}
	f := newTestForge(t, m.fakeGitLab, nil)
	ctx := context.Background()

	got, err := f.GetSummary(ctx, testRef)
	if err != nil || got != nil {
		t.Fatalf("no bot summary yet: %+v %v", got, err)
	}
	marker := state.Encode(state.Initial(3))
	if err := f.UpsertSummary(ctx, testRef, forge.SummaryTag+"\nreviewing\n"+marker); err != nil {
		t.Fatal(err)
	}
	got, err = f.GetSummary(ctx, testRef)
	if err != nil || got == nil || !strings.Contains(got.Body, "reviewing") {
		t.Fatalf("created summary: %+v %v", got, err)
	}
	if err := f.UpsertSummary(ctx, testRef, forge.SummaryTag+"\ndone"); err != nil {
		t.Fatal(err)
	}
	after, _ := f.GetSummary(ctx, testRef)
	if after.ID != got.ID || !strings.Contains(after.Body, "done") || !strings.Contains(after.Body, marker) {
		t.Fatalf("updated summary lost the marker or moved: %+v", after)
	}
	q := m.requests(http.MethodGet)[1].Query
	if q.Get("order_by") != "created_at" || q.Get("sort") != "asc" {
		t.Fatalf("notes query %v", q)
	}
	if len(m.requests(http.MethodPost)) != 1 || len(m.requests(http.MethodPut)) != 1 {
		t.Fatalf("writes %+v", m.writes())
	}

	if err := f.PostNote(ctx, testRef, "limit"); err != nil {
		t.Fatal(err)
	}
	if err := f.PostNote(ctx, forge.PRRef{}, "x"); err == nil {
		t.Fatal("empty ref: want error")
	}
	if _, err := f.GetSummary(ctx, forge.PRRef{}); err == nil {
		t.Fatal("empty ref: want error")
	}
}

func TestSummaryIgnoresFindingNoteQuotingTag(t *testing.T) {
	m := newMRFake(t)
	m.notes = []apiNote{
		{ID: 1, Body: forge.FindingTag("r1-aaaaaaaa-01020304") + "\nconst SummaryTag = \"" + forge.SummaryTag + "\"", Author: botUser},
	}
	f := newTestForge(t, m.fakeGitLab, nil)
	ctx := context.Background()
	if got, err := f.GetSummary(ctx, testRef); err != nil || got != nil {
		t.Fatalf("finding note taken as the summary: %+v %v", got, err)
	}
	if err := f.UpsertSummary(ctx, testRef, forge.SummaryTag+"\nreviewing"); err != nil {
		t.Fatal(err)
	}
	if len(m.requests(http.MethodPut)) != 0 || len(m.requests(http.MethodPost)) != 1 {
		t.Fatalf("writes %+v", m.writes())
	}
}

func TestUpsertSummaryDoesNotPostWhenListFails(t *testing.T) {
	f := newFake(t)
	f.json("GET /user", 200, botUser)
	f.json("GET "+mrBase+"/notes", 500, map[string]string{"message": "boom"})
	if err := newTestForge(t, f, nil).UpsertSummary(context.Background(), testRef, "x"); err == nil {
		t.Fatal("want error")
	}
	if len(f.writes()) != 0 {
		t.Fatalf("writes after failed list: %+v", f.writes())
	}
	noUser := newFake(t)
	noUser.json("GET /user", 500, map[string]string{})
	if _, err := newTestForge(t, noUser, nil).GetSummary(context.Background(), testRef); err == nil {
		t.Fatal("want error")
	}
	if _, err := newTestForge(t, noUser, nil).ListOCRThreads(context.Background(), testRef); err == nil {
		t.Fatal("want error")
	}
}

func TestListOCRThreads(t *testing.T) {
	m := newMRFake(t)
	human := apiUser{ID: 7, Username: "dev1"}
	m.addDiscussion(botUser, forge.FindingTag("r1-aaaa-01")+"\nbody")
	m.addDiscussion(botUser, forge.FindingTag("r1-aaaa-02")+"\nbody")
	m.discussions[1].Notes[0].Resolved = true
	m.addDiscussion(human, forge.FindingTag("r1-forged")+"\nbody")
	m.addDiscussion(botUser, "no tag")
	m.addDiscussion(botUser, forge.FindingTag("r1-unresolvable"))
	m.discussions[4].Notes[0].Resolvable = false
	m.discussions = append(m.discussions, apiDiscussion{ID: "empty"})
	threads, err := newTestForge(t, m.fakeGitLab, nil).ListOCRThreads(context.Background(), testRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 || threads[0].FindingID != "r1-aaaa-01" || threads[0].Resolved || !threads[1].Resolved || threads[0].ID != m.discussions[0].ID {
		t.Fatalf("threads %+v", threads)
	}
	if _, err := newTestForge(t, m.fakeGitLab, nil).ListOCRThreads(context.Background(), forge.PRRef{}); err == nil {
		t.Fatal("empty ref: want error")
	}
}

func TestReact(t *testing.T) {
	f := newFake(t)
	f.handle("POST "+mrBase+"/notes/{id}/award_emoji", func(w http.ResponseWriter, r *http.Request) {
		var p struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&p)
		switch r.PathValue("id") {
		case "1":
			writeJSON(w, 201, map[string]string{"name": p.Name})
		case "2":
			writeJSON(w, 409, map[string]string{"message": "conflict"})
		case "3":
			writeJSON(w, 404, map[string]string{"message": "404 Award Emoji Name has already been taken Not Found"})
		default:
			writeJSON(w, 403, map[string]string{"message": "403 Forbidden"})
		}
	})
	fg := newTestForge(t, f, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		id      string
		r       forge.Reaction
		wantErr bool
	}{
		{"1", forge.ReactionEyes, false},
		{"1", forge.ReactionSuccess, false},
		{"1", forge.ReactionFailure, false},
		{"2", forge.ReactionEyes, false},
		{"3", forge.ReactionEyes, false},
		{"4", forge.ReactionEyes, true},
		{"", forge.ReactionEyes, false},
		{"1", forge.Reaction("rocket"), true},
		{"x", forge.ReactionEyes, true},
		{"-1", forge.ReactionEyes, true},
	} {
		if err := fg.React(ctx, testRef, tc.id, tc.r); (err != nil) != tc.wantErr {
			t.Errorf("React(%q, %s): %v", tc.id, tc.r, err)
		}
	}
	var names []string
	for _, r := range f.requests(http.MethodPost)[:3] {
		var p struct{ Name string }
		_ = json.Unmarshal(r.Body, &p)
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "eyes,white_check_mark,x" {
		t.Fatalf("emoji %v", names)
	}
	if err := fg.React(ctx, forge.PRRef{}, "1", forge.ReactionEyes); err == nil {
		t.Fatal("empty ref: want error")
	}
}

func TestClone(t *testing.T) {
	f := newFake(t)
	f.json("GET /projects/42", 200, apiProject{ID: 42, HTTPURLToRepo: "https://git.example.test/team/svc.git"})
	f.json("GET /projects/team%2Fbad", 200, apiProject{ID: 43, HTTPURLToRepo: "https://user:pw@git.example.test/x.git"})
	f.json("GET /projects/team%2Fgone", 404, map[string]string{})
	f.json("GET /projects/team%2Fhostless", 200, apiProject{ID: 44, HTTPURLToRepo: "https:///x.git"})
	fg := newTestForge(t, f, nil)
	ctx := context.Background()
	spec, err := fg.Clone(ctx, forge.PR{Ref: testRef, TargetBranch: "release/1.0"})
	if err != nil {
		t.Fatal(err)
	}
	cred := base64.StdEncoding.EncodeToString([]byte("oauth2:" + testToken))
	want := forge.CloneSpec{URL: "https://git.example.test/team/svc.git", AuthHeader: "Authorization: Basic " + cred,
		HeadRef: "refs/merge-requests/5/head", BaseRef: "refs/heads/release/1.0"}
	if spec != want {
		t.Fatalf("spec %+v", spec)
	}
	if strings.Contains(spec.URL, testToken) {
		t.Fatal("token in URL")
	}
	for _, pr := range []forge.PR{
		{Ref: forge.PRRef{ProjectID: 42}, TargetBranch: "main"},
		{Ref: testRef, TargetBranch: "-x"},
		{Ref: forge.PRRef{Number: 5}, TargetBranch: "main"},
		{Ref: forge.PRRef{Repo: "team/bad", Number: 5}, TargetBranch: "main"},
		{Ref: forge.PRRef{Repo: "team/gone", Number: 5}, TargetBranch: "main"},
		{Ref: forge.PRRef{Repo: "team/hostless", Number: 5}, TargetBranch: "main"},
	} {
		if _, err := fg.Clone(ctx, pr); err == nil {
			t.Errorf("Clone(%+v): want error", pr)
		}
	}
}

func TestValidBranch(t *testing.T) {
	for _, ok := range []string{"main", "release/1.0", "feat/x-y_z"} {
		if !validBranch(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "-x", "/x", "x/", "a..b", "x.lock", "a@{b", "a//b", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`, "a\x7f"} {
		if validBranch(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestReadFile(t *testing.T) {
	f := newFake(t)
	f.handle("GET /projects/42/repository/files/{path}/raw", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != baseSHA {
			t.Errorf("ref %q", r.URL.Query().Get("ref"))
		}
		switch r.PathValue("path") {
		case ".opencodereview/serve.json":
			_, _ = w.Write([]byte(`{"max_rounds":1}`))
		case "big.json":
			_, _ = w.Write([]byte(strings.Repeat("x", serveconfig.MaxRepoConfigBytes+1)))
		case "denied.json":
			w.WriteHeader(403)
		default:
			w.WriteHeader(404)
		}
	})
	fg := newTestForge(t, f, nil)
	ctx := context.Background()
	b, ok, err := fg.ReadFile(ctx, testRef, baseSHA, ".opencodereview/serve.json")
	if err != nil || !ok || string(b) != `{"max_rounds":1}` {
		t.Fatalf("read: %q %v %v", b, ok, err)
	}
	if got := f.requests(http.MethodGet)[0].Path; got != "/projects/42/repository/files/.opencodereview%2Fserve.json/raw" {
		t.Fatalf("path not escaped: %s", got)
	}
	if _, ok, err := fg.ReadFile(ctx, testRef, baseSHA, "missing.json"); ok || err != nil {
		t.Fatalf("missing: %v %v", ok, err)
	}
	if _, _, err := fg.ReadFile(ctx, testRef, baseSHA, "big.json"); err == nil {
		t.Fatal("big: want error")
	}
	if _, _, err := fg.ReadFile(ctx, testRef, baseSHA, "denied.json"); !isStatus(err, 403) {
		t.Fatalf("denied: %v", err)
	}
	if _, _, err := fg.ReadFile(ctx, testRef, "", "x"); err == nil {
		t.Fatal("no sha: want error")
	}
	if _, _, err := fg.ReadFile(ctx, forge.PRRef{}, baseSHA, "x"); err == nil {
		t.Fatal("empty ref: want error")
	}
	f.srv.Close()
	if _, _, err := fg.ReadFile(ctx, testRef, baseSHA, "x"); err == nil {
		t.Fatal("closed server: want error")
	}
	if fg.Platform() != forge.GitLab {
		t.Fatal("platform")
	}
}
