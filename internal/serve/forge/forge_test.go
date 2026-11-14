// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package forge

import "testing"

func TestPRRefKey_IgnoresPlatformIDs(t *testing.T) {
	a := PRRef{Platform: GitHub, Repo: "o/r", Number: 7, InstallationID: 1}
	b := PRRef{Platform: GitHub, Repo: "o/r", Number: 7, InstallationID: 2}
	if a.Key() != "github:o/r#7" || a.Key() != b.Key() {
		t.Fatalf("keys: %q %q", a.Key(), b.Key())
	}
	c := PRRef{Platform: GitLab, Repo: "o/r", Number: 7}
	if c.Key() == a.Key() {
		t.Fatal("platform must be part of the key")
	}
}
