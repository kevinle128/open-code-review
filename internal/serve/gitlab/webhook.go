// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package gitlab

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// ErrUnauthorized is returned by Verify when the webhook token is missing or
// wrong, or when no secret is configured.
var ErrUnauthorized = errors.New("gitlab: webhook token rejected")

// Verify checks X-Gitlab-Token against the shared secret in constant time.
// An empty secret rejects every delivery, because comparing two empty
// strings would otherwise accept an unauthenticated request.
func (f *Forge) Verify(h http.Header, _ []byte) error {
	secret := f.cfg.WebhookSecret
	got := h.Get("X-Gitlab-Token")
	if secret == "" || got == "" {
		return ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		return ErrUnauthorized
	}
	return nil
}

type hookUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type hookProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

type hookLabel struct {
	Title string `json:"title"`
}

type hookCommit struct {
	ID string `json:"id"`
}

// hookMR is the merge request object of an MR event (object_attributes) and
// of a note event (merge_request); both share these fields.
type hookMR struct {
	IID            int         `json:"iid"`
	Title          string      `json:"title"`
	AuthorID       int64       `json:"author_id"`
	State          string      `json:"state"`
	Action         string      `json:"action"`
	OldRev         string      `json:"oldrev"`
	SourceBranch   string      `json:"source_branch"`
	TargetBranch   string      `json:"target_branch"`
	Draft          *bool       `json:"draft"`
	WorkInProgress *bool       `json:"work_in_progress"`
	LastCommit     hookCommit  `json:"last_commit"`
	Labels         []hookLabel `json:"labels"`
}

type hookNote struct {
	ID           int64  `json:"id"`
	Note         string `json:"note"`
	NoteableType string `json:"noteable_type"`
	System       bool   `json:"system"`
}

type hookChange struct {
	Previous json.RawMessage `json:"previous"`
	Current  json.RawMessage `json:"current"`
}

type hookPayload struct {
	ObjectKind       string                `json:"object_kind"`
	User             hookUser              `json:"user"`
	Project          hookProject           `json:"project"`
	ObjectAttributes json.RawMessage       `json:"object_attributes"`
	MergeRequest     *hookMR               `json:"merge_request"`
	Labels           []hookLabel           `json:"labels"`
	Changes          map[string]hookChange `json:"changes"`
}

// ParseEvent turns a verified delivery into a neutral event. Kinds the
// service does not act on come back as EventIgnored without an error; a body
// that is not a GitLab event is an error.
func (f *Forge) ParseEvent(h http.Header, body []byte) (forge.Event, error) {
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return forge.Event{}, fmt.Errorf("gitlab: decode webhook: %w", err)
	}
	if strings.TrimSpace(p.ObjectKind) == "" {
		return forge.Event{}, errors.New("gitlab: webhook body without object_kind")
	}
	ev := forge.Event{
		Kind:        forge.EventIgnored,
		DeliveryID:  deliveryID(h),
		Sender:      p.User.Username,
		SenderIsBot: f.isBotSender(p.User),
	}
	switch p.ObjectKind {
	case "merge_request":
		return f.parseMergeRequest(p, ev)
	case "note":
		return f.parseNote(p, ev)
	default:
		return ev, nil
	}
}

func deliveryID(h http.Header) string {
	if v := strings.TrimSpace(h.Get("X-Gitlab-Event-UUID")); v != "" {
		return v
	}
	return strings.TrimSpace(h.Get("Idempotency-Key"))
}

func (f *Forge) parseMergeRequest(p hookPayload, ev forge.Event) (forge.Event, error) {
	var mr hookMR
	if err := json.Unmarshal(p.ObjectAttributes, &mr); err != nil || len(p.ObjectAttributes) == 0 {
		return forge.Event{}, errors.New("gitlab: merge request event without object_attributes")
	}
	ref, err := hookRef(p.Project, mr.IID)
	if err != nil {
		return forge.Event{}, err
	}
	ev.Ref = ref
	labels := p.Labels
	if len(labels) == 0 {
		labels = mr.Labels
	}
	pr := hookPR(ref, mr, p.User, labels)
	ev.PR = &pr

	switch mr.Action {
	case "open":
		ev.Kind = forge.EventOpened
	case "reopen":
		ev.Kind = forge.EventReopened
	case "close", "merge":
		ev.Kind = forge.EventClosed
	case "update":
		// One update can both leave draft and add commits; ready wins because
		// it also reviews HEAD. Assigning the bot as reviewer asks for a
		// review; whether a draft gets one is the orchestrator's decision.
		switch {
		case draftBecameReady(p.Changes):
			ev.Kind = forge.EventReady
		case mr.OldRev != "":
			ev.Kind = forge.EventPush
		case f.botNewlyReviewer(p.Changes):
			ev.Kind = forge.EventCommand
			ev.CommentBody = "/review"
		}
	}
	return ev, nil
}

func (f *Forge) parseNote(p hookPayload, ev forge.Event) (forge.Event, error) {
	var note hookNote
	if err := json.Unmarshal(p.ObjectAttributes, &note); err != nil || len(p.ObjectAttributes) == 0 {
		return forge.Event{}, errors.New("gitlab: note event without object_attributes")
	}
	if note.NoteableType != "MergeRequest" || p.MergeRequest == nil || note.System {
		return ev, nil
	}
	ref, err := hookRef(p.Project, p.MergeRequest.IID)
	if err != nil {
		return forge.Event{}, err
	}
	ev.Ref = ref
	labels := p.MergeRequest.Labels
	if len(labels) == 0 {
		labels = p.Labels
	}
	pr := hookPR(ref, *p.MergeRequest, p.User, labels)
	ev.PR = &pr
	ev.CommentID = strconv.FormatInt(note.ID, 10)
	ev.CommentBody = note.Note
	ev.Kind = forge.EventNote
	if isCommand(note.Note) {
		ev.Kind = forge.EventCommand
	}
	return ev, nil
}

// isCommand reports whether the first non-empty line starts with "/". The
// orchestrator parses the command and answers unknown ones with help.
func isCommand(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return strings.HasPrefix(line, "/")
		}
	}
	return false
}

func hookRef(p hookProject, iid int) (forge.PRRef, error) {
	if p.ID <= 0 || iid <= 0 {
		return forge.PRRef{}, fmt.Errorf("gitlab: event without project id or merge request iid (project %d, iid %d)", p.ID, iid)
	}
	return forge.PRRef{Platform: forge.GitLab, Repo: p.PathWithNamespace, ProjectID: p.ID, Number: iid}, nil
}

// hookPR builds the partial PR a webhook carries. BaseSHA is not in the
// payload, and the author's username is only known when the actor is the
// author, so callers re-read the PR before acting on those fields.
func hookPR(ref forge.PRRef, mr hookMR, actor hookUser, labels []hookLabel) forge.PR {
	pr := forge.PR{
		Ref:          ref,
		Title:        mr.Title,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		HeadSHA:      mr.LastCommit.ID,
		Draft:        isDraft(mr),
		State:        prState(mr.State),
	}
	if actor.ID != 0 && actor.ID == mr.AuthorID {
		pr.Author = actor.Username
	}
	for _, l := range labels {
		if l.Title != "" {
			pr.Labels = append(pr.Labels, l.Title)
		}
	}
	return pr
}

// isDraft falls back to the title prefix for servers older than GitLab 16,
// which send neither draft nor work_in_progress.
func isDraft(mr hookMR) bool {
	switch {
	case mr.Draft != nil:
		return *mr.Draft
	case mr.WorkInProgress != nil:
		return *mr.WorkInProgress
	default:
		return strings.HasPrefix(strings.TrimSpace(mr.Title), "Draft:")
	}
}

// prState maps a GitLab state. "locked" means a merge is in progress; it
// stays open so the MR is still polled if the merge fails.
func prState(s string) string {
	switch s {
	case "closed":
		return forge.StateClosed
	case "merged":
		return forge.StateMerged
	default:
		return forge.StateOpen
	}
}

func draftBecameReady(changes map[string]hookChange) bool {
	if c, ok := changes["draft"]; ok {
		prev, okPrev := flag(c.Previous)
		cur, okCur := flag(c.Current)
		return okPrev && okCur && prev && !cur
	}
	if c, ok := changes["title"]; ok {
		var prev, cur string
		if json.Unmarshal(c.Previous, &prev) != nil || json.Unmarshal(c.Current, &cur) != nil {
			return false
		}
		return strings.Contains(prev, "Draft:") && !strings.Contains(cur, "Draft:")
	}
	return false
}

// flag reads a boolean that older servers send as the string "true"/"false".
func flag(raw json.RawMessage) (bool, bool) {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func (f *Forge) botNewlyReviewer(changes map[string]hookChange) bool {
	c, ok := changes["reviewers"]
	if !ok {
		return false
	}
	var prev, cur []hookUser
	if len(bytes.TrimSpace(c.Previous)) > 0 {
		_ = json.Unmarshal(c.Previous, &prev)
	}
	if json.Unmarshal(c.Current, &cur) != nil {
		return false
	}
	return f.containsBot(cur) && !f.containsBot(prev)
}

func (f *Forge) containsBot(users []hookUser) bool {
	for _, u := range users {
		if f.isBotUser(u.ID, u.Username) {
			return true
		}
	}
	return false
}

// isBotSender matches the service identity or a username that carries a bot
// indicator, so the service never reacts to its own notes or to other bots.
func (f *Forge) isBotSender(u hookUser) bool {
	if f.isBotUser(u.ID, u.Username) {
		return true
	}
	name := strings.ToLower(u.Username)
	for _, ind := range f.indicators {
		if ind != "" && strings.Contains(name, ind) {
			return true
		}
	}
	return false
}

// isBotUser compares against the identity cached by BotUserID and the
// configured bot username; ParseEvent has no context to call the API.
func (f *Forge) isBotUser(id int64, username string) bool {
	botID, botName := f.cachedBot()
	if botID != 0 && id == botID {
		return true
	}
	if username == "" {
		return false
	}
	for _, n := range []string{botName, f.cfg.BotUsername} {
		if n != "" && strings.EqualFold(n, username) {
			return true
		}
	}
	return false
}
