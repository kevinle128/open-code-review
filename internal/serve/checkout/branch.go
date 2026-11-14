// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package checkout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// gitSafety turns off what a file written into the checkout could use to
// run code inside the service: hooks and the file system monitor.
var gitSafety = []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}

// FetchBranch fetches like Fetch but checks out the head branch itself,
// spec.HeadRef being refs/heads/<branch>, so a commit made on it can be
// pushed. It records the repository config: Commit refuses to go on when
// the agent that edits the checkout changed it, because a url rewrite there
// would send the push and its token elsewhere.
func (w *workspace) FetchBranch(ctx context.Context, spec forge.CloneSpec, headSHA, baseSHA string) error {
	branch, ok := strings.CutPrefix(spec.HeadRef, "refs/heads/")
	if !ok || branch == "" {
		return fmt.Errorf("head ref %q is not a branch", spec.HeadRef)
	}
	if err := w.fetchRefs(ctx, spec, headSHA, baseSHA); err != nil {
		return err
	}
	// The branch comes from a validated ref and headSHA is a 40-hex id.
	// git checkout before 2.47 rejects --end-of-options.
	if _, err := runGit(ctx, w.dir, gitEnv(), "checkout", "-q", "-B", branch, headSHA); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	sum, err := w.gitConfigSum()
	if err != nil {
		return err
	}
	w.branch, w.start, w.configSum = branch, headSHA, sum
	return nil
}

func (w *workspace) gitConfigSum() ([sha256.Size]byte, error) {
	b, err := os.ReadFile(filepath.Join(w.dir, ".git", "config"))
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("read git config: %w", err)
	}
	return sha256.Sum256(b), nil
}

// ChangedFiles returns the files the branch changes since its merge base
// with base. A base the checkout does not have falls back to the fetched
// base branch, as RangeStart does.
func (w *workspace) ChangedFiles(ctx context.Context, base string) ([]string, error) {
	if w.start == "" {
		return nil, errors.New("changed files: no branch checked out")
	}
	from := RangeStart(ctx, w.dir, base)
	if !resolves(ctx, w.dir, from) {
		return nil, fmt.Errorf("changed files: base %q is not in the checkout", base)
	}
	out, err := runGit(ctx, w.dir, gitEnv(), "-c", "core.quotePath=false",
		"diff", "--name-only", "-z", "--no-renames", "--end-of-options", from+"..."+w.start)
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	var files []string
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) > 0 {
			files = append(files, string(p))
		}
	}
	return files, nil
}

// Commit stages every change in the checkout and commits it as ident. It
// returns changed false and no commit when the worktree is clean. It fails
// when the agent changed the repository config, moved HEAD or left the
// branch, since the service must push only its own commit.
func (w *workspace) Commit(ctx context.Context, ident forge.Identity, message string) (string, bool, error) {
	if w.branch == "" {
		return "", false, errors.New("commit: no branch checked out")
	}
	if ident.Name == "" || ident.Email == "" || strings.ContainsAny(ident.Name+ident.Email, "\r\n<>") {
		return "", false, fmt.Errorf("commit: invalid identity %q <%s>", ident.Name, ident.Email)
	}
	sum, err := w.gitConfigSum()
	if err != nil {
		return "", false, err
	}
	if sum != w.configSum {
		return "", false, errors.New("commit: the repository config changed during the run")
	}
	env := gitEnv()
	cur, err := runGit(ctx, w.dir, env, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(string(cur)) != "refs/heads/"+w.branch {
		return "", false, fmt.Errorf("commit: HEAD is no longer on branch %s", w.branch)
	}
	head, err := runGit(ctx, w.dir, env, "rev-parse", "--verify", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != w.start {
		return "", false, errors.New("commit: the branch moved during the run")
	}
	if _, err := runGit(ctx, w.dir, env, append(gitSafety, "add", "-A")...); err != nil {
		return "", false, fmt.Errorf("git add: %w", err)
	}
	if _, err := runGit(ctx, w.dir, env, "diff", "--cached", "--quiet"); err == nil {
		return "", false, nil
	}
	env = append(env,
		"GIT_AUTHOR_NAME="+ident.Name, "GIT_AUTHOR_EMAIL="+ident.Email,
		"GIT_COMMITTER_NAME="+ident.Name, "GIT_COMMITTER_EMAIL="+ident.Email)
	args := append(append([]string{}, gitSafety...), "-c", "commit.gpgsign=false", "commit", "-q", "--no-verify", "-m", message)
	if _, err := runGit(ctx, w.dir, env, args...); err != nil {
		return "", false, fmt.Errorf("git commit: %w", err)
	}
	out, err := runGit(ctx, w.dir, env, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("resolve commit: %w", err)
	}
	return strings.TrimSpace(string(out)), true, nil
}

// Push pushes sha to the branch FetchBranch checked out, without force, so
// a branch that moved since the fetch rejects it. spec should be fresh: its
// token may be short-lived.
func (w *workspace) Push(ctx context.Context, spec forge.CloneSpec, sha string) error {
	if w.branch == "" {
		return errors.New("push: no branch checked out")
	}
	if !shaPattern.MatchString(sha) {
		return fmt.Errorf("push: %q is not a commit id", sha)
	}
	if spec.HeadRef != "refs/heads/"+w.branch {
		return fmt.Errorf("push: ref %q is not the checked out branch %s", spec.HeadRef, w.branch)
	}
	if err := validateURL(spec.URL, spec.AuthHeader != ""); err != nil {
		return err
	}
	if strings.ContainsAny(spec.AuthHeader, "\r\n") {
		return errors.New("auth header must be a single line")
	}
	env := gitEnv()
	if spec.AuthHeader != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0="+spec.AuthHeader,
		)
	}
	args := append(append([]string{}, gitSafety...), "push", "--no-verify", "--quiet", "--end-of-options", spec.URL, sha+":"+spec.HeadRef)
	if _, err := runGit(ctx, w.dir, env, args...); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	return nil
}
