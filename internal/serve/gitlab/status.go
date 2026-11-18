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
	"slices"
	"strconv"
	"strings"
	"time"

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

// pipelineWaitDelays are the waits for the merge request pipeline of a new
// commit: GitLab creates it a moment after the push that the service sees.
var pipelineWaitDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// mrPipelineSource is the source of a merge request pipeline. The pipeline
// list of a merge request also holds branch (push) pipelines and the
// external pipelines that a status without a pipeline id makes.
const mrPipelineSource = "merge_request_event"

// SetStatus implements forge.Forge with the commit status API.
//
// When the merge request has a merge request pipeline for sha, the status
// goes into it as one more job: that pipeline is the only one "Pipelines
// must succeed" checks, so a status outside it would not block the merge.
// Without one the status becomes an external pipeline of the commit on the
// source branch, as before.
//
// A success in the newest merge request pipeline also starts the manual
// jobs named in AfterReviewJobs, so a project's tests run once on the
// reviewed commit and not on every fix commit of the chain.
//
// GitLab refuses to set a state the status already has ("Cannot transition
// status"); that answer means the status is already as asked, so it is not
// an error. A failed pipeline lookup still sends the status, as an external
// pipeline, and is returned for the log.
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
	pl, lookupErr := f.mrPipeline(ctx, path, pr.Ref.Number, sha)
	if pl.id != 0 {
		payload["pipeline_id"] = strconv.FormatInt(pl.id, 10)
	} else if pr.SourceBranch != "" {
		payload["ref"] = pr.SourceBranch
	}
	_, err = f.c.call(ctx, http.MethodPost, path+"/statuses/"+sha, nil, payload, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && strings.Contains(apiErr.Body, "Cannot transition status") {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("gitlab: set status: %w", err)
	}
	var errs []error
	if lookupErr != nil {
		errs = append(errs, fmt.Errorf("gitlab: find the merge request pipeline: %w", lookupErr))
	}
	if st == forge.StatusSuccess && pl.id != 0 && pl.newest {
		if err := f.playAfterReviewJobs(ctx, path, pl.id); err != nil {
			errs = append(errs, fmt.Errorf("gitlab: start the jobs that wait for the review: %w", err))
		}
	}
	return errors.Join(errs...)
}

type pipelineInfo struct {
	ID     int64  `json:"id"`
	SHA    string `json:"sha"`
	Source string `json:"source"`
}

type mrPipelineRef struct {
	id     int64
	newest bool // the newest merge request pipeline of the merge request
}

// mrPipeline finds the merge request pipeline of sha. When there is none
// yet and the project uses merge request pipelines, it waits a little,
// since GitLab creates the pipeline of a push a moment later. A project
// without merge request pipelines does not wait.
func (f *Forge) mrPipeline(ctx context.Context, path string, iid int, sha string) (mrPipelineRef, error) {
	if iid <= 0 {
		return mrPipelineRef{}, nil
	}
	usesMRPipelines := false
	for attempt := 0; ; attempt++ {
		var list []pipelineInfo
		if _, err := f.c.call(ctx, http.MethodGet, path+"/merge_requests/"+strconv.Itoa(iid)+"/pipelines", nil, nil, &list); err != nil {
			return mrPipelineRef{}, err
		}
		first := true
		for _, p := range list {
			if p.Source != mrPipelineSource {
				continue
			}
			usesMRPipelines = true
			if p.SHA == sha {
				return mrPipelineRef{id: p.ID, newest: first}, nil
			}
			first = false
		}
		if attempt == 0 && !usesMRPipelines {
			// A new merge request has no pipeline of its own yet: wait
			// only when other merge requests of the project had one.
			var any []pipelineInfo
			q := url.Values{"source": {mrPipelineSource}, "per_page": {"1"}}
			if _, err := f.c.call(ctx, http.MethodGet, path+"/pipelines", q, nil, &any); err != nil {
				return mrPipelineRef{}, err
			}
			usesMRPipelines = len(any) > 0
		}
		if !usesMRPipelines || attempt >= len(pipelineWaitDelays) {
			return mrPipelineRef{}, nil
		}
		if err := f.c.sleep(ctx, pipelineWaitDelays[attempt]); err != nil {
			return mrPipelineRef{}, err
		}
	}
}

type pipelineJob struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// playAfterReviewJobs starts the manual jobs of a pipeline whose names are
// in AfterReviewJobs. Other manual jobs, such as a deploy, stay as they are.
func (f *Forge) playAfterReviewJobs(ctx context.Context, path string, pipeline int64) error {
	if len(f.cfg.AfterReviewJobs) == 0 {
		return nil
	}
	var jobs []pipelineJob
	q := url.Values{"scope[]": {"manual"}}
	if _, err := f.c.call(ctx, http.MethodGet, path+"/pipelines/"+strconv.FormatInt(pipeline, 10)+"/jobs", q, nil, &jobs); err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		if !slices.Contains(f.cfg.AfterReviewJobs, j.Name) {
			continue
		}
		if _, err := f.c.call(ctx, http.MethodPost, path+"/jobs/"+strconv.FormatInt(j.ID, 10)+"/play", nil, nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("job %s: %w", j.Name, err))
		}
	}
	return errors.Join(errs...)
}

// cut shortens s to at most n runes.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}
