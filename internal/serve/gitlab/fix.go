// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"fmt"
	"regexp"
	"strings"

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

// CommitURL implements forge.Forge. The configured URL is the web root of
// the instance, the API being under /api/v4 of it.
func (f *Forge) CommitURL(ref forge.PRRef, sha string) string {
	return forge.CommitLink(strings.TrimSpace(f.cfg.URL), ref.Repo, "/-/commit/", sha)
}

var discussionIDRE = regexp.MustCompile(`^[A-Za-z0-9]+$`)
