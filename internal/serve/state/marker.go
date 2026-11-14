// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package state holds the review chain state of a PR, kept as a marker in
// the sticky summary, and the text the service posts.
package state

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// Round scopes: a full round reviews the whole PR, a commit round one
// commit against its first parent.
const (
	ScopeFull   = "full"
	ScopeCommit = "commit"
)

// Chain states. A chain runs from round 1 until a round finds no real
// finding, a round cannot push its fix, or the report-only round after the
// last fix round has reported; then it has succeeded. A round that fails
// leaves it failed until a new chain succeeds.
const (
	ChainRunning = "running"
	ChainSuccess = "success"
	ChainFailure = "failure"
)

const markerVersion = 2

// Round is one round of the chain. From is empty for a commit round. Real
// counts the findings the fix run judged real and Fixed the ones it fixed;
// Fix is the fix commit the round pushed, or empty.
type Round struct {
	N     int    `json:"n"`
	Scope string `json:"scope"`
	From  string `json:"from"`
	To    string `json:"to"`
	Real  int    `json:"real"`
	Fixed int    `json:"fixed"`
	Fix   string `json:"fix"`
}

// Marker is the chain state kept inside the sticky summary. Max is the
// number of fix rounds in force when written, for display only. Rounds
// holds the rounds of the current chain.
type Marker struct {
	V      int     `json:"v"`
	Status string  `json:"status"`
	Max    int     `json:"max"`
	Rounds []Round `json:"rounds"`
}

var (
	markerRE = regexp.MustCompile(`<!-- ocr-serve:v([0-9]+) ([A-Za-z0-9+/]+={0,2}) -->`)
	shaRE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// prefixRE finds anything that looks like a marker, valid or not, so a
	// body with one good and one mangled marker is still ambiguous.
	prefixRE = regexp.MustCompile(`<!-- ocr-serve:v`)
)

// Encode renders m as a hidden HTML comment.
func Encode(m Marker) string {
	m.V = markerVersion
	b, _ := json.Marshal(m)
	return fmt.Sprintf("<!-- ocr-serve:v%d %s -->", markerVersion, base64.StdEncoding.EncodeToString(b))
}

// Decode reads the marker in body. It returns nil, nil when body has no
// marker or only a version 1 marker, which the earlier thread-based flow
// wrote and which holds no chain. It returns an error when body has more
// than one marker or the one it has is not valid.
func Decode(body string) (*Marker, error) {
	switch n := len(prefixRE.FindAllStringIndex(body, -1)); {
	case n == 0:
		return nil, nil
	case n > 1:
		return nil, fmt.Errorf("found %d state markers, want one", n)
	}
	m := markerRE.FindStringSubmatch(body)
	if m == nil {
		return nil, errors.New("state marker is malformed")
	}
	if m[1] == "1" {
		return nil, nil
	}
	if m[1] != fmt.Sprint(markerVersion) {
		return nil, fmt.Errorf("state marker version %s is unknown", m[1])
	}
	raw, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil {
		return nil, fmt.Errorf("state marker is not base64: %w", err)
	}
	var out Marker
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("state marker is not JSON: %w", err)
	}
	if err := out.validate(); err != nil {
		return nil, fmt.Errorf("state marker is invalid: %w", err)
	}
	return &out, nil
}

func (m Marker) validate() error {
	if m.V != markerVersion {
		return fmt.Errorf("version %d", m.V)
	}
	switch m.Status {
	case ChainRunning, ChainSuccess, ChainFailure:
	default:
		return fmt.Errorf("status %q is unknown", m.Status)
	}
	if m.Max < 0 {
		return errors.New("negative round limit")
	}
	for i, r := range m.Rounds {
		if r.N < 1 || r.Real < 0 || r.Fixed < 0 {
			return fmt.Errorf("rounds[%d] has a negative or zero count", i)
		}
		switch r.Scope {
		case ScopeFull, ScopeCommit:
		default:
			return fmt.Errorf("rounds[%d].scope %q is unknown", i, r.Scope)
		}
		for field, v := range map[string]string{"from": r.From, "fix": r.Fix} {
			if v != "" && !shaRE.MatchString(v) {
				return fmt.Errorf("rounds[%d].%s is not a 40-hex SHA", i, field)
			}
		}
		if !shaRE.MatchString(r.To) {
			return fmt.Errorf("rounds[%d].to is not a 40-hex SHA", i)
		}
	}
	return nil
}

// CommitState is the commit status that shows the chain state.
func (m Marker) CommitState() forge.CommitState {
	switch m.Status {
	case ChainRunning:
		return forge.StatusPending
	case ChainSuccess:
		return forge.StatusSuccess
	}
	return forge.StatusFailure
}

// Preserve keeps the state when a summary body is rewritten without one: if
// newBody has no marker and oldBody has exactly one, that marker is appended.
// With two markers in oldBody nothing is copied, because picking one would
// silently resolve the ambiguity that Decode rejects.
func Preserve(newBody, oldBody string) string {
	if oldBody == "" || prefixRE.MatchString(newBody) {
		return newBody
	}
	if len(prefixRE.FindAllStringIndex(oldBody, -1)) != 1 {
		return newBody
	}
	found := markerRE.FindString(oldBody)
	if found == "" {
		return newBody
	}
	return newBody + "\n\n" + found
}
