// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package gitlab implements the review service forge and the webhook
// registration command for a self-managed GitLab instance.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

const (
	maxAttempts = 4
	perPage     = 100
	maxPages    = 50
	errBodyCap  = 300
)

// client is a minimal GitLab REST v4 client. Paths passed to it are already
// escaped, so a project path keeps its "%2F" through url.Parse.
type client struct {
	api   string
	token string
	httpc *http.Client
	sleep forge.Sleeper
}

func newClient(baseURL, token string, httpc *http.Client, sleep forge.Sleeper) (*client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("gitlab: invalid base URL %q", baseURL)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("gitlab: token is required")
	}
	if httpc == nil {
		httpc = http.DefaultClient
	}
	if sleep == nil {
		sleep = forge.SleepContext
	}
	return &client{api: u.String() + "/api/v4", token: token, httpc: httpc, sleep: sleep}, nil
}

// APIError is a non-2xx GitLab answer. The body is clipped so an error
// message never carries a large payload into logs.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitlab: %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func newAPIError(method, path string, status int, body []byte) *APIError {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > errBodyCap {
		text = text[:errBodyCap] + "..."
	}
	return &APIError{Method: method, Path: path, Status: status, Body: text}
}

func isStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// raw sends one request through forge.Do and returns the response without
// judging its status. attempts is 1 for writes that must not be repeated
// blindly.
func (c *client) raw(ctx context.Context, method, path string, query url.Values, payload any, attempts int) (*http.Response, []byte, error) {
	target := c.api + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var data []byte
	if payload != nil {
		var err error
		if data, err = json.Marshal(payload); err != nil {
			return nil, nil, fmt.Errorf("gitlab: encode %s %s: %w", method, path, err)
		}
	}
	build := func() (*http.Request, error) {
		var body io.Reader
		if data != nil {
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("PRIVATE-TOKEN", c.token)
		req.Header.Set("Accept", "application/json")
		if data != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}
	resp, body, err := forge.Do(ctx, c.httpc, build, c.sleep, attempts)
	if err != nil {
		return resp, body, fmt.Errorf("gitlab: %s %s: %w", method, path, err)
	}
	return resp, body, nil
}

// call sends a request, fails on a non-2xx status and decodes the JSON
// answer into out when out is not nil.
func (c *client) call(ctx context.Context, method, path string, query url.Values, payload, out any) (*http.Response, error) {
	var (
		resp *http.Response
		body []byte
		err  error
	)
	if method == http.MethodPost {
		resp, body, err = c.post(ctx, path, query, payload)
	} else {
		resp, body, err = c.raw(ctx, method, path, query, payload, maxAttempts)
	}
	if err != nil {
		return resp, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, newAPIError(method, path, resp.StatusCode, body)
	}
	if out != nil && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return resp, fmt.Errorf("gitlab: decode %s %s: %w", method, path, err)
		}
	}
	return resp, nil
}

// post retries only rate-limit answers, which GitLab sends before it applies
// the request. A 5xx or 408 may hide a hook, member or note that was created,
// and a blind retry would duplicate it.
func (c *client) post(ctx context.Context, path string, query url.Values, payload any) (*http.Response, []byte, error) {
	for attempt := 0; ; attempt++ {
		resp, body, err := c.raw(ctx, http.MethodPost, path, query, payload, 1)
		if err != nil || attempt+1 >= maxAttempts || resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout {
			return resp, body, err
		}
		delay, retry := forge.RetryDelay(resp, body, attempt, time.Now(), nil)
		if !retry {
			return resp, body, nil
		}
		if err := c.sleep(ctx, delay); err != nil {
			return nil, nil, err
		}
	}
}

// paginate reads every page of a list endpoint by following X-Next-Page. It
// fails rather than return a silently truncated list when the cap is hit,
// because callers use the full list to prove absence.
func paginate[T any](ctx context.Context, c *client, path string, query url.Values) ([]T, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", strconv.Itoa(perPage))
	var all []T
	page := 1
	for n := 0; n < maxPages; n++ {
		q.Set("page", strconv.Itoa(page))
		var items []T
		resp, err := c.call(ctx, http.MethodGet, path, q, nil, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		next := strings.TrimSpace(resp.Header.Get("X-Next-Page"))
		if next == "" {
			return all, nil
		}
		p, err := strconv.Atoi(next)
		if err != nil || p <= page {
			return nil, fmt.Errorf("gitlab: GET %s: bad X-Next-Page %q", path, next)
		}
		page = p
	}
	return nil, fmt.Errorf("gitlab: GET %s: more than %d pages", path, maxPages)
}

// projectPath returns "/projects/<id>" using the numeric id when known, else
// the escaped path with namespace.
func projectPath(ref forge.PRRef) (string, error) {
	if ref.ProjectID > 0 {
		return "/projects/" + strconv.FormatInt(ref.ProjectID, 10), nil
	}
	if strings.TrimSpace(ref.Repo) == "" {
		return "", errors.New("gitlab: project id and path are both empty")
	}
	return "/projects/" + url.PathEscape(ref.Repo), nil
}

func mrPath(ref forge.PRRef) (string, error) {
	p, err := projectPath(ref)
	if err != nil {
		return "", err
	}
	if ref.Number <= 0 {
		return "", fmt.Errorf("gitlab: invalid merge request iid %d", ref.Number)
	}
	return p + "/merge_requests/" + strconv.Itoa(ref.Number), nil
}
