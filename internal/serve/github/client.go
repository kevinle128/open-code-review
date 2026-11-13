// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

const (
	apiVersion = "2022-11-28"
	// Four sends match the three retries of the posting script this ports.
	maxAttempts = 4
	perPage     = 100
	// 50 pages of 100 bounds every list walk; GitHub itself caps a PR file
	// list at 3000 entries.
	maxPages        = 50
	errorBodyPrefix = 300
)

// apiError is a non-2xx answer after retries ran out. It keeps the status and
// body so callers can tell a 404 or a line-resolution 422 from other failures.
type apiError struct {
	Method string
	URL    string
	Status int
	Body   []byte
}

func (e *apiError) Error() string {
	body := strings.TrimSpace(string(e.Body))
	if len(body) > errorBodyPrefix {
		body = body[:errorBodyPrefix] + "..."
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, body)
}

func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// send performs one API call with retries. auth is the full Authorization
// value. The query string is dropped from error text because it may carry a
// ref but never a secret; the token only ever travels in the header.
func (f *Forge) send(ctx context.Context, auth, method, rawURL string, payload any) ([]byte, http.Header, error) {
	return f.sendN(ctx, auth, method, rawURL, payload, maxAttempts)
}

// sendN is send with a given number of attempts.
func (f *Forge) sendN(ctx context.Context, auth, method, rawURL string, payload any, attempts int) ([]byte, http.Header, error) {
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return nil, nil, fmt.Errorf("encode request: %w", err)
		}
	}
	build := func() (*http.Request, error) {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, rawURL, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", auth)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		req.Header.Set("User-Agent", "ocr-serve")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}
	resp, data, err := forge.Do(ctx, f.httpc, build, f.sleep, attempts)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, stripQuery(rawURL), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.Header, &apiError{Method: method, URL: stripQuery(rawURL), Status: resp.StatusCode, Body: data}
	}
	return data, resp.Header, nil
}

func stripQuery(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// call sends an installation-authenticated request. A 401 drops the cached
// token so a revoked or rotated token heals on the next call.
func (f *Forge) call(ctx context.Context, installationID int64, method, rawURL string, payload any) ([]byte, http.Header, error) {
	return f.callN(ctx, installationID, method, rawURL, payload, maxAttempts)
}

// callN is call with a given number of attempts.
func (f *Forge) callN(ctx context.Context, installationID int64, method, rawURL string, payload any, attempts int) ([]byte, http.Header, error) {
	token, err := f.installationToken(ctx, installationID)
	if err != nil {
		return nil, nil, err
	}
	data, h, err := f.sendN(ctx, "Bearer "+token, method, rawURL, payload, attempts)
	if statusOf(err) == http.StatusUnauthorized {
		f.dropToken(installationID)
	}
	return data, h, err
}

// create sends a POST that adds a comment or a review. GitHub can answer 5xx
// or 408 after it applied the write, so such an answer is not resent
// blindly: landed, when given, re-lists the App's comments and reports
// whether the write is there; without it the error is returned. A rate limit
// is retried as usual, because GitHub applied nothing.
func (f *Forge) create(ctx context.Context, installationID int64, rawURL string, payload any, landed func(context.Context) (bool, error)) error {
	for attempt := 0; ; attempt++ {
		_, h, err := f.callN(ctx, installationID, http.MethodPost, rawURL, payload, 1)
		var ae *apiError
		if err == nil || !errors.As(err, &ae) {
			return err
		}
		delay, retry := forge.RetryDelay(&http.Response{StatusCode: ae.Status, Header: h}, ae.Body, attempt, f.now(), nil)
		maybeApplied := ae.Status >= 500 || ae.Status == http.StatusRequestTimeout
		if !retry || attempt+1 >= maxAttempts || (maybeApplied && landed == nil) {
			return err
		}
		if serr := f.sleep(ctx, delay); serr != nil {
			return serr
		}
		if !maybeApplied {
			continue
		}
		found, lerr := landed(ctx)
		if lerr != nil {
			return fmt.Errorf("%w; not retried because the duplicate check failed: %v", err, lerr)
		}
		if found {
			return nil
		}
	}
}

// list walks a paginated REST collection, calling each for every element.
// truncated is true when the page cap stopped the walk with pages left.
func (f *Forge) list(ctx context.Context, installationID int64, path string, each func(json.RawMessage) error) (truncated bool, err error) {
	next := f.apiURL + path + "?per_page=" + strconv.Itoa(perPage)
	for page := 0; next != ""; page++ {
		if page == maxPages {
			return true, nil
		}
		if err := f.sameOrigin(next); err != nil {
			return false, err
		}
		data, h, err := f.call(ctx, installationID, http.MethodGet, next, nil)
		if err != nil {
			return false, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return false, fmt.Errorf("GET %s: decode list: %w", stripQuery(next), err)
		}
		for _, item := range items {
			if err := each(item); err != nil {
				return false, err
			}
		}
		next = nextLink(h.Get("Link"))
	}
	return false, nil
}

var linkNextRE = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		if m := linkNextRE.FindStringSubmatch(part); m != nil {
			return m[1]
		}
	}
	return ""
}

// sameOrigin refuses a pagination link that leaves the API host, so the
// installation token is never sent anywhere else.
func (f *Forge) sameOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, f.api.Scheme) || !strings.EqualFold(u.Host, f.api.Host) {
		return fmt.Errorf("github: refusing pagination link outside %s", f.apiURL)
	}
	return nil
}

var repoPartRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// repoPath validates a PR reference and returns "/repos/{owner}/{name}".
func repoPath(ref forge.PRRef) (string, error) {
	owner, name, ok := strings.Cut(ref.Repo, "/")
	if !ok || !repoPartRE.MatchString(owner) || !repoPartRE.MatchString(name) || name == "." || name == ".." {
		return "", fmt.Errorf("github: invalid repository %q", ref.Repo)
	}
	if ref.InstallationID <= 0 {
		return "", fmt.Errorf("github: %s has no installation id", ref.Repo)
	}
	if ref.Number <= 0 {
		return "", fmt.Errorf("github: invalid pull request number %d", ref.Number)
	}
	return "/repos/" + owner + "/" + name, nil
}

func pullPath(ref forge.PRRef) (string, error) {
	base, err := repoPath(ref)
	if err != nil {
		return "", err
	}
	return base + "/pulls/" + strconv.Itoa(ref.Number), nil
}

func issuePath(ref forge.PRRef) (string, error) {
	base, err := repoPath(ref)
	if err != nil {
		return "", err
	}
	return base + "/issues/" + strconv.Itoa(ref.Number), nil
}

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type ghPull struct {
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	State    string  `json:"state"`
	Draft    bool    `json:"draft"`
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
	User     ghUser  `json:"user"`
	Labels   []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

func (p ghPull) toPR(ref forge.PRRef) forge.PR {
	state := forge.StateOpen
	switch {
	case p.Merged || (p.MergedAt != nil && *p.MergedAt != ""):
		state = forge.StateMerged
	case p.State == "closed":
		state = forge.StateClosed
	}
	labels := make([]string, 0, len(p.Labels))
	for _, l := range p.Labels {
		labels = append(labels, l.Name)
	}
	return forge.PR{
		Ref:          ref,
		Title:        p.Title,
		Description:  p.Body,
		Author:       p.User.Login,
		AuthorIsBot:  p.User.Type == "Bot",
		Labels:       labels,
		SourceBranch: p.Head.Ref,
		TargetBranch: p.Base.Ref,
		HeadSHA:      p.Head.SHA,
		BaseSHA:      p.Base.SHA,
		Draft:        p.Draft,
		State:        state,
	}
}

// GetPR reads the pull request.
func (f *Forge) GetPR(ctx context.Context, ref forge.PRRef) (forge.PR, error) {
	path, err := pullPath(ref)
	if err != nil {
		return forge.PR{}, err
	}
	data, _, err := f.call(ctx, ref.InstallationID, http.MethodGet, f.apiURL+path, nil)
	if err != nil {
		return forge.PR{}, fmt.Errorf("github: get pull request: %w", err)
	}
	var p ghPull
	if err := json.Unmarshal(data, &p); err != nil {
		return forge.PR{}, fmt.Errorf("github: get pull request: decode: %w", err)
	}
	if p.Head.SHA == "" || p.Base.SHA == "" {
		return forge.PR{}, errors.New("github: get pull request: response has no head or base sha")
	}
	return p.toPR(ref), nil
}

// ReadFile reads a small file at sha through the contents API.
func (f *Forge) ReadFile(ctx context.Context, ref forge.PRRef, sha, path string, maxBytes int) ([]byte, bool, error) {
	base, err := repoPath(ref)
	if err != nil {
		return nil, false, err
	}
	if sha == "" {
		return nil, false, errors.New("github: read file: sha is required")
	}
	escaped, err := escapePath(path)
	if err != nil {
		return nil, false, err
	}
	rawURL := f.apiURL + base + "/contents/" + escaped + "?ref=" + url.QueryEscape(sha)
	data, _, err := f.call(ctx, ref.InstallationID, http.MethodGet, rawURL, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("github: read file: %w", err)
	}
	var file struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, false, fmt.Errorf("github: read file %s: decode: %w", path, err)
	}
	if file.Type != "file" {
		return nil, false, fmt.Errorf("github: read file %s: not a file", path)
	}
	if file.Size > maxBytes {
		return nil, false, fmt.Errorf("github: read file %s: %d bytes exceeds %d", path, file.Size, maxBytes)
	}
	if file.Encoding != "base64" {
		return nil, false, fmt.Errorf("github: read file %s: unexpected encoding %q", path, file.Encoding)
	}
	// The API wraps base64 content at 60 columns.
	content, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(file.Content))
	if err != nil {
		return nil, false, fmt.Errorf("github: read file %s: decode: %w", path, err)
	}
	if len(content) > maxBytes {
		return nil, false, fmt.Errorf("github: read file %s: exceeds %d bytes", path, maxBytes)
	}
	return content, true, nil
}

func escapePath(path string) (string, error) {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", fmt.Errorf("github: invalid file path %q", path)
		}
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/"), nil
}

// Clone returns the fetch settings for the PR. Fork PRs need no access to the
// fork: GitHub mirrors every PR head as refs/pull/N/head on the base repo.
func (f *Forge) Clone(ctx context.Context, pr forge.PR) (forge.CloneSpec, error) {
	base, err := repoPath(pr.Ref)
	if err != nil {
		return forge.CloneSpec{}, err
	}
	if pr.TargetBranch == "" {
		return forge.CloneSpec{}, errors.New("github: clone: target branch is required")
	}
	token, err := f.installationToken(ctx, pr.Ref.InstallationID)
	if err != nil {
		return forge.CloneSpec{}, err
	}
	creds := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return forge.CloneSpec{
		URL:        f.webURL + strings.TrimPrefix(base, "/repos") + ".git",
		AuthHeader: "Authorization: Basic " + creds,
		HeadRef:    "refs/pull/" + strconv.Itoa(pr.Ref.Number) + "/head",
		BaseRef:    "refs/heads/" + pr.TargetBranch,
	}, nil
}

// GitHub has no check-mark or cross reaction, so success and failure map to
// the closest allowed ones; the orchestrator's reply carries the literal mark.
var reactionContent = map[forge.Reaction]string{
	forge.ReactionEyes:    "eyes",
	forge.ReactionSuccess: "rocket",
	forge.ReactionFailure: "confused",
}

// React adds a reaction to an issue comment.
func (f *Forge) React(ctx context.Context, ref forge.PRRef, commentID string, r forge.Reaction) error {
	base, err := repoPath(ref)
	if err != nil {
		return err
	}
	content, ok := reactionContent[r]
	if !ok {
		return fmt.Errorf("github: unknown reaction %q", r)
	}
	id, err := strconv.ParseInt(commentID, 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("github: invalid comment id %q", commentID)
	}
	rawURL := f.apiURL + base + "/issues/comments/" + strconv.FormatInt(id, 10) + "/reactions"
	if _, _, err := f.call(ctx, ref.InstallationID, http.MethodPost, rawURL, map[string]string{"content": content}); err != nil {
		return fmt.Errorf("github: react: %w", err)
	}
	return nil
}
