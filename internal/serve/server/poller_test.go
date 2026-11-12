// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

func TestPollerSubmitsWaitingEachTick(t *testing.T) {
	h := newHarness(t, func(c *serveconfig.Config) { c.PollInterval = 5 * time.Millisecond })
	ref := glRef(1)
	pr := openPR(ref, sha2)
	h.gl.setPR(pr)
	h.gl.summaries[ref.Key()] = state.RenderSummary(state.Marker{V: 1, Auto: 1, Max: 3, Reviewed: sha1, Base: shaBase}, 1, h.loc, "")
	h.gl.threads[ref.Key()] = nil
	if err := h.srv.store.Put(Waiting{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for len(h.rev.requests()) == 0 {
		select {
		case <-deadline:
			t.Fatal("poll did not start the next round")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := h.rev.requests(); r[0].From != sha1 || r[0].To != sha2 {
		t.Fatalf("poll round %+v", r[0])
	}
	if len(h.srv.store.List()) != 0 {
		t.Fatal("resolved MR must leave the store")
	}
}

func TestPollOnceStopsAfterShutdown(t *testing.T) {
	h := newHarness(t)
	if err := h.srv.store.Put(Waiting{Ref: glRef(1)}); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.srv.pollOnce()
	if h.gl.calls.Load() != 0 {
		t.Fatal("poll after shutdown must not queue work")
	}
}
