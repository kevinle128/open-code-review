// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// Forge talks to one GitLab instance with the bot's personal access token.
type Forge struct {
	cfg        serveconfig.GitLabConfig
	c          *client
	indicators []string

	mu      sync.Mutex
	botID   int64
	botName string
}

var _ forge.Forge = (*Forge)(nil)

// newMRRetryDelays follows pr-agent: a just-opened MR can answer 404, or come
// back without diff refs, for a few seconds after its webhook fires.
var newMRRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// New builds the forge. httpc and sleep may be nil for the defaults.
func New(cfg serveconfig.GitLabConfig, httpc *http.Client, sleep forge.Sleeper) (*Forge, error) {
	c, err := newClient(cfg.URL, cfg.Token, httpc, sleep)
	if err != nil {
		return nil, err
	}
	return &Forge{cfg: cfg, c: c, indicators: []string{"bot"}}, nil
}

// SetBotIndicators replaces the substrings that mark a sender username as a
// bot (case-insensitive). Call it before the forge serves events.
func (f *Forge) SetBotIndicators(indicators []string) {
	out := make([]string, 0, len(indicators))
	for _, s := range indicators {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	f.indicators = out
}

// Platform implements forge.Forge.
func (f *Forge) Platform() forge.Platform { return forge.GitLab }

type apiUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

// BotUserID returns the id of the token's user and caches it with the
// username, which ParseEvent then uses to recognize the service's own events.
func (f *Forge) BotUserID(ctx context.Context) (int64, error) {
	if id, _ := f.cachedBot(); id != 0 {
		return id, nil
	}
	var u apiUser
	if _, err := f.c.call(ctx, http.MethodGet, "/user", nil, nil, &u); err != nil {
		return 0, err
	}
	if u.ID <= 0 {
		return 0, errors.New("gitlab: GET /user returned no id")
	}
	f.mu.Lock()
	f.botID, f.botName = u.ID, u.Username
	f.mu.Unlock()
	return u.ID, nil
}

func (f *Forge) cachedBot() (int64, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.botID, f.botName
}

type apiDiffRefs struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	StartSHA string `json:"start_sha"`
}

type apiMR struct {
	IID            int          `json:"iid"`
	Title          string       `json:"title"`
	Author         apiUser      `json:"author"`
	Labels         []string     `json:"labels"`
	SourceBranch   string       `json:"source_branch"`
	TargetBranch   string       `json:"target_branch"`
	SHA            string       `json:"sha"`
	DiffRefs       *apiDiffRefs `json:"diff_refs"`
	Draft          *bool        `json:"draft"`
	WorkInProgress *bool        `json:"work_in_progress"`
	State          string       `json:"state"`
}

// GetPR reads the merge request, retrying while a new MR is not readable yet.
func (f *Forge) GetPR(ctx context.Context, ref forge.PRRef) (forge.PR, error) {
	path, err := mrPath(ref)
	if err != nil {
		return forge.PR{}, err
	}
	for attempt := 0; ; attempt++ {
		var mr apiMR
		_, err := f.c.call(ctx, http.MethodGet, path, nil, nil, &mr)
		// Closed and merged MRs are returned as they are: the caller only
		// needs their state to stop tracking them.
		notReady := err == nil && prState(mr.State) == forge.StateOpen &&
			(mr.SHA == "" || mr.DiffRefs == nil || mr.DiffRefs.BaseSHA == "")
		if err == nil && !notReady {
			return f.toPR(ref, mr), nil
		}
		if (err != nil && !isStatus(err, http.StatusNotFound)) || attempt >= len(newMRRetryDelays) {
			if err == nil {
				err = fmt.Errorf("gitlab: GET %s: merge request has no head or base commit yet", path)
			}
			return forge.PR{}, err
		}
		if err := f.c.sleep(ctx, newMRRetryDelays[attempt]); err != nil {
			return forge.PR{}, err
		}
	}
}

func (f *Forge) toPR(ref forge.PRRef, mr apiMR) forge.PR {
	ref.Platform = forge.GitLab
	var base string
	if mr.DiffRefs != nil {
		base = mr.DiffRefs.BaseSHA
	}
	return forge.PR{
		Ref:          ref,
		Title:        mr.Title,
		Author:       mr.Author.Username,
		AuthorIsBot:  mr.Author.Bot || f.isBotSender(hookUser{ID: mr.Author.ID, Username: mr.Author.Username}),
		Labels:       mr.Labels,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		HeadSHA:      mr.SHA,
		BaseSHA:      base,
		Draft:        isDraft(hookMR{Title: mr.Title, Draft: mr.Draft, WorkInProgress: mr.WorkInProgress}),
		State:        prState(mr.State),
	}
}

type apiNote struct {
	ID         int64   `json:"id"`
	Body       string  `json:"body"`
	Author     apiUser `json:"author"`
	System     bool    `json:"system"`
	Resolvable bool    `json:"resolvable"`
	Resolved   bool    `json:"resolved"`
}

type apiDiscussion struct {
	ID    string    `json:"id"`
	Notes []apiNote `json:"notes"`
}

// summaryNote returns the newest bot-authored note carrying the summary tag.
// Only the bot's notes count, so a human cannot plant a marker.
func (f *Forge) summaryNote(ctx context.Context, ref forge.PRRef) (*apiNote, error) {
	path, err := mrPath(ref)
	if err != nil {
		return nil, err
	}
	botID, err := f.BotUserID(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"order_by": {"created_at"}, "sort": {"asc"}}
	notes, err := paginate[apiNote](ctx, f.c, path+"/notes", q)
	if err != nil {
		return nil, err
	}
	var found *apiNote
	for i := range notes {
		n := &notes[i]
		if !n.System && n.Author.ID == botID && strings.Contains(n.Body, forge.SummaryTag) {
			found = n
		}
	}
	return found, nil
}

// GetSummary implements forge.Forge.
func (f *Forge) GetSummary(ctx context.Context, ref forge.PRRef) (*forge.Comment, error) {
	n, err := f.summaryNote(ctx, ref)
	if err != nil || n == nil {
		return nil, err
	}
	return &forge.Comment{ID: strconv.FormatInt(n.ID, 10), Body: n.Body}, nil
}

// UpsertSummary edits the existing summary in place and keeps its state
// marker when body has none. A failed lookup is an error rather than a new
// note, so a read outage never produces a second summary.
func (f *Forge) UpsertSummary(ctx context.Context, ref forge.PRRef, body string) error {
	n, err := f.summaryNote(ctx, ref)
	if err != nil {
		return err
	}
	if n == nil {
		return f.PostNote(ctx, ref, body)
	}
	path, _ := mrPath(ref)
	payload := map[string]string{"body": state.Preserve(body, n.Body)}
	_, err = f.c.call(ctx, http.MethodPut, path+"/notes/"+strconv.FormatInt(n.ID, 10), nil, payload, nil)
	return err
}

// PostNote implements forge.Forge.
func (f *Forge) PostNote(ctx context.Context, ref forge.PRRef, body string) error {
	path, err := mrPath(ref)
	if err != nil {
		return err
	}
	_, err = f.c.call(ctx, http.MethodPost, path+"/notes", nil, map[string]string{"body": body}, nil)
	return err
}

// botDiscussions lists the discussions whose first note the bot wrote.
func (f *Forge) botDiscussions(ctx context.Context, ref forge.PRRef) ([]apiDiscussion, error) {
	path, err := mrPath(ref)
	if err != nil {
		return nil, err
	}
	botID, err := f.BotUserID(ctx)
	if err != nil {
		return nil, err
	}
	all, err := paginate[apiDiscussion](ctx, f.c, path+"/discussions", nil)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, d := range all {
		if len(d.Notes) > 0 && d.Notes[0].Author.ID == botID && !d.Notes[0].System {
			out = append(out, d)
		}
	}
	return out, nil
}

// ListOCRThreads implements forge.Forge with one paginated read, because the
// poller calls it for every waiting MR.
func (f *Forge) ListOCRThreads(ctx context.Context, ref forge.PRRef) ([]forge.Thread, error) {
	ds, err := f.botDiscussions(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []forge.Thread
	for _, d := range ds {
		root := d.Notes[0]
		id, ok := forge.ParseFindingID(root.Body)
		if !ok || !root.Resolvable {
			continue
		}
		out = append(out, forge.Thread{ID: d.ID, FindingID: id, Resolved: root.Resolved})
	}
	return out, nil
}

var emojiNames = map[forge.Reaction]string{
	forge.ReactionEyes:    "eyes",
	forge.ReactionSuccess: "white_check_mark",
	forge.ReactionFailure: "x",
}

var alreadyAwardedRE = regexp.MustCompile(`(?i)already (been )?(taken|awarded)`)

// React awards an emoji on a note. An empty commentID is a command the
// platform raised itself (bot assigned as reviewer), with nothing to react
// on. An emoji the bot already awarded counts as success.
func (f *Forge) React(ctx context.Context, ref forge.PRRef, commentID string, r forge.Reaction) error {
	if commentID == "" {
		return nil
	}
	name, ok := emojiNames[r]
	if !ok {
		return fmt.Errorf("gitlab: unknown reaction %q", r)
	}
	if id, err := strconv.ParseInt(commentID, 10, 64); err != nil || id <= 0 {
		return fmt.Errorf("gitlab: invalid note id %q", commentID)
	}
	path, err := mrPath(ref)
	if err != nil {
		return err
	}
	_, err = f.c.call(ctx, http.MethodPost, path+"/notes/"+commentID+"/award_emoji", nil, map[string]string{"name": name}, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusConflict ||
		(apiErr.Status >= 400 && apiErr.Status < 500 && alreadyAwardedRE.MatchString(apiErr.Body))) {
		return nil
	}
	return err
}

type apiProject struct {
	ID            int64  `json:"id"`
	HTTPURLToRepo string `json:"http_url_to_repo"`
}

// Clone implements forge.Forge. The token goes in a Basic header, never in
// the URL, so it cannot leak through git's remote configuration or errors.
func (f *Forge) Clone(ctx context.Context, pr forge.PR) (forge.CloneSpec, error) {
	if pr.Ref.Number <= 0 {
		return forge.CloneSpec{}, fmt.Errorf("gitlab: invalid merge request iid %d", pr.Ref.Number)
	}
	if !validBranch(pr.TargetBranch) {
		return forge.CloneSpec{}, fmt.Errorf("gitlab: invalid target branch %q", pr.TargetBranch)
	}
	path, err := projectPath(pr.Ref)
	if err != nil {
		return forge.CloneSpec{}, err
	}
	var p apiProject
	if _, err := f.c.call(ctx, http.MethodGet, path, nil, nil, &p); err != nil {
		return forge.CloneSpec{}, err
	}
	if u, err := url.Parse(p.HTTPURLToRepo); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return forge.CloneSpec{}, fmt.Errorf("gitlab: project has no usable http_url_to_repo")
	}
	cred := base64.StdEncoding.EncodeToString([]byte("oauth2:" + f.cfg.Token))
	return forge.CloneSpec{
		URL:        p.HTTPURLToRepo,
		AuthHeader: "Authorization: Basic " + cred,
		HeadRef:    "refs/merge-requests/" + strconv.Itoa(pr.Ref.Number) + "/head",
		BaseRef:    "refs/heads/" + pr.TargetBranch,
	}, nil
}

// validBranch rejects names git would refuse or read as an option, since the
// value ends up in a fetch refspec.
func validBranch(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") || strings.Contains(name, "..") ||
		strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r) {
			return false
		}
	}
	return true
}

// ReadFile implements forge.Forge. Files above the per-repo config limit are
// an error rather than a silent truncation.
func (f *Forge) ReadFile(ctx context.Context, ref forge.PRRef, sha, path string) ([]byte, bool, error) {
	if strings.TrimSpace(sha) == "" || strings.TrimSpace(path) == "" {
		return nil, false, errors.New("gitlab: ReadFile needs a ref and a path")
	}
	pp, err := projectPath(ref)
	if err != nil {
		return nil, false, err
	}
	filePath := pp + "/repository/files/" + url.PathEscape(path) + "/raw"
	resp, body, err := f.c.raw(ctx, http.MethodGet, filePath, url.Values{"ref": {sha}}, nil, maxAttempts)
	if err != nil {
		return nil, false, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, false, newAPIError(http.MethodGet, filePath, resp.StatusCode, body)
	case len(body) > serveconfig.MaxRepoConfigBytes:
		return nil, false, fmt.Errorf("gitlab: %s is larger than %d bytes", path, serveconfig.MaxRepoConfigBytes)
	}
	return body, true, nil
}
