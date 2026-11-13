// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agent

import (
	"bytes"
	"maps"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/config/rules"
	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/stdout"
)

func TestLogExclusions_LargeData(t *testing.T) {
	var buf bytes.Buffer
	restore := stdout.Swap(&buf)
	defer restore()
	New(Args{}).logExclusions([]fileDecision{
		{Diff: model.Diff{NewPath: "data/big.json", Insertions: 1500, Deletions: 2}, Reason: ExcludeLargeData},
	})
	if !strings.Contains(buf.String(), "Skipping data/big.json — data file with 1502 changed lines") {
		t.Errorf("output = %q", buf.String())
	}
}

func TestWhyExcluded_ReviewTests(t *testing.T) {
	cases := []struct {
		path        string
		reviewTests bool
		want        ExcludeReason
	}{
		{"pkg/foo_test.go", false, ExcludeDefaultPath},
		{"pkg/foo_test.go", true, ExcludeNone},
		{"web/a.spec.ts", true, ExcludeNone},
		{"pkg/testdata/x.go", true, ExcludeDefaultPath},
		{"web/__snapshots__/a.js", true, ExcludeDefaultPath},
		{"node_modules/a/index.js", true, ExcludeDefaultPath},
	}
	for _, tc := range cases {
		a := New(Args{FileFilter: &rules.FileFilter{ReviewTests: tc.reviewTests}})
		if got := a.whyExcluded(model.Diff{NewPath: tc.path}); got != tc.want {
			t.Errorf("%s reviewTests=%v: got %q, want %q", tc.path, tc.reviewTests, got, tc.want)
		}
	}
}

func TestSelectFiles_LargeDataFile(t *testing.T) {
	diffs := []model.Diff{
		{NewPath: "data/big.json", Insertions: 900, Deletions: 101},
		{NewPath: "data/edge.json", Insertions: 1000},
		{NewPath: "conf/small.yaml", Insertions: 10},
		{NewPath: "conf/Big.YML", Insertions: 2000},
		{NewPath: "src/big.go", Insertions: 5000},
		{NewPath: "data/gone.json", Deletions: 5000, IsDeleted: true},
	}
	want := []ExcludeReason{ExcludeLargeData, ExcludeNone, ExcludeNone, ExcludeLargeData, ExcludeNone, ExcludeDeleted}

	a := New(Args{FileFilter: &rules.FileFilter{MaxDataChangedLines: 1000}})
	for i, dec := range a.selectFiles(diffs) {
		if dec.Reason != want[i] {
			t.Errorf("%s: got %q, want %q", diffs[i].NewPath, dec.Reason, want[i])
		}
	}

	for _, dec := range New(Args{}).selectFiles(diffs[:1]) {
		if dec.Reason != ExcludeNone {
			t.Errorf("without a limit a large data file is reviewed, got %q", dec.Reason)
		}
	}
}

func TestSkippedByReason(t *testing.T) {
	got := skippedByReason([]fileDecision{
		{Reason: ExcludeNone}, {Reason: ExcludeUserRule}, {Reason: ExcludeUserRule},
		{Reason: ExcludeLargeData}, {Reason: ExcludeDeleted},
	})
	want := map[string]int{"user_exclude": 2, "large_data": 1, "deleted": 1}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := skippedByReason([]fileDecision{{Reason: ExcludeNone}}); got != nil {
		t.Errorf("nothing skipped = %v, want nil", got)
	}
}
