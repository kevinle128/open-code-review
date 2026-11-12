// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"reflect"
	"strings"
	"testing"
)

func TestMergeRepo_AllowlistedKeys(t *testing.T) {
	base := RepoConfig{Enabled: true, MaxRounds: 3, Exclude: []string{"vendor/**"}}
	raw := `{"enabled": false, "max_rounds": 5, "exclude": ["**/*.pb.go"], "background": "payments service",
		"effort": "high", "language": "en", "workers": 9}`
	got, ignored, err := MergeRepo(base, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := RepoConfig{Enabled: false, MaxRounds: 5, Exclude: []string{"vendor/**", "**/*.pb.go"}, Background: "payments service", Effort: "high"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if !reflect.DeepEqual(ignored, []string{"language", "workers"}) {
		t.Fatalf("ignored %v", ignored)
	}
	if len(base.Exclude) != 1 {
		t.Fatalf("base mutated: %v", base.Exclude)
	}
}

func TestMergeRepo_EmptyAndNull(t *testing.T) {
	base := RepoConfig{Enabled: true, MaxRounds: 3}
	for _, raw := range []string{"", "  \n", `{}`, `{"enabled": null, "max_rounds": null}`} {
		got, ignored, err := MergeRepo(base, []byte(raw))
		if err != nil || !reflect.DeepEqual(got, base) || ignored != nil {
			t.Fatalf("%q: got %+v %v %v", raw, got, ignored, err)
		}
	}
}

func TestMergeRepo_Rejects(t *testing.T) {
	base := RepoConfig{Enabled: true, MaxRounds: 3}
	many := `["` + strings.Repeat(`a",  "`, MaxExcludePatterns) + `a"]`
	cases := []struct {
		name, raw, want string
	}{
		{"not json", `{`, "JSON object"},
		{"array", `[]`, "JSON object"},
		{"too large", `{"background":"` + strings.Repeat("x", MaxRepoConfigBytes) + `"}`, "exceeds"},
		{"enabled type", `{"enabled": "yes"}`, "enabled"},
		{"rounds high", `{"max_rounds": 11}`, "max_rounds"},
		{"rounds low", `{"max_rounds": 0}`, "max_rounds"},
		{"rounds type", `{"max_rounds": "3"}`, "max_rounds"},
		{"exclude type", `{"exclude": "a"}`, "exclude"},
		{"exclude too many", `{"exclude": ` + many + `}`, "at most 50"},
		{"exclude empty glob", `{"exclude": [" "]}`, "invalid glob"},
		{"exclude bad glob", `{"exclude": ["[a-"]}`, "invalid glob"},
		{"background type", `{"background": 1}`, "background"},
		{"background long", `{"background": "` + strings.Repeat("\u00e9", MaxBackgroundChars+1) + `"}`, "at most 4000"},
		{"effort value", `{"effort": "max"}`, "effort"},
		{"effort type", `{"effort": 2}`, "effort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := MergeRepo(base, []byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
			if !reflect.DeepEqual(got, base) {
				t.Fatalf("on error got %+v, want base", got)
			}
		})
	}
}

func TestMergeRepo_BackgroundAtLimit(t *testing.T) {
	raw := `{"background": "` + strings.Repeat("\u00e9", MaxBackgroundChars) + `"}`
	got, _, err := MergeRepo(RepoConfig{}, []byte(raw))
	if err != nil || len([]rune(got.Background)) != MaxBackgroundChars {
		t.Fatalf("err=%v len=%d", err, len([]rune(got.Background)))
	}
}

func TestParseCommandOverrides(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		effort string
		err    string
	}{
		{"none", nil, "", ""},
		{"effort", []string{"--effort=low"}, "low", ""},
		{"effort high", []string{"--effort=high"}, "high", ""},
		{"bad value", []string{"--effort=max"}, "", "invalid effort"},
		{"twice", []string{"--effort=low", "--effort=high"}, "", "more than once"},
		{"other key", []string{"--pr_reviewer.num_max_findings=1"}, "", "unsupported option"},
		{"positional", []string{"please"}, "", "unsupported option"},
		{"long arg clipped", []string{"--" + strings.Repeat("x", 200)}, "", "..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effort, err := ParseCommandOverrides(tc.args)
			if tc.err == "" {
				if err != nil || effort != tc.effort {
					t.Fatalf("effort=%q err=%v", effort, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) || effort != "" {
				t.Fatalf("effort=%q err=%v want %q", effort, err, tc.err)
			}
		})
	}
}
