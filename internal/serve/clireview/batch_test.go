// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package clireview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/agentcli"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

func batchPaths(bs []batch) [][]string {
	out := make([][]string, len(bs))
	for i, b := range bs {
		out[i] = b.paths
	}
	return out
}

func TestPlanBatches(t *testing.T) {
	cases := []struct {
		name     string
		paths    []string
		lines    map[string]int
		maxLines int
		maxFiles int
		want     [][]string
	}{
		{"small PR is one batch", []string{"b/x.go", "a.go", "b/y.go"}, map[string]int{"a.go": 10, "b/x.go": 20, "b/y.go": 30}, 100, 10,
			[][]string{{"a.go", "b/x.go", "b/y.go"}}},
		{"directories stay together", []string{"a/1.go", "b/1.go", "a/2.go", "b/2.go"},
			map[string]int{"a/1.go": 40, "a/2.go": 40, "b/1.go": 40, "b/2.go": 40}, 100, 10,
			[][]string{{"a/1.go", "a/2.go"}, {"b/1.go", "b/2.go"}}},
		{"file limit", []string{"a/1.go", "a/2.go", "a/3.go"}, nil, 100, 2,
			[][]string{{"a/1.go", "a/2.go"}, {"a/3.go"}}},
		{"large directory is split", []string{"a/1.go", "a/2.go", "a/3.go"}, map[string]int{"a/1.go": 60, "a/2.go": 60, "a/3.go": 30}, 100, 10,
			[][]string{{"a/1.go"}, {"a/2.go", "a/3.go"}}},
		{"oversize file has its own batch", []string{"a.go", "big.go", "c/d.go"}, map[string]int{"a.go": 10, "big.go": 500, "c/d.go": 10}, 100, 10,
			[][]string{{"a.go"}, {"big.go"}, {"c/d.go"}}},
		{"small directory joins the batch", []string{"a/1.go", "b/1.go", "c/1.go"}, map[string]int{"a/1.go": 30, "b/1.go": 30, "c/1.go": 70}, 100, 10,
			[][]string{{"a/1.go", "b/1.go"}, {"c/1.go"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planBatches(tc.paths, tc.lines, tc.maxLines, tc.maxFiles)
			if !reflect.DeepEqual(batchPaths(got), tc.want) {
				t.Fatalf("batches %v want %v", batchPaths(got), tc.want)
			}
			for i, b := range got {
				if b.index != i || b.count != len(got) {
					t.Fatalf("batch %d %+v", i, b)
				}
			}
		})
	}
}

func TestBatchingDefaults(t *testing.T) {
	var b Batching
	if b.lines() != serveconfig.DefaultBatchLines || b.files() != serveconfig.DefaultBatchFiles || b.concurrency() != 1 {
		t.Fatalf("defaults %d %d %d", b.lines(), b.files(), b.concurrency())
	}
	b = Batching{Lines: 10, Files: 2, Concurrency: 9}
	if b.lines() != 10 || b.files() != 2 || b.concurrency() != serveconfig.MaxBatchConcurrency {
		t.Fatalf("set %d %d %d", b.lines(), b.files(), b.concurrency())
	}
}

// batchRun is what one batch run saw.
type batchRun struct {
	pr        prInput
	diff      string
	prompt    string
	dismissed bool
}

// batchCLI answers each batch with answer(index), where index counts from
// 1, and records what every run saw. With gate set, each run waits until
// gate runs are active at once, or a second passes.
type batchCLI struct {
	answer func(int) (string, error)
	gate   int

	mu      sync.Mutex
	runs    map[int]batchRun
	active  int
	maxSeen int
}

func (*batchCLI) Name() string { return "fake" }

func (c *batchCLI) Exec(_ context.Context, inv agentcli.Invocation) (string, error) {
	var pr prInput
	b, _ := os.ReadFile(filepath.Join(inv.Run.Input, "pr.json"))
	_ = json.Unmarshal(b, &pr)
	diff, _ := os.ReadFile(filepath.Join(inv.Run.Input, "diff.patch"))
	_, err := os.Stat(filepath.Join(inv.Run.Input, DismissedFile))
	index := 1
	if pr.Batch != nil {
		index = pr.Batch.Index
	}

	c.mu.Lock()
	if c.runs == nil {
		c.runs = map[int]batchRun{}
	}
	c.runs[index] = batchRun{pr: pr, diff: string(diff), prompt: inv.Prompt, dismissed: err == nil}
	c.active++
	c.maxSeen = max(c.maxSeen, c.active)
	c.mu.Unlock()
	if c.gate > 0 {
		deadline := time.After(time.Second)
	wait:
		for {
			c.mu.Lock()
			n := c.active
			c.mu.Unlock()
			if n >= c.gate {
				break
			}
			select {
			case <-deadline:
				break wait
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return c.answer(index)
}

// batchRepo commits three files in two directories and returns a selection
// that puts each directory in its own batch.
func batchRepo(t *testing.T) (repo, Selection) {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "README.md", "x\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	write(t, dir, "api/a.go", "package api\n\nfunc A() {}\n")
	write(t, dir, "api/b.go", "package api\n\nfunc B() {}\n")
	write(t, dir, "web/c.go", "package web\n\nfunc C() {}\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "change")
	r := repo{dir: dir, base: base, head: git(t, dir, "rev-parse", "HEAD")}
	sel := Selection{Base: r.base, Head: r.head, Paths: []string{"api/a.go", "api/b.go", "web/c.go"},
		Lines: map[string]int{"api/a.go": 3, "api/b.go": 3, "web/c.go": 3}}
	return r, sel
}

func answerJSON(fs ...string) string {
	return `{"summary":"ok","findings":[` + strings.Join(fs, ",") + `]}`
}

func findingJSON(path string, line int, title string) string {
	return fmt.Sprintf(`{"path":%q,"line":%d,"end_line":null,"severity":"medium","category":"bug","title":%q,"body":"Body of %s.","suggestion":null}`,
		path, line, title, title)
}

func TestReviewRunsBatches(t *testing.T) {
	r, sel := batchRepo(t)
	cli := &batchCLI{answer: func(i int) (string, error) {
		if i == 1 {
			return answerJSON(findingJSON("api/a.go", 3, "Nil check"), findingJSON("pr.json", 1, "PR body lacks a test plan")), nil
		}
		return answerJSON(findingJSON("web/c.go", 3, "Unused"), findingJSON("api/a.go", 3, "nil check"),
			findingJSON("", 0, "PR body again")), nil
	}}
	rev, root := newReviewer(t, fakeSelector{sel: sel}, cli)
	rev.Batching = Batching{Lines: 6}
	var logs bytes.Buffer
	rev.Log = slog.New(slog.NewTextHandler(&logs, nil))
	req := review.Request{RepoDir: r.dir, From: r.base, To: r.head, Dismissed: []review.Dismissed{{Path: "web/c.go", Line: 1, Reason: "fine"}}}
	res, err := rev.Review(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Terminal != review.TerminalComplete || res.Coverage.Reviewed != 3 || len(res.Coverage.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	var got []string
	for _, f := range res.Findings {
		got = append(got, fmt.Sprintf("%s:%d", f.Path, f.StartLine))
	}
	if strings.Join(got, " ") != "api/a.go:3 :0 web/c.go:3" {
		t.Fatalf("findings %v", got)
	}
	warnings := strings.Join(res.Warnings, "\n")
	if !strings.Contains(warnings, `duplicate finding on "api/a.go" line 3`) || !strings.Contains(warnings, "only the first batch reports those") {
		t.Fatalf("warnings %v", res.Warnings)
	}

	if len(cli.runs) != 2 {
		t.Fatalf("runs %d", len(cli.runs))
	}
	first, second := cli.runs[1], cli.runs[2]
	for i, run := range []batchRun{first, second} {
		if strings.Join(run.pr.Files, ",") != "api/a.go,api/b.go,web/c.go" || run.pr.Batch == nil ||
			run.pr.Batch.Count != 2 || !run.dismissed {
			t.Fatalf("batch %d pr.json %+v dismissed %v", i+1, run.pr, run.dismissed)
		}
		if !strings.Contains(run.prompt, fmt.Sprintf("This run is batch %d of 2", i+1)) ||
			!strings.Contains(run.prompt, refText("review-workflow.md")) {
			t.Fatalf("batch %d prompt\n%s", i+1, run.prompt)
		}
	}
	if strings.Join(first.pr.Batch.Files, ",") != "api/a.go,api/b.go" || strings.Join(second.pr.Batch.Files, ",") != "web/c.go" {
		t.Fatalf("batch files %v %v", first.pr.Batch.Files, second.pr.Batch.Files)
	}
	if !strings.Contains(first.diff, "api/b.go") || strings.Contains(first.diff, "web/c.go") ||
		!strings.Contains(second.diff, "web/c.go") || strings.Contains(second.diff, "api/a.go") {
		t.Fatalf("diffs\n%s\n---\n%s", first.diff, second.diff)
	}
	const noPR = "Do not check the PR description"
	if strings.Contains(first.prompt, noPR) || !strings.Contains(second.prompt, noPR) {
		t.Fatal("only later batches skip the PR description check")
	}
	if n := strings.Count(logs.String(), `msg="review batch"`); n != 2 || !strings.Contains(logs.String(), "batches=2 files=2 lines=6 findings=2") {
		t.Fatalf("logs\n%s", logs.String())
	}
	if runDirsLeft(t, root) != 0 {
		t.Fatal("run dirs left")
	}
}

func TestReviewSmallPRIsOneRunWithoutBatchBlock(t *testing.T) {
	r, sel := batchRepo(t)
	cli := &batchCLI{answer: func(int) (string, error) { return answerJSON(), nil }}
	rev, _ := newReviewer(t, fakeSelector{sel: sel}, cli)
	if _, err := rev.Review(context.Background(), review.Request{RepoDir: r.dir}); err != nil {
		t.Fatal(err)
	}
	run := cli.runs[1]
	if len(cli.runs) != 1 || run.pr.Batch != nil || strings.Contains(run.prompt, "This run is batch") ||
		!strings.Contains(run.diff, "api/a.go") || !strings.Contains(run.diff, "web/c.go") {
		t.Fatalf("runs %+v", cli.runs)
	}
}

func TestReviewWithAFailedBatchIsPartial(t *testing.T) {
	r, sel := batchRepo(t)
	cli := &batchCLI{answer: func(i int) (string, error) {
		if i == 1 {
			return "", errors.New("usage limit")
		}
		return answerJSON(findingJSON("web/c.go", 3, "Unused")), nil
	}}
	rev, _ := newReviewer(t, fakeSelector{sel: sel}, cli)
	rev.Batching = Batching{Lines: 6}
	var logs bytes.Buffer
	rev.Log = slog.New(slog.NewTextHandler(&logs, nil))
	res, err := rev.Review(context.Background(), review.Request{RepoDir: r.dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Terminal != review.TerminalPartial || res.Coverage.Total != 3 || res.Coverage.Reviewed != 1 || len(res.Findings) != 1 {
		t.Fatalf("result %+v", res)
	}
	var failed []string
	for _, f := range res.Coverage.Failed {
		if f.Class != "provider" || !strings.Contains(f.Reason, "batch 1/2: fake review: usage limit") {
			t.Fatalf("failed item %+v", f)
		}
		failed = append(failed, f.Path)
	}
	if strings.Join(failed, ",") != "api/a.go,api/b.go" {
		t.Fatalf("failed %v", failed)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "batch 1/2: fake review: usage limit") ||
		!strings.Contains(logs.String(), `msg="review batch failed"`) {
		t.Fatalf("warnings %v logs %s", res.Warnings, logs.String())
	}
}

func TestReviewFailsWhenEveryBatchFails(t *testing.T) {
	r, sel := batchRepo(t)
	cli := &batchCLI{answer: func(int) (string, error) { return "no json", nil }}
	rev, _ := newReviewer(t, fakeSelector{sel: sel}, cli)
	rev.Batching = Batching{Lines: 6}
	res, err := rev.Review(context.Background(), review.Request{RepoDir: r.dir})
	if err == nil || !strings.Contains(err.Error(), "batch 1/2") || !strings.Contains(err.Error(), "batch 2/2") ||
		res.Terminal != review.TerminalFailed || len(res.Coverage.Failed) != 0 {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestReviewBatchConcurrency(t *testing.T) {
	for _, conc := range []int{1, 2} {
		t.Run(fmt.Sprint(conc), func(t *testing.T) {
			r, sel := batchRepo(t)
			cli := &batchCLI{answer: func(int) (string, error) { return answerJSON(), nil }, gate: conc}
			rev, _ := newReviewer(t, fakeSelector{sel: sel}, cli)
			rev.Batching = Batching{Lines: 6, Concurrency: conc}
			if _, err := rev.Review(context.Background(), review.Request{RepoDir: r.dir}); err != nil {
				t.Fatal(err)
			}
			if cli.maxSeen != conc {
				t.Fatalf("max parallel runs %d want %d", cli.maxSeen, conc)
			}
		})
	}
}

func TestReviewBatchesStopWhenTheRunEnds(t *testing.T) {
	r, sel := batchRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cli := &batchCLI{answer: func(int) (string, error) {
		cancel()
		return "", context.Canceled
	}}
	rev, _ := newReviewer(t, fakeSelector{sel: sel}, cli)
	rev.Batching = Batching{Lines: 6}
	res, err := rev.Review(ctx, review.Request{RepoDir: r.dir})
	if !errors.Is(err, context.Canceled) || len(cli.runs) != 1 {
		t.Fatalf("res %+v err %v runs %d", res, err, len(cli.runs))
	}
}

func TestFailureClass(t *testing.T) {
	for err, want := range map[error]string{
		context.DeadlineExceeded:                        "timeout",
		fmt.Errorf("x: %w", context.Canceled):           "cancelled",
		errors.New("the agent answer lacks a findings"): "provider",
	} {
		if got := failureClass(err); got != want {
			t.Errorf("failureClass(%v) = %s want %s", err, got, want)
		}
	}
}
