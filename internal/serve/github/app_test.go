// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func TestNewValidation(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(rsaKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	good := serveconfig.GitHubConfig{AppID: 1, PrivateKeyPEM: pkcs1PEM(t), WebhookSecret: "s"}
	cases := []struct {
		name string
		edit func(*serveconfig.GitHubConfig)
		want string
	}{
		{"pkcs1 default api", func(*serveconfig.GitHubConfig) {}, ""},
		{"pkcs8", func(c *serveconfig.GitHubConfig) {
			c.PrivateKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
		}, ""},
		{"no app id", func(c *serveconfig.GitHubConfig) { c.AppID = 0 }, "app id"},
		{"no secret", func(c *serveconfig.GitHubConfig) { c.WebhookSecret = "" }, "webhook secret"},
		{"no pem", func(c *serveconfig.GitHubConfig) { c.PrivateKeyPEM = []byte("junk") }, "no PEM block"},
		{"bad der", func(c *serveconfig.GitHubConfig) {
			c.PrivateKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")})
		}, "not an RSA private key"},
		{"ec key", func(c *serveconfig.GitHubConfig) {
			c.PrivateKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})
		}, "not an RSA private key"},
		{"bad api url", func(c *serveconfig.GitHubConfig) { c.APIURL = "ftp://x" }, "invalid API URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			tc.edit(&cfg)
			f, err := New(cfg, nil, nil, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if f.Platform() != forge.GitHub || f.apiURL != serveconfig.DefaultGitHubAPIURL || f.httpc == nil || f.now == nil || f.sleep == nil {
					t.Fatalf("defaults not applied: %+v", f)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEndpoints(t *testing.T) {
	cases := []struct{ api, graphql, web string }{
		{"https://api.github.com", "https://api.github.com/graphql", "https://github.com"},
		{"https://ghe.example.com/api/v3", "https://ghe.example.com/api/graphql", "https://ghe.example.com"},
		{"http://127.0.0.1:9999", "http://127.0.0.1:9999/graphql", "http://127.0.0.1:9999"},
	}
	for _, tc := range cases {
		u, _ := url.Parse(tc.api)
		if got := graphqlEndpoint(tc.api); got != tc.graphql {
			t.Errorf("graphqlEndpoint(%s) = %s", tc.api, got)
		}
		if got := webBase(u); got != tc.web {
			t.Errorf("webBase(%s) = %s", tc.api, got)
		}
	}
}

func TestAppJWT(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	token, err := f.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifyJWT(&rsaKey(t).PublicKey, token)
	if err != nil {
		t.Fatalf("signature: %v", err)
	}
	now := fa.clock.now().Unix()
	if claims["iss"] != "123" {
		t.Errorf("iss = %v", claims["iss"])
	}
	if int64(claims["iat"].(float64)) != now-60 {
		t.Errorf("iat = %v, want now-60", claims["iat"])
	}
	if int64(claims["exp"].(float64)) != now+9*60 {
		t.Errorf("exp = %v, want now+540", claims["exp"])
	}
	if !strings.HasPrefix(token, "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.") {
		t.Errorf("header = %s", strings.Split(token, ".")[0])
	}
}

func TestInstallationTokenCache(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	get := func() string {
		t.Helper()
		tok, err := f.installationToken(ctx, testInst)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	if tok := get(); tok != "tok-1" {
		t.Fatalf("token = %s", tok)
	}
	fa.clock.advance(54*time.Minute + 59*time.Second)
	if tok := get(); tok != "tok-1" {
		t.Fatalf("token refreshed too early: %s", tok)
	}
	fa.clock.advance(time.Second)
	if tok := get(); tok != "tok-2" {
		t.Fatalf("token not refreshed at the 5 minute margin: %s", tok)
	}
	if fa.tokenCount != 2 {
		t.Fatalf("token fetches = %d", fa.tokenCount)
	}
}

func TestInstallationTokenErrors(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	if _, err := f.installationToken(ctx, 0); err == nil {
		t.Error("zero installation accepted")
	}
	path := "POST /app/installations/4242/access_tokens"
	fa.override[path] = func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 201, "{") }
	if _, err := f.installationToken(ctx, testInst); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v", err)
	}
	fa.override[path] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 201, map[string]any{"token": "x", "expires_at": "soon"})
	}
	if _, err := f.installationToken(ctx, testInst); err == nil || !strings.Contains(err.Error(), "no token or expiry") {
		t.Errorf("err = %v", err)
	}
	delete(fa.override, path)
	fa.fail[path] = []failure{{status: 403, body: `{"message":"suspended"}`}}
	if _, err := f.installationToken(ctx, testInst); statusOf(err) != 403 {
		t.Errorf("err = %v", err)
	}
}

func TestBotLogin(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	fa.fail["GET /app"] = []failure{{status: 401, body: `{"message":"bad jwt"}`}}
	if _, err := f.BotLogin(ctx); statusOf(err) != 401 {
		t.Fatalf("err = %v", err)
	}
	for i := 0; i < 2; i++ {
		login, err := f.BotLogin(ctx)
		if err != nil || login != testBot {
			t.Fatalf("BotLogin = %q, %v", login, err)
		}
	}
	if fa.appCount != 1 {
		t.Fatalf("GET /app served %d times, want 1 (cached)", fa.appCount)
	}
}

func TestBotLoginBadResponses(t *testing.T) {
	for name, body := range map[string]string{"decode": "{", "no slug": "{}"} {
		t.Run(name, func(t *testing.T) {
			fa := newFakeAPI(t)
			f, _ := fa.forge(t)
			fa.override["GET /app"] = func(w http.ResponseWriter, r *http.Request) { writeRaw(w, 200, body) }
			if _, err := f.BotLogin(context.Background()); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("err = %v, want %q", err, name)
			}
		})
	}
}

func TestInstallationTokensAreSeparate(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	a, err := f.installationToken(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.installationToken(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || fa.tokenCount != 2 {
		t.Fatalf("tokens %s and %s, fetches %d", a, b, fa.tokenCount)
	}
	f.dropToken(1)
	if again, _ := f.installationToken(ctx, 2); again != b {
		t.Fatal("dropping one installation's token cleared another")
	}
	if again, _ := f.installationToken(ctx, 1); again == a {
		t.Fatal("dropped token reused")
	}
}
