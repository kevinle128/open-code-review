// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// markerPrefix finds a summary whose first line was lost but whose state
// marker survived, so it is still updated instead of duplicated.
const markerPrefix = "<!-- ocr-serve:v1 "

type ghComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User ghUser `json:"user"`
}

// ownedBy reports whether the App wrote the comment. GitHub sets the author
// of an App comment to "<slug>[bot]" with type Bot, and no user can post
// under that login, so a marker a human pastes into a comment is not trusted.
func ownedBy(u ghUser, botLogin string) bool {
	return u.Type == "Bot" && strings.EqualFold(u.Login, botLogin)
}

// GetSummary returns the App's sticky summary, or nil when there is none.
func (f *Forge) GetSummary(ctx context.Context, ref forge.PRRef) (*forge.Comment, error) {
	c, err := f.findSummary(ctx, ref)
	if err != nil || c == nil {
		return nil, err
	}
	return &forge.Comment{ID: strconv.FormatInt(c.ID, 10), Body: c.Body}, nil
}

func (f *Forge) findSummary(ctx context.Context, ref forge.PRRef) (*ghComment, error) {
	path, err := issuePath(ref)
	if err != nil {
		return nil, err
	}
	bot, err := f.BotLogin(ctx)
	if err != nil {
		return nil, err
	}
	var found []ghComment
	_, err = f.list(ctx, ref.InstallationID, path+"/comments", func(raw json.RawMessage) error {
		var c ghComment
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("decode issue comment: %w", err)
		}
		if ownedBy(c.User, bot) && (strings.Contains(c.Body, forge.SummaryTag) || strings.Contains(c.Body, markerPrefix)) {
			found = append(found, c)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("github: list issue comments: %w", err)
	}
	if len(found) == 0 {
		return nil, nil
	}
	newest := found[0]
	for _, c := range found[1:] {
		if c.ID > newest.ID {
			newest = c
		}
	}
	if len(found) > 1 {
		f.log.Warn("several bot summaries on one pull request; using the newest",
			"pr", ref.Key(), "count", len(found), "comment_id", newest.ID)
	}
	return &newest, nil
}

// UpsertSummary edits the sticky summary in place, or creates it. A body
// without a state marker keeps the old one, so a progress update never drops
// the round count.
func (f *Forge) UpsertSummary(ctx context.Context, ref forge.PRRef, body string) error {
	existing, err := f.findSummary(ctx, ref)
	if err != nil {
		return err
	}
	if !strings.Contains(body, forge.SummaryTag) {
		body = forge.SummaryTag + "\n" + body
	}
	if existing == nil {
		return f.PostNote(ctx, ref, body)
	}
	body = state.Preserve(body, existing.Body)
	base, _ := repoPath(ref)
	rawURL := f.apiURL + base + "/issues/comments/" + strconv.FormatInt(existing.ID, 10)
	if _, _, err := f.call(ctx, ref.InstallationID, http.MethodPatch, rawURL, map[string]string{"body": body}); err != nil {
		return fmt.Errorf("github: update summary: %w", err)
	}
	return nil
}

// PostNote adds a top-level PR comment.
func (f *Forge) PostNote(ctx context.Context, ref forge.PRRef, body string) error {
	path, err := issuePath(ref)
	if err != nil {
		return err
	}
	if _, _, err := f.call(ctx, ref.InstallationID, http.MethodPost, f.apiURL+path+"/comments", map[string]string{"body": body}); err != nil {
		return fmt.Errorf("github: post comment: %w", err)
	}
	return nil
}
