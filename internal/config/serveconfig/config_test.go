// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func files(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		v, ok := m[p]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(v), nil
	}
}

var (
	keyOnce               sync.Once
	pkcs1PEM, pkcs8PEM    string
	ecPEM, garbagePEMText string
)

func testKeys(t *testing.T) {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		pkcs1PEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		pkcs8PEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		ecDER, err := x509.MarshalPKCS8PrivateKey(ek)
		if err != nil {
			t.Fatal(err)
		}
		ecPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}))
		garbagePEMText = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")}))
	})
}

func TestLoad_Defaults(t *testing.T) {
	c, err := Load(env(nil), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Addr: DefaultAddr, MaxRounds: 3, PollInterval: 2 * time.Minute, ReviewTimeout: 2 * time.Hour,
		Workers: 2, MaxBodyBytes: 10485760, DataDir: "/data", WorkDir: "/work", BotIndicators: []string{"bot"},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v\nwant %+v", c, want)
	}
	if got := c.RepoDefaults(); !reflect.DeepEqual(got, RepoConfig{Enabled: true, MaxRounds: 3}) {
		t.Fatalf("repo defaults %+v", got)
	}
}

func TestLoad_NilFuncsUseProcessEnv(t *testing.T) {
	t.Setenv(EnvMaxRounds, "4")
	c, err := Load(nil, nil)
	if err != nil || c.MaxRounds != 4 {
		t.Fatalf("c=%+v err=%v", c, err)
	}
}

func TestLoad_Overrides(t *testing.T) {
	testKeys(t)
	c, err := Load(env(map[string]string{
		EnvAddr: "0.0.0.0:3012", EnvMaxRounds: "5", EnvPollInterval: "30s", EnvReviewTimeout: "3h",
		EnvWorkers: "4", EnvMaxBodyBytes: "2048", EnvDataDir: "/d", EnvWorkDir: "/w",
		EnvIgnoreRepos: "^infra/, sandbox$", EnvIgnoreAuthors: "renovate", EnvIgnoreTitles: `(?i)^\[wip\]`,
		EnvIgnoreSourceBranch: "^release/", EnvIgnoreTargetBranch: "^gh-pages$", EnvIgnoreLabels: "no-review, skip ocr",
		EnvBotIndicators: "Bot, Automation",
		EnvGitHubAppID:   "12345", EnvGitHubPrivateKey + "_FILE": "/run/key.pem", EnvGitHubWebhookSecret: "gh-secret",
		EnvGitHubAPIURL: "https://ghe.example.com/api/v3/",
		EnvGitLabURL:    "https://git.example.com/", EnvGitLabToken + "_FILE": "/run/gl", EnvGitLabWebhookSecret: " gl-secret ",
		EnvGitLabAdminToken: "admin", EnvGitLabHookURL: "https://hooks.example.com/ocr/gitlab/webhook", EnvGitLabBotUsername: "ocr-bot",
	}), files(map[string]string{"/run/key.pem": pkcs1PEM, "/run/gl": "glpat-x\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "0.0.0.0:3012" || c.MaxRounds != 5 || c.PollInterval != 30*time.Second || c.ReviewTimeout != 3*time.Hour ||
		c.Workers != 4 || c.MaxBodyBytes != 2048 || c.DataDir != "/d" || c.WorkDir != "/w" {
		t.Fatalf("scalars %+v", c)
	}
	if len(c.Ignore.Repos) != 2 || c.Ignore.Repos[1].String() != "sandbox$" || len(c.Ignore.Titles) != 1 ||
		!reflect.DeepEqual(c.Ignore.Labels, []string{"no-review", "skip ocr"}) {
		t.Fatalf("ignore %+v", c.Ignore)
	}
	if !reflect.DeepEqual(c.BotIndicators, []string{"bot", "automation"}) {
		t.Fatalf("bots %v", c.BotIndicators)
	}
	if c.GitHub == nil || c.GitHub.AppID != 12345 || c.GitHub.WebhookSecret != "gh-secret" ||
		c.GitHub.APIURL != "https://ghe.example.com/api/v3" || !strings.Contains(string(c.GitHub.PrivateKeyPEM), "RSA PRIVATE KEY") {
		t.Fatalf("github %+v", c.GitHub)
	}
	wantGL := &GitLabConfig{URL: "https://git.example.com", Token: "glpat-x", WebhookSecret: "gl-secret",
		AdminToken: "admin", HookURL: "https://hooks.example.com/ocr/gitlab/webhook", BotUsername: "ocr-bot"}
	if !reflect.DeepEqual(c.GitLab, wantGL) {
		t.Fatalf("gitlab %+v", c.GitLab)
	}
	if err := c.RequireServe(); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_GitHubPKCS8Key(t *testing.T) {
	testKeys(t)
	c, err := Load(env(map[string]string{EnvGitHubAppID: "1", EnvGitHubPrivateKey: pkcs8PEM, EnvGitHubWebhookSecret: "s"}), files(nil))
	if err != nil || c.GitHub.APIURL != DefaultGitHubAPIURL {
		t.Fatalf("c=%+v err=%v", c, err)
	}
}

// A key passed through a stack environment or a .env line arrives on one line
// with literal "\n" escapes; a key with real newlines must pass unchanged.
func TestLoad_GitHubKeyNewlineForms(t *testing.T) {
	testKeys(t)
	oneLine := strings.ReplaceAll(strings.TrimSpace(pkcs1PEM), "\n", `\n`)
	if strings.Contains(oneLine, "\n") {
		t.Fatal("escaped key still has a real newline")
	}
	for name, raw := range map[string]string{"escaped one line": oneLine, "real newlines": pkcs1PEM} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(env(map[string]string{EnvGitHubAppID: "1", EnvGitHubPrivateKey: raw, EnvGitHubWebhookSecret: "s"}), files(nil))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(c.GitHub.PrivateKeyPEM); got != strings.TrimSpace(pkcs1PEM) {
				t.Fatalf("key changed:\n%s", got)
			}
		})
	}
}

func TestUnescapePEM_KeepsBackslashWhenRealNewlinePresent(t *testing.T) {
	in := "line one\nliteral \\n stays"
	if got := unescapePEM(in); got != in {
		t.Fatalf("got %q", got)
	}
}

func TestLoad_InvalidValues(t *testing.T) {
	testKeys(t)
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"rounds too high", map[string]string{EnvMaxRounds: "11"}, EnvMaxRounds},
		{"rounds zero", map[string]string{EnvMaxRounds: "0"}, EnvMaxRounds},
		{"rounds not a number", map[string]string{EnvMaxRounds: "three"}, EnvMaxRounds},
		{"workers too high", map[string]string{EnvWorkers: "17"}, EnvWorkers},
		{"body bytes zero", map[string]string{EnvMaxBodyBytes: "0"}, EnvMaxBodyBytes},
		{"poll negative", map[string]string{EnvPollInterval: "-1m"}, EnvPollInterval},
		{"timeout zero", map[string]string{EnvReviewTimeout: "0s"}, EnvReviewTimeout},
		{"timeout garbage", map[string]string{EnvReviewTimeout: "soon"}, EnvReviewTimeout},
		{"bad regex", map[string]string{EnvIgnoreTitles: "([a-"}, EnvIgnoreTitles},
		{"github app id", map[string]string{EnvGitHubAppID: "abc", EnvGitHubPrivateKey: pkcs1PEM, EnvGitHubWebhookSecret: "s"}, EnvGitHubAppID},
		{"github missing key", map[string]string{EnvGitHubAppID: "1", EnvGitHubWebhookSecret: "s"}, EnvGitHubPrivateKey},
		{"github missing secret", map[string]string{EnvGitHubAppID: "1", EnvGitHubPrivateKey: pkcs1PEM}, EnvGitHubWebhookSecret},
		{"github key not pem", map[string]string{EnvGitHubAppID: "1", EnvGitHubPrivateKey: "nope", EnvGitHubWebhookSecret: "s"}, "no PEM block"},
		{"github key garbage der", map[string]string{EnvGitHubAppID: "1", EnvGitHubPrivateKey: garbagePEMText, EnvGitHubWebhookSecret: "s"}, "not an RSA private key"},
		{"github key not rsa", map[string]string{EnvGitHubAppID: "1", EnvGitHubPrivateKey: ecPEM, EnvGitHubWebhookSecret: "s"}, "not an RSA private key"},
		{"github api url", map[string]string{EnvGitHubAppID: "1", EnvGitHubPrivateKey: pkcs1PEM, EnvGitHubWebhookSecret: "s", EnvGitHubAPIURL: "ftp://x"}, EnvGitHubAPIURL},
		{"gitlab url relative", map[string]string{EnvGitLabURL: "git.example.com"}, EnvGitLabURL},
		{"gitlab url with query", map[string]string{EnvGitLabURL: "https://git.example.com/?a=1"}, EnvGitLabURL},
		{"gitlab hook url", map[string]string{EnvGitLabURL: "https://git.example.com", EnvGitLabHookURL: "::"}, EnvGitLabHookURL},
		{"secret twice", map[string]string{EnvGitLabURL: "https://g", EnvGitLabToken: "a", EnvGitLabToken + "_FILE": "/x"}, "set only one of"},
		{"secret file missing", map[string]string{EnvGitLabURL: "https://g", EnvGitLabToken + "_FILE": "/missing"}, EnvGitLabToken + "_FILE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(tc.env), files(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("c=%+v err=%v, want error naming %q", c, err, tc.want)
			}
		})
	}
}

func TestLoad_ReportsEveryError(t *testing.T) {
	_, err := Load(env(map[string]string{EnvMaxRounds: "0", EnvWorkers: "0"}), files(nil))
	if err == nil || !strings.Contains(err.Error(), EnvMaxRounds) || !strings.Contains(err.Error(), EnvWorkers) {
		t.Fatalf("err=%v", err)
	}
}

func TestRequireServe(t *testing.T) {
	if err := (&Config{}).RequireServe(); err == nil || !strings.Contains(err.Error(), "no platform") {
		t.Fatalf("err=%v", err)
	}
	err := (&Config{GitLab: &GitLabConfig{URL: "https://g"}}).RequireServe()
	if err == nil || !strings.Contains(err.Error(), EnvGitLabToken) || !strings.Contains(err.Error(), EnvGitLabWebhookSecret) {
		t.Fatalf("err=%v", err)
	}
	if err := (&Config{GitHub: &GitHubConfig{AppID: 1}}).RequireServe(); err != nil {
		t.Fatal(err)
	}
}

func TestIgnoreRules_Match(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvIgnoreRepos: "^infra/", EnvIgnoreAuthors: "^renovate", EnvIgnoreTitles: `(?i)\[wip\]`,
		EnvIgnoreSourceBranch: "^release/", EnvIgnoreTargetBranch: "^gh-pages$", EnvIgnoreLabels: "no-review",
	}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		s    IgnoreSubject
		ok   bool
		rule string
	}{
		{"nothing matches", IgnoreSubject{Repo: "team/app", Author: "kevin", Title: "fix", SourceBranch: "feat/x", TargetBranch: "main", Labels: []string{"bug"}}, false, ""},
		{"repo", IgnoreSubject{Repo: "infra/terraform"}, true, "repo:^infra/"},
		{"author", IgnoreSubject{Author: "renovate[bot]"}, true, "author:^renovate"},
		{"title anywhere", IgnoreSubject{Title: "Draft [WIP] change"}, true, `title:(?i)\[wip\]`},
		{"source branch", IgnoreSubject{SourceBranch: "release/1.2"}, true, "source_branch:^release/"},
		{"target branch", IgnoreSubject{TargetBranch: "gh-pages"}, true, "target_branch:^gh-pages$"},
		{"label exact", IgnoreSubject{Labels: []string{"x", "no-review"}}, true, "label:no-review"},
		{"label not partial", IgnoreSubject{Labels: []string{"no-review-needed"}}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, rule := c.Ignore.Match(tc.s)
			if ok != tc.ok || rule != tc.rule {
				t.Fatalf("got %v %q want %v %q", ok, rule, tc.ok, tc.rule)
			}
		})
	}
}

func TestIgnoreRules_EmptyValueNeverMatches(t *testing.T) {
	c, err := Load(env(map[string]string{EnvIgnoreTitles: ".*"}), files(nil))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Ignore.Match(IgnoreSubject{}); ok {
		t.Fatal("empty title matched")
	}
}

func TestSecretFileReadError(t *testing.T) {
	boom := errors.New("permission denied")
	_, err := Load(env(map[string]string{EnvGitLabURL: "https://g", EnvGitLabWebhookSecret + "_FILE": "/s"}),
		func(string) ([]byte, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
}
