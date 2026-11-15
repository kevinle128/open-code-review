// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// journalFile holds the rounds that started and did not end. A round cut by
// a restart or a crash stays in it, and the next start runs it again.
const journalFile = "rounds.json"

// journalEntry is one round of one PR, with what the round needs to run.
type journalEntry struct {
	Platform       forge.Platform `json:"platform"`
	Repo           string         `json:"repo"`
	ProjectID      int64          `json:"project_id,omitempty"`
	InstallationID int64          `json:"installation_id,omitempty"`
	Number         int            `json:"number"`
	Round          int            `json:"round"`
	Scope          string         `json:"scope"`
	Commit         string         `json:"commit,omitempty"`
	FixCommit      bool           `json:"fix_commit,omitempty"`
	Effort         string         `json:"effort,omitempty"`
	Token          string         `json:"token"`
}

func (e journalEntry) ref() forge.PRRef {
	return forge.PRRef{Platform: e.Platform, Repo: e.Repo, ProjectID: e.ProjectID,
		InstallationID: e.InstallationID, Number: e.Number}
}

func (e journalEntry) roundJob() roundJob {
	return roundJob{n: e.Round, scope: e.Scope, commit: e.Commit, fixCommit: e.FixCommit, effort: e.Effort}
}

// journal keeps at most one round per PR, the latest one. A nil journal
// (no data directory) records nothing.
type journal struct {
	path    string
	mu      sync.Mutex
	entries map[string]journalEntry
	seq     atomic.Uint64
}

// openJournal reads the journal in dir. An unreadable file is reported and
// replaced, so one bad write cannot stop the service.
func openJournal(dir string) (*journal, error) {
	if dir == "" {
		return nil, nil
	}
	j := &journal{path: filepath.Join(dir, journalFile), entries: map[string]journalEntry{}}
	b, err := os.ReadFile(j.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return j, nil
	case err != nil:
		return j, fmt.Errorf("read %s: %w", j.path, err)
	}
	if err := json.Unmarshal(b, &j.entries); err != nil {
		j.entries = map[string]journalEntry{}
		return j, fmt.Errorf("parse %s: %w", j.path, err)
	}
	return j, nil
}

// put records rj as the round of ref and returns its token.
func (j *journal) put(ref forge.PRRef, rj roundJob) (string, error) {
	if j == nil {
		return "", nil
	}
	token := strconv.FormatUint(j.seq.Add(1), 10)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries[ref.Key()] = journalEntry{Platform: ref.Platform, Repo: ref.Repo, ProjectID: ref.ProjectID,
		InstallationID: ref.InstallationID, Number: ref.Number, Round: rj.n, Scope: rj.scope,
		Commit: rj.commit, FixCommit: rj.fixCommit, Effort: rj.effort, Token: token}
	return token, j.saveLocked()
}

// remove drops the round of ref when it is still the one with token; a
// later round of the PR has replaced it otherwise.
func (j *journal) remove(ref forge.PRRef, token string) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[ref.Key()]
	if !ok || e.Token != token {
		return nil
	}
	delete(j.entries, ref.Key())
	return j.saveLocked()
}

// pending lists the recorded rounds in a stable order.
func (j *journal) pending() []journalEntry {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]journalEntry, 0, len(j.entries))
	for _, e := range j.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ref().Key() < out[b].ref().Key() })
	return out
}

// saveLocked writes the file through a rename, so a crash leaves the old or
// the new content, never a part of it.
func (j *journal) saveLocked() error {
	b, err := json.MarshalIndent(j.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, j.path)
}
