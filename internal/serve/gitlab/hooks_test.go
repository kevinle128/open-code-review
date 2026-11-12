// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	hookURL   = "https://ocr.example.test/ocr/gitlab/webhook"
	otherURL  = "https://pr-agent.example.test/webhook"
	adminTok  = "glpat-admin"
	hookToken = "hook-secret"
)

type storedHook map[string]any

type fakeProject struct {
	ID       int64
	Path     string
	Resolved bool
	Hooks    []storedHook
}

// adminFake emulates the admin API surface the registrar uses, with state,
// so apply twice and remove can be checked end to end.
type adminFake struct {
	*fakeGitLab
	mu          sync.Mutex
	nextID      int64
	system      []storedHook
	groups      []apiGroup
	members     map[int64]map[int64]bool
	groupHooks  map[int64][]storedHook // a group missing here answers 404
	projects    []*fakeProject
	users       map[string]int64
	denyAll     bool
	failProject bool
}

func newAdminFake(t *testing.T) *adminFake {
	t.Helper()
	a := &adminFake{
		fakeGitLab: newFake(t),
		nextID:     500,
		groups:     []apiGroup{{ID: 1, FullPath: "team"}, {ID: 2, FullPath: "ops"}},
		members:    map[int64]map[int64]bool{2: {testBotID: true}},
		groupHooks: map[int64][]storedHook{1: {{"id": float64(77), "url": hookURL}, {"id": float64(78), "url": otherURL}}},
		projects: []*fakeProject{
			{ID: 42, Path: "team/svc", Hooks: []storedHook{{"id": float64(5), "url": otherURL}}},
			{ID: 43, Path: "ops/infra", Resolved: true},
		},
		users:  map[string]int64{"pr-review": testBotID},
		system: []storedHook{{"id": float64(1), "url": otherURL}},
	}
	h := a.handle
	h("GET /hooks", a.lock(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, a.system) }))
	h("POST /hooks", a.lock(func(w http.ResponseWriter, r *http.Request) { a.system = a.create(w, r, a.system) }))
	h("PUT /hooks/{hid}", a.lock(func(w http.ResponseWriter, r *http.Request) { a.update(w, r, a.system) }))
	h("DELETE /hooks/{hid}", a.lock(func(w http.ResponseWriter, r *http.Request) { a.system = a.remove(w, r, a.system) }))
	h("GET /users", a.lock(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := a.users[r.URL.Query().Get("username")]; ok {
			writeJSON(w, 200, []apiUser{{ID: id, Username: r.URL.Query().Get("username")}})
			return
		}
		writeJSON(w, 200, []apiUser{})
	}))
	h("GET /groups", a.lock(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("top_level_only") != "true" || r.URL.Query().Get("all_available") != "true" {
			t.Errorf("groups query %v", r.URL.Query())
		}
		writeJSON(w, 200, a.groups)
	}))
	h("GET /groups/{gid}/members/{uid}", a.lock(func(w http.ResponseWriter, r *http.Request) {
		if a.members[pathID(r, "gid")][pathID(r, "uid")] {
			writeJSON(w, 200, map[string]any{"id": pathID(r, "uid"), "access_level": 30})
			return
		}
		writeJSON(w, 404, map[string]string{"message": "404 Not found"})
	}))
	h("POST /groups/{gid}/members", a.lock(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			UserID      int64 `json:"user_id"`
			AccessLevel int   `json:"access_level"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p.AccessLevel != developerAccess {
			t.Errorf("access level %d", p.AccessLevel)
		}
		gid := pathID(r, "gid")
		if a.members[gid] == nil {
			a.members[gid] = map[int64]bool{}
		}
		a.members[gid][p.UserID] = true
		writeJSON(w, 201, p)
	}))
	h("GET /groups/{gid}/hooks", a.lock(func(w http.ResponseWriter, r *http.Request) {
		hooks, ok := a.groupHooks[pathID(r, "gid")]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "404 Not found"})
			return
		}
		writeJSON(w, 200, hooks)
	}))
	h("DELETE /groups/{gid}/hooks/{hid}", a.lock(func(w http.ResponseWriter, r *http.Request) {
		gid := pathID(r, "gid")
		a.groupHooks[gid] = a.remove(w, r, a.groupHooks[gid])
	}))
	h("GET /projects", a.lock(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("archived") != "false" || r.URL.Query().Has("simple") {
			t.Errorf("projects query %v", r.URL.Query())
		}
		out := []map[string]any{}
		for _, p := range a.projects {
			out = append(out, a.projectJSON(p))
		}
		writeJSON(w, 200, out)
	}))
	h("GET /projects/{pid}", a.lock(func(w http.ResponseWriter, r *http.Request) {
		if p := a.project(r); p != nil {
			writeJSON(w, 200, a.projectJSON(p))
			return
		}
		writeJSON(w, 404, map[string]string{"message": "404 Project Not Found"})
	}))
	h("PUT /projects/{pid}", a.lock(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&p)
		proj := a.project(r)
		proj.Resolved = p["only_allow_merge_if_all_discussions_are_resolved"]
		writeJSON(w, 200, a.projectJSON(proj))
	}))
	h("GET /projects/{pid}/hooks", a.lock(func(w http.ResponseWriter, r *http.Request) {
		if a.failProject {
			writeJSON(w, 500, map[string]string{"message": "boom"})
			return
		}
		writeJSON(w, 200, a.project(r).Hooks)
	}))
	h("POST /projects/{pid}/hooks", a.lock(func(w http.ResponseWriter, r *http.Request) {
		p := a.project(r)
		p.Hooks = a.create(w, r, p.Hooks)
	}))
	h("PUT /projects/{pid}/hooks/{hid}", a.lock(func(w http.ResponseWriter, r *http.Request) { a.update(w, r, a.project(r).Hooks) }))
	h("DELETE /projects/{pid}/hooks/{hid}", a.lock(func(w http.ResponseWriter, r *http.Request) {
		p := a.project(r)
		p.Hooks = a.remove(w, r, p.Hooks)
	}))
	return a
}

func (a *adminFake) lock(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.denyAll {
			writeJSON(w, 403, map[string]string{"message": "403 Forbidden"})
			return
		}
		h(w, r)
	}
}

func pathID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id
}

func (a *adminFake) project(r *http.Request) *fakeProject {
	key := r.PathValue("pid")
	for _, p := range a.projects {
		if strconv.FormatInt(p.ID, 10) == key || p.Path == key {
			return p
		}
	}
	return nil
}

func (a *adminFake) projectJSON(p *fakeProject) map[string]any {
	return map[string]any{"id": p.ID, "path_with_namespace": p.Path, "only_allow_merge_if_all_discussions_are_resolved": p.Resolved}
}

func (a *adminFake) create(w http.ResponseWriter, r *http.Request, hooks []storedHook) []storedHook {
	var h storedHook
	_ = json.NewDecoder(r.Body).Decode(&h)
	a.nextID++
	h["id"] = float64(a.nextID)
	writeJSON(w, 201, h)
	return append(hooks, h)
}

func (a *adminFake) update(w http.ResponseWriter, r *http.Request, hooks []storedHook) {
	var body storedHook
	_ = json.NewDecoder(r.Body).Decode(&body)
	for _, h := range hooks {
		if strconv.FormatFloat(h["id"].(float64), 'f', -1, 64) == r.PathValue("hid") {
			for k, v := range body {
				h[k] = v
			}
			writeJSON(w, 200, h)
			return
		}
	}
	writeJSON(w, 404, map[string]string{})
}

func (a *adminFake) remove(w http.ResponseWriter, r *http.Request, hooks []storedHook) []storedHook {
	out := hooks[:0]
	for _, h := range hooks {
		if strconv.FormatFloat(h["id"].(float64), 'f', -1, 64) != r.PathValue("hid") {
			out = append(out, h)
		}
	}
	w.WriteHeader(204)
	return out
}

func (a *adminFake) registrar(bot string) *HookRegistrar {
	return NewHookRegistrar(a.srv.URL, adminTok, hookURL, hookToken, bot, a.srv.Client(), (&sleeps{}).sleep)
}

func (a *adminFake) ours(hooks []storedHook) []storedHook {
	var out []storedHook
	for _, h := range hooks {
		if h["url"] == hookURL {
			out = append(out, h)
		}
	}
	return out
}

func changeLines(cs []HookChange) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.String())
	}
	return out
}

func TestHookDryRunWritesNothing(t *testing.T) {
	a := newAdminFake(t)
	var out bytes.Buffer
	changes, err := a.registrar("pr-review").Run(context.Background(), HookOptions{Mode: HookDryRun}, &out)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"create system hook",
		"add-member group team (bot pr-review as Developer)",
		"delete group team hook (77, not sent on Free tier)",
		"create project team/svc hook",
		"enable project team/svc (all threads must be resolved)",
		"create project ops/infra hook",
	}
	if got := changeLines(changes); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("changes:\n%s", strings.Join(got, "\n"))
	}
	if len(a.writes()) != 0 {
		t.Fatalf("dry run wrote: %+v", a.writes())
	}
	if !strings.HasSuffix(out.String(), "\ndry run, nothing changed: 6 change(s)\n") || !strings.HasPrefix(out.String(), "create system hook\n") {
		t.Fatalf("output:\n%s", out.String())
	}
	for _, r := range a.requests("") {
		if r.Token != adminTok {
			t.Fatalf("request without admin token: %+v", r)
		}
	}
}

func TestHookApplyTwice(t *testing.T) {
	a := newAdminFake(t)
	r := a.registrar("pr-review")
	ctx := context.Background()
	var out bytes.Buffer
	changes, err := r.Run(ctx, HookOptions{Mode: HookApply}, &out)
	if err != nil || len(changes) != 6 || !strings.HasSuffix(out.String(), "\ndone: 6 change(s)\n") {
		t.Fatalf("first apply: %v %v\n%s", changes, err, out.String())
	}
	sys := a.ours(a.system)
	if len(sys) != 1 || sys[0]["merge_requests_events"] != true || sys[0]["push_events"] != false || sys[0]["token"] != hookToken {
		t.Fatalf("system hook %+v", sys)
	}
	for _, p := range a.projects {
		ph := a.ours(p.Hooks)
		if len(ph) != 1 || ph[0]["note_events"] != true || ph[0]["merge_requests_events"] != false || ph[0]["token"] != hookToken || !p.Resolved {
			t.Fatalf("project %s: %+v resolved=%v", p.Path, ph, p.Resolved)
		}
	}
	if !a.members[1][testBotID] || len(a.ours(a.groupHooks[1])) != 0 || len(a.groupHooks[1]) != 1 {
		t.Fatalf("groups: members %v hooks %v", a.members, a.groupHooks)
	}

	out.Reset()
	changes, err = r.Run(ctx, HookOptions{Mode: HookApply}, &out)
	if err != nil || len(changes) != 0 || !strings.HasSuffix(out.String(), "\ndone: 0 change(s)\n") {
		t.Fatalf("second apply: %v %v\n%s", changes, err, out.String())
	}
	if !strings.Contains(out.String(), "keep   system hook (") || !strings.Contains(out.String(), "keep   project team/svc hook (") {
		t.Fatalf("keep lines missing:\n%s", out.String())
	}
	if len(a.ours(a.system)) != 1 || len(a.ours(a.projects[0].Hooks)) != 1 {
		t.Fatal("second apply duplicated hooks")
	}
}

func TestHookRemove(t *testing.T) {
	a := newAdminFake(t)
	ctx := context.Background()
	if _, err := a.registrar("pr-review").Run(ctx, HookOptions{Mode: HookApply}, nil); err != nil {
		t.Fatal(err)
	}
	a.groupHooks[2] = []storedHook{{"id": float64(90), "url": hookURL}}
	before := len(a.requests(""))
	var out bytes.Buffer
	changes, err := a.registrar("").Run(ctx, HookOptions{Mode: HookRemove}, &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Action != "delete" {
			t.Fatalf("remove made a non-delete change: %v", c)
		}
	}
	if len(changes) != 4 || len(a.ours(a.system)) != 0 || len(a.ours(a.projects[0].Hooks)) != 0 || len(a.ours(a.groupHooks[2])) != 0 {
		t.Fatalf("remove: %v", changeLines(changes))
	}
	if len(a.system) != 1 || len(a.projects[0].Hooks) != 1 || !a.members[1][testBotID] || !a.projects[0].Resolved {
		t.Fatal("remove touched hooks of other URLs, membership or the merge setting")
	}
	for _, r := range a.requests("")[before:] {
		if r.Path == "/users" || strings.Contains(r.Path, "/members/") {
			t.Fatalf("remove looked up the bot: %+v", r)
		}
	}
}

func TestHookSingleProject(t *testing.T) {
	a := newAdminFake(t)
	ctx := context.Background()
	var out bytes.Buffer
	changes, err := a.registrar("").Run(ctx, HookOptions{Mode: HookApply, Project: "team/svc"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	want := "create project team/svc hook\nenable project team/svc (all threads must be resolved)"
	if strings.Join(changeLines(changes), "\n") != want {
		t.Fatalf("changes %v", changeLines(changes))
	}
	ph := a.ours(a.projects[0].Hooks)
	if len(ph) != 1 || ph[0]["merge_requests_events"] != true || ph[0]["note_events"] != true {
		t.Fatalf("project hook %+v", ph)
	}
	for _, r := range a.requests("") {
		if !strings.HasPrefix(r.Path, "/projects/team%2Fsvc") && !strings.HasPrefix(r.Path, "/projects/42") {
			t.Fatalf("single-project run touched %s %s", r.Method, r.Path)
		}
	}
	if len(a.ours(a.system)) != 0 || a.projects[1].Hooks != nil {
		t.Fatal("other scopes changed")
	}

	changes, err = a.registrar("").Run(ctx, HookOptions{Mode: HookRemove, Project: "team/svc"}, nil)
	if err != nil || len(changes) != 1 || len(a.ours(a.projects[0].Hooks)) != 0 || !a.projects[0].Resolved {
		t.Fatalf("single-project remove: %v %v", changeLines(changes), err)
	}

	if _, err := a.registrar("").Run(ctx, HookOptions{Mode: HookDryRun, Project: "team/missing"}, nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing project: %v", err)
	}
}

func TestHookErrors(t *testing.T) {
	ctx := context.Background()
	a := newAdminFake(t)
	if _, err := a.registrar("ghost").Run(ctx, HookOptions{Mode: HookDryRun}, nil); err == nil || !strings.Contains(err.Error(), `bot user "ghost" not found`) {
		t.Fatalf("missing bot: %v", err)
	}

	denied := newAdminFake(t)
	denied.denyAll = true
	var out bytes.Buffer
	_, err := denied.registrar("pr-review").Run(ctx, HookOptions{Mode: HookApply}, &out)
	if !isStatus(err, 403) || strings.Contains(out.String(), "done") {
		t.Fatalf("non-admin token: %v\n%s", err, out.String())
	}
	if len(denied.requests("")) != 1 {
		t.Fatalf("run continued after 403: %d requests", len(denied.requests("")))
	}

	broken := newAdminFake(t)
	broken.failProject = true
	changes, err := broken.registrar("pr-review").Run(ctx, HookOptions{Mode: HookDryRun}, nil)
	if !isStatus(err, 500) || len(changes) != 3 {
		t.Fatalf("project failure: %v %v", changeLines(changes), err)
	}

	for _, tc := range []struct {
		name string
		r    *HookRegistrar
		opts HookOptions
		want string
	}{
		{"mode", a.registrar("pr-review"), HookOptions{Mode: "--apply"}, "unknown mode"},
		{"hook url", NewHookRegistrar(a.srv.URL, adminTok, "ftp://x", hookToken, "b", nil, nil), HookOptions{Mode: HookDryRun}, "hook URL"},
		{"empty hook url", NewHookRegistrar(a.srv.URL, adminTok, " ", hookToken, "b", nil, nil), HookOptions{Mode: HookDryRun}, "hook URL"},
		{"secret", NewHookRegistrar(a.srv.URL, adminTok, hookURL, "", "b", nil, nil), HookOptions{Mode: HookApply, Project: "team/svc"}, "secret"},
		{"bot", NewHookRegistrar(a.srv.URL, adminTok, hookURL, hookToken, "", nil, nil), HookOptions{Mode: HookDryRun}, "bot username"},
		{"token", NewHookRegistrar(a.srv.URL, "", hookURL, hookToken, "b", nil, nil), HookOptions{Mode: HookDryRun}, "token"},
	} {
		if _, err := tc.r.Run(ctx, tc.opts, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

type failingWriter struct{ after int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("write failed")
	}
	w.after--
	return len(p), nil
}

func TestHookOutputErrors(t *testing.T) {
	ctx := context.Background()
	a := newAdminFake(t)
	if _, err := a.registrar("pr-review").Run(ctx, HookOptions{Mode: HookDryRun}, &failingWriter{}); err == nil {
		t.Error("change line failure: want error")
	}
	if _, err := a.registrar("pr-review").Run(ctx, HookOptions{Mode: HookApply}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.registrar("pr-review").Run(ctx, HookOptions{Mode: HookDryRun}, &failingWriter{}); err == nil {
		t.Error("keep line failure: want error")
	}
	a.groups, a.projects = nil, nil
	if _, err := a.registrar("pr-review").Run(ctx, HookOptions{Mode: HookDryRun}, &failingWriter{after: 1}); err == nil {
		t.Error("summary line failure: want error")
	}
}

func TestHookAPIFailuresStopTheRun(t *testing.T) {
	ctx := context.Background()
	steps := []string{
		"GET /hooks", "GET /users", "GET /groups", "GET /groups/1/members/99", "POST /groups/1/members",
		"GET /groups/2/hooks", "DELETE /groups/1/hooks/77", "GET /projects", "POST /projects/42/hooks",
		"PUT /projects/42", "PUT /hooks/1",
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			a := newAdminFake(t)
			a.groupHooks[2] = []storedHook{}
			if step == "PUT /hooks/1" {
				a.system[0]["url"] = hookURL
			}
			a.fail(step, 500)
			if _, err := a.registrar("pr-review").Run(ctx, HookOptions{Mode: HookApply}, nil); !isStatus(err, 500) {
				t.Fatalf("want 500, got %v", err)
			}
		})
	}
	gone := newAdminFake(t)
	gone.fail("DELETE /groups/1/hooks/77", 404)
	if _, err := gone.registrar("pr-review").Run(ctx, HookOptions{Mode: HookApply}, nil); err != nil {
		t.Fatalf("delete of a hook already gone: %v", err)
	}
	a := newAdminFake(t)
	a.fail("GET /projects/team%2Fsvc", 500)
	if _, err := a.registrar("").Run(ctx, HookOptions{Mode: HookDryRun, Project: "team/svc"}, nil); !isStatus(err, 500) {
		t.Fatalf("project lookup: %v", err)
	}
}
