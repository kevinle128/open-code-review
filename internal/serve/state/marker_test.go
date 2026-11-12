// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

var (
	shaA = strings.Repeat("a", 40)
	shaB = strings.Repeat("b", 40)
	shaC = strings.Repeat("c", 40)
	shaD = strings.Repeat("d", 40)
)

func sampleMarker() Marker {
	return Marker{V: 1, Auto: 2, Max: 3, Reviewed: shaB, Base: shaA, Runs: []Run{
		{N: 1, Kind: KindFull, From: shaA, To: shaC, IDs: []string{"r1-cccccccc-01020304"}},
		{N: 2, Kind: KindIncremental, From: shaC, To: shaB, IDs: []string{}},
	}}
}

func rawMarker(json string) string {
	return "<!-- ocr-serve:v1 " + base64.StdEncoding.EncodeToString([]byte(json)) + " -->"
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
	init := Initial(4)
	got, err = Decode(Encode(init))
	if err != nil || got.Auto != 0 || got.Max != 4 || got.V != 1 {
		t.Fatalf("initial: %+v %v", got, err)
	}
}

func TestDecode_Absent(t *testing.T) {
	for _, body := range []string{"", "no marker here", "<!-- ocr-checkpoint:v1 abc= -->"} {
		m, err := Decode(body)
		if m != nil || err != nil {
			t.Fatalf("%q: %v %v", body, m, err)
		}
	}
}

func TestDecode_Rejects(t *testing.T) {
	good := Encode(sampleMarker())
	cases := []struct{ name, body, want string }{
		{"two markers", good + "\n" + good, "found 2"},
		{"one good one mangled", good + "\n<!-- ocr-serve:v1 !!! -->", "found 2"},
		{"malformed", "<!-- ocr-serve:v1 not base64! -->", "malformed"},
		{"future version tag", "<!-- ocr-serve:v2 YWJj -->", "malformed"},
		{"garbage base64", "<!-- ocr-serve:v1 YWJj= -->", "not base64"},
		{"not json", rawMarker("hello"), "not JSON"},
		{"wrong version", rawMarker(`{"v":2}`), "version 2"},
		{"negative count", rawMarker(`{"v":1,"auto":-1}`), "negative"},
		{"short reviewed sha", rawMarker(`{"v":1,"auto":1,"reviewed":"abc123"}`), "reviewed"},
		{"upper case base", rawMarker(`{"v":1,"base":"` + strings.Repeat("A", 40) + `"}`), "base"},
		{"bad run from", rawMarker(`{"v":1,"runs":[{"n":1,"kind":"full","from":"x","to":"` + shaA + `"}]}`), "runs[0].from"},
		{"unknown run kind", rawMarker(`{"v":1,"runs":[{"n":1,"kind":"partial","to":"` + shaA + `"}]}`), "runs[0].kind"},
		{"missing run to", rawMarker(`{"v":1,"runs":[{"n":1,"kind":"full","from":"` + shaA + `"}]}`), "runs[0].to"},
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

func TestPreserve(t *testing.T) {
	old := "old summary\n\n" + Encode(sampleMarker())
	got := Preserve("new summary", old)
	if m, err := Decode(got); err != nil || m == nil || m.Auto != 2 || !strings.HasPrefix(got, "new summary\n\n") {
		t.Fatalf("got %q m=%v err=%v", got, m, err)
	}

	withOwn := "new summary\n" + Encode(Initial(3))
	if got := Preserve(withOwn, old); got != withOwn {
		t.Fatalf("new marker replaced: %q", got)
	}

	twoOld := old + "\n" + Encode(Initial(3))
	if got := Preserve("new summary", twoOld); got != "new summary" {
		t.Fatalf("copied from ambiguous body: %q", got)
	}

	mangledOld := "old <!-- ocr-serve:v1 !!! -->"
	if got := Preserve("new summary", mangledOld); got != "new summary" {
		t.Fatalf("copied mangled marker: %q", got)
	}

	if got := Preserve("new summary", ""); got != "new summary" {
		t.Fatalf("empty old: %q", got)
	}
}
