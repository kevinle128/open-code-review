// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/alibaba/open-code-review/internal/config/rules"
	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
	"github.com/alibaba/open-code-review/internal/serve/state"
)

// errHeadMoved asks the queue to run the job again: HEAD changed while the
// checkout was fetched, so the round would cover the wrong commit.
var errHeadMoved = errors.New("head moved during checkout")

// errShutdown ends a round that a shutdown cut; the journal keeps it, so it
// runs again after the restart.
var errShutdown = errors.New("round cut by shutdown")

// process runs one job and reports whether the queue should run it again.
func (s *Server) process(ref forge.PRRef, j job) bool {
	log := withPR(withRequest(s.log, j.requestID), ref)
	f := s.forges[ref.Platform]
	var err error
	switch {
	case j.cmd != nil && j.cmd.reject != "":
		log.Info("command rejected", "reason", j.cmd.reject)
		s.commandFailed(f, ref, j.cmd, j.cmd.reject, log)
		return false
	case j.cmd != nil:
		rj := roundJob{n: 1, scope: state.ScopeFull, gen: j.cmd.gen, effort: j.cmd.Effort}
		if j.cmd.Kind == CommandReviewCommit {
			rj.scope = state.ScopeCommit
		}
		err = s.round(f, ref, rj, j, log)
	case j.round != nil:
		err = s.round(f, ref, *j.round, j, log)
	default:
		err = s.evaluate(f, ref, j, log)
	}
	if errors.Is(err, errHeadMoved) {
		log.Info("head moved, running again")
		return true
	}
	return false
}

// repoConfig reads serve.json at the base SHA, never the PR head, so a PR
// cannot change its own limits. A missing or invalid file falls back to the
// defaults. A read that fails returns an error instead: the defaults would
// turn review on for a repository that set "enabled": false.
func (s *Server) repoConfig(ctx context.Context, f forge.Forge, ref forge.PRRef, pr forge.PR, log *slog.Logger) (serveconfig.RepoConfig, error) {
	base := s.cfg.RepoDefaults()
	raw, found, err := f.ReadFile(ctx, ref, pr.BaseSHA, serveconfig.RepoConfigPath, serveconfig.MaxRepoConfigBytes)
	if err != nil {
		return base, fmt.Errorf("read repository config: %w", err)
	}
	if !found {
		return base, nil
	}
	out, ignoredKeys, err := serveconfig.MergeRepo(base, raw)
	if len(ignoredKeys) > 0 {
		log.Warn("repository config keys ignored", "keys", ignoredKeys)
	}
	if err != nil {
		log.Error("repository config invalid; using defaults", "error", err.Error())
	}
	return out, nil
}

// maxLoggedReason bounds each warning and failed file's error text in the
// log; a large PR can fail many files with the same long provider error.
const maxLoggedReason = 300

// logCoverage names each file a run could not review, with its failure
// class and error text, so a partial or failed run can be explained.
func logCoverage(log *slog.Logger, res review.Result) {
	c := res.Coverage
	if len(c.Failed) == 0 {
		return
	}
	items := make([]string, len(c.Failed))
	for i, it := range c.Failed {
		name := it.Path
		if it.OldPath != "" && it.OldPath != it.Path {
			name = it.OldPath + " -> " + it.Path
		}
		items[i] = name + " (" + it.Class + "): " + truncate(it.Reason, maxLoggedReason)
	}
	log.Warn("review coverage incomplete", "terminal", res.Terminal, "files", c.Total,
		"reviewed_files", c.Reviewed, "failed_files", len(c.Failed), "failed", items)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// projectRule reads rule.json, and the rule files it names, at the base SHA
// for the same reason as serve.json: the checkout is the PR head, so a PR
// could otherwise exclude its own changes or rewrite its own review rules.
// It runs before anything is posted. When rule.json cannot be read, the run
// does not start: a review without the repository's excludes would post
// findings on files it excludes. A rule file that cannot be read leaves only
// its own rule empty, as in the CLI. An invalid rule.json is the
// repository's choice, so it reviews with the built-in rules.
func (s *Server) projectRule(ctx context.Context, f forge.Forge, ref forge.PRRef, pr forge.PR, log *slog.Logger) (*rules.ProjectRule, error) {
	raw, found, err := f.ReadFile(ctx, ref, pr.BaseSHA, rules.ProjectRulePath, rules.MaxRuleFileBytes)
	if err != nil || !found {
		return nil, err
	}
	out, err := rules.ParseProjectRule(raw, func(path string) ([]byte, bool, error) {
		b, ok, err := f.ReadFile(ctx, ref, pr.BaseSHA, path, rules.MaxRuleFileBytes)
		if err != nil {
			log.Warn("read rule file failed; its rule is left empty", "path", path, "error", err.Error())
		}
		return b, ok, err
	})
	if err != nil {
		log.Error("project rules invalid; using built-in rules only", "error", err.Error())
		return nil, nil
	}
	return out, nil
}

// fetch checks out HEAD. When the fetch fails because HEAD moved, the PR is
// read again so the caller can tell a new push from a real failure.
func (s *Server) fetch(ctx context.Context, f forge.Forge, ref forge.PRRef, pr forge.PR, ws review.Workspace) error {
	spec, err := f.Clone(ctx, pr)
	if err != nil {
		return fmt.Errorf("clone spec: %w", err)
	}
	ferr := ws.Fetch(ctx, spec, pr.HeadSHA, pr.BaseSHA)
	if ferr == nil {
		return nil
	}
	if cur, err := f.GetPR(ctx, ref); err == nil && cur.HeadSHA != pr.HeadSHA {
		return fmt.Errorf("%w: %s -> %s", errHeadMoved, pr.HeadSHA, cur.HeadSHA)
	}
	return ferr
}

// failureReason keeps internal error text out of PR comments; the request id
// leads an operator to the full error in the log.
func (s *Server) failureReason(stage, requestID string) string {
	return fmt.Sprintf("review round failed at %s (request %s)", stage, requestID)
}

// commandDone acknowledges a command whose first round ended; the round
// comment is the answer.
func (s *Server) commandDone(f forge.Forge, ref forge.PRRef, cmd *cmdJob, log *slog.Logger) {
	if cmd == nil || cmd.commentID == "" {
		return
	}
	ctx, cancel := s.sideContext()
	defer cancel()
	if err := f.React(ctx, ref, cmd.commentID, forge.ReactionSuccess); err != nil {
		log.Warn("reaction failed", "reaction", string(forge.ReactionSuccess), "error", err.Error())
	}
}

func (s *Server) commandFailed(f forge.Forge, ref forge.PRRef, cmd *cmdJob, reason string, log *slog.Logger) {
	if cmd == nil || cmd.commentID == "" {
		return
	}
	ctx, cancel := s.sideContext()
	defer cancel()
	if err := f.React(ctx, ref, cmd.commentID, forge.ReactionFailure); err != nil {
		log.Warn("reaction failed", "reaction", string(forge.ReactionFailure), "error", err.Error())
	}
	if err := f.PostNote(ctx, ref, state.RenderCommandError(reason, s.mention(ref.Platform), s.loc)); err != nil {
		log.Error("post command reply", "error", err.Error())
	}
}

// sideContext outlives a cancelled review so the result can still be
// reported, but stays bounded so shutdown is not held up for long.
func (s *Server) sideContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.runCtx), sideCallTimeout)
}
