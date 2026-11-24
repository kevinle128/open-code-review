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
	"time"

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
	// StartedUnixNano is when this round first took a worker. FixStartedUnixNano
	// is when its fix stage first started. Both stay zero until then, so a
	// journal written by an older build still loads. Cuts counts shutdowns of
	// this build; Running is set while a process is inside the round, so a
	// process that died without a shutdown still counts as a cut.
	StartedUnixNano    int64  `json:"started_unix_nano,omitempty"`
	FixStartedUnixNano int64  `json:"fix_started_unix_nano,omitempty"`
	Cuts               int    `json:"cuts,omitempty"`
	Version            string `json:"version,omitempty"`
	Running            bool   `json:"running,omitempty"`
	// Proc is the process that wrote the entry. A new process has a different
	// id, so an entry that is still running is one crash. The same process
	// writes a queued round twice (the round that finishes records the next
	// one, then that round records itself); that second write is not a crash.
	Proc string `json:"proc,omitempty"`
}

func (e journalEntry) ref() forge.PRRef {
	return forge.PRRef{Platform: e.Platform, Repo: e.Repo, ProjectID: e.ProjectID,
		InstallationID: e.InstallationID, Number: e.Number}
}

func (e journalEntry) roundJob() roundJob {
	return roundJob{n: e.Round, scope: e.Scope, commit: e.Commit, fixCommit: e.FixCommit, effort: e.Effort,
		started: unixNanoTime(e.StartedUnixNano), fixStarted: unixNanoTime(e.FixStartedUnixNano), cuts: e.Cuts}
}

func unixNanoTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// journal keeps at most one round per PR, the latest one. A nil journal
// (no data directory) records nothing.
type journal struct {
	path    string
	proc    string
	mu      sync.Mutex
	entries map[string]journalEntry
	seq     atomic.Uint64
}

// openJournal reads the journal in dir. An unreadable file is reported and
// replaced, so one bad write cannot stop the service. proc is random for
// this open, including a second open in the same operating system process.
// A counter would start at the same value in every process, so a crash
// restart would not count.
func openJournal(dir string) (*journal, error) {
	if dir == "" {
		return nil, nil
	}
	j := &journal{path: filepath.Join(dir, journalFile), entries: map[string]journalEntry{},
		proc: newRequestID()}
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

// put records rj as the round of ref and returns its token and the entry
// that was written. A resume of the same round keeps the start time and the
// cut count, including a round that is still queued and has no start time.
// A round whose previous process is still marked running died without a
// shutdown, so that counts as a cut unless the build changed: a different
// version is a deploy and starts the cut count over. A second write of the
// same round by this process is the hand-off from the round that queued it,
// not a crash. The clock itself starts later, when a worker takes the round.
func (j *journal) put(ref forge.PRRef, rj roundJob, version string) (string, journalEntry, error) {
	if j == nil {
		return "", journalEntry{}, nil
	}
	token := strconv.FormatUint(j.seq.Add(1), 10)
	j.mu.Lock()
	defer j.mu.Unlock()
	prev, ok := j.entries[ref.Key()]
	same := ok && prev.Round == rj.n && prev.Scope == rj.scope && prev.Commit == rj.commit && prev.FixCommit == rj.fixCommit
	e := journalEntry{Platform: ref.Platform, Repo: ref.Repo, ProjectID: ref.ProjectID,
		InstallationID: ref.InstallationID, Number: ref.Number, Round: rj.n, Scope: rj.scope,
		Commit: rj.commit, FixCommit: rj.fixCommit, Effort: rj.effort, Token: token,
		Version: version, Running: true, Proc: j.proc}
	if same {
		e.StartedUnixNano = prev.StartedUnixNano
		e.FixStartedUnixNano = prev.FixStartedUnixNano
		e.Cuts = prev.Cuts
		if !rj.started.IsZero() {
			e.StartedUnixNano = rj.started.UnixNano()
		}
		if !rj.fixStarted.IsZero() {
			e.FixStartedUnixNano = rj.fixStarted.UnixNano()
		}
		if rj.cuts > e.Cuts {
			e.Cuts = rj.cuts
		}
		// prev.Proc is empty on a journal written before this field existed.
		// An empty id is not this process, so a still-running old entry
		// counts as a crash. A graceful shutdown has cleared Running.
		if prev.Running && prev.Proc != j.proc && sameBuild(prev.Version, version) {
			e.Cuts++
		}
		if prev.Version != "" && version != "" && prev.Version != version {
			e.Cuts = 0
		}
	}
	j.entries[ref.Key()] = e
	return token, e, j.saveLocked()
}

// sameBuild reports whether a cut belongs to the running build. An empty
// version on either side stays in the same count: a missing version must
// not wipe a crash loop.
func sameBuild(prev, cur string) bool {
	if prev == "" || cur == "" {
		return true
	}
	return prev == cur
}

// noteStart records the first time a worker took this round. A later call
// for the same token leaves the original time in place.
func (j *journal) noteStart(ref forge.PRRef, token string, started time.Time) error {
	if j == nil || token == "" || started.IsZero() {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[ref.Key()]
	if !ok || e.Token != token || e.StartedUnixNano != 0 {
		return nil
	}
	e.StartedUnixNano = started.UnixNano()
	j.entries[ref.Key()] = e
	return j.saveLocked()
}

// noteFixStart records the first time the fix stage started.
func (j *journal) noteFixStart(ref forge.PRRef, token string, started time.Time) error {
	if j == nil || token == "" || started.IsZero() {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[ref.Key()]
	if !ok || e.Token != token || e.FixStartedUnixNano != 0 {
		return nil
	}
	e.FixStartedUnixNano = started.UnixNano()
	j.entries[ref.Key()] = e
	return j.saveLocked()
}

// noteShutdown counts one shutdown cut of the round with token and reports
// the new count. A token that is no longer current is left alone.
func (j *journal) noteShutdown(ref forge.PRRef, token string) (int, error) {
	if j == nil || token == "" {
		return 0, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[ref.Key()]
	if !ok || e.Token != token {
		return 0, nil
	}
	e.Running = false
	e.Cuts++
	j.entries[ref.Key()] = e
	return e.Cuts, j.saveLocked()
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
