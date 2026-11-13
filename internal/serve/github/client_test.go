// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

const (
	testAppID  = 123
	testSlug   = "ocr-test"
	testBot    = "ocr-test[bot]"
	testSecret = "s3cret"
	testInst   = 4242
	headSHA    = "1111111111111111111111111111111111111111"
	baseSHA    = "2222222222222222222222222222222222222222"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func pkcs1PEM(t *testing.T) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey(t))})
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type failure struct {
	status int
	body   string
	header map[string]string
}

// fakeAPI emulates the GitHub REST and GraphQL endpoints the forge uses. It
// keeps posted comments so later reads see them.
type fakeAPI struct {
	t     *testing.T
	srv   *httptest.Server
	clock *fakeClock

	mu             sync.Mutex
	calls          []string
	fail           map[string][]failure
	override       map[string]http.HandlerFunc
	pageSize       int
	tokenTTL       time.Duration
	tokenCount     int
	appCount       int
	token          string
	pull           map[string]any
	files          []map[string]any
	issueComments  []map[string]any
	reviewComments []map[string]any
	reviews        []map[string]any
	singlePosts    []map[string]any
	patches        []map[string]any
	reactions      []string
	contents       map[string]map[string]any
	threads        []map[string]any
	threadPage     int
	graphqlVars    []map[string]any
	nextID         int64
	// reviewReject, when set, decides the status of each createReview call.
	reviewReject func(comments []any) (int, string)
	// singleReject, when set, decides the status of each single comment post.
	singleReject func(payload map[string]any) (int, string)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	fa := &fakeAPI{
		t:        t,
		clock:    &fakeClock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)},
		fail:     map[string][]failure{},
		override: map[string]http.HandlerFunc{},
		pageSize: 100,
		tokenTTL: time.Hour,
		contents: map[string]map[string]any{},
		nextID:   1000,
		pull: map[string]any{
			"number": 7, "title": "Add retry", "body": "Retries the import.", "state": "open", "draft": false, "merged": false,
			"user":   map[string]any{"login": "alice", "type": "User"},
			"labels": []any{map[string]any{"name": "backend"}},
			"head":   map[string]any{"ref": "feat/retry", "sha": headSHA},
			"base":   map[string]any{"ref": "main", "sha": baseSHA},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app", fa.handleApp)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", fa.handleToken)
	mux.HandleFunc("GET /repos/acme/widgets/pulls/7", fa.installation(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, fa.pull)
	}))
	mux.HandleFunc("GET /repos/acme/widgets/pulls/7/files", fa.installation(func(w http.ResponseWriter, r *http.Request) {
		fa.page(w, r, fa.files)
	}))
	mux.HandleFunc("GET /repos/acme/widgets/pulls/7/comments", fa.installation(func(w http.ResponseWriter, r *http.Request) {
		fa.page(w, r, fa.reviewComments)
	}))
	mux.HandleFunc("POST /repos/acme/widgets/pulls/7/comments", fa.installation(fa.handleSingle))
	mux.HandleFunc("POST /repos/acme/widgets/pulls/7/reviews", fa.installation(fa.handleReview))
	mux.HandleFunc("GET /repos/acme/widgets/issues/7/comments", fa.installation(func(w http.ResponseWriter, r *http.Request) {
		fa.page(w, r, fa.issueComments)
	}))
	mux.HandleFunc("POST /repos/acme/widgets/issues/7/comments", fa.installation(func(w http.ResponseWriter, r *http.Request) {
		payload := decodeBody(t, r)
		c := fa.addComment(&fa.issueComments, payload["body"].(string), testBot, "Bot")
		writeJSON(w, 201, c)
	}))
	mux.HandleFunc("PATCH /repos/acme/widgets/issues/comments/{id}", fa.installation(func(w http.ResponseWriter, r *http.Request) {
		payload := decodeBody(t, r)
		payload["id"] = r.PathValue("id")
		fa.patches = append(fa.patches, payload)
		writeJSON(w, 200, payload)
	}))
	mux.HandleFunc("POST /repos/acme/widgets/issues/comments/{id}/reactions", fa.installation(func(w http.ResponseWriter, r *http.Request) {
		payload := decodeBody(t, r)
		fa.reactions = append(fa.reactions, r.PathValue("id")+":"+payload["content"].(string))
		writeJSON(w, 201, map[string]any{"id": 1})
	}))
	mux.HandleFunc("GET /repos/acme/widgets/contents/{path...}", fa.installation(func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("path") + "@" + r.URL.Query().Get("ref")
		c, ok := fa.contents[key]
		if !ok {
			writeJSON(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		writeJSON(w, 200, c)
	}))
	mux.HandleFunc("POST /graphql", fa.installation(fa.handleGraphQL))
	fa.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		defer fa.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		fa.calls = append(fa.calls, key)
		if q := fa.fail[key]; len(q) > 0 {
			fa.fail[key] = q[1:]
			for k, v := range q[0].header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(q[0].status)
			_, _ = io.WriteString(w, q[0].body)
			return
		}
		if h, ok := fa.override[key]; ok {
			h(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(fa.srv.Close)
	return fa
}

func (fa *fakeAPI) forge(t *testing.T) (*Forge, *[]time.Duration) {
	t.Helper()
	var slept []time.Duration
	var mu sync.Mutex
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return nil
	}
	f, err := New(serveconfig.GitHubConfig{
		AppID: testAppID, PrivateKeyPEM: pkcs1PEM(t), WebhookSecret: testSecret, APIURL: fa.srv.URL + "/",
	}, fa.srv.Client(), fa.clock.now, sleep)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f, &slept
}

func (fa *fakeAPI) count(key string) int {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	n := 0
	for _, c := range fa.calls {
		if c == key {
			n++
		}
	}
	return n
}

func (fa *fakeAPI) handleApp(w http.ResponseWriter, r *http.Request) {
	fa.checkJWT(r)
	fa.appCount++
	writeJSON(w, 200, map[string]any{"slug": testSlug})
}

func (fa *fakeAPI) handleToken(w http.ResponseWriter, r *http.Request) {
	fa.checkJWT(r)
	fa.tokenCount++
	fa.token = "tok-" + strconv.Itoa(fa.tokenCount)
	writeJSON(w, 201, map[string]any{
		"token":      fa.token,
		"expires_at": fa.clock.now().Add(fa.tokenTTL).Format(time.RFC3339),
	})
}

func (fa *fakeAPI) checkJWT(r *http.Request) {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		fa.t.Errorf("app call without bearer JWT")
		return
	}
	if _, err := verifyJWT(&rsaKey(fa.t).PublicKey, jwt); err != nil {
		fa.t.Errorf("app call with bad JWT: %v", err)
	}
}

func (fa *fakeAPI) installation(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fa.token == "" || r.Header.Get("Authorization") != "Bearer "+fa.token {
			writeJSON(w, 401, map[string]any{"message": "Bad credentials"})
			return
		}
		if r.Header.Get("X-GitHub-Api-Version") != apiVersion || r.Header.Get("Accept") != "application/vnd.github+json" {
			fa.t.Errorf("%s %s: missing API headers", r.Method, r.URL.Path)
		}
		h(w, r)
	}
}

func (fa *fakeAPI) page(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	if r.URL.Query().Get("per_page") != "100" {
		fa.t.Errorf("%s: per_page = %q", r.URL.Path, r.URL.Query().Get("per_page"))
	}
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	start := min((page-1)*fa.pageSize, len(items))
	end := min(start+fa.pageSize, len(items))
	if end < len(items) {
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=%d>; rel="next", <%s%s?page=99>; rel="last"`,
			fa.srv.URL, r.URL.Path, page+1, fa.srv.URL, r.URL.Path))
	}
	out := items[start:end]
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

func (fa *fakeAPI) addComment(list *[]map[string]any, body, login, typ string) map[string]any {
	fa.nextID++
	c := map[string]any{"id": fa.nextID, "body": body, "user": map[string]any{"login": login, "type": typ}}
	*list = append(*list, c)
	return c
}

func (fa *fakeAPI) handleReview(w http.ResponseWriter, r *http.Request) {
	payload := decodeBody(fa.t, r)
	fa.reviews = append(fa.reviews, payload)
	comments, _ := payload["comments"].([]any)
	if fa.reviewReject != nil {
		if status, body := fa.reviewReject(comments); status != 0 {
			writeRaw(w, status, body)
			return
		}
	}
	for _, c := range comments {
		fa.addComment(&fa.reviewComments, c.(map[string]any)["body"].(string), testBot, "Bot")
	}
	writeJSON(w, 200, map[string]any{"id": 1})
}

func (fa *fakeAPI) handleSingle(w http.ResponseWriter, r *http.Request) {
	payload := decodeBody(fa.t, r)
	fa.singlePosts = append(fa.singlePosts, payload)
	if fa.singleReject != nil {
		if status, body := fa.singleReject(payload); status != 0 {
			writeRaw(w, status, body)
			return
		}
	}
	writeJSON(w, 201, fa.addComment(&fa.reviewComments, payload["body"].(string), testBot, "Bot"))
}

func (fa *fakeAPI) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	payload := decodeBody(fa.t, r)
	if !strings.Contains(payload["query"].(string), "reviewThreads(first: 100, after: $cursor)") {
		fa.t.Errorf("unexpected GraphQL query")
	}
	vars := payload["variables"].(map[string]any)
	fa.graphqlVars = append(fa.graphqlVars, vars)
	size := fa.threadPage
	if size == 0 {
		size = 100
	}
	start := 0
	if c, ok := vars["cursor"].(string); ok {
		start, _ = strconv.Atoi(c)
	}
	end := min(start+size, len(fa.threads))
	nodes := fa.threads[start:end]
	if nodes == nil {
		nodes = []map[string]any{}
	}
	writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"reviewThreads": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": end < len(fa.threads), "endCursor": strconv.Itoa(end)},
			"nodes":    nodes,
		},
	}}}})
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Errorf("decode request body: %v", err)
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	writeRaw(w, status, string(b))
}

func writeRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// verifyJWT checks an RS256 token against pub and returns its claims.
func verifyJWT(pub *rsa.PublicKey, token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("want 3 parts, got %d", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	return claims, json.Unmarshal(raw, &claims)
}

var testRef = forge.PRRef{Platform: forge.GitHub, Repo: "acme/widgets", InstallationID: testInst, Number: 7}

func TestGetPR(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	pr, err := f.GetPR(context.Background(), testRef)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	want := forge.PR{
		Ref: testRef, Title: "Add retry", Description: "Retries the import.", Author: "alice", Labels: []string{"backend"},
		SourceBranch: "feat/retry", TargetBranch: "main", HeadSHA: headSHA, BaseSHA: baseSHA, State: forge.StateOpen,
	}
	if fmt.Sprint(pr) != fmt.Sprint(want) {
		t.Fatalf("GetPR = %+v, want %+v", pr, want)
	}
}

func TestGetPRStates(t *testing.T) {
	cases := []struct {
		name   string
		patch  map[string]any
		state  string
		bot    bool
		draft  bool
		errMsg string
	}{
		{name: "merged flag", patch: map[string]any{"state": "closed", "merged": true}, state: forge.StateMerged},
		{name: "merged_at", patch: map[string]any{"state": "closed", "merged_at": "2026-11-12T00:00:00Z"}, state: forge.StateMerged},
		{name: "closed", patch: map[string]any{"state": "closed"}, state: forge.StateClosed},
		{name: "draft bot", patch: map[string]any{"draft": true, "user": map[string]any{"login": "x[bot]", "type": "Bot"}}, state: forge.StateOpen, bot: true, draft: true},
		{name: "no head sha", patch: map[string]any{"head": map[string]any{"ref": "x"}}, errMsg: "no head or base sha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAPI(t)
			for k, v := range tc.patch {
				fa.pull[k] = v
			}
			f, _ := fa.forge(t)
			pr, err := f.GetPR(context.Background(), testRef)
			if tc.errMsg != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errMsg) {
					t.Fatalf("err = %v, want %q", err, tc.errMsg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if pr.State != tc.state || pr.AuthorIsBot != tc.bot || pr.Draft != tc.draft {
				t.Fatalf("got state=%s bot=%v draft=%v", pr.State, pr.AuthorIsBot, pr.Draft)
			}
		})
	}
}

func TestGetPRErrors(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	bad := []forge.PRRef{
		{Repo: "acme", InstallationID: 1, Number: 1},
		{Repo: "acme/../x", InstallationID: 1, Number: 1},
		{Repo: "acme/..", InstallationID: 1, Number: 1},
		{Repo: "acme/widgets", Number: 1},
		{Repo: "acme/widgets", InstallationID: 1},
	}
	for _, ref := range bad {
		if _, err := f.GetPR(ctx, ref); err == nil {
			t.Errorf("GetPR(%+v) succeeded", ref)
		}
	}
	fa.override["GET /repos/acme/widgets/pulls/7"] = func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 200, "{") }
	if _, err := f.GetPR(ctx, testRef); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v", err)
	}
	fa.override["GET /repos/acme/widgets/pulls/7"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]any{"message": "Not Found"})
	}
	if _, err := f.GetPR(ctx, testRef); statusOf(err) != 404 || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v", err)
	}
}

func TestRetry(t *testing.T) {
	t.Run("secondary rate limit then success", func(t *testing.T) {
		fa := newFakeAPI(t)
		f, slept := fa.forge(t)
		fa.fail["GET /repos/acme/widgets/pulls/7"] = []failure{{status: 403, body: `{"message":"You have exceeded a secondary rate limit"}`}}
		if _, err := f.GetPR(context.Background(), testRef); err != nil {
			t.Fatal(err)
		}
		if len(*slept) != 1 || (*slept)[0] < 60*time.Second {
			t.Fatalf("slept = %v, want one wait of at least 60s", *slept)
		}
	})
	t.Run("429 with Retry-After", func(t *testing.T) {
		fa := newFakeAPI(t)
		f, slept := fa.forge(t)
		fa.fail["GET /repos/acme/widgets/pulls/7"] = []failure{{status: 429, header: map[string]string{"Retry-After": "7"}}}
		if _, err := f.GetPR(context.Background(), testRef); err != nil {
			t.Fatal(err)
		}
		if len(*slept) != 1 || (*slept)[0] != 7*time.Second {
			t.Fatalf("slept = %v, want [7s]", *slept)
		}
	})
	t.Run("5xx exhausted", func(t *testing.T) {
		fa := newFakeAPI(t)
		f, slept := fa.forge(t)
		fa.fail["GET /repos/acme/widgets/pulls/7"] = []failure{{status: 502}, {status: 502}, {status: 503}, {status: 500}, {status: 500}}
		_, err := f.GetPR(context.Background(), testRef)
		if statusOf(err) != 500 {
			t.Fatalf("err = %v, want HTTP 500", err)
		}
		if len(*slept) != maxAttempts-1 || fa.count("GET /repos/acme/widgets/pulls/7") != maxAttempts {
			t.Fatalf("slept %d times, %d sends", len(*slept), fa.count("GET /repos/acme/widgets/pulls/7"))
		}
	})
	t.Run("transport error", func(t *testing.T) {
		fa := newFakeAPI(t)
		f, _ := fa.forge(t)
		if _, err := f.BotLogin(context.Background()); err != nil {
			t.Fatal(err)
		}
		fa.srv.Close()
		if _, err := f.GetPR(context.Background(), testRef); err == nil {
			t.Fatal("want error from a closed server")
		}
	})
}

func TestUnauthorizedDropsToken(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	if _, err := f.GetPR(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	fa.fail["GET /repos/acme/widgets/pulls/7"] = []failure{{status: 401, body: `{"message":"Bad credentials"}`}}
	if _, err := f.GetPR(ctx, testRef); statusOf(err) != 401 {
		t.Fatalf("err = %v, want 401", err)
	}
	if _, err := f.GetPR(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	if fa.tokenCount != 2 {
		t.Fatalf("token fetched %d times, want 2", fa.tokenCount)
	}
}

func TestPagination(t *testing.T) {
	fa := newFakeAPI(t)
	fa.pageSize = 2
	for i := 0; i < 5; i++ {
		fa.addComment(&fa.issueComments, fmt.Sprintf("c%d", i), "alice", "User")
	}
	f, _ := fa.forge(t)
	var got []string
	truncated, err := f.list(context.Background(), testInst, "/repos/acme/widgets/issues/7/comments", func(raw json.RawMessage) error {
		var c ghComment
		_ = json.Unmarshal(raw, &c)
		got = append(got, c.Body)
		return nil
	})
	if err != nil || truncated {
		t.Fatalf("list: truncated=%v err=%v", truncated, err)
	}
	if strings.Join(got, ",") != "c0,c1,c2,c3,c4" {
		t.Fatalf("got %v", got)
	}
}

func TestPaginationCapAndErrors(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	path := "/repos/acme/widgets/issues/7/comments"
	fa.override["GET "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=2>; rel="next"`, fa.srv.URL, path))
		writeJSON(w, 200, []any{})
	}
	truncated, err := f.list(ctx, testInst, path, func(json.RawMessage) error { return nil })
	if err != nil || !truncated {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
	if n := fa.count("GET " + path); n != maxPages {
		t.Fatalf("pages fetched = %d, want %d", n, maxPages)
	}

	fa.override["GET "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://evil.example/steal?page=2>; rel="next"`)
		writeJSON(w, 200, []any{})
	}
	if _, err := f.list(ctx, testInst, path, func(json.RawMessage) error { return nil }); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v, want refusal of a foreign link", err)
	}

	fa.override["GET "+path] = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{}) }
	if _, err := f.list(ctx, testInst, path, func(json.RawMessage) error { return nil }); err == nil || !strings.Contains(err.Error(), "decode list") {
		t.Fatalf("err = %v", err)
	}

	fa.override["GET "+path] = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, []any{1}) }
	if _, err := f.list(ctx, testInst, path, func(json.RawMessage) error { return fmt.Errorf("stop") }); err == nil || err.Error() != "stop" {
		t.Fatalf("err = %v", err)
	}
}

func TestNextLink(t *testing.T) {
	cases := map[string]string{
		"": "",
		`<https://x/a?page=2>; rel="next", <https://x/a?page=5>; rel="last"`: "https://x/a?page=2",
		`<https://x/a?page=1>; rel="prev", <https://x/a?page=3>; rel="next"`: "https://x/a?page=3",
		`<https://x/a?page=1>; rel="first"`:                                  "",
	}
	for in, want := range cases {
		if got := nextLink(in); got != want {
			t.Errorf("nextLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadFile(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	content := `{"max_rounds": 2}`
	enc := base64.StdEncoding.EncodeToString([]byte(content))
	wrapped := enc[:10] + "\n" + enc[10:] + "\n"
	fa.contents[".opencodereview/serve.json@"+baseSHA] = map[string]any{
		"type": "file", "encoding": "base64", "size": len(content), "content": wrapped,
	}
	got, ok, err := f.ReadFile(ctx, testRef, baseSHA, serveconfig.RepoConfigPath, serveconfig.MaxRepoConfigBytes)
	if err != nil || !ok || string(got) != content {
		t.Fatalf("ReadFile = %q, %v, %v", got, ok, err)
	}

	got, ok, err = f.ReadFile(ctx, testRef, headSHA, serveconfig.RepoConfigPath, serveconfig.MaxRepoConfigBytes)
	if err != nil || ok || got != nil {
		t.Fatalf("missing file: %q, %v, %v", got, ok, err)
	}

	cases := []struct {
		name string
		file map[string]any
		want string
	}{
		{"directory", map[string]any{"type": "dir"}, "not a file"},
		{"too large", map[string]any{"type": "file", "encoding": "base64", "size": serveconfig.MaxRepoConfigBytes + 1}, "exceeds"},
		{"encoding", map[string]any{"type": "file", "encoding": "none", "size": 3}, "unexpected encoding"},
		{"bad base64", map[string]any{"type": "file", "encoding": "base64", "size": 3, "content": "!!!"}, "decode"},
		{"decoded too large", map[string]any{"type": "file", "encoding": "base64", "size": 1,
			"content": base64.StdEncoding.EncodeToString(make([]byte, serveconfig.MaxRepoConfigBytes+1))}, "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa.contents["x.json@"+baseSHA] = tc.file
			if _, _, err := f.ReadFile(ctx, testRef, baseSHA, "x.json", serveconfig.MaxRepoConfigBytes); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	big := strings.Repeat("r", serveconfig.MaxRepoConfigBytes+1)
	fa.contents["rules.md@"+baseSHA] = map[string]any{"type": "file", "encoding": "base64", "size": len(big),
		"content": base64.StdEncoding.EncodeToString([]byte(big))}
	if got, ok, err := f.ReadFile(ctx, testRef, baseSHA, "rules.md", len(big)); err != nil || !ok || string(got) != big {
		t.Fatalf("a file within the caller's limit must be read: %v %v", ok, err)
	}

	fa.override["GET /repos/acme/widgets/contents/arr.json"] = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, []any{}) }
	if _, _, err := f.ReadFile(ctx, testRef, baseSHA, "arr.json", serveconfig.MaxRepoConfigBytes); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v", err)
	}
	fa.fail["GET /repos/acme/widgets/contents/y.json"] = []failure{{status: 403, body: "forbidden"}}
	if _, _, err := f.ReadFile(ctx, testRef, baseSHA, "y.json", serveconfig.MaxRepoConfigBytes); statusOf(err) != 403 {
		t.Fatalf("err = %v", err)
	}
	for _, p := range []string{"", "a//b", "../x", "a/./b"} {
		if _, _, err := f.ReadFile(ctx, testRef, baseSHA, p, serveconfig.MaxRepoConfigBytes); err == nil {
			t.Errorf("ReadFile(%q) accepted", p)
		}
	}
	if _, _, err := f.ReadFile(ctx, testRef, "", "x.json", serveconfig.MaxRepoConfigBytes); err == nil {
		t.Error("empty sha accepted")
	}
	if _, _, err := f.ReadFile(ctx, forge.PRRef{Repo: "bad"}, baseSHA, "x.json", serveconfig.MaxRepoConfigBytes); err == nil {
		t.Error("bad ref accepted")
	}
}

func TestClone(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	pr := forge.PR{Ref: testRef, TargetBranch: "main"}
	spec, err := f.Clone(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	creds := base64.StdEncoding.EncodeToString([]byte("x-access-token:tok-1"))
	want := forge.CloneSpec{
		URL:        fa.srv.URL + "/acme/widgets.git",
		AuthHeader: "Authorization: Basic " + creds,
		HeadRef:    "refs/pull/7/head",
		BaseRef:    "refs/heads/main",
	}
	if spec != want {
		t.Fatalf("Clone = %+v, want %+v", spec, want)
	}
	if strings.Contains(spec.URL, "tok-") {
		t.Fatal("token leaked into the clone URL")
	}
	if _, err := f.Clone(ctx, forge.PR{Ref: testRef}); err == nil {
		t.Error("missing target branch accepted")
	}
	if _, err := f.Clone(ctx, forge.PR{Ref: forge.PRRef{Repo: "x"}}); err == nil {
		t.Error("bad ref accepted")
	}
	fa.fail["POST /app/installations/4243/access_tokens"] = []failure{{status: 404, body: "{}"}}
	other := testRef
	other.InstallationID = 4243
	if _, err := f.Clone(ctx, forge.PR{Ref: other, TargetBranch: "main"}); err == nil {
		t.Error("token failure ignored")
	}
}

func TestReact(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	for _, r := range []forge.Reaction{forge.ReactionEyes, forge.ReactionSuccess, forge.ReactionFailure} {
		if err := f.React(ctx, testRef, "9001", r); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(fa.reactions, ","); got != "9001:eyes,9001:rocket,9001:confused" {
		t.Fatalf("reactions = %s", got)
	}
	if err := f.React(ctx, testRef, "9001", "heart"); err == nil {
		t.Error("unknown reaction accepted")
	}
	for _, id := range []string{"", "abc", "0", "-1"} {
		if err := f.React(ctx, testRef, id, forge.ReactionEyes); err == nil {
			t.Errorf("comment id %q accepted", id)
		}
	}
	if err := f.React(ctx, forge.PRRef{Repo: "x"}, "1", forge.ReactionEyes); err == nil {
		t.Error("bad ref accepted")
	}
	fa.fail["POST /repos/acme/widgets/issues/comments/9001/reactions"] = []failure{{status: 422, body: "{}"}}
	if err := f.React(ctx, testRef, "9001", forge.ReactionEyes); err == nil {
		t.Error("API failure ignored")
	}
}

func TestAPIErrorText(t *testing.T) {
	e := &apiError{Method: "GET", URL: "https://x/y", Status: 500, Body: []byte(strings.Repeat("a", 400))}
	if msg := e.Error(); !strings.HasSuffix(msg, "...") || !strings.Contains(msg, "HTTP 500") {
		t.Fatalf("Error() = %q", msg)
	}
	if stripQuery("https://x/y?ref=1") != "https://x/y" || stripQuery("https://x/y") != "https://x/y" {
		t.Fatal("stripQuery")
	}
	if statusOf(fmt.Errorf("plain")) != 0 {
		t.Fatal("statusOf on a plain error")
	}
}

func TestSendEncodeError(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	if _, _, err := f.send(context.Background(), "x", http.MethodPost, fa.srv.URL+"/x", map[string]any{"c": make(chan int)}); err == nil {
		t.Fatal("want encode error")
	}
	if _, _, err := f.send(context.Background(), "x", http.MethodGet, "://bad", nil); err == nil {
		t.Fatal("want build error")
	}
}
