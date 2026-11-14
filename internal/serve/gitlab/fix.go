// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// PushSpec implements forge.Forge. The bot token pushes when the bot may
// push to the source branch (Developer on an unprotected branch); GitLab
// decides that at push time.
func (f *Forge) PushSpec(ctx context.Context, pr forge.PR) (forge.CloneSpec, error) {
	if pr.Fork {
		return forge.CloneSpec{}, forge.ErrFork
	}
	if !validBranch(pr.SourceBranch) {
		return forge.CloneSpec{}, fmt.Errorf("gitlab: invalid source branch %q", pr.SourceBranch)
	}
	return f.cloneSpec(ctx, pr, "refs/heads/"+pr.SourceBranch)
}

// CommitIdentity implements forge.Forge with the bot user's name and email.
func (f *Forge) CommitIdentity(ctx context.Context, _ forge.PRRef) (forge.Identity, error) {
	if _, err := f.BotUserID(ctx); err != nil {
		return forge.Identity{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identity, nil
}

var discussionIDRE = regexp.MustCompile(`^[A-Za-z0-9]+$`)

func (f *Forge) discussionPath(ref forge.PRRef, threadID string) (string, error) {
	if !discussionIDRE.MatchString(threadID) {
		return "", fmt.Errorf("gitlab: invalid discussion id %q", threadID)
	}
	path, err := mrPath(ref)
	if err != nil {
		return "", err
	}
	return path + "/discussions/" + threadID, nil
}

// ReplyThread implements forge.Forge.
func (f *Forge) ReplyThread(ctx context.Context, ref forge.PRRef, threadID, body string) error {
	path, err := f.discussionPath(ref, threadID)
	if err != nil {
		return err
	}
	if body == "" {
		return errors.New("gitlab: reply: body is required")
	}
	_, err = f.c.call(ctx, http.MethodPost, path+"/notes", nil, map[string]string{"body": body}, nil)
	return err
}

// ResolveThread implements forge.Forge.
func (f *Forge) ResolveThread(ctx context.Context, ref forge.PRRef, threadID string) error {
	path, err := f.discussionPath(ref, threadID)
	if err != nil {
		return err
	}
	_, err = f.c.call(ctx, http.MethodPut, path, url.Values{"resolved": {"true"}}, nil, nil)
	return err
}
