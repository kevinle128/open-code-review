// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package diff

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/alibaba/open-code-review/internal/gitcmd"
	"github.com/alibaba/open-code-review/internal/llm"
	"github.com/alibaba/open-code-review/internal/model"
)

// A stats parse keeps at most this much of one file's patch, which is
// enough to count tokens the same way a full parse would. A larger patch
// is counted by size and then dropped, so one generated file cannot pin
// the whole change in memory.
const (
	maxKeptPatch = 256 << 10
	maxStatLine  = 1 << 20
)

// statsDiffSet streams the same git diff GetDiffSet would buffer and keeps
// only what selection needs: paths, line counts, a byte size and a token
// count. It does not read new file bodies.
func (p *Provider) statsDiffSet(ctx context.Context) (DiffSet, error) {
	args, err := p.statsArgs(ctx)
	if err != nil {
		// A cancelled merge-base reads as "not found"; keep the reason.
		if ctx.Err() != nil {
			return DiffSet{}, ctx.Err()
		}
		return DiffSet{}, err
	}
	runner := p.runner
	if runner == nil {
		runner = gitcmd.New(0)
	}
	var diffs []model.Diff
	err = runner.Stream(ctx, p.repoDir, func(stdout io.Reader) error {
		var perr error
		diffs, perr = parseStatDiff(stdout)
		return perr
	}, args...)
	if err != nil {
		// A killed git reports the signal, not the deadline that killed it.
		if ctx.Err() != nil {
			return DiffSet{}, fmt.Errorf("git diff stats: %w", ctx.Err())
		}
		return DiffSet{}, fmt.Errorf("git diff stats: %w", err)
	}
	return p.partitionDiffs(diffs), nil
}

func (p *Provider) statsArgs(ctx context.Context) ([]string, error) {
	common := []string{"-c", "core.quotepath=false"}
	switch p.mode {
	case ModeRange:
		base := p.MergeBase(ctx)
		if base == "" {
			return nil, fmt.Errorf("cannot find merge-base between %s and %s", p.from, p.to)
		}
		return append(common, "diff", "--no-ext-diff", "--no-textconv", "--find-renames",
			"--src-prefix=a/", "--dst-prefix=b/", "--no-color", "-U"+fmt.Sprint(DiffContextLines),
			"--end-of-options", base, p.to, "--"), nil
	case ModeCommit:
		return append(common, "show", "--no-ext-diff", "--no-textconv", "--find-renames",
			"--src-prefix=a/", "--dst-prefix=b/", "--no-color", "--diff-merges=first-parent",
			"-U"+fmt.Sprint(DiffContextLines), "--end-of-options", p.commit), nil
	default:
		return nil, fmt.Errorf("stats diff is not used for workspace mode")
	}
}

func parseStatDiff(r io.Reader) ([]model.Diff, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var diffs []model.Diff
	var current *model.Diff
	var buf strings.Builder
	var raw int
	var dropped bool
	inHunk := false

	flush := func() {
		if current == nil {
			return
		}
		current.StatBytes = int64(raw)
		if dropped {
			current.Diff = ""
			current.DiscardedPatch = true
			current.TokensCounted = true
			// Same fallback CountTokens uses when it cannot encode: four
			// bytes to a token. The patch is too large to keep for an
			// exact count.
			n := raw / 4
			if n > math.MaxInt32 {
				n = math.MaxInt32
			}
			current.TokenCount = n
		} else {
			current.Diff = strings.TrimSuffix(buf.String(), "\n")
			current.TokenCount = llm.CountTokens(current.Diff)
			current.TokensCounted = true
			current.Diff = ""
		}
		current.NewFileContent = ""
		if current.IsDeleted || current.NewPath == "/dev/null" {
			current.NewPath = "/dev/null"
		}
		diffs = append(diffs, *current)
		buf.Reset()
		raw = 0
		dropped = false
		inHunk = false
		current = nil
	}

	for {
		line, n, err := readStatLine(br)
		if err == io.EOF {
			flush()
			return diffs, nil
		}
		if err != nil {
			return nil, err
		}
		if oldPath, newPath, ok := parseDiffHeaderLine(line); ok {
			flush()
			current = &model.Diff{OldPath: oldPath, NewPath: newPath}
			raw = n
			inHunk = false
		} else if current != nil {
			raw += n
			if dropped || buf.Len()+len(line)+1 > maxKeptPatch || n > maxStatLine {
				dropped = true
				buf.Reset()
			}
		}
		if current == nil {
			continue
		}
		keep := true
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk && strings.HasPrefix(line, "index "):
			keep = false
		case !inHunk && binaryRe.MatchString(line):
			current.IsBinary = true
		case strings.HasPrefix(line, "new file mode "):
			current.IsNew = true
		case strings.HasPrefix(line, "deleted file mode "):
			current.IsDeleted = true
		case strings.HasPrefix(line, "rename from "):
			current.OldPath = unquoteRenamePath(strings.TrimPrefix(line, "rename from "))
			current.IsRenamed = true
		case strings.HasPrefix(line, "rename to "):
			current.NewPath = unquoteRenamePath(strings.TrimPrefix(line, "rename to "))
			current.IsRenamed = true
		case !inHunk && strings.HasPrefix(line, "copy from "):
			current.OldPath = unquoteRenamePath(strings.TrimPrefix(line, "copy from "))
		case !inHunk && strings.HasPrefix(line, "copy to "):
			current.NewPath = unquoteRenamePath(strings.TrimPrefix(line, "copy to "))
		case !inHunk && line == "--- /dev/null":
			current.IsNew = true
		case !inHunk && line == "+++ /dev/null":
			current.IsDeleted = true
		case inHunk && strings.HasPrefix(line, "+"):
			current.Insertions++
		case inHunk && strings.HasPrefix(line, "-"):
			current.Deletions++
		}
		if keep && !dropped {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
}

// readStatLine reads one diff line. A line longer than maxStatLine is
// drained so it cannot pin the rest of the diff, and n is its full size.
func readStatLine(r *bufio.Reader) (string, int, error) {
	var buf []byte
	n := 0
	for {
		frag, err := r.ReadSlice('\n')
		n += len(frag)
		if len(buf) < maxStatLine {
			room := maxStatLine - len(buf)
			if len(frag) > room {
				buf = append(buf, frag[:room]...)
			} else {
				buf = append(buf, frag...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && n > 0 {
				return trimStatLine(buf), n, nil
			}
			return "", n, err
		}
		return trimStatLine(buf), n, nil
	}
}

func trimStatLine(buf []byte) string {
	s := string(buf)
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}
