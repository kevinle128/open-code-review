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
	"sync"
	"time"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// ErrStoreCorrupt reports that the waiting file could not be decoded. The
// store returned with it is empty and usable: the next push or note on each
// merge request adds its entry again.
var ErrStoreCorrupt = errors.New("waiting store is corrupt")

// Waiting is a GitLab merge request whose OCR threads are open. GitLab sends
// no webhook when a thread is resolved, so the poller re-evaluates these.
type Waiting struct {
	Ref   forge.PRRef `json:"ref"`
	Since time.Time   `json:"since"`
}

// Store keeps the waiting merge requests in a JSON file so a restart does not
// forget them.
type Store struct {
	path    string
	mu      sync.Mutex
	entries map[string]Waiting
}

// OpenStore loads path, creating its directory. A missing file is an empty
// store. A file that does not decode returns an empty store together with an
// error that wraps ErrStoreCorrupt.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	s := &Store{path: path, entries: map[string]Waiting{}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read waiting store: %w", err)
	}
	var entries map[string]Waiting
	if err := json.Unmarshal(raw, &entries); err != nil {
		return s, fmt.Errorf("%w: %s: %v", ErrStoreCorrupt, path, err)
	}
	for k, w := range entries {
		if k == w.Ref.Key() {
			s.entries[k] = w
		}
	}
	return s, nil
}

// Put adds w, keeping the original Since when the entry already exists.
func (s *Store) Put(w Waiting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := w.Ref.Key()
	if old, ok := s.entries[key]; ok {
		w.Since = old.Since
		if old == w {
			return nil
		}
	}
	prev, had := s.entries[key]
	s.entries[key] = w
	if err := s.saveLocked(); err != nil {
		if had {
			s.entries[key] = prev
		} else {
			delete(s.entries, key)
		}
		return err
	}
	return nil
}

// Delete removes the entry for key; a missing key is not an error.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.entries[key]
	if !ok {
		return nil
	}
	delete(s.entries, key)
	if err := s.saveLocked(); err != nil {
		s.entries[key] = prev
		return err
	}
	return nil
}

// List returns the entries ordered by key.
func (s *Store) List() []Waiting {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Waiting, 0, len(s.entries))
	for _, w := range s.entries {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.Key() < out[j].Ref.Key() })
	return out
}

// saveLocked writes a temp file and renames it, so a crash leaves either the
// old or the new file, never a torn one.
func (s *Store) saveLocked() error {
	raw, err := json.Marshal(s.entries)
	if err != nil {
		return fmt.Errorf("encode waiting store: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".waiting-*.json")
	if err != nil {
		return fmt.Errorf("write waiting store: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write waiting store: %w", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write waiting store: %w", err)
	}
	return nil
}
