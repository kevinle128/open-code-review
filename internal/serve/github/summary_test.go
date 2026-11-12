// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

func TestGetSummaryOwnership(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()

	c, err := f.GetSummary(ctx, testRef)
	if err != nil || c != nil {
		t.Fatalf("empty PR: %v, %v", c, err)
	}

	forged := forge.SummaryTag + "\nfake\n" + state.Encode(state.Initial(3))
	fa.addComment(&fa.issueComments, forged, "mallory", "User")
	fa.addComment(&fa.issueComments, forged, "mallory[bot]", "User")
	fa.addComment(&fa.issueComments, forged, "other-app[bot]", "Bot")
	fa.addComment(&fa.issueComments, "plain bot note", testBot, "Bot")
	c, err = f.GetSummary(ctx, testRef)
	if err != nil || c != nil {
		t.Fatalf("forged markers trusted: %+v, %v", c, err)
	}

	markerOnly := "lost first line\n" + state.Encode(state.Initial(3))
	own := fa.addComment(&fa.issueComments, markerOnly, "OCR-Test[bot]", "Bot")
	c, err = f.GetSummary(ctx, testRef)
	if err != nil || c == nil || c.Body != markerOnly || c.ID != strconv.FormatInt(own["id"].(int64), 10) {
		t.Fatalf("GetSummary = %+v, %v", c, err)
	}
}

func TestGetSummaryNewestWins(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	var logs bytes.Buffer
	f.log = slog.New(slog.NewJSONHandler(&logs, nil))
	fa.addComment(&fa.issueComments, forge.SummaryTag+"\nold", testBot, "Bot")
	newer := fa.addComment(&fa.issueComments, forge.SummaryTag+"\nnew", testBot, "Bot")
	// Listing order is not trusted; the higher id is the newer comment.
	fa.issueComments[0], fa.issueComments[1] = fa.issueComments[1], fa.issueComments[0]

	c, err := f.GetSummary(context.Background(), testRef)
	if err != nil || c == nil || !strings.HasSuffix(c.Body, "new") || c.ID != strconv.FormatInt(newer["id"].(int64), 10) {
		t.Fatalf("GetSummary = %+v, %v", c, err)
	}
	if !strings.Contains(logs.String(), "several bot summaries") || !strings.Contains(logs.String(), `"count":2`) {
		t.Fatalf("no warning logged: %s", logs.String())
	}
}

func TestGetSummaryErrors(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	if _, err := f.GetSummary(ctx, forge.PRRef{Repo: "bad"}); err == nil {
		t.Error("bad ref accepted")
	}
	fa.fail["GET /app"] = []failure{{status: 500}, {status: 500}, {status: 500}, {status: 500}}
	if _, err := f.GetSummary(ctx, testRef); err == nil {
		t.Error("bot login failure ignored")
	}
	fa.issueComments = append(fa.issueComments, map[string]any{"id": "not-a-number"})
	if _, err := f.GetSummary(ctx, testRef); err == nil || !strings.Contains(err.Error(), "decode issue comment") {
		t.Errorf("err = %v", err)
	}
}

func TestUpsertSummary(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()

	first := forge.SummaryTag + "\nreviewing\n" + state.Encode(state.Initial(3))
	if err := f.UpsertSummary(ctx, testRef, first); err != nil {
		t.Fatal(err)
	}
	if len(fa.issueComments) != 1 || fa.issueComments[0]["body"] != first {
		t.Fatalf("summary not created: %+v", fa.issueComments)
	}
	id := strconv.FormatInt(fa.issueComments[0]["id"].(int64), 10)

	if err := f.UpsertSummary(ctx, testRef, "progress only"); err != nil {
		t.Fatal(err)
	}
	if len(fa.issueComments) != 1 || len(fa.patches) != 1 {
		t.Fatalf("summary duplicated: comments=%d patches=%d", len(fa.issueComments), len(fa.patches))
	}
	patched := fa.patches[0]["body"].(string)
	if fa.patches[0]["id"] != id || !strings.HasPrefix(patched, forge.SummaryTag+"\nprogress only") ||
		!strings.Contains(patched, state.Encode(state.Initial(3))) {
		t.Fatalf("patch = %+v", fa.patches[0])
	}

	fa.fail["PATCH /repos/acme/widgets/issues/comments/"+id] = []failure{{status: 404, body: "{}"}}
	if err := f.UpsertSummary(ctx, testRef, first); err == nil {
		t.Error("PATCH failure ignored")
	}
	if err := f.UpsertSummary(ctx, forge.PRRef{Repo: "bad"}, first); err == nil {
		t.Error("bad ref accepted")
	}
}

func TestPostNote(t *testing.T) {
	fa := newFakeAPI(t)
	f, _ := fa.forge(t)
	ctx := context.Background()
	if err := f.PostNote(ctx, testRef, "limit reached"); err != nil {
		t.Fatal(err)
	}
	if len(fa.issueComments) != 1 || fa.issueComments[0]["body"] != "limit reached" {
		t.Fatalf("comments = %+v", fa.issueComments)
	}
	fa.fail["POST /repos/acme/widgets/issues/7/comments"] = []failure{{status: 403, body: "{}"}}
	if err := f.PostNote(ctx, testRef, "x"); err == nil {
		t.Error("API failure ignored")
	}
	if err := f.PostNote(ctx, forge.PRRef{Repo: "bad"}, "x"); err == nil {
		t.Error("bad ref accepted")
	}
}
