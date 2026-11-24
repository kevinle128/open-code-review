// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"errors"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/serve/review"
)

func TestSanitizePublicError(t *testing.T) {
	raw := errors.New("boom https://user:secret@example.com/a token=abc /Users/dale/data/rounds.json ghp_abcdefghijklmnopqrstuvwxyz\nstack line")
	got := SanitizePublicError(raw)
	for _, bad := range []string{"https://", "secret", "/Users/", "ghp_", "stack line", "token="} {
		if strings.Contains(got, bad) {
			t.Errorf("kept %q in %q", bad, got)
		}
	}
	if got == "" || strings.Contains(got, "\n") {
		t.Fatalf("got %q", got)
	}
	long := errors.New(strings.Repeat("x", 400))
	if n := len([]rune(SanitizePublicError(long))); n > maxPublicError+3 {
		t.Fatalf("length %d", n)
	}
}

func TestRenderFailureNotice(t *testing.T) {
	loc, err := LoadLocale("en")
	if err != nil {
		t.Fatal(err)
	}
	got := RenderFailureNotice(FailureNotice{Class: FailureTime, Stage: "review", Elapsed: "2h", Limit: "1h"}, loc)
	if !strings.Contains(got, "time limit of 1h") || !strings.Contains(got, "2h elapsed") || !strings.Contains(got, "`/review`") {
		t.Fatalf("time notice %q", got)
	}
	cuts := RenderFailureNotice(FailureNotice{Class: FailureCuts, Cuts: 6}, loc)
	if !strings.Contains(cuts, "restarted 6 times") {
		t.Fatalf("cuts notice %q", cuts)
	}
	engine := RenderFailureNotice(FailureNotice{Class: FailureEngine, Error: "NOT_FOUND"}, loc)
	if !strings.Contains(engine, "NOT_FOUND") || strings.Contains(engine, "{") {
		t.Fatalf("engine notice %q", engine)
	}
	empty := RenderFailureNotice(FailureNotice{Class: FailureEngine}, loc)
	if fallback := loc.Text("notice.engine_fallback"); !strings.HasPrefix(empty, fallback+"\n") || strings.Count(empty, fallback) != 1 {
		t.Fatalf("empty engine notice %q", empty)
	}
}

func TestSanitizePublicErrorTable(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig"
	cases := []struct {
		name  string
		in    string
		want  string
		has   []string
		lacks []string
	}{
		{
			name: "authorization bearer",
			in:   "upstream error Authorization: Bearer " + jwt + " failed",
			want: "upstream error failed",
		},
		{name: "authorization basic", in: "Authorization: Basic dXNlcjpwYXNz", want: ""},
		{name: "bearer alone", in: "Bearer " + jwt, want: ""},
		{name: "authorization assignment", in: "Authorization=supersecret", want: ""},
		{name: "quoted json", in: `"authorization":"Bearer ` + jwt + `"`, want: ""},
		{name: "quoted api key", in: `upstream "api_key":"short" failed`, want: "upstream failed"},
		{name: "quoted access token", in: `upstream "access_token":"abc" failed`, want: "upstream failed"},
		{name: "assigned api key", in: "upstream api_key=short failed", want: "upstream failed"},
		{name: "dashed token", in: "upstream access-token=abc failed", want: "upstream failed"},
		{name: "dotted cookie", in: "upstream session.cookie=xyz failed", want: "upstream failed"},
		{
			name: "ordinary words",
			in:   "the keyboard and the monkey stayed",
			want: "the keyboard and the monkey stayed",
		},
		{
			name:  "url token path and prefix",
			in:    "boom https://user:secret@example.com/a token=abc /Users/dale/data/rounds.json ghp_abcdefghijklmnopqrstuvwxyz",
			want:  "boom",
			lacks: []string{"https://", "secret", "token=", "/Users/", "ghp_"},
		},
		{
			name:  "gitlab token",
			in:    "glpat-abcdefghij123456",
			want:  "",
			lacks: []string{"glpat-"},
		},
		{name: "authorization token scheme", in: "upstream Authorization: token s3cr3tvalue failed", want: "upstream failed"},
		{name: "authorization apikey scheme", in: "Authorization: ApiKey s3cr3tvalue", want: ""},
		{name: "quoted value with spaces", in: `upstream password="two words" failed`, want: "upstream failed"},
		{name: "quoted key value with spaces", in: `upstream "secret"="two words" failed`, want: "upstream failed"},
		{name: "app installation token", in: "push ghs_abcdefghij1234567890 failed", want: "push failed"},
		{name: "google token", in: "agy ya29.abcdefghij123456 failed", want: "agy failed"},
		{name: "invalid byte in token", in: "push ghp_abc\xffdefghijklmnop failed", want: "push failed"},
		{name: "colour around path", in: "read \x1b[31m/Users/dale/rounds.json\x1b[0m failed", want: "read failed"},
		{name: "unicode line break", in: "first\u2028/Users/dale/second", want: "first"},
		{name: "path in backticks", in: "open `/home/dale/pr.json` failed", has: []string{"open", "failed"}, lacks: []string{"/home", "pr.json"}},
		{name: "path in brackets", in: "dirs [/home/dale/skills] missing", has: []string{"dirs", "missing"}, lacks: []string{"/home", "skills"}},
		{name: "path list", in: "PATH /usr/bin:/home/dale/bin", has: []string{"PATH"}, lacks: []string{"/usr", "/home", "dale"}},
		{name: "scoped package path", in: "run /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js failed", want: "run failed"},
		{
			name: "mentions",
			in:   "ping @alice and @org/team",
			want: "ping @\u200balice and @\u200borg/team",
		},
		{
			name: "html",
			in:   "bad <script>alert(1)</script>",
			want: `bad &lt;script&gt;alert\(1\)&lt;/script&gt;`,
		},
		{
			name:  "markdown link and code",
			in:    "see [docs](http://evil.example) and `code`",
			has:   []string{"docs", "code", "\\`"},
			lacks: []string{"http://", "]("},
		},
		{
			name: "heading and emphasis",
			in:   "# heading *bold* _em_",
			has:  []string{`\# heading`, `\*bold\*`, `\_em\_`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizePublicError(errors.New(tc.in))
			if tc.want != "" || len(tc.has) == 0 {
				if got != tc.want {
					t.Fatalf("got %q want %q", got, tc.want)
				}
			}
			for _, s := range tc.has {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in %q", s, got)
				}
			}
			for _, s := range tc.lacks {
				if strings.Contains(got, s) {
					t.Errorf("kept %q in %q", s, got)
				}
			}
		})
	}
}

func TestVietnameseFailureOversizeAndTrimmed(t *testing.T) {
	loc, err := LoadLocale("vi")
	if err != nil {
		t.Fatal(err)
	}
	timePhrase := "giới hạn thời gian" // allow-non-english: Vietnamese failure notice
	cancelPhrase := "bị hủy"           // allow-non-english: Vietnamese failure notice
	overPhrase := "quá lớn"            // allow-non-english: Vietnamese oversize sentence
	trimPhrase := "Bỏ qua 2"           // allow-non-english: Vietnamese trimmed sentence
	failed := RenderFailureNotice(FailureNotice{Class: FailureTime, Stage: "review", Elapsed: "2h", Limit: "1h"}, loc)
	if !strings.Contains(failed, timePhrase) || strings.Contains(failed, "time limit of") {
		t.Fatalf("time notice %q", failed)
	}
	cancelled := RenderFailureNotice(FailureNotice{Class: FailureEngine, Error: SanitizePublicError(errors.New(loc.Text("notice.cancelled")))}, loc)
	if !strings.Contains(cancelled, cancelPhrase) || strings.Contains(cancelled, "the review was cancelled") || strings.Contains(cancelled, "The review stopped") {
		t.Fatalf("cancelled notice %q", cancelled)
	}
	over := RenderRound(RoundReport{N: 1, Max: 3, Skipped: true, Notice: review.NoticeOversize, OverFiles: 400, OverBytes: 9 << 20}, loc)
	if !strings.Contains(over, overPhrase) || !strings.Contains(over, "400") || !strings.Contains(over, "9 MiB") || strings.Contains(over, "a.go") {
		t.Fatalf("oversize %q", over)
	}
	trimmed := RenderRound(RoundReport{N: 1, Max: 3, Coverage: review.Coverage{Skipped: map[string]int{"user_exclude": 2}}}, loc)
	if !strings.Contains(trimmed, trimPhrase) || strings.Contains(trimmed, ".go") {
		t.Fatalf("trimmed %q", trimmed)
	}
}
