// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// HookMode selects what HookRegistrar.Run does.
type HookMode string

const (
	HookDryRun HookMode = "dry-run"
	HookApply  HookMode = "apply"
	HookRemove HookMode = "remove"
)

// HookOptions configures one registration run. Project limits the run to
// one project (path with namespace): no system hook and no group work, which
// lets one test project reach the service while the rest still goes
// elsewhere.
type HookOptions struct {
	Mode    HookMode
	Project string
}

// HookChange is one write the run made, or would make in a dry run.
type HookChange struct {
	Action string
	Target string
	Detail string
}

func (c HookChange) String() string {
	s := c.Action + " " + c.Target
	if c.Detail != "" {
		s += " (" + c.Detail + ")"
	}
	return s
}

const developerAccess = 30

// HookRegistrar connects a GitLab Free instance to the service:
//
//   - a system hook sends merge request events for every project, including
//     projects created later;
//   - a project hook per project sends note events, which system hooks never
//     send, so run it again after new projects appear;
//   - the bot is a Developer in every top-level group, so it can read and
//     comment everywhere;
//   - group hooks with the service URL are deleted, because GitLab sends them
//     only with a Premium license;
//   - every project requires all threads to be resolved before merge, which
//     turns open findings into a merge block.
//
// Hooks are matched by URL, so a run is idempotent. Remove deletes the hooks
// only; bot membership and the merge setting stay.
type HookRegistrar struct {
	baseURL     string
	adminToken  string
	hookURL     string
	hookSecret  string
	botUsername string
	httpc       *http.Client
	sleep       forge.Sleeper
}

// NewHookRegistrar builds a registrar. adminToken needs the api and
// admin_mode scopes; it is used for this run only.
func NewHookRegistrar(baseURL, adminToken, hookURL, hookSecret, botUsername string, httpc *http.Client, sleep forge.Sleeper) *HookRegistrar {
	return &HookRegistrar{
		baseURL:     baseURL,
		adminToken:  adminToken,
		hookURL:     strings.TrimSpace(hookURL),
		hookSecret:  hookSecret,
		botUsername: strings.TrimSpace(botUsername),
		httpc:       httpc,
		sleep:       sleep,
	}
}

type apiHook struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

type apiGroup struct {
	ID       int64  `json:"id"`
	FullPath string `json:"full_path"`
}

type apiHookProject struct {
	ID                  int64  `json:"id"`
	PathWithNamespace   string `json:"path_with_namespace"`
	OnlyAllowIfResolved *bool  `json:"only_allow_merge_if_all_discussions_are_resolved"`
}

type hookRun struct {
	r       *HookRegistrar
	c       *client
	mode    HookMode
	out     io.Writer
	changes []HookChange
}

// Run applies opts and writes one line per change (and per kept hook) to
// out, then a total line. It stops at the first API error other than a 404
// on a list or membership read, and returns the changes made so far.
func (r *HookRegistrar) Run(ctx context.Context, opts HookOptions, out io.Writer) ([]HookChange, error) {
	if out == nil {
		out = io.Discard
	}
	if err := r.validate(opts); err != nil {
		return nil, err
	}
	c, err := newClient(r.baseURL, r.adminToken, r.httpc, r.sleep)
	if err != nil {
		return nil, err
	}
	run := &hookRun{r: r, c: c, mode: opts.Mode, out: out}
	if project := strings.TrimSpace(opts.Project); project != "" {
		err = run.oneProject(ctx, project)
	} else {
		err = run.instance(ctx)
	}
	if err != nil {
		return run.changes, err
	}
	label := "done"
	if opts.Mode == HookDryRun {
		label = "dry run, nothing changed"
	}
	if _, err := fmt.Fprintf(out, "\n%s: %d change(s)\n", label, len(run.changes)); err != nil {
		return run.changes, err
	}
	return run.changes, nil
}

func (r *HookRegistrar) validate(opts HookOptions) error {
	switch opts.Mode {
	case HookDryRun, HookApply, HookRemove:
	default:
		return fmt.Errorf("gitlab hooks: unknown mode %q (want dry-run, apply or remove)", opts.Mode)
	}
	u, err := url.Parse(r.hookURL)
	if r.hookURL == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("gitlab hooks: hook URL must be an absolute http(s) URL, got %q", r.hookURL)
	}
	if opts.Mode == HookApply && r.hookSecret == "" {
		return errors.New("gitlab hooks: the webhook secret is required to apply")
	}
	if opts.Mode != HookRemove && strings.TrimSpace(opts.Project) == "" && r.botUsername == "" {
		return errors.New("gitlab hooks: the bot username is required")
	}
	return nil
}

func (h *hookRun) instance(ctx context.Context) error {
	system, err := h.listHooks(ctx, "/hooks")
	if err != nil {
		return err
	}
	if err := h.syncHook(ctx, "/hooks", "system", system, h.systemHookBody()); err != nil {
		return err
	}
	var botID int64
	if h.mode != HookRemove {
		if botID, err = h.botID(ctx); err != nil {
			return err
		}
	}
	groups, err := paginate[apiGroup](ctx, h.c, "/groups", url.Values{"top_level_only": {"true"}, "all_available": {"true"}})
	if err != nil {
		return err
	}
	for _, g := range groups {
		if err := h.group(ctx, g, botID); err != nil {
			return err
		}
	}
	projects, err := paginate[apiHookProject](ctx, h.c, "/projects", url.Values{"archived": {"false"}})
	if err != nil {
		return err
	}
	for _, p := range projects {
		if err := h.project(ctx, p, false); err != nil {
			return err
		}
	}
	return nil
}

func (h *hookRun) oneProject(ctx context.Context, path string) error {
	var p apiHookProject
	_, err := h.c.call(ctx, http.MethodGet, "/projects/"+url.PathEscape(path), nil, nil, &p)
	if isStatus(err, http.StatusNotFound) {
		return fmt.Errorf("gitlab hooks: project %q not found", path)
	}
	if err != nil {
		return err
	}
	return h.project(ctx, p, true)
}

func (h *hookRun) botID(ctx context.Context) (int64, error) {
	var users []apiUser
	if _, err := h.c.call(ctx, http.MethodGet, "/users", url.Values{"username": {h.r.botUsername}}, nil, &users); err != nil {
		return 0, err
	}
	if len(users) == 0 || users[0].ID <= 0 {
		return 0, fmt.Errorf("gitlab hooks: bot user %q not found", h.r.botUsername)
	}
	return users[0].ID, nil
}

func (h *hookRun) group(ctx context.Context, g apiGroup, botID int64) error {
	base := "/groups/" + strconv.FormatInt(g.ID, 10)
	if h.mode != HookRemove {
		_, err := h.c.call(ctx, http.MethodGet, base+"/members/"+strconv.FormatInt(botID, 10), nil, nil, nil)
		switch {
		case isStatus(err, http.StatusNotFound):
			change := HookChange{Action: "add-member", Target: "group " + g.FullPath, Detail: "bot " + h.r.botUsername + " as Developer"}
			body := map[string]int64{"user_id": botID, "access_level": developerAccess}
			if err := h.write(ctx, change, http.MethodPost, base+"/members", body); err != nil {
				return err
			}
		case err != nil:
			return err
		}
	}
	hooks, err := h.listHooks(ctx, base+"/hooks")
	if err != nil {
		return err
	}
	for _, hook := range hooks {
		if hook.URL != h.r.hookURL {
			continue
		}
		change := HookChange{Action: "delete", Target: "group " + g.FullPath + " hook", Detail: strconv.FormatInt(hook.ID, 10) + ", not sent on Free tier"}
		if err := h.write(ctx, change, http.MethodDelete, base+"/hooks/"+strconv.FormatInt(hook.ID, 10), nil); err != nil {
			return err
		}
	}
	return nil
}

func (h *hookRun) project(ctx context.Context, p apiHookProject, withMerges bool) error {
	base := "/projects/" + strconv.FormatInt(p.ID, 10)
	hooks, err := h.listHooks(ctx, base+"/hooks")
	if err != nil {
		return err
	}
	if err := h.syncHook(ctx, base+"/hooks", "project "+p.PathWithNamespace, hooks, h.projectHookBody(withMerges)); err != nil {
		return err
	}
	if h.mode == HookRemove || (p.OnlyAllowIfResolved != nil && *p.OnlyAllowIfResolved) {
		return nil
	}
	change := HookChange{Action: "enable", Target: "project " + p.PathWithNamespace, Detail: "all threads must be resolved"}
	return h.write(ctx, change, http.MethodPut, base, map[string]bool{"only_allow_merge_if_all_discussions_are_resolved": true})
}

// syncHook creates, refreshes or deletes the hooks under base whose URL is
// the service URL. A kept hook is refreshed on apply, which also rotates its
// secret, but is not counted as a change.
func (h *hookRun) syncHook(ctx context.Context, base, name string, hooks []apiHook, body map[string]any) error {
	var matching []apiHook
	for _, hook := range hooks {
		if hook.URL == h.r.hookURL {
			matching = append(matching, hook)
		}
	}
	if h.mode == HookRemove {
		for _, hook := range matching {
			change := HookChange{Action: "delete", Target: name + " hook", Detail: strconv.FormatInt(hook.ID, 10)}
			if err := h.write(ctx, change, http.MethodDelete, base+"/"+strconv.FormatInt(hook.ID, 10), nil); err != nil {
				return err
			}
		}
		return nil
	}
	if len(matching) == 0 {
		return h.write(ctx, HookChange{Action: "create", Target: name + " hook"}, http.MethodPost, base, body)
	}
	id := strconv.FormatInt(matching[0].ID, 10)
	if _, err := fmt.Fprintf(h.out, "keep   %s hook (%s), refresh settings\n", name, id); err != nil {
		return err
	}
	if h.mode != HookApply {
		return nil
	}
	_, err := h.c.call(ctx, http.MethodPut, base+"/"+id, nil, body, nil)
	return err
}

// write prints and records a change, and sends it unless this is a dry run.
func (h *hookRun) write(ctx context.Context, change HookChange, method, path string, body any) error {
	if _, err := fmt.Fprintln(h.out, change.String()); err != nil {
		return err
	}
	h.changes = append(h.changes, change)
	if h.mode == HookDryRun {
		return nil
	}
	_, err := h.c.call(ctx, method, path, nil, body, nil)
	// A retried DELETE whose first try landed finds nothing left to delete.
	if method == http.MethodDelete && isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// listHooks treats 404 as no hooks: GitLab answers 404 for hook lists a
// license tier or a missing feature does not provide.
func (h *hookRun) listHooks(ctx context.Context, path string) ([]apiHook, error) {
	hooks, err := paginate[apiHook](ctx, h.c, path, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	return hooks, err
}

func (h *hookRun) systemHookBody() map[string]any {
	return h.withSecret(map[string]any{
		"url":                      h.r.hookURL,
		"merge_requests_events":    true,
		"push_events":              false,
		"tag_push_events":          false,
		"repository_update_events": false,
		"enable_ssl_verification":  true,
	})
}

// projectHookBody sends note events only when the system hook covers merge
// request events; a single-project run has no system hook, so its project
// hook carries both.
func (h *hookRun) projectHookBody(withMerges bool) map[string]any {
	return h.withSecret(map[string]any{
		"url":                     h.r.hookURL,
		"note_events":             true,
		"merge_requests_events":   withMerges,
		"push_events":             false,
		"enable_ssl_verification": true,
	})
}

// withSecret adds the token only on apply, so a dry run never sends it.
func (h *hookRun) withSecret(body map[string]any) map[string]any {
	if h.mode == HookApply {
		body["token"] = h.r.hookSecret
	}
	return body
}
