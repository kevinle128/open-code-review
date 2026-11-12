// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// reviewBatchSize keeps each createReview request well under GitHub's payload
// limits while still posting a typical round as one review.
const reviewBatchSize = 50

type placed struct {
	finding   forge.Finding
	placement forge.Placement
}

type reviewComment struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
}

// poster carries the state of one PostFindings call.
type poster struct {
	f        *Forge
	pr       forge.PR
	pullPath string
	inv      forge.Inventory
	first    string
	render   func(forge.Finding, forge.Placement) string
	result   forge.PostResult
	// noteIDs holds finding ids on App-authored PR comments. It is loaded
	// only when a finding has no file to anchor on, which is rare.
	noteIDs map[string]bool
}

// PostFindings posts each finding as one resolvable review thread. Findings
// whose id already sits on an App-authored review comment are skipped, so a
// retried round does not post twice.
func (f *Forge) PostFindings(ctx context.Context, pr forge.PR, fs []forge.Finding, render func(forge.Finding, forge.Placement) string) (forge.PostResult, error) {
	path, err := pullPath(pr.Ref)
	if err != nil {
		return forge.PostResult{}, err
	}
	if render == nil {
		return forge.PostResult{}, errors.New("github: post findings: render is required")
	}
	if pr.HeadSHA == "" {
		return forge.PostResult{}, errors.New("github: post findings: head sha is required")
	}
	if len(fs) == 0 {
		return forge.PostResult{}, nil
	}
	p := &poster{f: f, pr: pr, pullPath: path, render: render}
	posted, err := p.postedIDs(ctx)
	if err != nil {
		return forge.PostResult{}, err
	}
	if err := p.loadInventory(ctx); err != nil {
		return forge.PostResult{}, err
	}

	var lines, others []placed
	for _, fd := range fs {
		if fd.ID != "" {
			if posted[fd.ID] {
				p.result.Skipped++
				continue
			}
			posted[fd.ID] = true
		}
		pl := forge.Place(fd, p.inv, p.first)
		if pl.Kind == forge.Line || pl.Kind == forge.NearestLine {
			lines = append(lines, placed{fd, pl})
		} else {
			others = append(others, placed{fd, pl})
		}
	}
	for start := 0; start < len(lines); start += reviewBatchSize {
		if err := p.postBatch(ctx, lines[start:min(start+reviewBatchSize, len(lines))]); err != nil {
			return p.result, err
		}
	}
	for _, it := range others {
		if err := p.postOther(ctx, it); err != nil {
			return p.result, err
		}
	}
	return p.result, nil
}

// postedIDs collects the finding ids on App-authored review comments.
func (p *poster) postedIDs(ctx context.Context) (map[string]bool, error) {
	bot, err := p.f.BotLogin(ctx)
	if err != nil {
		return nil, err
	}
	return p.findingIDs(ctx, bot, p.pullPath+"/comments", "review comments")
}

// findingIDs collects the finding ids on App-authored comments of a list. A
// walk cut short by the page cap is used as is: refusing to post on a PR with
// thousands of comments would be worse than a rare duplicate there.
func (p *poster) findingIDs(ctx context.Context, bot, path, what string) (map[string]bool, error) {
	ids := map[string]bool{}
	truncated, err := p.f.list(ctx, p.pr.Ref.InstallationID, path, func(raw json.RawMessage) error {
		var c ghComment
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("decode comment: %w", err)
		}
		if ownedBy(c.User, bot) {
			if id, ok := forge.ParseFindingID(c.Body); ok {
				ids[id] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("github: list %s: %w", what, err)
	}
	if truncated {
		p.f.log.Warn("comment list hit the page cap; duplicate check is partial", "pr", p.pr.Ref.Key(), "list", what)
	}
	return ids, nil
}

// loadInventory builds the right-side diff view from the PR file list. An
// empty or capped list proves nothing about paths missing from it.
func (p *poster) loadInventory(ctx context.Context) error {
	p.inv = forge.Inventory{
		Known:     map[string]bool{},
		Ranges:    map[string][]forge.LineRange{},
		Positions: map[string]map[int]forge.LinePos{},
	}
	count := 0
	truncated, err := p.f.list(ctx, p.pr.Ref.InstallationID, p.pullPath+"/files", func(raw json.RawMessage) error {
		var file struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		}
		if err := json.Unmarshal(raw, &file); err != nil {
			return fmt.Errorf("decode pull request file: %w", err)
		}
		if file.Filename == "" {
			return nil
		}
		count++
		if p.first == "" {
			p.first = file.Filename
		}
		p.inv.Known[file.Filename] = true
		if ranges, positions, complete := forge.ParsePatch(file.Patch); complete {
			p.inv.Ranges[file.Filename] = ranges
			p.inv.Positions[file.Filename] = positions
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("github: list pull request files: %w", err)
	}
	p.inv.Complete = !truncated && count > 0
	return nil
}

func (p *poster) lineComment(it placed) reviewComment {
	c := reviewComment{
		Path: it.placement.Path,
		Body: p.render(it.finding, it.placement),
		Line: it.placement.EndLine,
		Side: "RIGHT",
	}
	if it.placement.StartLine > 0 && it.placement.StartLine < it.placement.EndLine {
		c.StartLine, c.StartSide = it.placement.StartLine, "RIGHT"
	}
	return c
}

// postBatch posts line comments as one review. GitHub rejects the whole
// review when any comment cannot be resolved against the diff and does not
// say which one, so the batch is then retried one comment at a time and only
// the comments that fail again move to file level.
func (p *poster) postBatch(ctx context.Context, batch []placed) error {
	comments := make([]reviewComment, 0, len(batch))
	for _, it := range batch {
		comments = append(comments, p.lineComment(it))
	}
	_, _, err := p.f.call(ctx, p.pr.Ref.InstallationID, http.MethodPost, p.f.apiURL+p.pullPath+"/reviews", map[string]any{
		"commit_id": p.pr.HeadSHA,
		"event":     "COMMENT",
		"comments":  comments,
	})
	if err == nil {
		for _, it := range batch {
			p.count(it.placement)
		}
		return nil
	}
	if !isLineFailure(err) {
		return fmt.Errorf("github: create review: %w", err)
	}
	for _, it := range batch {
		if err := p.postSingleLine(ctx, it); err != nil {
			return err
		}
	}
	return nil
}

func (p *poster) postSingleLine(ctx context.Context, it placed) error {
	c := p.lineComment(it)
	payload := map[string]any{"commit_id": p.pr.HeadSHA, "path": c.Path, "body": c.Body, "line": c.Line, "side": c.Side}
	if c.StartLine > 0 {
		payload["start_line"], payload["start_side"] = c.StartLine, c.StartSide
	}
	_, _, err := p.f.call(ctx, p.pr.Ref.InstallationID, http.MethodPost, p.f.apiURL+p.pullPath+"/comments", payload)
	if err == nil {
		p.count(it.placement)
		return nil
	}
	if !isLineFailure(err) {
		return fmt.Errorf("github: create review comment: %w", err)
	}
	return p.postOther(ctx, placed{it.finding, p.demote(it.finding)})
}

// demote picks the file-level anchor for a finding whose line was rejected:
// its own file when that is in the diff, else the first changed file.
func (p *poster) demote(fd forge.Finding) forge.Placement {
	path := p.first
	if p.inv.Known[fd.Path] {
		path = fd.Path
	}
	if path == "" {
		return forge.Placement{Kind: forge.General, Relocated: true}
	}
	return forge.Placement{Kind: forge.FileLevel, Path: path, Relocated: path != fd.Path}
}

// postOther posts a file-level thread, or a plain PR comment when the PR
// lists no file to anchor on. The reviews API takes no file-level entries,
// so file-level threads go through the single-comment endpoint.
func (p *poster) postOther(ctx context.Context, it placed) error {
	body := p.render(it.finding, it.placement)
	if it.placement.Kind == forge.General {
		posted, err := p.postedAsNote(ctx, it.finding.ID)
		if err != nil {
			return err
		}
		if posted {
			p.result.Skipped++
			return nil
		}
		if err := p.f.PostNote(ctx, p.pr.Ref, body); err != nil {
			return err
		}
		p.count(it.placement)
		return nil
	}
	_, _, err := p.f.call(ctx, p.pr.Ref.InstallationID, http.MethodPost, p.f.apiURL+p.pullPath+"/comments", map[string]any{
		"commit_id":    p.pr.HeadSHA,
		"path":         it.placement.Path,
		"body":         body,
		"subject_type": "file",
	})
	if err != nil {
		return fmt.Errorf("github: create file comment: %w", err)
	}
	p.count(it.placement)
	return nil
}

// postedAsNote reports whether a general finding already sits on a PR
// comment; general findings are not review comments, so postedIDs misses them.
func (p *poster) postedAsNote(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	if p.noteIDs == nil {
		bot, err := p.f.BotLogin(ctx)
		if err != nil {
			return false, err
		}
		issue, _ := issuePath(p.pr.Ref)
		if p.noteIDs, err = p.findingIDs(ctx, bot, issue+"/comments", "issue comments"); err != nil {
			return false, err
		}
	}
	return p.noteIDs[id], nil
}

func (p *poster) count(pl forge.Placement) {
	p.result.Posted++
	if pl.Relocated {
		p.result.Relocated++
	}
	switch pl.Kind {
	case forge.FileLevel:
		p.result.FileLevel++
	case forge.General:
		p.result.General++
	}
}

func isLineFailure(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && forge.IsLineResolutionFailure(ae.Status, ae.Body)
}
