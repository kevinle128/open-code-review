// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agentcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHomesNewAndRemove(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cli-home")
	h, err := NewHomes(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.New()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(r.Dir) != root || !strings.HasPrefix(filepath.Base(r.Dir), runPrefix) {
		t.Fatalf("run dir %s", r.Dir)
	}
	for _, d := range []string{r.Home, r.Input, r.Out, r.Tmp} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() || filepath.Dir(d) != r.Dir {
			t.Fatalf("%s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.Home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Dir); !os.IsNotExist(err) {
		t.Fatalf("run dir left: %v", err)
	}
}

func TestHomesSweepRemovesOnlyRunDirs(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"run-old1", "run-old2/home", "keep"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h, err := NewHomes(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Sweep(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatalf("left %v", entries)
	}
}

func TestHomesErrors(t *testing.T) {
	if _, err := NewHomes("relative"); err == nil {
		t.Fatal("relative root accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHomes(filepath.Join(file, "sub")); err == nil {
		t.Fatal("root under a file accepted")
	}
	h := &Homes{root: filepath.Join(t.TempDir(), "gone")}
	if err := h.Sweep(); err == nil {
		t.Fatal("sweep of a missing root succeeded")
	}
	if _, err := h.New(); err == nil {
		t.Fatal("run dir in a missing root created")
	}
}
