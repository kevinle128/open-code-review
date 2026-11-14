// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package forge defines the platform-neutral types and the Forge interface
// that the GitHub and GitLab clients implement for the review service.
package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// ErrNotFound matches, through errors.Is, a forge answer that the PR or file
// does not exist, so callers can stop tracking a deleted PR.
var ErrNotFound = errors.New("not found")

// Platform names a code hosting service.
type Platform string

const (
	GitHub Platform = "github"
	GitLab Platform = "gitlab"
)

// PRRef identifies one pull request or merge request. GitHub sets
// InstallationID; GitLab sets ProjectID. Repo is "owner/name" on GitHub and
// the project path with namespace on GitLab.
type PRRef struct {
	Platform       Platform
	Repo           string
	ProjectID      int64
	InstallationID int64
	Number         int
}

// Key is the stable identity used for per-PR serialization and the waiting
// store. It deliberately ignores InstallationID and ProjectID so the same PR
// maps to one key whichever event delivered it.
func (r PRRef) Key() string {
	return fmt.Sprintf("%s:%s#%d", r.Platform, r.Repo, r.Number)
}

// PR is the PR state read from the forge API.
type PR struct {
	Ref          PRRef
	Title        string
	Description  string
	Author       string
	AuthorIsBot  bool
	Labels       []string
	SourceBranch string
	TargetBranch string
	HeadSHA      string
	BaseSHA      string
	Draft        bool
	State        string // open, closed or merged
	// Fork is true when the head branch lives in another repository, or in
	// one that no longer exists; the service cannot push to such a branch.
	Fork bool
}

// PR states.
const (
	StateOpen   = "open"
	StateClosed = "closed"
	StateMerged = "merged"
)

// EventKind classifies a parsed webhook delivery.
type EventKind string

const (
	EventOpened   EventKind = "opened"
	EventReady    EventKind = "ready"
	EventReopened EventKind = "reopened"
	EventPush     EventKind = "push"
	EventClosed   EventKind = "closed"
	EventNote     EventKind = "note"
	EventCommand  EventKind = "command"
	EventIgnored  EventKind = "ignored"
)

// Event is a verified webhook delivery in platform-neutral form. PR is set
// when the payload carries enough PR data; CommentID and CommentBody are set
// for note and command events. PrevCommentBody holds the body before an edit,
// so an edited comment runs only the command lines the edit added. An empty
// CommentID on a command event means the platform raised the command itself
// (a GitLab reviewer assignment), so there is no comment to react or reply to.
type Event struct {
	Kind            EventKind
	Ref             PRRef
	DeliveryID      string
	Sender          string
	SenderIsBot     bool
	PR              *PR
	CommentID       string
	CommentBody     string
	PrevCommentBody string
}

// Identity is the git author the service commits as.
type Identity struct {
	Name  string
	Email string
}

// Comment is a top-level PR comment or note.
type Comment struct {
	ID   string
	Body string
}

// CloneSpec tells the checkout how to fetch a PR. AuthHeader is a full
// "Authorization: ..." header line passed to git through the environment,
// never through the URL.
type CloneSpec struct {
	URL        string
	AuthHeader string
	HeadRef    string
	BaseRef    string
}

// Reaction is a neutral reaction that each forge maps to its own emoji.
type Reaction string

const (
	ReactionEyes    Reaction = "eyes"
	ReactionSuccess Reaction = "success"
	ReactionFailure Reaction = "failure"
)

// SummaryTag is the first line of the sticky summary. Forges find the summary
// by this tag rather than by the state marker, so a damaged marker is still
// found and reported instead of silently starting over.
const SummaryTag = "<!-- ocr-serve-summary -->"

// Forge is the platform client used by the orchestrator. GetSummary, which
// reads service-owned state, must only return a comment authored by the
// service identity, so a human cannot forge a marker.
type Forge interface {
	Platform() Platform
	Verify(h http.Header, body []byte) error
	ParseEvent(h http.Header, body []byte) (Event, error)
	// CommandNames are the names users tag the bot with to run a command,
	// without the "@". The first one is shown in help texts. It makes no API
	// call, so it may be empty until the bot identity is known.
	CommandNames() []string
	GetPR(ctx context.Context, ref PRRef) (PR, error)
	// GetSummary returns the bot-authored comment that starts with SummaryTag, or nil.
	GetSummary(ctx context.Context, ref PRRef) (*Comment, error)
	UpsertSummary(ctx context.Context, ref PRRef, body string) error
	PostNote(ctx context.Context, ref PRRef, body string) error
	React(ctx context.Context, ref PRRef, commentID string, r Reaction) error
	Clone(ctx context.Context, pr PR) (CloneSpec, error)
	// ReadFile reads path at sha. The bool is false when the file does not
	// exist; a file over maxBytes is an error.
	ReadFile(ctx context.Context, ref PRRef, sha, path string, maxBytes int) ([]byte, bool, error)
	// PushSpec returns fetch and push settings for the PR head branch itself
	// (HeadRef is refs/heads/<source branch>) with a token that may push.
	// It refuses a fork PR. Call it again right before a push: the token of
	// an earlier call can expire during a long run.
	PushSpec(ctx context.Context, pr PR) (CloneSpec, error)
	// CommitIdentity is the git author for commits the service pushes.
	CommitIdentity(ctx context.Context, ref PRRef) (Identity, error)
	// SetStatus sets the StatusContext commit status of sha, a commit of
	// the PR head branch. description is one short line.
	SetStatus(ctx context.Context, pr PR, sha string, st CommitState, description string) error
}

// StatusContext names the commit status the service sets on the PR head.
// Branch protection (GitHub) or "Pipelines must succeed" (GitLab) makes it
// a merge gate.
const StatusContext = "ocr-review"

// CommitState is the state of the StatusContext commit status. Each forge
// maps it to its own spelling.
type CommitState string

const (
	StatusPending CommitState = "pending"
	StatusSuccess CommitState = "success"
	StatusFailure CommitState = "failure"
)

// ErrFork is returned by PushSpec for a PR whose head branch is in a fork.
var ErrFork = errors.New("the head branch is in a fork")
