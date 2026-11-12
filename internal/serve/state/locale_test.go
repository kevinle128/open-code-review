// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"io/fs"
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

func TestLocale_EveryCategoryAndSeverityHasAKey(t *testing.T) {
	en := mustLocale(t, "en")
	for _, c := range []string{"bug", "security", "performance", "maintainability", "test", "style", "documentation", "other"} {
		if _, ok := en["category."+c]; !ok {
			t.Fatalf("category.%s missing", c)
		}
	}
	for _, s := range []string{"critical", "high", "medium", "low"} {
		if _, ok := en["severity."+s]; !ok {
			t.Fatalf("severity.%s missing", s)
		}
	}
	for _, k := range []string{KindFull, KindIncremental, KindManualFull, KindManualCommit} {
		if _, ok := en["run.kind."+k]; !ok {
			t.Fatalf("run.kind.%s missing", k)
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
