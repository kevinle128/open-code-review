// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

var errTest = errors.New("test failure")

func waitGroupDone(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queue did not drain")
	}
}

func TestQueueCoalescesBurst(t *testing.T) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ran []job
	release := make(chan struct{})
	first := make(chan struct{})
	q := newQueue(&wg, func(_ forge.PRRef, j job) bool {
		mu.Lock()
		ran = append(ran, j)
		n := len(ran)
		mu.Unlock()
		if n == 1 {
			close(first)
			<-release
		}
		return false
	})
	ref := ghRef(1)
	q.submit(ref, job{trigger: state.TriggerOpened, requestID: "0"})
	<-first
	for i := range 10 {
		q.submit(ref, job{trigger: state.TriggerPush, requestID: string(rune('a' + i))})
	}
	q.submit(ref, job{trigger: state.TriggerThreadResolved, requestID: "z"})
	close(release)
	waitGroupDone(t, &wg)
	if len(ran) != 2 {
		t.Fatalf("want one follow-up evaluation, ran %d", len(ran))
	}
	if ran[1].trigger != state.TriggerPush {
		t.Fatalf("a push must survive a later thread trigger, got %q", ran[1].trigger)
	}
	if len(q.prs) != 0 {
		t.Fatal("idle PR entry must be removed")
	}
}

func TestQueueCommandsInOrderBeforeEvaluation(t *testing.T) {
	var wg sync.WaitGroup
	var order []string
	block := make(chan struct{})
	started := make(chan struct{})
	q := newQueue(&wg, func(_ forge.PRRef, j job) bool {
		if j.requestID == "first" {
			close(started)
			<-block
		}
		order = append(order, j.requestID)
		return false
	})
	ref := ghRef(1)
	q.submit(ref, job{trigger: state.TriggerPush, requestID: "first"})
	<-started
	q.submit(ref, job{trigger: state.TriggerNote, requestID: "note"})
	q.submit(ref, job{cmd: &cmdJob{Command: Command{Kind: CommandReview}}, requestID: "cmd1"})
	q.submit(ref, job{cmd: &cmdJob{Command: Command{Kind: CommandReviewCommit}}, requestID: "cmd2"})
	q.submit(ref, job{trigger: state.TriggerPoll, requestID: "poll"})
	close(block)
	waitGroupDone(t, &wg)
	want := []string{"first", "cmd1", "cmd2", "poll"}
	if len(order) != len(want) {
		t.Fatalf("order %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order %v want %v", order, want)
		}
	}
}

func TestQueueRerunsAreCapped(t *testing.T) {
	var wg sync.WaitGroup
	var runs atomic.Int32
	q := newQueue(&wg, func(forge.PRRef, job) bool {
		runs.Add(1)
		return true
	})
	q.submit(ghRef(1), job{cmd: &cmdJob{}, requestID: "c"})
	q.submit(ghRef(2), job{trigger: state.TriggerPush, requestID: "p"})
	waitGroupDone(t, &wg)
	if got := runs.Load(); got != 2*(maxReruns+1) {
		t.Fatalf("runs %d", got)
	}
}

// TestQueueNoLostTrigger submits from many goroutines while the loop is
// finishing; every submit must be followed by at least one run that starts
// after it.
func TestQueueNoLostTrigger(t *testing.T) {
	var wg sync.WaitGroup
	var runs atomic.Int64
	q := newQueue(&wg, func(forge.PRRef, job) bool {
		runs.Add(1)
		return false
	})
	var senders sync.WaitGroup
	for i := range 50 {
		senders.Add(1)
		go func() {
			defer senders.Done()
			q.submit(ghRef(i%3), job{trigger: state.TriggerPush})
		}()
	}
	senders.Wait()
	waitGroupDone(t, &wg)
	if runs.Load() < 3 {
		t.Fatalf("runs %d", runs.Load())
	}
	if len(q.prs) != 0 {
		t.Fatal("entries left")
	}
}

func TestReviewsBoundedByWorkers(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.Workers = 2 })
	var running, peak atomic.Int32
	release := make(chan struct{})
	h.rev.set(func(context.Context, review.Request) (review.Result, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return review.Result{Terminal: review.TerminalComplete}, nil
	})
	for i := 1; i <= 4; i++ {
		h.gh.setPR(openPR(ghRef(i), sha1))
		h.srv.queue.submit(ghRef(i), job{trigger: state.TriggerOpened, requestID: "r"})
	}
	deadline := time.After(5 * time.Second)
	for peak.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("reviews did not run in parallel")
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	if peak.Load() != 2 {
		t.Fatalf("peak %d exceeds workers", peak.Load())
	}
	close(release)
	h.drain()
	if len(h.rev.requests()) != 4 {
		t.Fatalf("reviews %d", len(h.rev.requests()))
	}
}
