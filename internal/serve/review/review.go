// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package review declares the review engine and checkout interfaces the
// orchestrator uses, so it can be tested without git or an LLM.
package review

import (
	"context"
	"errors"

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
//
// FixCommit marks Commit as a fix commit the service pushed in the previous
// round. An agent engine then reviews only what that commit changed and
// does not check the PR description or scope again, which the first round
// already did. The OCR engine ignores it.
//
// Language is the output language of the review text, such as English or
// Vietnamese; empty means the language of the app config.
//
// Checkpoint is a directory where an engine may keep the parts of the run
// it finished, so the same run started again after a restart does them
// only once; empty means none. The in-process OCR engine ignores it.
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
	FixCommit   bool
	Language    string
	Checkpoint  string
}

// Terminal states of a review run.
const (
	TerminalComplete = "complete"
	TerminalPartial  = "partial"
	TerminalSkipped  = "skipped"
	TerminalFailed   = "failed"
)

// NoticeOversize means the change was past the file or byte cap, so the
// review did not run. The round still ends as a report: merge is allowed
// and the author sees why.
const (
	NoticeOversize = "oversize"
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
	// Notice is set when the review did not run for a reason the round
	// comment has to say. NoticeFiles and NoticeBytes are the size that
	// tripped NoticeOversize.
	Notice      string
	NoticeFiles int
	NoticeBytes int64
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
	// Rebase fetches the current tip of the branch and replays the fix
	// commit on it, leaving the checkout on the result. A fix that does
	// not apply is undone and gives ErrRebaseConflict; Rebased.Head is the
	// fetched tip even then.
	Rebase(ctx context.Context, ident forge.Identity, spec forge.CloneSpec, fix string) (Rebased, error)
	// PushLease pushes sha to the checked out branch only while the branch
	// is still at lease, the tip a Rebase fetched.
	PushLease(ctx context.Context, spec forge.CloneSpec, sha, lease string) error
	// CommitDiff returns the unified diff of sha against its first parent,
	// with three lines of context.
	CommitDiff(ctx context.Context, sha string) (string, error)
}

// Rebased is the result of Workspace.Rebase: Head is the branch tip that
// was fetched, Fix the fix commit replayed on it.
type Rebased struct{ Head, Fix string }

// ErrRebaseConflict means the fix commit does not apply on the new tip of
// the branch.
var ErrRebaseConflict = errors.New("the fix conflicts with the moved branch")

// Workspaces hands out checkout slots. The release func empties the slot and
// must be called exactly once.
type Workspaces interface {
	Acquire(ctx context.Context) (Workspace, func(), error)
}

// Finding is one review finding a fix run judges. ID is unique within the
// run; Path is empty for a finding about the PR as a whole.
type Finding struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	EndLine  int    `json:"end_line"`
	Severity string `json:"severity"`
	Category string `json:"category"`
	Content  string `json:"content"`
}

// FixRequest is one fix run on a checkout of the PR head branch. Files are
// the files the PR changes. Base and Head are the reviewed range; Base is
// empty when one commit, Head, was reviewed. ReportOnly asks for no edit:
// the run only judges the findings and proposes fixes. Language is as in
// Request.
type FixRequest struct {
	RepoDir    string
	PR         forge.PR
	Files      []string
	Base       string
	Head       string
	Findings   []Finding
	ReportOnly bool
	Language   string
}

// Fixed is a finding the fix run judged real and fixed. Title is one short
// line that names the problem, empty when the run gave none. Scenario is how
// the code failed; TestsAdded names the tests the fix added, if any.
// Changes are the places the fix edited, which tie the hunks of the fix
// commit to this finding.
type Fixed struct {
	ID         string
	Path       string
	Line       int
	Title      string
	Problem    string
	Scenario   string
	Fix        string
	TestsAdded string
	Changes    []Change
}

// Change is a line range of the new file that a fix edited. A range with
// StartLine 0 covers the whole file, as for a deleted file.
type Change struct {
	Path      string
	StartLine int
	EndLine   int
}

// Reported is a real finding the run did not fix: in a report-only run,
// for a finding about the PR as a whole, or when no safe fix exists.
// ProposedDiff is a unified diff against the reviewed head that was not
// applied; it is empty when the run gave none. Title is as in Fixed.
type Reported struct {
	ID           string
	Path         string
	Line         int
	Title        string
	Problem      string
	Scenario     string
	ProposedFix  string
	ProposedDiff string
}

// Tests is the test run after the fix. Command is empty when no test ran.
type Tests struct {
	Command    string
	Passed     bool
	OutputTail string
}

// Dropped is a finding the fix run judged not real. Reason says why; Title
// is as in Fixed. Both may be empty.
type Dropped struct {
	ID     string
	Title  string
	Reason string
}

// FixResult is what the agent reports. Fixed, Reported and Dropped hold
// only ids from the request, each once; a finding in none of them was
// judged not real without a reason. CommitMessage is one line.
type FixResult struct {
	Fixed         []Fixed
	Reported      []Reported
	Dropped       []Dropped
	Tests         Tests
	Summary       string
	CommitMessage string
	Warnings      []string
}

// Fixer judges review findings, fixes the real ones in the checkout and
// runs the tests. It leaves committing and pushing to the caller.
type Fixer interface {
	Fix(ctx context.Context, req FixRequest) (FixResult, error)
}
