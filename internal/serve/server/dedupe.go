// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"container/list"
	"sync"
	"time"
)

const (
	dedupeTTL     = time.Hour
	dedupeMaxKeys = 10000
)

type seenKey struct {
	key string
	at  time.Time
}

// dedupe remembers recent keys for a fixed time. It only saves work: an
// evaluation reads all state from the forge, so a missed duplicate is safe.
type dedupe struct {
	mu    sync.Mutex
	now   func() time.Time
	ttl   time.Duration
	max   int
	order *list.List // oldest first
	keys  map[string]*list.Element
}

func newDedupe(now func() time.Time, ttl time.Duration, max int) *dedupe {
	return &dedupe{now: now, ttl: ttl, max: max, order: list.New(), keys: map[string]*list.Element{}}
}

// Seen reports whether key was recorded within the TTL and records it if not.
func (d *dedupe) Seen(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for e := d.order.Front(); e != nil; e = d.order.Front() {
		k := e.Value.(seenKey)
		if now.Sub(k.at) < d.ttl {
			break
		}
		d.order.Remove(e)
		delete(d.keys, k.key)
	}
	if _, ok := d.keys[key]; ok {
		return true
	}
	for d.order.Len() >= d.max {
		e := d.order.Front()
		d.order.Remove(e)
		delete(d.keys, e.Value.(seenKey).key)
	}
	d.keys[key] = d.order.PushBack(seenKey{key: key, at: now})
	return false
}
