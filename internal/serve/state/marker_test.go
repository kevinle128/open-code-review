// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

var (
	shaA = strings.Repeat("a", 40)
	shaB = strings.Repeat("b", 40)
	shaC = strings.Repeat("c", 40)
)

func sampleMarker() Marker {
	return Marker{V: 2, Status: ChainRunning, Max: 3, Rounds: []Round{
		{N: 1, Scope: ScopeFull, From: shaA, To: shaB, Real: 2, Fixed: 2, Fix: shaC},
		{N: 2, Scope: ScopeCommit, To: shaC},
	}}
}

func rawMarker(version, json string) string {
	return "<!-- ocr-serve:v" + version + " " + base64.StdEncoding.EncodeToString([]byte(json)) + " -->"
}

func TestMarker_RoundTrip(t *testing.T) {
	m := sampleMarker()
	got, err := Decode("summary text\n\n" + Encode(m) + "\ntrailer")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, m) {
		t.Fatalf("got %+v want %+v", *got, m)
	}
}

func TestDecode_AbsentOrLegacy(t *testing.T) {
	for _, body := range []string{"", "no marker here", "<!-- ocr-checkpoint:v1 abc= -->", rawMarker("1", `{"v":1,"auto":2}`)} {
		m, err := Decode(body)
		if m != nil || err != nil {
			t.Fatalf("%q: %v %v", body, m, err)
		}
	}
}

func TestDecode_Rejects(t *testing.T) {
	good := Encode(sampleMarker())
	run := func(r string) string {
		return rawMarker("2", `{"v":2,"status":"success","rounds":[`+r+`]}`)
	}
	cases := []struct{ name, body, want string }{
		{"two markers", good + "\n" + good, "found 2"},
		{"one good one mangled", good + "\n<!-- ocr-serve:v2 !!! -->", "found 2"},
		{"malformed", "<!-- ocr-serve:v2 not base64! -->", "malformed"},
		{"future version tag", rawMarker("3", "{}"), "version 3"},
		{"garbage base64", "<!-- ocr-serve:v2 YWJj= -->", "not base64"},
		{"not json", rawMarker("2", "hello"), "not JSON"},
		{"wrong version", rawMarker("2", `{"v":5,"status":"success"}`), "version 5"},
		{"unknown status", rawMarker("2", `{"v":2,"status":"done"}`), "status"},
		{"negative max", rawMarker("2", `{"v":2,"status":"success","max":-1}`), "negative"},
		{"zero round", run(`{"n":0,"scope":"full","to":"` + shaA + `"}`), "rounds[0]"},
		{"unknown scope", run(`{"n":1,"scope":"partial","to":"` + shaA + `"}`), "rounds[0].scope"},
		{"bad from", run(`{"n":1,"scope":"full","from":"x","to":"` + shaA + `"}`), "rounds[0].from"},
		{"bad fix", run(`{"n":1,"scope":"full","fix":"x","to":"` + shaA + `"}`), "rounds[0].fix"},
		{"missing to", run(`{"n":1,"scope":"full"}`), "rounds[0].to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Decode(tc.body)
			if m != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("m=%v err=%v want %q", m, err, tc.want)
			}
		})
	}
}

func TestMarker_CommitState(t *testing.T) {
	cases := map[string]forge.CommitState{ChainRunning: forge.StatusPending, ChainSuccess: forge.StatusSuccess, ChainFailure: forge.StatusFailure}
	for st, want := range cases {
		if got := (Marker{Status: st}).CommitState(); got != want {
			t.Errorf("%s: %s, want %s", st, got, want)
		}
	}
}

func TestPreserve(t *testing.T) {
	old := "old summary\n\n" + Encode(sampleMarker())
	got := Preserve("new summary", old)
	if m, err := Decode(got); err != nil || m == nil || len(m.Rounds) != 2 || !strings.HasPrefix(got, "new summary\n\n") {
		t.Fatalf("got %q m=%v err=%v", got, m, err)
	}
	withOwn := "new summary\n" + Encode(Marker{Status: ChainSuccess})
	if got := Preserve(withOwn, old); got != withOwn {
		t.Fatalf("new marker replaced: %q", got)
	}
	twoOld := old + "\n" + Encode(Marker{Status: ChainSuccess})
	if got := Preserve("new summary", twoOld); got != "new summary" {
		t.Fatalf("copied from ambiguous body: %q", got)
	}
	if got := Preserve("new summary", "old <!-- ocr-serve:v2 !!! -->"); got != "new summary" {
		t.Fatalf("copied mangled marker: %q", got)
	}
	if got := Preserve("new summary", ""); got != "new summary" {
		t.Fatalf("empty old: %q", got)
	}
}
