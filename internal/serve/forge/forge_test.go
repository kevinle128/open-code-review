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

func TestFindingTag_RoundTrip(t *testing.T) {
	id := "r1-1a2b3c4d-0f0e0d0c"
	body := FindingTag(id) + "\n**[bug]** text"
	got, ok := ParseFindingID(body)
	if !ok || got != id {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestParseFindingID_Absent(t *testing.T) {
	for _, body := range []string{"", "plain text", "<!-- ocr-finding: -->", "<!-- ocr-finding:bad id -->"} {
		if id, ok := ParseFindingID(body); ok {
			t.Fatalf("%q parsed as %q", body, id)
		}
	}
}
