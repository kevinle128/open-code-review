// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package checkout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// diffBatch bounds the paths of one git call, so a PR that changes
// thousands of files stays under the argument length limit.
const diffBatch = 200

var objectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// WriteDiff writes the unified diff of paths between base and head in dir
// to out. An empty base diffs head as a root commit. Paths are literal, so a
// file name with glob characters selects only itself. Rename detection
// needs both sides in the pathspec, so a renamed file shows as an added one
// when only its new path is given.
func WriteDiff(ctx context.Context, dir, base, head string, paths []string, out io.Writer) error {
	if !objectID.MatchString(head) || (base != "" && !objectID.MatchString(base)) {
		return fmt.Errorf("diff needs commit ids, got base %q head %q", base, head)
	}
	if len(paths) == 0 {
		return errors.New("diff needs at least one path")
	}
	env := gitEnv()
	for start := 0; start < len(paths); start += diffBatch {
		args := []string{"--literal-pathspecs", "-c", "core.quotePath=false"}
		if base == "" {
			args = append(args, "diff-tree", "-p", "--root", "--no-commit-id")
		} else {
			args = append(args, "diff")
		}
		args = append(args, "--no-color", "--no-ext-diff", "--no-textconv", "--end-of-options")
		if base != "" {
			args = append(args, base)
		}
		args = append(args, head, "--")
		args = append(args, paths[start:min(start+diffBatch, len(paths))]...)
		b, err := runGit(ctx, dir, env, args...)
		if err != nil {
			return fmt.Errorf("git diff: %w", err)
		}
		if _, err := out.Write(b); err != nil {
			return fmt.Errorf("write diff: %w", err)
		}
	}
	return nil
}
