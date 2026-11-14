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
//
// PR is the pull request as read from the forge. Engines that hand the
// review to an external agent pass its identity and description on; the
// in-process OCR engine ignores it.
type Request struct {
	RepoDir     string
	From        string
	To          string
	Commit      string
	Background  string
	Effort      string
	Excludes    []string
	ProjectRule *rules.ProjectRule
	PR          forge.PR
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

// Coverage counts the files a run selected for review, after filtering, and
// the ones it finished. Failed lists the files it could not review. Skipped
// counts the changed files the filters left out, by exclude reason.
type Coverage struct {
	Total    int
	Reviewed int
	Failed   []FailedItem
	Skipped  map[string]int
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

// Reviewer runs a review engine on a checkout.
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
	// FetchBranch checks out the PR head branch itself (spec.HeadRef is
	// refs/heads/<branch>) so a fix can be committed and pushed.
	FetchBranch(ctx context.Context, spec forge.CloneSpec, headSHA, baseSHA string) error
	// ChangedFiles lists the files the checked out branch changes since its
	// merge base with base.
	ChangedFiles(ctx context.Context, base string) ([]string, error)
	// Commit commits every change as ident; changed is false when there is
	// nothing to commit.
	Commit(ctx context.Context, ident forge.Identity, message string) (sha string, changed bool, err error)
	// Push pushes sha to the checked out branch without force.
	Push(ctx context.Context, spec forge.CloneSpec, sha string) error
}

// Workspaces hands out checkout slots. The release func empties the slot and
// must be called exactly once.
type Workspaces interface {
	Acquire(ctx context.Context) (Workspace, func(), error)
}
