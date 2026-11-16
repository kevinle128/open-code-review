// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// maxStatusDescription is the length GitHub accepts for a status description.
const maxStatusDescription = 140

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// SetStatus implements forge.Forge with the commit statuses API, which needs
// the App permission "Commit statuses: Read and write". A status is set on
// the commit, so it applies to the PR whose head the commit is.
func (f *Forge) SetStatus(ctx context.Context, pr forge.PR, sha string, st forge.CommitState, description string) error {
	base, err := repoPath(pr.Ref)
	if err != nil {
		return err
	}
	if !shaRE.MatchString(sha) {
		return fmt.Errorf("github: set status: %q is not a commit id", sha)
	}
	payload := map[string]string{
		"state":       string(st),
		"context":     forge.StatusContext,
		"description": cut(description, maxStatusDescription),
	}
	if _, _, err := f.call(ctx, pr.Ref.InstallationID, http.MethodPost, f.apiURL+base+"/statuses/"+sha, payload); err != nil {
		return fmt.Errorf("github: set status: %w", err)
	}
	return nil
}

// cut shortens s to at most n runes.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}
