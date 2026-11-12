// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

func TestStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", waitingFile)
	s, err := OpenStore(path)
	if err != nil || len(s.List()) != 0 {
		t.Fatalf("new store: %v", err)
	}
	t0 := time.Unix(1_800_000_000, 0).UTC()
	if err := s.Put(Waiting{Ref: glRef(2), Since: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Waiting{Ref: glRef(1), Since: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Waiting{Ref: glRef(1), Since: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	again, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := again.List()
	if len(got) != 2 || got[0].Ref.Number != 1 || !got[0].Since.Equal(t0) || got[0].Ref.ProjectID != 9 {
		t.Fatalf("reopened %+v", got)
	}
	if err := again.Delete(glRef(1).Key()); err != nil {
		t.Fatal(err)
	}
	if err := again.Delete("missing"); err != nil {
		t.Fatal(err)
	}
	if third, _ := OpenStore(path); len(third.List()) != 1 {
		t.Fatal("delete not persisted")
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".waiting-*")); len(matches) != 0 {
		t.Fatalf("temp files left: %v", matches)
	}
}

func TestStoreCorruptAndForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), waitingFile)
	if err := writeFile(path, "{"); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if !errors.Is(err, ErrStoreCorrupt) || s == nil || len(s.List()) != 0 {
		t.Fatalf("corrupt: %v", err)
	}
	if err := writeFile(path, `{"wrong-key":{"ref":{"Platform":"gitlab","Repo":"g/a","Number":1}}}`); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenStore(path); err != nil || len(s.List()) != 0 {
		t.Fatal("an entry under the wrong key must be dropped")
	}
}

func TestStoreErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenStore(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "isdir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(filepath.Join(dir, "isdir")); err == nil {
		t.Fatal("reading a directory must fail")
	}
	if _, err := OpenStore("/dev/null/x/" + waitingFile); err == nil {
		t.Fatal("bad directory must fail")
	}

	s, err := OpenStore(filepath.Join(dir, "rename", waitingFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Waiting{Ref: glRef(1)}); err != nil {
		t.Fatal(err)
	}
	// A directory at the target path makes the rename fail.
	if err := os.Remove(s.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(s.path, "x"), "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Waiting{Ref: glRef(2)}); err == nil {
		t.Fatal("rename onto a directory must fail")
	}
	if err := s.Put(Waiting{Ref: glRef(1)}); err != nil {
		t.Fatal("an unchanged entry needs no write")
	}
	if err := s.Delete(glRef(1).Key()); err == nil {
		t.Fatal("delete must report the write failure")
	}
	if len(s.List()) != 1 || s.List()[0].Ref.Number != 1 {
		t.Fatalf("failed writes must roll back: %+v", s.List())
	}

	s.path = filepath.Join(dir, "gone", waitingFile)
	if err := s.Put(Waiting{Ref: glRef(3)}); err == nil {
		t.Fatal("missing directory must fail")
	}
	s.path = filepath.Join(dir, "rename", waitingFile)
	if err := os.RemoveAll(s.path); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Waiting{Ref: glRef(1), Since: time.Unix(5, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Waiting{Ref: glRef(4)}); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(dir, "gone", waitingFile)
	if err := s.Put(Waiting{Ref: glRef(4), Since: time.Unix(9, 0)}); err != nil {
		t.Fatal("same entry keeps its Since and needs no write")
	}
}
