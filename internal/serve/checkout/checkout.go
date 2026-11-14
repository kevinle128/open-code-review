// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package checkout fetches pull request heads into a fixed set of slot
// directories, so concurrent reviews never share a working tree and the
// per-repository session directories OCR keeps stay bounded.
package checkout

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

// Local refs the fetched PR head and base land on.
const (
	HeadRef = "refs/ocr/head"
	BaseRef = "refs/ocr/base"
)

// ErrHeadMoved reports that the PR head ref no longer points at the commit
// the caller asked for, because a push landed after the PR was read.
var ErrHeadMoved = errors.New("head moved")

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// maxGitOutput caps how much git output an error message carries.
const maxGitOutput = 512

// Manager hands out slot directories <WorkDir>/slot-<i>.
type Manager struct {
	workDir string
	free    chan int
}

var _ review.Workspaces = (*Manager)(nil)

// NewManager creates workDir and slots empty slot directories in it. Slot
// contents left by an earlier process are removed.
func NewManager(workDir string, slots int) (*Manager, error) {
	if slots < 1 {
		return nil, fmt.Errorf("checkout slots must be at least 1, got %d", slots)
	}
	if strings.TrimSpace(workDir) == "" {
		return nil, errors.New("checkout work dir is required")
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout work dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create checkout work dir: %w", err)
	}
	m := &Manager{workDir: abs, free: make(chan int, slots)}
	for i := range slots {
		if err := m.reset(i); err != nil {
			return nil, err
		}
		m.free <- i
	}
	return m, nil
}

// Acquire waits for a free slot and returns it empty. The release func
// empties the slot and returns it to the pool; extra calls do nothing.
func (m *Manager) Acquire(ctx context.Context) (review.Workspace, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var slot int
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case slot = <-m.free:
	}
	if err := m.reset(slot); err != nil {
		m.free <- slot
		return nil, nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			// The release func has no error path. A slot that cannot be
			// emptied now is emptied again by the next Acquire, which
			// reports the error and does not hand out a dirty slot.
			_ = m.reset(slot)
			m.free <- slot
		})
	}
	return &workspace{dir: m.slotDir(slot)}, release, nil
}

func (m *Manager) slotDir(i int) string {
	return filepath.Join(m.workDir, "slot-"+strconv.Itoa(i))
}

func (m *Manager) reset(i int) error {
	dir := m.slotDir(i)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("empty checkout slot %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create checkout slot %s: %w", dir, err)
	}
	return nil
}

type workspace struct {
	dir string
	// Set by FetchBranch: the branch a fix commits on, the commit it starts
	// from and a digest of the repository config when the agent run starts.
	branch    string
	start     string
	configSum [sha256.Size]byte
}

var _ review.Workspace = (*workspace)(nil)

func (w *workspace) Dir() string { return w.dir }

// Fetch initializes the slot as a repository, fetches the PR head and base
// refs and checks out headSHA detached. The auth header reaches git only
// through the environment of the fetch process, so it is never written to
// the repository config or the remote URL. baseSHA and spec.BaseRef may be
// empty; a base commit that the fetch did not bring in is not an error,
// because RangeStart falls back to BaseRef.
func (w *workspace) Fetch(ctx context.Context, spec forge.CloneSpec, headSHA, baseSHA string) error {
	if err := w.fetchRefs(ctx, spec, headSHA, baseSHA); err != nil {
		return err
	}
	if _, err := runGit(ctx, w.dir, gitEnv(), "-c", "advice.detachedHead=false",
		"checkout", "-q", "--detach", "--end-of-options", headSHA); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	return nil
}

// fetchRefs initializes the slot, fetches the head and base refs and checks
// that the head ref is at headSHA.
func (w *workspace) fetchRefs(ctx context.Context, spec forge.CloneSpec, headSHA, baseSHA string) error {
	if err := validateFetch(spec, headSHA, baseSHA); err != nil {
		return err
	}
	env := gitEnv()
	if _, err := runGit(ctx, w.dir, env, "init", "-q", "--template="); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	refspecs := []string{"+" + spec.HeadRef + ":" + HeadRef}
	if spec.BaseRef != "" {
		refspecs = append(refspecs, "+"+spec.BaseRef+":"+BaseRef)
	}
	fetchEnv := env
	if spec.AuthHeader != "" {
		fetchEnv = append(fetchEnv,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0="+spec.AuthHeader,
		)
	}
	args := append([]string{"fetch", "--no-tags", "--quiet", "--end-of-options", spec.URL}, refspecs...)
	if _, err := runGit(ctx, w.dir, fetchEnv, args...); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	out, err := runGit(ctx, w.dir, env, "rev-parse", "--verify", "--end-of-options", HeadRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve fetched head: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != headSHA {
		return fmt.Errorf("%w: %s is at %s, want %s", ErrHeadMoved, spec.HeadRef, got, headSHA)
	}
	return nil
}

// IsAncestor returns the exit code of `git merge-base --is-ancestor a b`:
// 0 yes, 1 no, 128 an object is missing, -1 invalid input or any other
// failure.
func (w *workspace) IsAncestor(ctx context.Context, a, b string) int {
	if !shaPattern.MatchString(a) || !shaPattern.MatchString(b) {
		return -1
	}
	_, err := runGit(ctx, w.dir, gitEnv(), "merge-base", "--is-ancestor", "--end-of-options", a, b)
	if err == nil {
		return 0
	}
	if ctx.Err() != nil {
		return -1
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch code := exitErr.ExitCode(); code {
		case 1, 128:
			return code
		}
	}
	return -1
}

// RangeStart returns the commit a range review in dir should start from.
// The PR base can be force-pushed between reading the PR and the fetch;
// then from is absent from the checkout and the fetched base tip is the
// closest valid start. from is returned unchanged when it is not a commit
// id, when it resolves, or when dir has no fetched base, so a malformed
// value still reaches the caller's ref validation.
func RangeStart(ctx context.Context, dir, from string) string {
	if !shaPattern.MatchString(from) || resolves(ctx, dir, from) {
		return from
	}
	if resolves(ctx, dir, BaseRef) {
		return BaseRef
	}
	return from
}

func resolves(ctx context.Context, dir, ref string) bool {
	_, err := runGit(ctx, dir, gitEnv(), "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	return err == nil
}

func validateFetch(spec forge.CloneSpec, headSHA, baseSHA string) error {
	if !shaPattern.MatchString(headSHA) {
		return fmt.Errorf("head sha %q is not a 40-hex commit id", headSHA)
	}
	if baseSHA != "" && !shaPattern.MatchString(baseSHA) {
		return fmt.Errorf("base sha %q is not a 40-hex commit id", baseSHA)
	}
	if err := validateURL(spec.URL, spec.AuthHeader != ""); err != nil {
		return err
	}
	if spec.HeadRef == "" {
		return errors.New("head ref is required")
	}
	for _, ref := range []string{spec.HeadRef, spec.BaseRef} {
		if ref != "" && !validRef(ref) {
			return fmt.Errorf("ref %q is not a valid fetch ref", ref)
		}
	}
	if strings.ContainsAny(spec.AuthHeader, "\r\n") {
		return errors.New("auth header must be a single line")
	}
	return nil
}

func validateURL(raw string, authenticated bool) error {
	if raw == "" || strings.HasPrefix(raw, "-") {
		return fmt.Errorf("clone url %q is not valid", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("clone url does not parse")
	}
	switch u.Scheme {
	case "https", "file":
	case "http":
		if authenticated {
			return errors.New("clone url must use https when an auth header is set")
		}
	default:
		return fmt.Errorf("clone url scheme %q is not allowed", u.Scheme)
	}
	if u.User != nil {
		return errors.New("clone url must not carry credentials")
	}
	return nil
}

// validRef accepts a full ref name that git can use on the left side of a
// refspec without it being read as an option, a pattern or a second ref.
func validRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".lock") || strings.Contains(ref, "..") ||
		strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return false
	}
	for _, r := range ref {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(":~^?*[\\", r) {
			return false
		}
	}
	return true
}

// gitEnv is the process environment without the system and global git
// config and without inherited config injection, so a url rewrite, a
// credential helper, an extra header or a hook configured on the host
// cannot change where the fetch goes or what it sends.
func gitEnv() []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "GIT_CONFIG_COUNT" || key == "GIT_CONFIG_PARAMETERS" ||
			key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_SYSTEM" || key == "GIT_CONFIG_NOSYSTEM" ||
			strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
	)
}

func runGit(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
				if len(msg) > maxGitOutput {
					msg = msg[:maxGitOutput]
				}
				return out, fmt.Errorf("%w: %s", err, msg)
			}
		}
		return out, err
	}
	return out, nil
}
