// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"io/fs"
	"slices"
	"sort"
	"strings"
	"testing"
)

func mustLocale(t *testing.T, name string) Locale {
	t.Helper()
	loc, err := LoadLocale(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestLocaleFiles_SameKeys(t *testing.T) {
	en, err := readLocale("en")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(localeFS, "locale")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".txt")
		if name == "en" {
			continue
		}
		other, err := readLocale(name)
		if err != nil {
			t.Fatal(err)
		}
		var missing, extra []string
		for k := range en {
			if _, ok := other[k]; !ok {
				missing = append(missing, k)
			}
		}
		for k := range other {
			if _, ok := en[k]; !ok {
				extra = append(extra, k)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 || len(extra) > 0 {
			t.Fatalf("%s.txt drifted from en.txt: missing %v, extra %v", name, missing, extra)
		}
	}
}

func TestLocale_EveryKeyTheServiceUsesExists(t *testing.T) {
	en := mustLocale(t, "en")
	keys := []string{"summary.status." + ChainRunning, "summary.status." + ChainSuccess, "summary.status." + ChainFailure,
		"status." + ChainSuccess, "status." + ChainFailure, "status.draft", "status.waiting", "round.other_changes", "round.diff_summary", "round.diff_shared",
		"round.diff_cut", "round.proposed_diff", "round.proposal_cut", "round.body_cut", "round.range_full", "round.range_fix",
		"round.range_commit", "round.found_value", "round.found_value_one", "round.found_zero", "round.fixed_value",
		"round.fixed_count", "round.fixed_nothing", "round.tests_output", "round.scope_value", "round.next_value", "round.end_value",
		"finding.section_title", "finding.location", "finding.whole_pr"}
	for _, r := range []string{resultFixed, resultReported, resultDropped} {
		keys = append(keys, "finding.result."+r)
	}
	for s := range severityOrder {
		keys = append(keys, "severity."+s)
	}
	for _, r := range []string{ReportLimit, ReportFork, ReportPush, ReportEngine, ReportNoEdit} {
		keys = append(keys, "round.report."+r)
	}
	// The per-file failure classes of session.FailureClass.
	for _, c := range []string{"provider", "timeout", "cancelled", "configuration", "input", "budget", "panic", "unknown"} {
		keys = append(keys, "failure."+c)
	}
	for _, k := range keys {
		if _, ok := en[k]; !ok {
			t.Fatalf("%s missing", k)
		}
	}
}

func TestLoadLocale_Errors(t *testing.T) {
	for _, name := range []string{"", "fr", "../en", "en.txt"} {
		if _, err := LoadLocale(name); err == nil {
			t.Fatalf("%q loaded", name)
		}
	}
}

func TestLoadLocale_FallsBackToEnglish(t *testing.T) {
	vi := mustLocale(t, "vi")
	en := mustLocale(t, "en")
	if vi["summary.title"] == en["summary.title"] {
		t.Fatal("vi did not override en")
	}
	if len(vi) != len(en) {
		t.Fatalf("merged size %d, en %d", len(vi), len(en))
	}
}

func TestParseLocale_Sections(t *testing.T) {
	loc, err := parseLocale("t", []byte("top = 1\n[round]\ntitle = x\n [ finding.result ] \nfixed = y\n"))
	if err != nil {
		t.Fatal(err)
	}
	if loc["top"] != "1" || loc["round.title"] != "x" || loc["finding.result.fixed"] != "y" || len(loc) != 3 {
		t.Fatalf("%q", loc)
	}
	for _, bad := range []string{"[round", "[]", "[a b]", "[a..b]"} {
		if _, err := parseLocale("t", []byte(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	if _, err := parseLocale("t", []byte("[a]\nk = 1\n[b]\nk = 2\n[a]\nk = 3")); err == nil {
		t.Fatal("a key repeated in a reopened section parsed")
	}
}

func TestMerge_FallsBackToEnglish(t *testing.T) {
	base := Locale{"a": "A {x}", "b": "B {x} {y}", "c": "C", "d": "D {x}"}
	over := Locale{"a": "a2 {x} {x}", "b": "b2 {x}", "d": "d2 {z}", "extra": "e"}
	got := merge(base, over)
	want := Locale{"a": "a2 {x} {x}", "b": "B {x} {y}", "c": "C", "d": "D {x}"}
	if len(got) != len(want) {
		t.Fatalf("%q", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestLocaleFiles_PlaceholdersMatch(t *testing.T) {
	en, err := readLocale("en")
	if err != nil {
		t.Fatal(err)
	}
	vi, err := readLocale("vi")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range vi {
		if !slices.Equal(placeholders(v), placeholders(en[k])) {
			t.Fatalf("vi %s has placeholders %v, en %v", k, placeholders(v), placeholders(en[k]))
		}
	}
}

func TestParseLocale(t *testing.T) {
	loc, err := parseLocale("t", []byte("# comment\n\n a = one\\ntwo \nb=x=y\nempty =\n"))
	if err != nil {
		t.Fatal(err)
	}
	if loc["a"] != "one\ntwo" || loc["b"] != "x=y" || loc["empty"] != "" {
		t.Fatalf("%q", loc)
	}
	for _, bad := range []string{"no equals sign", " = value", "a = 1\na = 2"} {
		if _, err := parseLocale("t", []byte(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestLocaleText(t *testing.T) {
	loc := Locale{"k": "{a} and {b} and {a}", "plain": "x"}
	if got := loc.Text("k", "a", "1", "b", "2"); got != "1 and 2 and 1" {
		t.Fatal(got)
	}
	if got := loc.Text("k"); got != "{a} and {b} and {a}" {
		t.Fatal(got)
	}
	if got := loc.Text("k", "a"); got != "{a} and {b} and {a}" {
		t.Fatal(got)
	}
	if got := loc.Text("missing.key"); got != "missing.key" {
		t.Fatal(got)
	}
}
