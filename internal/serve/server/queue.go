// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"sync"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// trigger is the webhook event that queued an automatic job.
type trigger string

const (
	triggerOpened trigger = "opened"
	triggerReady  trigger = "ready"
)

// job is one unit of per-PR work: an automatic evaluation (trigger set), a
// command (cmd set), a CI trigger (ci set) or a round of a running chain
// (round set).
type job struct {
	trigger   trigger
	cmd       *cmdJob
	round     *roundJob
	ci        *ciJob
	requestID string
	reruns    int
}

// cmdJob is a command from a comment. An empty commentID means the platform
// raised the command itself, so there is nothing to react or reply to. A
// non-empty reject answers the comment with an error and runs nothing. gen
// is the chain the command started.
type cmdJob struct {
	Command
	commentID string
	reject    string
	gen       uint64
}

type prWork struct {
	ref     forge.PRRef
	running bool
	dirty   bool
	auto    job
	ordered []job
}

// queue runs at most one job per PR at a time. Commands and rounds queue in
// order; automatic triggers that arrive while a PR is busy collapse into
// one follow-up evaluation, because each evaluation reads the whole state
// again.
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
	if j.cmd != nil || j.round != nil || j.ci != nil {
		if front {
			w.ordered = append([]job{j}, w.ordered...)
		} else {
			w.ordered = append(w.ordered, j)
		}
		return
	}
	w.auto, w.dirty = j, true
}

func (q *queue) loop(key string) {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		w := q.prs[key]
		var j job
		switch {
		case len(w.ordered) > 0:
			j, w.ordered = w.ordered[0], w.ordered[1:]
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
