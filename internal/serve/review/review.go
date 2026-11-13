// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package review declares the review engine and checkout interfaces the
// orchestrator uses, so it can be tested without git or an LLM.
package review

import (
	"context"

	"github.com/alibaba/open-code-review/internal/config/rules"
	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// Request is one review run on a checkout. From and To give a range review
// (the reviewer takes their merge base); Commit alone reviews that commit
// against its first parent. Excludes are globs added to the OCR excludes.
//
// ProjectRule is the repository's rule.json as read at the PR base, or nil
// for none. It replaces the file in the checkout, which the PR controls, so
// a PR cannot change its own review rules or exclusions.
type Request struct {
	RepoDir     string
	From        string
	To          string
	Commit      string
	Background  string
	Effort      string
	Excludes    []string
	ProjectRule *rules.ProjectRule
}

// Terminal states of a review run.
const (
	TerminalComplete = "complete"
	TerminalPartial  = "partial"
	TerminalSkipped  = "skipped"
	TerminalFailed   = "failed"
)

// Result is the outcome of a review run. Base is the commit the diff was
// taken against: for a range review the merge base of From and To, not
// From itself. It is empty when the run did not resolve one.
type Result struct {
	Findings []model.LlmComment
	Terminal string
	Warnings []string
	Coverage Coverage
	Base     string
}

// Coverage counts the files a run selected for review and the ones it
// finished. Failed lists the files it could not review.
type Coverage struct {
	Total    int
	Reviewed int
	Failed   []FailedItem
}

// FailedItem is a file the run could not review. Class is a
// session.FailureClass value such as "timeout" or "provider". Reason is the
// redacted error text, for logs only: it can name internal endpoints.
type FailedItem struct {
	Path    string
	OldPath string
	Class   string
	Reason  string
}

// Reviewer runs OCR on a checkout.
type Reviewer interface {
	Review(ctx context.Context, req Request) (Result, error)
}

// Workspace is one checkout slot.
type Workspace interface {
	Dir() string
	Fetch(ctx context.Context, spec forge.CloneSpec, headSHA, baseSHA string) error
	// IsAncestor returns the exit code of `git merge-base --is-ancestor a b`:
	// 0 yes, 1 no, 128 an object is missing, -1 any other failure.
	IsAncestor(ctx context.Context, a, b string) int
}

// Workspaces hands out checkout slots. The release func empties the slot and
// must be called exactly once.
type Workspaces interface {
	Acquire(ctx context.Context) (Workspace, func(), error)
}
