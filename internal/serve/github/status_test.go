// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func TestSetStatus(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	sha := strings.Repeat("c", 40)
	var got map[string]any
	fa.override["POST /repos/acme/widgets/statuses/"+sha] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("status sent without a token")
		}
		got = decodeBody(t, r)
		writeJSON(w, 201, map[string]any{"id": 1})
	}
	long := strings.Repeat("x", 200)
	if err := f.SetStatus(context.Background(), forge.PR{Ref: testRef}, sha, forge.StatusPending, long); err != nil {
		t.Fatal(err)
	}
	desc, _ := got["description"].(string)
	if got["state"] != "pending" || got["context"] != forge.StatusContext || len([]rune(desc)) != maxStatusDescription || !strings.HasSuffix(desc, "...") {
		t.Fatalf("payload %v", got)
	}
}

func TestSetStatusErrors(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	sha := strings.Repeat("c", 40)
	if err := f.SetStatus(ctx, forge.PR{Ref: forge.PRRef{Repo: "bad"}}, sha, forge.StatusSuccess, "x"); err == nil {
		t.Fatal("invalid repository accepted")
	}
	if err := f.SetStatus(ctx, forge.PR{Ref: testRef}, "HEAD", forge.StatusSuccess, "x"); err == nil {
		t.Fatal("non-sha accepted")
	}
	fa.override["POST /repos/acme/widgets/statuses/"+sha] = func(w http.ResponseWriter, r *http.Request) {
		writeRaw(w, 403, `{"message":"Resource not accessible by integration"}`)
	}
	if err := f.SetStatus(ctx, forge.PR{Ref: testRef}, sha, forge.StatusFailure, "x"); err == nil || !strings.Contains(err.Error(), "set status") {
		t.Fatalf("err %v", err)
	}
}
