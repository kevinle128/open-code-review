// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"time"

	"github.com/alibaba/open-code-review/internal/serve/state"
)

// poll re-evaluates waiting GitLab merge requests every interval, because
// GitLab sends no webhook when a thread is resolved. A poll job collapses
// with any webhook job queued for the same merge request.
func (s *Server) poll(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollOnce()
		}
	}
}

func (s *Server) pollOnce() {
	for _, w := range s.store.List() {
		requestID := "poll-" + newRequestID()
		if !s.intake(func() { s.queue.submit(w.Ref, job{trigger: state.TriggerPoll, requestID: requestID}) }) {
			return
		}
	}
}
