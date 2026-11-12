// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package serveconfig loads the settings of the `ocr serve` webhook service
// from the environment and merges per-repository overrides.
package serveconfig

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Environment variable names.
const (
	EnvAddr                = "OCR_SERVE_ADDR"
	EnvMaxRounds           = "OCR_SERVE_MAX_ROUNDS"
	EnvPollInterval        = "OCR_SERVE_POLL_INTERVAL"
	EnvWorkers             = "OCR_SERVE_WORKERS"
	EnvReviewTimeout       = "OCR_SERVE_REVIEW_TIMEOUT"
	EnvMaxBodyBytes        = "OCR_SERVE_MAX_BODY_BYTES"
	EnvDataDir             = "OCR_SERVE_DATA_DIR"
	EnvWorkDir             = "OCR_SERVE_WORK_DIR"
	EnvIgnoreRepos         = "OCR_SERVE_IGNORE_REPOS"
	EnvIgnoreAuthors       = "OCR_SERVE_IGNORE_AUTHORS"
	EnvIgnoreTitles        = "OCR_SERVE_IGNORE_TITLES"
	EnvIgnoreSourceBranch  = "OCR_SERVE_IGNORE_SOURCE_BRANCHES"
	EnvIgnoreTargetBranch  = "OCR_SERVE_IGNORE_TARGET_BRANCHES"
	EnvIgnoreLabels        = "OCR_SERVE_IGNORE_LABELS"
	EnvBotIndicators       = "OCR_SERVE_BOT_INDICATORS"
	EnvGitHubAppID         = "OCR_SERVE_GITHUB_APP_ID"
	EnvGitHubPrivateKey    = "OCR_SERVE_GITHUB_PRIVATE_KEY"
	EnvGitHubWebhookSecret = "OCR_SERVE_GITHUB_WEBHOOK_SECRET"
	EnvGitHubAPIURL        = "OCR_SERVE_GITHUB_API_URL"
	EnvGitLabURL           = "OCR_SERVE_GITLAB_URL"
	EnvGitLabToken         = "OCR_SERVE_GITLAB_TOKEN"
	EnvGitLabWebhookSecret = "OCR_SERVE_GITLAB_WEBHOOK_SECRET"
	EnvGitLabAdminToken    = "OCR_SERVE_GITLAB_ADMIN_TOKEN"
	EnvGitLabHookURL       = "OCR_SERVE_GITLAB_HOOK_URL"
	EnvGitLabBotUsername   = "OCR_SERVE_GITLAB_BOT_USERNAME"
)

// Defaults and bounds.
const (
	DefaultAddr          = "127.0.0.1:3012"
	DefaultMaxRounds     = 3
	DefaultPollInterval  = 2 * time.Minute
	DefaultWorkers       = 2
	DefaultReviewTimeout = time.Hour
	DefaultMaxBodyBytes  = 10 << 20
	DefaultDataDir       = "/data"
	DefaultWorkDir       = "/work"
	DefaultGitHubAPIURL  = "https://api.github.com"

	MinRounds        = 1
	MaxRounds        = 10
	MinWorkers       = 1
	MaxWorkers       = 16
	MaxBodyBytesHigh = 100 << 20
)

// Config is the global service configuration.
type Config struct {
	Addr          string
	MaxRounds     int
	PollInterval  time.Duration
	ReviewTimeout time.Duration
	Workers       int
	MaxBodyBytes  int64
	DataDir       string
	WorkDir       string
	Ignore        IgnoreRules
	BotIndicators []string
	GitHub        *GitHubConfig // nil = disabled
	GitLab        *GitLabConfig // nil = disabled
}

// GitHubConfig holds the GitHub App settings.
type GitHubConfig struct {
	AppID         int64
	PrivateKeyPEM []byte
	WebhookSecret string
	APIURL        string
}

// GitLabConfig holds the GitLab settings. AdminToken, HookURL and BotUsername
// are used only by the hook registration command; the admin token is passed
// per invocation and never kept in the service environment.
type GitLabConfig struct {
	URL           string
	Token         string
	WebhookSecret string
	AdminToken    string
	HookURL       string
	BotUsername   string
}

// IgnoreRules skip automatic reviews. Patterns are RE2 and match anywhere in
// the value, as pr-agent's re.search did; labels match exactly.
type IgnoreRules struct {
	Repos          []*regexp.Regexp
	Authors        []*regexp.Regexp
	Titles         []*regexp.Regexp
	SourceBranches []*regexp.Regexp
	TargetBranches []*regexp.Regexp
	Labels         []string
}

// IgnoreSubject is the PR data that ignore rules look at.
type IgnoreSubject struct {
	Repo         string
	Author       string
	Title        string
	SourceBranch string
	TargetBranch string
	Labels       []string
}

// Match reports whether a rule matches s and names it as "<field>:<rule>".
// Empty values never match, so a payload without a title is not ignored by a
// title rule such as ".*".
func (r IgnoreRules) Match(s IgnoreSubject) (bool, string) {
	checks := []struct {
		field string
		res   []*regexp.Regexp
		value string
	}{
		{"repo", r.Repos, s.Repo},
		{"author", r.Authors, s.Author},
		{"title", r.Titles, s.Title},
		{"source_branch", r.SourceBranches, s.SourceBranch},
		{"target_branch", r.TargetBranches, s.TargetBranch},
	}
	for _, c := range checks {
		if c.value == "" {
			continue
		}
		for _, re := range c.res {
			if re.MatchString(c.value) {
				return true, c.field + ":" + re.String()
			}
		}
	}
	for _, want := range r.Labels {
		for _, have := range s.Labels {
			if have == want {
				return true, "label:" + want
			}
		}
	}
	return false, ""
}

// Load reads the configuration. getenv and readFile are injected for tests;
// nil means os.Getenv and os.ReadFile. Secrets also accept a "<NAME>_FILE"
// variable that points to a file holding the value.
//
// GitHub is enabled by OCR_SERVE_GITHUB_APP_ID and then needs a private key
// and a webhook secret. GitLab is enabled by OCR_SERVE_GITLAB_URL; its token
// and webhook secret are checked by RequireServe because the hook command
// can run a dry run without them.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	l := loader{getenv: getenv, readFile: readFile}
	c := &Config{
		Addr:          l.str(EnvAddr, DefaultAddr),
		MaxRounds:     l.intIn(EnvMaxRounds, DefaultMaxRounds, MinRounds, MaxRounds),
		PollInterval:  l.duration(EnvPollInterval, DefaultPollInterval),
		ReviewTimeout: l.duration(EnvReviewTimeout, DefaultReviewTimeout),
		Workers:       l.intIn(EnvWorkers, DefaultWorkers, MinWorkers, MaxWorkers),
		MaxBodyBytes:  int64(l.intIn(EnvMaxBodyBytes, DefaultMaxBodyBytes, 1, MaxBodyBytesHigh)),
		DataDir:       l.str(EnvDataDir, DefaultDataDir),
		WorkDir:       l.str(EnvWorkDir, DefaultWorkDir),
		Ignore: IgnoreRules{
			Repos:          l.regexps(EnvIgnoreRepos),
			Authors:        l.regexps(EnvIgnoreAuthors),
			Titles:         l.regexps(EnvIgnoreTitles),
			SourceBranches: l.regexps(EnvIgnoreSourceBranch),
			TargetBranches: l.regexps(EnvIgnoreTargetBranch),
			Labels:         splitList(getenv(EnvIgnoreLabels)),
		},
		BotIndicators: l.botIndicators(),
	}
	c.GitHub = l.github()
	c.GitLab = l.gitlab()
	if err := errors.Join(l.errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// RequireServe checks what the webhook service needs on top of Load: at
// least one platform, and the GitLab token and webhook secret when GitLab is
// enabled.
func (c *Config) RequireServe() error {
	if c.GitHub == nil && c.GitLab == nil {
		return fmt.Errorf("no platform enabled: set %s or %s", EnvGitHubAppID, EnvGitLabURL)
	}
	var errs []error
	if c.GitLab != nil {
		if c.GitLab.Token == "" {
			errs = append(errs, fmt.Errorf("%s is required when %s is set", EnvGitLabToken, EnvGitLabURL))
		}
		if c.GitLab.WebhookSecret == "" {
			errs = append(errs, fmt.Errorf("%s is required when %s is set", EnvGitLabWebhookSecret, EnvGitLabURL))
		}
	}
	return errors.Join(errs...)
}

type loader struct {
	getenv   func(string) string
	readFile func(string) ([]byte, error)
	errs     []error
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

func (l *loader) str(key, def string) string {
	if v := strings.TrimSpace(l.getenv(key)); v != "" {
		return v
	}
	return def
}

func (l *loader) intIn(key string, def, lo, hi int) int {
	v := strings.TrimSpace(l.getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		l.fail("%s must be an integer in %d..%d, got %q", key, lo, hi, v)
		return def
	}
	return n
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(l.getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		l.fail("%s must be a positive duration such as 2m, got %q", key, v)
		return def
	}
	return d
}

// regexps splits a comma-separated list, so a pattern cannot contain a comma.
func (l *loader) regexps(key string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range splitList(l.getenv(key)) {
		re, err := regexp.Compile(p)
		if err != nil {
			l.fail("%s: invalid pattern %q: %v", key, p, err)
			continue
		}
		out = append(out, re)
	}
	return out
}

func (l *loader) botIndicators() []string {
	raw := l.getenv(EnvBotIndicators)
	if strings.TrimSpace(raw) == "" {
		return []string{"bot"}
	}
	var out []string
	for _, s := range splitList(raw) {
		out = append(out, strings.ToLower(s))
	}
	return out
}

// secret reads key or key_FILE. Setting both is an error so an operator never
// wonders which one is in force. Surrounding whitespace, typically the
// trailing newline of a secret file, is dropped.
func (l *loader) secret(key string) string {
	direct := l.getenv(key)
	path := strings.TrimSpace(l.getenv(key + "_FILE"))
	switch {
	case direct != "" && path != "":
		l.fail("set only one of %s and %s_FILE", key, key)
		return ""
	case path != "":
		b, err := l.readFile(path)
		if err != nil {
			l.fail("%s_FILE: %w", key, err)
			return ""
		}
		return strings.TrimSpace(string(b))
	default:
		return strings.TrimSpace(direct)
	}
}

func (l *loader) github() *GitHubConfig {
	rawID := strings.TrimSpace(l.getenv(EnvGitHubAppID))
	if rawID == "" {
		return nil
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		l.fail("%s must be a positive integer, got %q", EnvGitHubAppID, rawID)
	}
	g := &GitHubConfig{
		AppID:         id,
		PrivateKeyPEM: []byte(unescapePEM(l.secret(EnvGitHubPrivateKey))),
		WebhookSecret: l.secret(EnvGitHubWebhookSecret),
		APIURL:        l.baseURL(EnvGitHubAPIURL, DefaultGitHubAPIURL),
	}
	if len(g.PrivateKeyPEM) == 0 {
		l.fail("%s or %s_FILE is required when %s is set", EnvGitHubPrivateKey, EnvGitHubPrivateKey, EnvGitHubAppID)
	} else if err := checkRSAKey(g.PrivateKeyPEM); err != nil {
		l.fail("%s: %v", EnvGitHubPrivateKey, err)
	}
	if g.WebhookSecret == "" {
		l.fail("%s is required when %s is set", EnvGitHubWebhookSecret, EnvGitHubAppID)
	}
	return g
}

func (l *loader) gitlab() *GitLabConfig {
	if strings.TrimSpace(l.getenv(EnvGitLabURL)) == "" {
		return nil
	}
	g := &GitLabConfig{
		URL:           l.baseURL(EnvGitLabURL, ""),
		Token:         l.secret(EnvGitLabToken),
		WebhookSecret: l.secret(EnvGitLabWebhookSecret),
		AdminToken:    l.secret(EnvGitLabAdminToken),
		BotUsername:   strings.TrimSpace(l.getenv(EnvGitLabBotUsername)),
	}
	if strings.TrimSpace(l.getenv(EnvGitLabHookURL)) != "" {
		g.HookURL = l.baseURL(EnvGitLabHookURL, "")
	}
	return g
}

// baseURL accepts an absolute http(s) URL without query or fragment and
// drops a trailing slash so callers can append paths.
func (l *loader) baseURL(key, def string) string {
	raw := l.str(key, def)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		l.fail("%s must be an absolute http(s) URL without query, got %q", key, raw)
		return raw
	}
	return strings.TrimRight(raw, "/")
}

// unescapePEM turns a key given on one line with literal "\n" escapes back
// into PEM. Stack environments and .env lines cannot hold real newlines, so
// that is the only way to pass the key without a file. A value that already
// has a real newline is left as is.
func unescapePEM(v string) string {
	if strings.Contains(v, "\n") || !strings.Contains(v, `\n`) {
		return v
	}
	return strings.ReplaceAll(v, `\n`, "\n")
}

// checkRSAKey fails early on a key the App JWT signer could not use.
func checkRSAKey(pemBytes []byte) error {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return errors.New("no PEM block found")
	}
	if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("not an RSA private key: %w", err)
	}
	if _, ok := key.(*rsa.PrivateKey); !ok {
		return errors.New("not an RSA private key")
	}
	return nil
}

func splitList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// RepoDefaults is the per-repository configuration before serve.json applies.
func (c *Config) RepoDefaults() RepoConfig {
	return RepoConfig{Enabled: true, MaxRounds: c.MaxRounds}
}
