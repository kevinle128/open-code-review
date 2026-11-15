// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// PushSpec implements forge.Forge. The installation token can push when the
// App has Contents write access; GitHub decides that at push time.
func (f *Forge) PushSpec(ctx context.Context, pr forge.PR) (forge.CloneSpec, error) {
	if pr.Fork {
		return forge.CloneSpec{}, forge.ErrFork
	}
	if pr.SourceBranch == "" {
		return forge.CloneSpec{}, errors.New("github: push: source branch is required")
	}
	return f.cloneSpec(ctx, pr, "refs/heads/"+pr.SourceBranch)
}

// CommitIdentity implements forge.Forge with the identity GitHub shows for
// the App's own commits: "<slug>[bot]" and its noreply address, which holds
// the bot user id.
func (f *Forge) CommitIdentity(ctx context.Context, ref forge.PRRef) (forge.Identity, error) {
	login, err := f.BotLogin(ctx)
	if err != nil {
		return forge.Identity{}, err
	}
	id, err := f.botUserID(ctx, ref.InstallationID, login)
	if err != nil {
		return forge.Identity{}, err
	}
	return forge.Identity{Name: login, Email: strconv.FormatInt(id, 10) + "+" + login + "@users.noreply.github.com"}, nil
}

// CommitURL implements forge.Forge.
func (f *Forge) CommitURL(ref forge.PRRef, sha string) string {
	return forge.CommitLink(f.webURL, ref.Repo, "/commit/", sha)
}

func (f *Forge) botUserID(ctx context.Context, installationID int64, login string) (int64, error) {
	f.botMu.Lock()
	id := f.botID
	f.botMu.Unlock()
	if id > 0 {
		return id, nil
	}
	data, _, err := f.call(ctx, installationID, http.MethodGet, f.apiURL+"/users/"+url.PathEscape(login), nil)
	if err != nil {
		return 0, fmt.Errorf("github: get bot user: %w", err)
	}
	var u struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return 0, fmt.Errorf("github: get bot user: decode: %w", err)
	}
	if u.ID <= 0 {
		return 0, errors.New("github: get bot user: response has no id")
	}
	f.botMu.Lock()
	f.botID = u.ID
	f.botMu.Unlock()
	return u.ID, nil
}
