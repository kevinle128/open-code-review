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

func TestCommitLink(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	if got := CommitLink("https://git.example/", "g/p", "/-/commit/", sha); got != "https://git.example/g/p/-/commit/"+sha {
		t.Fatalf("link %q", got)
	}
	for _, c := range []struct{ base, repo, sha string }{
		{"", "g/p", sha}, {"https://x", "p", sha}, {"https://x", "g/../p", sha},
		{"https://x", "g/p)](javascript:x", sha}, {"https://x", "g/p", "HEAD"}, {"https://x", "g/p", sha[:39]},
	} {
		if got := CommitLink(c.base, c.repo, "/commit/", c.sha); got != "" {
			t.Fatalf("CommitLink(%q, %q, %q) = %q", c.base, c.repo, c.sha, got)
		}
	}
}

func TestLineLink(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	if got := LineLink("https://github.com", "o/r", "/blob/", sha, "a b/c#d.go", 88); got != "https://github.com/o/r/blob/"+sha+"/a%20b/c%23d.go#L88" {
		t.Fatalf("link %q", got)
	}
	if got := LineLink("https://git.example/", "g/p", "/-/blob/", sha, "README.md", 0); got != "https://git.example/g/p/-/blob/"+sha+"/README.md" {
		t.Fatalf("link %q", got)
	}
	for _, c := range []struct{ repo, sha, path string }{
		{"o/r", sha, ""}, {"o/r", sha, "a/../b"}, {"o/r", sha, "a//b"}, {"o/r", sha, "./a"}, {"o/r", "HEAD", "a"}, {"o", sha, "a"},
	} {
		if got := LineLink("https://x", c.repo, "/blob/", c.sha, c.path, 1); got != "" {
			t.Fatalf("LineLink(%q, %q, %q) = %q", c.repo, c.sha, c.path, got)
		}
	}
}
