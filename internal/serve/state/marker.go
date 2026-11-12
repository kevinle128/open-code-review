// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package state holds the round state of a PR: the marker codec, the pure
// decision table, and the text the service posts.
package state

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Run kinds recorded in the marker.
const (
	KindFull         = "full"
	KindIncremental  = "incremental"
	KindManualFull   = "manual-full"
	KindManualCommit = "manual-commit"
)

const (
	markerVersion = 1
	maxRuns       = 30
)

// Run is one review run. N is the automatic round number, 0 for manual runs.
// From is empty for a manual commit run, which reviews HEAD against its first
// parent.
type Run struct {
	N    int      `json:"n"`
	Kind string   `json:"kind"`
	From string   `json:"from"`
	To   string   `json:"to"`
	IDs  []string `json:"ids"`
}

// Marker is the round state kept inside the sticky summary. Auto counts the
// automatic rounds done. Max is the limit in force when written, for display
// only; the configuration is authoritative. Reviewed and Base are the HEAD
// and base SHA of the last automatic round or manual full review.
type Marker struct {
	V          int    `json:"v"`
	Auto       int    `json:"auto"`
	Max        int    `json:"max"`
	Reviewed   string `json:"reviewed"`
	Base       string `json:"base"`
	LimitNoted bool   `json:"limit_noted"`
	Runs       []Run  `json:"runs"`
}

var (
	markerRE = regexp.MustCompile(`<!-- ocr-serve:v1 ([A-Za-z0-9+/]+={0,2}) -->`)
	shaRE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// prefixRE finds anything that looks like a marker, valid or not, so a
	// body with one good and one mangled marker is still ambiguous.
	prefixRE = regexp.MustCompile(`<!-- ocr-serve:v`)
)

// Encode renders m as a hidden HTML comment.
func Encode(m Marker) string {
	b, _ := json.Marshal(m)
	return "<!-- ocr-serve:v1 " + base64.StdEncoding.EncodeToString(b) + " -->"
}

// Decode reads the marker in body. It returns nil, nil when body has no
// marker, and an error when it has more than one or the one it has is not
// valid. Callers must not treat an error as "no marker": that would reset the
// round count.
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
	raw, err := base64.StdEncoding.DecodeString(m[1])
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
	if m.Auto < 0 || m.Max < 0 {
		return errors.New("negative round count")
	}
	if err := optionalSHA("reviewed", m.Reviewed); err != nil {
		return err
	}
	if err := optionalSHA("base", m.Base); err != nil {
		return err
	}
	for i, r := range m.Runs {
		switch r.Kind {
		case KindFull, KindIncremental, KindManualFull, KindManualCommit:
		default:
			return fmt.Errorf("runs[%d].kind %q is unknown", i, r.Kind)
		}
		if err := optionalSHA(fmt.Sprintf("runs[%d].from", i), r.From); err != nil {
			return err
		}
		if !shaRE.MatchString(r.To) {
			return fmt.Errorf("runs[%d].to is not a 40-hex SHA", i)
		}
	}
	return nil
}

func optionalSHA(field, v string) error {
	if v != "" && !shaRE.MatchString(v) {
		return fmt.Errorf("%s is not a 40-hex SHA", field)
	}
	return nil
}

// Initial is the marker written with the round-1 placeholder, so a round 1
// that fails or is interrupted leaves a readable marker with Auto 0.
func Initial(limit int) Marker {
	return Marker{V: markerVersion, Max: limit}
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
