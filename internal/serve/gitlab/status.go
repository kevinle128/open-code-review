// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// maxStatusDescription is the length GitLab accepts for a status description.
const maxStatusDescription = 255

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// gitlabStates maps the neutral states to the commit status API spelling.
var gitlabStates = map[forge.CommitState]string{
	forge.StatusPending: "pending",
	forge.StatusSuccess: "success",
	forge.StatusFailure: "failed",
}

// SetStatus implements forge.Forge with the commit status API. The status
// becomes an external pipeline of the commit on the source branch, which
// "Pipelines must succeed" counts. GitLab refuses to set a state the status
// already has ("Cannot transition status"); that answer means the status
// is already as asked, so it is not an error.
func (f *Forge) SetStatus(ctx context.Context, pr forge.PR, sha string, st forge.CommitState, description string) error {
	path, err := projectPath(pr.Ref)
	if err != nil {
		return err
	}
	state, ok := gitlabStates[st]
	if !ok {
		return fmt.Errorf("gitlab: set status: unknown state %q", st)
	}
	if !shaRE.MatchString(sha) {
		return fmt.Errorf("gitlab: set status: %q is not a commit id", sha)
	}
	payload := map[string]string{
		"state":       state,
		"name":        forge.StatusContext,
		"description": cut(description, maxStatusDescription),
	}
	if pr.SourceBranch != "" {
		payload["ref"] = pr.SourceBranch
	}
	_, err = f.c.call(ctx, http.MethodPost, path+"/statuses/"+sha, nil, payload, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && strings.Contains(apiErr.Body, "Cannot transition status") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gitlab: set status: %w", err)
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
