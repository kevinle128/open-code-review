// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package clireview

import (
	"path"
	"slices"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
)

// Batching limits how much one CLI run reviews. A batch holds at most Files
// files and, unless a single file is larger, at most Lines changed lines.
// Concurrency batches run at the same time. A zero value takes the service
// default.
type Batching struct {
	Lines       int
	Files       int
	Concurrency int
}

func (b Batching) lines() int {
	if b.Lines > 0 {
		return b.Lines
	}
	return serveconfig.DefaultBatchLines
}

func (b Batching) files() int {
	if b.Files > 0 {
		return b.Files
	}
	return serveconfig.DefaultBatchFiles
}

func (b Batching) concurrency() int {
	if b.Concurrency > 0 {
		return min(b.Concurrency, serveconfig.MaxBatchConcurrency)
	}
	return serveconfig.DefaultBatchConcurrency
}

// batch is one CLI run of a review. Index counts from 0; lines is the sum of
// the changed lines of its paths.
type batch struct {
	index int
	count int
	paths []string
	lines int
}

// planBatches splits the selected paths into batches. Files of one
// directory stay together, since they usually change together and the agent
// reads them for each other's context; a directory too large for one batch
// is split across several. Directories go in path order, so a parent and
// its subdirectories end up close. A selection within both limits is one
// batch.
func planBatches(paths []string, lines map[string]int, maxLines, maxFiles int) []batch {
	byDir := map[string][]string{}
	var dirs []string
	for _, p := range paths {
		d := path.Dir(p)
		if _, ok := byDir[d]; !ok {
			dirs = append(dirs, d)
		}
		byDir[d] = append(byDir[d], p)
	}
	slices.Sort(dirs)

	var out []batch
	var cur batch
	flush := func() {
		if len(cur.paths) > 0 {
			out = append(out, cur)
			cur = batch{}
		}
	}
	fits := func(files, n int) bool {
		return cur.lines+n <= maxLines && len(cur.paths)+files <= maxFiles
	}
	for _, d := range dirs {
		group := byDir[d]
		groupLines := 0
		for _, p := range group {
			groupLines += lines[p]
		}
		if len(cur.paths) > 0 && !fits(len(group), groupLines) {
			flush()
		}
		for _, p := range group {
			if len(cur.paths) > 0 && !fits(1, lines[p]) {
				flush()
			}
			cur.paths = append(cur.paths, p)
			cur.lines += lines[p]
		}
	}
	flush()
	for i := range out {
		out[i].index, out[i].count = i, len(out)
	}
	return out
}
