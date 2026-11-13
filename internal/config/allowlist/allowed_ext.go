// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package allowedext provides file-level filtering for code review:
// an extension allowlist (which file types to review) and a path-based
// exclude list (which files to skip regardless of extension).
//
// # The default_exclude_patterns.json format
//
// The file holds a JSON array of strings, each one a glob exclude pattern.
// Supported wildcard syntax (as implemented by the doublestar library):
//
//	"*"        matches any character within a single path segment (never crosses /)
//	           e.g. "*_test.go" matches "foo_test.go" but not "pkg/foo_test.go"
//
//	"**"       matches zero or more path segments (may cross /)
//	           e.g. "**/*_test.go" matches both "foo_test.go" and "a/b/c_test.go"
//
//	"{a,b,c}"  brace expansion, matches any one of the alternatives
//	           e.g. "**/*.{js,ts}" matches .js and .ts files at any depth
//
// Combined examples:
//
//	"**/*_test.go"                 — Go test files at any depth
//	"**/src/test/java/**/*.java"   — everything under the standard Java test directory
//	"**/*.spec.{js,jsx,ts,tsx}"    — frontend spec test files at any depth
//	"*_test.go"                    — Go test files in the root directory only (does not cross directories)
package allowedext

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
)

//go:embed supported_file_types.json
var defaultData []byte

//go:embed default_exclude_patterns.json
var excludeData []byte

var (
	supported map[string]bool
	initOnce  sync.Once
)

var (
	excludePatterns []string // raw patterns from JSON (may contain {a,b} syntax)
	nonTestPatterns []string // excludePatterns without testPatterns
	excludeOnce     sync.Once
)

// testPatterns are the default exclude patterns that name only test code.
// IsExcludedNonTestPath drops them so a caller can review tests. Test data,
// fixtures, snapshots, generated code and dependency or build directories
// are not in this list: they stay excluded either way.
var testPatterns = []string{
	"**/*_test.go",
	"**/src/test/java/**/*.java",
	"**/src/test/**/*.{kt,kts}",
	"**/*Test.fs",
	"**/*.test.{js,jsx,ts,tsx}",
	"**/*.spec.{js,jsx,ts,tsx}",
	"**/__tests__/**",
	"**/test/**/*_test.py",
	"**/tests/**/*_test.py",
	"**/*_test.py",
	"**/test_*.py",
	"**/*_spec.rb",
	"**/spec/**/*_spec.rb",
	"**/*Test.java",
	"**/*Tests.java",
	"**/*_test.rs",
	"**/*.test.ets",
	"**/test/**/*.jl",
	"**/test/**/*.hs",
	"**/*Spec.hs",
	"**/test/**/*.lhs",
	"**/*Spec.lhs",
	"**/tests/**/*.nim",
	"**/tests/**/*.nu",
	"**/test/**/*.nu",
	"**/*_test.nu",
	"**/test_*.nu",
	"**/test-*.nu",
	"**/tests/**/*.R",
	"**/test/**/*.{clj,cljs,cljc}",
	"**/*_test.{clj,cljs,cljc}",
	"**/*Test.swift",
	"**/*Tests.swift",
	"**/Tests/**/*.swift",
	"**/tests/**/*.elm",
	"**/test/**/*.zig",
	"**/*_test.zig",
	"**/test/**/*.ml",
	"**/tb_*.{v,sv,vhd,vhdl}",
	"**/*_tb.{v,sv,vhd,vhdl}",
	"**/*.t.sol",
	"**/test/**/*.sol",
	"**/tests/**/*.sol",
	"**/test/**/*.vy",
	"**/tests/**/*.vy",
}

func initMap() {
	var exts []string
	if err := json.Unmarshal(defaultData, &exts); err != nil {
		panic("allowedext: failed to parse supported_file_types.json: " + err.Error())
	}
	supported = make(map[string]bool, len(exts))
	for _, e := range exts {
		supported[strings.ToLower(e)] = true
	}
}

func initExclude() {
	if err := json.Unmarshal(excludeData, &excludePatterns); err != nil {
		panic("allowedext: failed to parse default_exclude_patterns.json: " + err.Error())
	}
	tests := make(map[string]bool, len(testPatterns))
	for _, p := range testPatterns {
		tests[strings.ToLower(p)] = true
	}
	for i, p := range excludePatterns {
		excludePatterns[i] = strings.ToLower(p)
		if !tests[excludePatterns[i]] {
			nonTestPatterns = append(nonTestPatterns, excludePatterns[i])
		}
	}
}

// IsAllowedExt returns true when the given file extension is in the supported types list.
// The check is case-insensitive.
func IsAllowedExt(ext string) bool {
	initOnce.Do(initMap)
	return supported[strings.ToLower(ext)]
}

// IsExcludedPath returns true when the given file path matches any default exclude pattern.
// Patterns support ** (recursive directory matching), * (single-segment wildcard),
// and {a,b,c} brace expansion. The check is case-insensitive.
//
// Example patterns and their behavior:
//
//	"**/*_test.go"       matches "foo_test.go", "pkg/bar_test.go", "a/b/c_test.go"
//	"*_test.go"          matches "foo_test.go" only (no directory traversal)
//	"**/*.test.{js,ts}"  matches "src/app.test.js", "lib/util.test.ts"
func IsExcludedPath(path string) bool {
	excludeOnce.Do(initExclude)
	return matchesAny(excludePatterns, path)
}

// IsExcludedNonTestPath is IsExcludedPath without the test-file patterns, for
// a review that covers tests too.
func IsExcludedNonTestPath(path string) bool {
	excludeOnce.Do(initExclude)
	return matchesAny(nonTestPatterns, path)
}

func matchesAny(patterns []string, path string) bool {
	lowerPath := strings.ToLower(path)
	for _, pattern := range patterns {
		if matched, _ := doublestar.Match(pattern, lowerPath); matched {
			return true
		}
	}
	return false
}
