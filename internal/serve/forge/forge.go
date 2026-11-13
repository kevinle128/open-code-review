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
	"regexp"
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
	EventOpened         EventKind = "opened"
	EventReady          EventKind = "ready"
	EventReopened       EventKind = "reopened"
	EventPush           EventKind = "push"
	EventClosed         EventKind = "closed"
	EventThreadResolved EventKind = "thread-resolved"
	EventNote           EventKind = "note"
	EventCommand        EventKind = "command"
	EventIgnored        EventKind = "ignored"
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

// Finding is one review finding ready to post.
type Finding struct {
	ID             string
	Path           string
	StartLine      int
	EndLine        int
	Category       string
	Severity       string
	Content        string
	ExistingCode   string
	SuggestionCode string
}

// Thread is a resolvable thread whose root comment the service posted.
type Thread struct {
	ID        string
	FindingID string
	Resolved  bool
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

// PostResult counts how findings were placed.
type PostResult struct {
	Posted    int
	Relocated int
	FileLevel int
	General   int
	Skipped   int
}

// SummaryTag is the first line of the sticky summary. Forges find the summary
// by this tag rather than by the state marker, so a damaged marker is still
// found and reported instead of silently starting over.
const SummaryTag = "<!-- ocr-serve-summary -->"

var findingTagRE = regexp.MustCompile(`<!-- ocr-finding:([A-Za-z0-9_-]+) -->`)

// FindingTag returns the hidden tag that starts every finding thread.
func FindingTag(id string) string {
	return "<!-- ocr-finding:" + id + " -->"
}

// ParseFindingID returns the id of the first finding tag in body.
func ParseFindingID(body string) (string, bool) {
	m := findingTagRE.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Forge is the platform client used by the orchestrator. Methods that read
// service-owned state (GetSummary, ListOCRThreads) must only return comments
// authored by the service identity, so a human cannot forge a marker.
type Forge interface {
	Platform() Platform
	Verify(h http.Header, body []byte) error
	ParseEvent(h http.Header, body []byte) (Event, error)
	GetPR(ctx context.Context, ref PRRef) (PR, error)
	// GetSummary returns the bot-authored comment that starts with SummaryTag, or nil.
	GetSummary(ctx context.Context, ref PRRef) (*Comment, error)
	UpsertSummary(ctx context.Context, ref PRRef, body string) error
	// ListOCRThreads returns threads whose bot-authored root carries a finding tag.
	ListOCRThreads(ctx context.Context, ref PRRef) ([]Thread, error)
	PostFindings(ctx context.Context, pr PR, fs []Finding, render func(Finding, Placement) string) (PostResult, error)
	PostNote(ctx context.Context, ref PRRef, body string) error
	React(ctx context.Context, ref PRRef, commentID string, r Reaction) error
	Clone(ctx context.Context, pr PR) (CloneSpec, error)
	// ReadFile reads path at sha. The bool is false when the file does not
	// exist; a file over maxBytes is an error.
	ReadFile(ctx context.Context, ref PRRef, sha, path string, maxBytes int) ([]byte, bool, error)
}
