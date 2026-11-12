// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

const transientRetryBase = 2 * time.Second

type apiDiff struct {
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
	Diff    string `json:"diff"`
}

type apiVersion struct {
	HeadCommitSHA  string `json:"head_commit_sha"`
	BaseCommitSHA  string `json:"base_commit_sha"`
	StartCommitSHA string `json:"start_commit_sha"`
	CreatedAt      string `json:"created_at"`
}

type apiLineBound struct {
	LineCode string `json:"line_code"`
	Type     string `json:"type,omitempty"`
	OldLine  int    `json:"old_line,omitempty"`
	NewLine  int    `json:"new_line"`
}

type apiLineRange struct {
	Start apiLineBound `json:"start"`
	End   apiLineBound `json:"end"`
}

type apiPosition struct {
	PositionType string        `json:"position_type"`
	BaseSHA      string        `json:"base_sha"`
	StartSHA     string        `json:"start_sha"`
	HeadSHA      string        `json:"head_sha"`
	OldPath      string        `json:"old_path"`
	NewPath      string        `json:"new_path"`
	NewLine      int           `json:"new_line"`
	OldLine      int           `json:"old_line,omitempty"`
	LineRange    *apiLineRange `json:"line_range,omitempty"`
}

type discussionPayload struct {
	Body     string       `json:"body"`
	Position *apiPosition `json:"position,omitempty"`
}

// diffView is the MR diff that placement and positions are built from.
type diffView struct {
	inv      forge.Inventory
	oldPaths map[string]string
}

// PostFindings posts each finding as one resolvable discussion. Posted counts
// every discussion created; Relocated and General count the subset that did
// not land on the finding's own lines. GitLab has no file-level thread, so a
// file-level placement becomes a discussion without a position.
//
// Findings already posted by the bot (same finding id) are skipped, so a
// retried round does not duplicate threads. A finding that fails does not
// stop the others; the failures are joined into the returned error.
func (f *Forge) PostFindings(ctx context.Context, pr forge.PR, fs []forge.Finding, render func(forge.Finding, forge.Placement) string) (forge.PostResult, error) {
	var res forge.PostResult
	if len(fs) == 0 {
		return res, nil
	}
	if render == nil {
		return res, errors.New("gitlab: PostFindings needs a render function")
	}
	path, err := mrPath(pr.Ref)
	if err != nil {
		return res, err
	}
	existing, err := f.botDiscussions(ctx, pr.Ref)
	if err != nil {
		return res, fmt.Errorf("gitlab: list discussions before posting: %w", err)
	}
	posted := findingIDs(existing)
	view := f.diffView(ctx, path)
	refs := f.diffRefs(ctx, path, pr)

	var errs []error
	for _, fd := range fs {
		if fd.ID != "" && posted[fd.ID] {
			res.Skipped++
			continue
		}
		kind, err := f.postFinding(ctx, pr.Ref, path, fd, view, refs, render)
		if err != nil {
			errs = append(errs, fmt.Errorf("finding %s (%s:%d): %w", fd.ID, fd.Path, fd.EndLine, err))
			continue
		}
		res.Posted++
		switch kind {
		case forge.NearestLine:
			res.Relocated++
		case forge.General:
			res.General++
		}
		if fd.ID != "" {
			posted[fd.ID] = true
		}
	}
	return res, errors.Join(errs...)
}

var generalPlacement = forge.Placement{Kind: forge.General, Relocated: true}

func (f *Forge) postFinding(ctx context.Context, ref forge.PRRef, path string, fd forge.Finding, view diffView, refs *apiVersion, render func(forge.Finding, forge.Placement) string) (forge.PlacementKind, error) {
	p := forge.Place(fd, view.inv, "")
	if p.Kind == forge.FileLevel || refs == nil {
		p = generalPlacement
	}
	if p.Kind == forge.General {
		return forge.General, f.postDiscussion(ctx, ref, path, discussionPayload{Body: render(fd, p)}, fd.ID)
	}
	payload := discussionPayload{Body: render(fd, p), Position: buildPosition(p, view, refs)}
	err := f.postDiscussion(ctx, ref, path, payload, fd.ID)
	var apiErr *APIError
	if errors.As(err, &apiErr) && forge.IsLineResolutionFailure(apiErr.Status, []byte(apiErr.Body)) {
		return forge.General, f.postDiscussion(ctx, ref, path, discussionPayload{Body: render(fd, generalPlacement)}, fd.ID)
	}
	return p.Kind, err
}

// postDiscussion sends one POST per attempt instead of letting forge.Do
// retry, because a 5xx or a dropped connection may hide a discussion that
// was created. Before such a retry it re-lists the bot's discussions and
// stops when the finding is already there, or when that check cannot run.
func (f *Forge) postDiscussion(ctx context.Context, ref forge.PRRef, path string, payload discussionPayload, findingID string) error {
	for attempt := 0; ; attempt++ {
		resp, body, err := f.c.raw(ctx, http.MethodPost, path+"/discussions", nil, payload, 1)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var (
			failure      error
			delay        time.Duration
			retry        bool
			maybeCreated bool
		)
		if err != nil {
			failure, retry, maybeCreated = err, true, true
			delay = transientRetryBase << attempt
		} else {
			failure = newAPIError(http.MethodPost, path+"/discussions", resp.StatusCode, body)
			delay, retry = forge.RetryDelay(resp, body, attempt, time.Now(), nil)
			maybeCreated = resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout
		}
		if !retry || attempt+1 >= maxAttempts {
			return failure
		}
		if err := f.c.sleep(ctx, delay); err != nil {
			return err
		}
		if maybeCreated && findingID != "" {
			ds, err := f.botDiscussions(ctx, ref)
			if err != nil {
				return fmt.Errorf("%w; not retried because the duplicate check failed: %v", failure, err)
			}
			if findingIDs(ds)[findingID] {
				return nil
			}
		}
	}
}

// findingIDs collects finding ids from bot-authored root notes only, so a
// human quoting a finding tag cannot suppress a finding.
func findingIDs(ds []apiDiscussion) map[string]bool {
	ids := map[string]bool{}
	for _, d := range ds {
		if len(d.Notes) == 0 {
			continue
		}
		if id, ok := forge.ParseFindingID(d.Notes[0].Body); ok {
			ids[id] = true
		}
	}
	return ids
}

// diffView reads the MR diff. A failed read yields an incomplete inventory:
// placement then keeps findings on their lines and relies on the 400
// fallback instead of failing the whole post.
func (f *Forge) diffView(ctx context.Context, path string) diffView {
	v := diffView{
		inv: forge.Inventory{
			Known:     map[string]bool{},
			Ranges:    map[string][]forge.LineRange{},
			Positions: map[string]map[int]forge.LinePos{},
		},
		oldPaths: map[string]string{},
	}
	diffs, err := paginate[apiDiff](ctx, f.c, path+"/diffs", nil)
	if err != nil {
		return v
	}
	for _, d := range diffs {
		p := d.NewPath
		if p == "" {
			p = d.OldPath
		}
		if p == "" {
			continue
		}
		v.inv.Known[p] = true
		v.oldPaths[p] = d.OldPath
		if ranges, positions, ok := forge.ParsePatch(d.Diff); ok {
			v.inv.Ranges[p] = ranges
			v.inv.Positions[p] = positions
		}
	}
	v.inv.Complete = len(v.inv.Known) > 0
	return v
}

// diffRefs picks the MR version that matches the reviewed head, so positions
// point at the diff the review saw. When none matches, the newest version is
// used and the 400 fallback covers any line that moved. nil means no version
// could be read and every finding is posted without a position.
func (f *Forge) diffRefs(ctx context.Context, path string, pr forge.PR) *apiVersion {
	versions, err := paginate[apiVersion](ctx, f.c, path+"/versions", nil)
	if err != nil || len(versions) == 0 {
		return nil
	}
	var headOnly *apiVersion
	for i := range versions {
		v := &versions[i]
		if pr.HeadSHA == "" || v.HeadCommitSHA != pr.HeadSHA {
			continue
		}
		if pr.BaseSHA == "" || v.BaseCommitSHA == pr.BaseSHA {
			return v
		}
		if headOnly == nil {
			headOnly = v
		}
	}
	if headOnly != nil {
		return headOnly
	}
	sort.SliceStable(versions, func(i, j int) bool { return versions[i].CreatedAt > versions[j].CreatedAt })
	return &versions[0]
}

func buildPosition(p forge.Placement, view diffView, refs *apiVersion) *apiPosition {
	oldPath := view.oldPaths[p.Path]
	if oldPath == "" {
		oldPath = p.Path
	}
	pos := &apiPosition{
		PositionType: "text",
		BaseSHA:      refs.BaseCommitSHA,
		StartSHA:     refs.StartCommitSHA,
		HeadSHA:      refs.HeadCommitSHA,
		OldPath:      oldPath,
		NewPath:      p.Path,
		NewLine:      p.EndLine,
	}
	positions := view.inv.Positions[p.Path]
	// An unchanged context line is anchored by both line numbers; with only
	// new_line GitLab rejects it.
	if lp, ok := positions[p.EndLine]; ok && !lp.New && lp.OldLine > 0 {
		pos.OldLine = lp.OldLine
	}
	if p.StartLine < p.EndLine {
		pos.LineRange = buildLineRange(p.Path, p.StartLine, p.EndLine, positions)
	}
	return pos
}

// buildLineRange returns nil when a boundary has no right-side position, so
// GitLab anchors the end line alone instead of rejecting a bad line_code.
func buildLineRange(path string, start, end int, positions map[int]forge.LinePos) *apiLineRange {
	// GitLab defines line_code with the SHA-1 of the path; not a security use.
	sum := sha1.Sum([]byte(path))
	pathSHA := hex.EncodeToString(sum[:])
	s, okS := lineBound(pathSHA, start, positions)
	e, okE := lineBound(pathSHA, end, positions)
	if !okS || !okE {
		return nil
	}
	return &apiLineRange{Start: s, End: e}
}

func lineBound(pathSHA string, line int, positions map[int]forge.LinePos) (apiLineBound, bool) {
	lp, ok := positions[line]
	if !ok {
		return apiLineBound{}, false
	}
	if lp.New {
		return apiLineBound{LineCode: fmt.Sprintf("%s_0_%d", pathSHA, line), Type: "new", NewLine: line}, true
	}
	return apiLineBound{LineCode: fmt.Sprintf("%s_%d_%d", pathSHA, lp.OldLine, line), OldLine: lp.OldLine, NewLine: line}, true
}
