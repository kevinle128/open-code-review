// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"sync"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// job is one unit of per-PR work: an automatic evaluation (trigger set) or a
// command (cmd set).
type job struct {
	trigger   state.Trigger
	cmd       *cmdJob
	requestID string
	reruns    int
}

// cmdJob is a command from a comment. An empty commentID means the platform
// raised the command itself, so there is nothing to react or reply to. A
// non-empty reject answers the comment with an error and runs nothing.
type cmdJob struct {
	Command
	commentID string
	reject    string
}

type prWork struct {
	ref     forge.PRRef
	running bool
	dirty   bool
	auto    job
	cmds    []job
}

// queue runs at most one job per PR at a time. Commands queue in order;
// automatic triggers that arrive while a PR is busy collapse into one
// follow-up evaluation, because each evaluation reads the whole state again.
type queue struct {
	mu   sync.Mutex
	prs  map[string]*prWork
	wg   *sync.WaitGroup
	exec func(forge.PRRef, job) bool // true: run the job again
}

func newQueue(wg *sync.WaitGroup, exec func(forge.PRRef, job) bool) *queue {
	return &queue{prs: map[string]*prWork{}, wg: wg, exec: exec}
}

func (q *queue) submit(ref forge.PRRef, j job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := ref.Key()
	w := q.prs[key]
	if w == nil {
		w = &prWork{}
		q.prs[key] = w
	}
	w.ref = ref
	q.addLocked(w, j, false)
	if !w.running {
		w.running = true
		q.wg.Add(1)
		go q.loop(key)
	}
}

func (q *queue) addLocked(w *prWork, j job, front bool) {
	if j.cmd != nil {
		if front {
			w.cmds = append([]job{j}, w.cmds...)
		} else {
			w.cmds = append(w.cmds, j)
		}
		return
	}
	if !w.dirty || startsRound(j.trigger) || !startsRound(w.auto.trigger) {
		w.auto = j
	}
	w.dirty = true
}

// startsRound reports whether a trigger can start round 1. When triggers
// collapse, such a trigger must survive, or a push followed by a note would
// lose the first round.
func startsRound(t state.Trigger) bool {
	switch t {
	case state.TriggerOpened, state.TriggerReady, state.TriggerReopened, state.TriggerPush:
		return true
	}
	return false
}

func (q *queue) loop(key string) {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		w := q.prs[key]
		var j job
		switch {
		case len(w.cmds) > 0:
			j, w.cmds = w.cmds[0], w.cmds[1:]
		case w.dirty:
			j, w.dirty, w.auto = w.auto, false, job{}
		default:
			// Checked under the lock, so a submit either sees running and
			// queues for this loop, or sees no entry and starts a new loop.
			delete(q.prs, key)
			q.mu.Unlock()
			return
		}
		ref := w.ref
		q.mu.Unlock()

		if q.exec(ref, j) && j.reruns < maxReruns {
			j.reruns++
			q.mu.Lock()
			q.addLocked(w, j, true)
			q.mu.Unlock()
		}
	}
}
