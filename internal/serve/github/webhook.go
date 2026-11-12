// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

var (
	errMissingSignature = errors.New("github: missing X-Hub-Signature-256 header")
	errBadSignature     = errors.New("github: webhook signature mismatch")
)

// Verify checks the HMAC-SHA256 signature GitHub computes over the raw body
// with the webhook secret.
func (f *Forge) Verify(h http.Header, body []byte) error {
	sig := h.Get("X-Hub-Signature-256")
	if sig == "" {
		return errMissingSignature
	}
	hexSig, ok := strings.CutPrefix(sig, "sha256=")
	if !ok {
		return errBadSignature
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return errBadSignature
	}
	mac := hmac.New(sha256.New, f.secret)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return errBadSignature
	}
	return nil
}

type webhookPayload struct {
	Action       string `json:"action"`
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender      ghUser  `json:"sender"`
	PullRequest *ghPull `json:"pull_request"`
	Issue       *struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Comment *struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	Changes *struct {
		Body *struct {
			From string `json:"from"`
		} `json:"body"`
	} `json:"changes"`
}

var pullRequestActions = map[string]forge.EventKind{
	"opened":           forge.EventOpened,
	"ready_for_review": forge.EventReady,
	"reopened":         forge.EventReopened,
	"synchronize":      forge.EventPush,
	"closed":           forge.EventClosed,
}

// ParseEvent maps a verified delivery to a forge.Event. Deliveries the
// service does not act on come back as EventIgnored with a nil error; an
// error means a payload it should act on is malformed.
func (f *Forge) ParseEvent(h http.Header, body []byte) (forge.Event, error) {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return forge.Event{}, fmt.Errorf("github: decode webhook: %w", err)
	}
	ev := forge.Event{
		Kind:        forge.EventIgnored,
		DeliveryID:  h.Get("X-GitHub-Delivery"),
		Sender:      p.Sender.Login,
		SenderIsBot: p.Sender.Type == "Bot",
	}
	switch h.Get("X-GitHub-Event") {
	case "pull_request":
		kind, ok := pullRequestActions[p.Action]
		if !ok {
			return ev, nil
		}
		return withPull(ev, kind, p)
	case "pull_request_review_thread":
		if p.Action != "resolved" {
			return ev, nil
		}
		return withPull(ev, forge.EventThreadResolved, p)
	case "issue_comment":
		return commentEvent(ev, p)
	default:
		return ev, nil
	}
}

func withPull(ev forge.Event, kind forge.EventKind, p webhookPayload) (forge.Event, error) {
	if p.PullRequest == nil {
		return forge.Event{}, errors.New("github: webhook has no pull_request")
	}
	ref, err := eventRef(p, p.PullRequest.Number)
	if err != nil {
		return forge.Event{}, err
	}
	pr := p.PullRequest.toPR(ref)
	ev.Kind, ev.Ref, ev.PR = kind, ref, &pr
	return ev, nil
}

// commentEvent passes every slash command on a PR to the orchestrator, which
// answers unknown commands with help. An edit carries the old body so only
// command lines the edit added run.
func commentEvent(ev forge.Event, p webhookPayload) (forge.Event, error) {
	if p.Issue == nil || p.Comment == nil || !present(p.Issue.PullRequest) {
		return ev, nil
	}
	switch p.Action {
	case "created":
	case "edited":
		if p.Changes == nil || p.Changes.Body == nil {
			return ev, nil
		}
		ev.PrevCommentBody = p.Changes.Body.From
	default:
		return ev, nil
	}
	if !isCommand(p.Comment.Body) {
		return ev, nil
	}
	ref, err := eventRef(p, p.Issue.Number)
	if err != nil {
		return forge.Event{}, err
	}
	ev.Kind, ev.Ref = forge.EventCommand, ref
	ev.CommentID = strconv.FormatInt(p.Comment.ID, 10)
	ev.CommentBody = p.Comment.Body
	return ev, nil
}

// present is false for a missing or null JSON value; issue_comment payloads
// mark a PR conversation by a non-null issue.pull_request.
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func isCommand(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return strings.HasPrefix(line, "/")
		}
	}
	return false
}

func eventRef(p webhookPayload, number int) (forge.PRRef, error) {
	if p.Installation == nil || p.Installation.ID <= 0 {
		return forge.PRRef{}, errors.New("github: webhook has no installation id")
	}
	ref := forge.PRRef{
		Platform:       forge.GitHub,
		Repo:           p.Repository.FullName,
		InstallationID: p.Installation.ID,
		Number:         number,
	}
	if _, err := repoPath(ref); err != nil {
		return forge.PRRef{}, err
	}
	return ref, nil
}
