// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package forge

import (
	"reflect"
	"testing"
)

func TestParsePatch_Ranges(t *testing.T) {
	cases := []struct {
		name     string
		patch    string
		want     []LineRange
		complete bool
	}{
		{"single hunk with deletion", "@@ -10,3 +10,4 @@\n context line 10\n-deleted line 11\n+added line 11\n+added line 12\n context line 13", []LineRange{{10, 13}}, true},
		{"two hunks stay separate", "@@ -1,3 +1,3 @@\n a\n b\n c\n@@ -50,3 +50,3 @@\n x\n y\n z", []LineRange{{1, 3}, {50, 52}}, true},
		{"pure deletion", "@@ -5,2 +4,0 @@\n-gone\n-also gone", nil, true},
		{"no newline marker", "@@ -1,1 +1,2 @@\n kept\n+added\n\\ No newline at end of file", []LineRange{{1, 2}}, true},
		{"trailing newline", "@@ -1,1 +1,1 @@\n context\n", []LineRange{{1, 1}}, true},
		{"count omitted means one line", "@@ -3 +3 @@\n+x", []LineRange{{3, 3}}, true},
		{"basic offset", "@@ -1,3 +5,3 @@\n ctx\n-old\n+new\n ctx2\n", []LineRange{{5, 7}}, true},
		{"clipped patch", "@@ -1,3 +5,5 @@\n ctx\n", []LineRange{{5, 5}}, false},
		{"no hunk header", "just text\n+added", nil, false},
		{"empty", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, complete := ParsePatch(tc.patch)
			if !reflect.DeepEqual(got, tc.want) || complete != tc.complete {
				t.Fatalf("got %v complete=%v, want %v complete=%v", got, complete, tc.want, tc.complete)
			}
		})
	}
}

func TestParsePatch_Positions(t *testing.T) {
	_, pos, _ := ParsePatch("@@ -1,3 +5,4 @@\n ctx\n-gone\n+added\n ctx2\n")
	want := map[int]LinePos{
		5: {OldLine: 1},
		6: {New: true},
		7: {OldLine: 3},
	}
	if !reflect.DeepEqual(pos, want) {
		t.Fatalf("got %v want %v", pos, want)
	}
	if _, pos, _ := ParsePatch(""); len(pos) != 0 {
		t.Fatalf("empty patch gave %v", pos)
	}
}

func inventory() Inventory {
	return Inventory{
		Complete: true,
		Known:    map[string]bool{"a.go": true, "gone.go": true, "bin.png": true},
		Ranges: map[string][]LineRange{
			"a.go":    {{10, 20}, {40, 45}},
			"gone.go": nil,
		},
	}
}

func TestPlace(t *testing.T) {
	cases := []struct {
		name  string
		f     Finding
		inv   Inventory
		first string
		want  Placement
	}{
		{"span inside one hunk", Finding{Path: "a.go", StartLine: 12, EndLine: 15}, inventory(), "a.go",
			Placement{Kind: Line, Path: "a.go", StartLine: 12, EndLine: 15}},
		{"end line only", Finding{Path: "a.go", EndLine: 42}, inventory(), "a.go",
			Placement{Kind: Line, Path: "a.go", StartLine: 42, EndLine: 42}},
		{"before first hunk", Finding{Path: "a.go", StartLine: 2, EndLine: 3}, inventory(), "a.go",
			Placement{Kind: NearestLine, Path: "a.go", StartLine: 10, EndLine: 10, Relocated: true}},
		{"between hunks nearer the lower", Finding{Path: "a.go", StartLine: 25, EndLine: 25}, inventory(), "a.go",
			Placement{Kind: NearestLine, Path: "a.go", StartLine: 20, EndLine: 20, Relocated: true}},
		{"between hunks nearer the upper", Finding{Path: "a.go", StartLine: 38, EndLine: 38}, inventory(), "a.go",
			Placement{Kind: NearestLine, Path: "a.go", StartLine: 40, EndLine: 40, Relocated: true}},
		{"tie goes to lower line", Finding{Path: "a.go", StartLine: 30, EndLine: 30}, inventory(), "a.go",
			Placement{Kind: NearestLine, Path: "a.go", StartLine: 20, EndLine: 20, Relocated: true}},
		{"span straddles two hunks", Finding{Path: "a.go", StartLine: 18, EndLine: 42}, inventory(), "a.go",
			Placement{Kind: NearestLine, Path: "a.go", StartLine: 18, EndLine: 18, Relocated: true}},
		{"after last hunk", Finding{Path: "a.go", StartLine: 90, EndLine: 99}, inventory(), "a.go",
			Placement{Kind: NearestLine, Path: "a.go", StartLine: 45, EndLine: 45, Relocated: true}},
		{"pure deletion file", Finding{Path: "gone.go", StartLine: 3, EndLine: 3}, inventory(), "a.go",
			Placement{Kind: FileLevel, Path: "gone.go"}},
		{"file without patch", Finding{Path: "bin.png", StartLine: 1, EndLine: 1}, inventory(), "a.go",
			Placement{Kind: FileLevel, Path: "bin.png"}},
		{"known file without lines", Finding{Path: "a.go"}, inventory(), "a.go",
			Placement{Kind: FileLevel, Path: "a.go"}},
		{"path not in diff goes to first file", Finding{Path: "other.go", StartLine: 5, EndLine: 6}, inventory(), "a.go",
			Placement{Kind: FileLevel, Path: "a.go", Relocated: true}},
		{"path not in diff without file-level support", Finding{Path: "other.go", StartLine: 5, EndLine: 6}, inventory(), "",
			Placement{Kind: General, Relocated: true}},
		{"incomplete inventory cannot prove outside", Finding{Path: "other.go", StartLine: 5, EndLine: 6}, Inventory{Known: map[string]bool{}}, "a.go",
			Placement{Kind: Line, Path: "other.go", StartLine: 5, EndLine: 6}},
		{"PR-level finding goes to first file", Finding{}, inventory(), "a.go",
			Placement{Kind: FileLevel, Path: "a.go", Relocated: true}},
		{"PR-level finding without file-level support", Finding{}, Inventory{}, "",
			Placement{Kind: General, Relocated: true}},
		{"incomplete inventory and no lines", Finding{Path: "other.go"}, Inventory{}, "",
			Placement{Kind: General, Relocated: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Place(tc.f, tc.inv, tc.first); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestIsLineResolutionFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"github live wording", 422, `{"message":"Unprocessable Entity","errors":["Line could not be resolved"]}`, true},
		{"github two failures joined", 422, `{"errors":["Line could not be resolved and Start line must be part of the diff"]}`, true},
		{"github structured field", 422, `{"message":"Validation Failed","errors":[{"resource":"PullRequestReviewComment","field":"start_line","code":"invalid"}]}`, true},
		{"github unrelated field", 422, `{"message":"Validation Failed","errors":[{"field":"body","code":"too_long"}]}`, false},
		{"github bare message only", 422, `{"message":"Unprocessable Entity"}`, false},
		{"github empty body", 422, ``, false},
		{"gitlab line code", 400, `{"message":"400 Bad request - Note {:line_code=>[\"must be a valid line code\"]}"}`, true},
		{"gitlab unrelated", 400, `{"message":"body is too long"}`, false},
		{"gitlab wording on github status", 422, `{"message":"line_code is invalid"}`, false},
		{"other status", 500, `could not be resolved`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLineResolutionFailure(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
