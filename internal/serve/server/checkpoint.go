// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/review"
)

// checkpointsDir holds, per PR, the parts of a round that finished: each
// review batch answer and the whole review result. A round that a restart
// or a failure stopped runs again from its start, but reuses these parts
// instead of running them again, so a retry loses only the unfinished work.
const checkpointsDir = "checkpoints"

// reviewFile keeps the result of a finished review stage.
const reviewFile = "review.json"

// savedReview is the review result of one round with what it reviewed. A
// saved result is used only for the same base, head, scope, commit and effort.
type savedReview struct {
	Base   string        `json:"base,omitempty"`
	Head   string        `json:"head"`
	Scope  string        `json:"scope"`
	Commit string        `json:"commit,omitempty"`
	Effort string        `json:"effort,omitempty"`
	Result review.Result `json:"result"`
}

// prCheckpoints is the checkpoint directory of a PR, or "" without a data
// directory. The name hashes the PR key, which holds '/' and ':'.
func (s *Server) prCheckpoints(ref forge.PRRef) string {
	if s.cfg.DataDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ref.Key()))
	return filepath.Join(s.cfg.DataDir, checkpointsDir, hex.EncodeToString(sum[:])[:24])
}

// roundCheckpoints is the checkpoint directory of round n of a PR.
func (s *Server) roundCheckpoints(ref forge.PRRef, n int) string {
	dir := s.prCheckpoints(ref)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "round-"+strconv.Itoa(n))
}

// clearCheckpoints drops the saved parts of a PR after its round finishes.
func (s *Server) clearCheckpoints(ref forge.PRRef, log *slog.Logger) {
	dir := s.prCheckpoints(ref)
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Warn("remove round checkpoints", "error", err.Error())
	}
}

// loadReview returns the saved review of the round when it reviewed the
// same base, head, scope, commit and effort.
func loadReview(dir, base, head, scope, commit, effort string) (review.Result, bool) {
	if dir == "" {
		return review.Result{}, false
	}
	b, err := os.ReadFile(filepath.Join(dir, reviewFile))
	if err != nil {
		return review.Result{}, false
	}
	var sv savedReview
	if json.Unmarshal(b, &sv) != nil || sv.Base != base || sv.Head != head || sv.Scope != scope || sv.Commit != commit || sv.Effort != effort {
		return review.Result{}, false
	}
	return sv.Result, true
}

// saveReview keeps the review result through a rename, so a restart leaves
// the whole file or none of it.
func saveReview(dir string, sv savedReview) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(sv)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, reviewFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, reviewFile))
}
