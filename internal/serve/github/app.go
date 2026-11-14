// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package github implements forge.Forge for a GitHub App: App JWT and
// installation tokens, webhook verification and parsing, REST and GraphQL
// reads, and finding posting.
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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

const (
	// GitHub rejects an App JWT whose iat is in the future, so iat is set back
	// to absorb clock drift; exp stays under the 10 minute maximum.
	jwtBackdate = 60 * time.Second
	jwtLifetime = 9 * time.Minute
	// A cached installation token is replaced this long before it expires, so
	// a token handed to a git fetch still has time left when the fetch runs.
	tokenRefreshMargin = 5 * time.Minute
	defaultHTTPTimeout = 60 * time.Second
)

// Forge is the GitHub App client. It is safe for concurrent use.
type Forge struct {
	appID      int64
	key        *rsa.PrivateKey
	secret     []byte
	apiURL     string
	api        *url.URL
	graphqlURL string
	webURL     string
	httpc      *http.Client
	now        func() time.Time
	sleep      forge.Sleeper
	log        *slog.Logger

	tokenMu sync.Mutex
	tokens  map[int64]*tokenSlot

	botMu    sync.Mutex
	botLogin string
	botID    int64
}

// tokenSlot caches one installation's token. Its own lock is held across a
// fetch so concurrent callers wait for one request instead of each minting a
// token, while other installations are not blocked.
type tokenSlot struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

var _ forge.Forge = (*Forge)(nil)

// New builds a Forge from the App settings. A nil httpc gets a client with a
// timeout, a nil now uses time.Now, and a nil sleep uses forge.SleepContext.
func New(cfg serveconfig.GitHubConfig, httpc *http.Client, now func() time.Time, sleep forge.Sleeper) (*Forge, error) {
	if cfg.AppID <= 0 {
		return nil, errors.New("github: app id must be positive")
	}
	if cfg.WebhookSecret == "" {
		return nil, errors.New("github: webhook secret is required")
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	apiURL := strings.TrimRight(cfg.APIURL, "/")
	if apiURL == "" {
		apiURL = serveconfig.DefaultGitHubAPIURL
	}
	api, err := url.Parse(apiURL)
	if err != nil || (api.Scheme != "https" && api.Scheme != "http") || api.Host == "" {
		return nil, fmt.Errorf("github: invalid API URL %q", cfg.APIURL)
	}
	if httpc == nil {
		httpc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = forge.SleepContext
	}
	return &Forge{
		appID:      cfg.AppID,
		key:        key,
		secret:     []byte(cfg.WebhookSecret),
		apiURL:     apiURL,
		api:        api,
		graphqlURL: graphqlEndpoint(apiURL),
		webURL:     webBase(api),
		httpc:      httpc,
		now:        now,
		sleep:      sleep,
		log:        slog.Default(),
		tokens:     map[int64]*tokenSlot{},
	}, nil
}

// Platform returns forge.GitHub.
func (f *Forge) Platform() forge.Platform { return forge.GitHub }

func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("private key: no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key: not an RSA private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key: not an RSA private key")
	}
	return key, nil
}

// graphqlEndpoint maps the REST base to the GraphQL endpoint. GitHub
// Enterprise Server serves REST under /api/v3 and GraphQL under /api/graphql.
func graphqlEndpoint(apiURL string) string {
	if base, ok := strings.CutSuffix(apiURL, "/api/v3"); ok {
		return base + "/api/graphql"
	}
	return apiURL + "/graphql"
}

// webBase is the host that serves git over HTTPS for the API host.
func webBase(api *url.URL) string {
	if strings.EqualFold(api.Host, "api.github.com") {
		return "https://github.com"
	}
	path := strings.TrimSuffix(strings.TrimRight(api.Path, "/"), "/api/v3")
	return api.Scheme + "://" + api.Host + path
}

// appJWT signs the RS256 token that authenticates as the App itself.
func (f *Forge) appJWT() (string, error) {
	now := f.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": strconv.FormatInt(f.appID, 10),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (f *Forge) slot(installationID int64) *tokenSlot {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	s, ok := f.tokens[installationID]
	if !ok {
		s = &tokenSlot{}
		f.tokens[installationID] = s
	}
	return s
}

// installationToken returns the cached token for the installation, or
// fetches a new one when it is missing or near expiry.
func (f *Forge) installationToken(ctx context.Context, installationID int64) (string, error) {
	if installationID <= 0 {
		return "", errors.New("github: installation id is required")
	}
	s := f.slot(installationID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value != "" && f.now().Before(s.expires.Add(-tokenRefreshMargin)) {
		return s.value, nil
	}
	jwt, err := f.appJWT()
	if err != nil {
		return "", err
	}
	data, _, err := f.send(ctx, "Bearer "+jwt, http.MethodPost,
		f.apiURL+"/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", nil)
	if err != nil {
		return "", fmt.Errorf("github: installation token: %w", err)
	}
	var resp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("github: installation token: decode: %w", err)
	}
	expires, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if resp.Token == "" || err != nil {
		return "", errors.New("github: installation token: response has no token or expiry")
	}
	s.value, s.expires = resp.Token, expires
	return resp.Token, nil
}

// dropToken forgets a token the API rejected, so the next call mints a new one.
func (f *Forge) dropToken(installationID int64) {
	s := f.slot(installationID)
	s.mu.Lock()
	s.value = ""
	s.mu.Unlock()
}

// BotLogin returns "<app-slug>[bot]", the login GitHub shows on everything
// the App posts. Only a successful lookup is cached.
func (f *Forge) BotLogin(ctx context.Context) (string, error) {
	f.botMu.Lock()
	defer f.botMu.Unlock()
	if f.botLogin != "" {
		return f.botLogin, nil
	}
	jwt, err := f.appJWT()
	if err != nil {
		return "", err
	}
	data, _, err := f.send(ctx, "Bearer "+jwt, http.MethodGet, f.apiURL+"/app", nil)
	if err != nil {
		return "", fmt.Errorf("github: get app: %w", err)
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(data, &app); err != nil {
		return "", fmt.Errorf("github: get app: decode: %w", err)
	}
	if app.Slug == "" {
		return "", errors.New("github: get app: response has no slug")
	}
	f.botLogin = app.Slug + "[bot]"
	return f.botLogin, nil
}
