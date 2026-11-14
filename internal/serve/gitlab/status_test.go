// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

func TestSetStatus(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	f.json("POST /projects/42/statuses/"+sha, 201, map[string]any{"id": 1})
	fg := newTestForge(t, f, nil)
	pr := forge.PR{Ref: testRef, SourceBranch: "feat/x"}
	if err := fg.SetStatus(context.Background(), pr, sha, forge.StatusFailure, strings.Repeat("x", 300)); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 1 || w[0].Path != "/projects/42/statuses/"+sha {
		t.Fatalf("writes %+v", w)
	}
	var body map[string]string
	if err := json.Unmarshal(w[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["state"] != "failed" || body["name"] != forge.StatusContext || body["ref"] != "feat/x" || len([]rune(body["description"])) != maxStatusDescription {
		t.Fatalf("body %v", body)
	}
}

// TestSetStatusSameState treats GitLab's refusal to set the state a status
// already has as success.
func TestSetStatusSameState(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	f.json("POST /projects/42/statuses/"+sha, 400, map[string]string{"message": "Cannot transition status via :enqueue from :pending"})
	fg := newTestForge(t, f, nil)
	if err := fg.SetStatus(context.Background(), forge.PR{Ref: testRef}, sha, forge.StatusPending, "x"); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	_ = json.Unmarshal(f.writes()[0].Body, &body)
	if _, ok := body["ref"]; ok {
		t.Fatalf("ref sent without a source branch: %v", body)
	}
}

func TestSetStatusErrors(t *testing.T) {
	f := newFake(t)
	sha := strings.Repeat("c", 40)
	f.json("POST /projects/42/statuses/"+sha, 403, map[string]string{"message": "403 Forbidden"})
	fg := newTestForge(t, f, nil)
	ctx := context.Background()
	if err := fg.SetStatus(ctx, forge.PR{Ref: testRef}, sha, forge.StatusSuccess, "x"); !isStatus(err, 403) {
		t.Fatalf("err %v", err)
	}
	if err := fg.SetStatus(ctx, forge.PR{}, sha, forge.StatusSuccess, "x"); err == nil {
		t.Fatal("empty project accepted")
	}
	if err := fg.SetStatus(ctx, forge.PR{Ref: testRef}, sha, "running", "x"); err == nil {
		t.Fatal("unknown state accepted")
	}
	if err := fg.SetStatus(ctx, forge.PR{Ref: testRef}, "main", forge.StatusSuccess, "x"); err == nil {
		t.Fatal("non-sha accepted")
	}
}
