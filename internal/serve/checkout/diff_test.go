// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package checkout

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWriteDiff(t *testing.T) {
	r := newRemote(t)
	var out bytes.Buffer
	if err := WriteDiff(context.Background(), r.dir, r.base, r.head, []string{"a.txt"}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "diff --git a/a.txt b/a.txt") || !strings.Contains(got, "+two") || strings.Contains(got, "b.txt") {
		t.Fatalf("diff\n%s", got)
	}
}

func TestWriteDiffBatchesPathsAndKeepsThemLiteral(t *testing.T) {
	r := newRemote(t)
	paths := make([]string, 0, diffBatch+2)
	for i := 0; i < diffBatch; i++ {
		paths = append(paths, "missing-*.txt")
	}
	paths = append(paths, "a.txt", "b.txt")
	var out bytes.Buffer
	if err := WriteDiff(context.Background(), r.dir, r.base, r.head, paths, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "diff --git") != 2 || !strings.Contains(got, "b/b.txt") {
		t.Fatalf("diff\n%s", got)
	}
	out.Reset()
	if err := WriteDiff(context.Background(), r.dir, r.base, r.head, []string{"*.txt"}, &out); err != nil || out.Len() != 0 {
		t.Fatalf("a glob matched files: %v\n%s", err, out.String())
	}
}

func TestWriteDiffRootCommit(t *testing.T) {
	r := newRemote(t)
	var out bytes.Buffer
	if err := WriteDiff(context.Background(), r.dir, "", r.base, []string{"a.txt"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "new file mode") || !strings.Contains(got, "+one") {
		t.Fatalf("diff\n%s", got)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteDiffErrors(t *testing.T) {
	r := newRemote(t)
	ctx := context.Background()
	cases := []struct {
		name       string
		base, head string
		paths      []string
		out        interface{ Write([]byte) (int, error) }
		want       string
	}{
		{"bad head", r.base, "HEAD", []string{"a.txt"}, &bytes.Buffer{}, "commit ids"},
		{"bad base", "--output=/tmp/x", r.head, []string{"a.txt"}, &bytes.Buffer{}, "commit ids"},
		{"no paths", r.base, r.head, nil, &bytes.Buffer{}, "at least one path"},
		{"unknown commit", strings.Repeat("0", 40), r.head, []string{"a.txt"}, &bytes.Buffer{}, "git diff"},
		{"write fails", r.base, r.head, []string{"a.txt"}, failWriter{}, "disk full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := WriteDiff(ctx, r.dir, tc.base, tc.head, tc.paths, tc.out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v", err)
			}
		})
	}
}
