// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package forge

import (
	"reflect"
	"testing"
)

func TestCommandLine(t *testing.T) {
	names := []string{"ocr-bot", "pr.review"}
	cases := []struct {
		body      string
		line      string
		mentioned bool
	}{
		{"/fix", "/fix", false},
		{"\n  /review --effort=high  \nmore", "/review --effort=high", false},
		{"@ocr-bot /fix", "/fix", true},
		{"@ocr-bot fix", "/fix", true},
		{"@OCR-Bot review", "/review", true},
		{"@ocr-bot /review --effort=high", "/review --effort=high", true},
		{"@ocr-bot review-commit", "/review-commit", true},
		{"@ocr-bot: fix", "/fix", true},
		{"@ocr-bot, fix", "/fix", true},
		{"@ocr-bot:fix", "/fix", true},
		{"@ocr-bot\tfix", "/fix", true},
		{"@pr.review fix", "/fix", true},
		{"@ocr-bot", "", true},
		{"@ocr-bot:", "", true},
		{"  @ocr-bot  \nfix", "", true},
		{"@ocr-bot hello there", "/hello there", true},
		{"@someone fix", "@someone fix", false},
		{"@ocr-bot-x fix", "@ocr-bot-x fix", false},
		{"@ocr fix", "@ocr fix", false},
		{"please @ocr-bot fix", "please @ocr-bot fix", false},
		{"ocr-bot fix", "ocr-bot fix", false},
		{"", "", false},
	}
	for _, c := range cases {
		line, mentioned := CommandLine(c.body, names)
		if line != c.line || mentioned != c.mentioned {
			t.Errorf("CommandLine(%q) = %q, %v; want %q, %v", c.body, line, mentioned, c.line, c.mentioned)
		}
	}
}

func TestCommandLine_NoNames(t *testing.T) {
	if line, mentioned := CommandLine("@ocr-bot fix", nil); mentioned || line != "@ocr-bot fix" {
		t.Fatalf("got %q %v", line, mentioned)
	}
	if line, mentioned := CommandLine("@x fix", []string{""}); mentioned || line != "@x fix" {
		t.Fatalf("empty name: got %q %v", line, mentioned)
	}
}

func TestIsCommand(t *testing.T) {
	names := []string{"ocr-bot"}
	for body, want := range map[string]bool{
		"/review":            true,
		"@ocr-bot fix":       true,
		"@ocr-bot":           true,
		"@someone fix":       false,
		"looks good":         false,
		"text\n@ocr-bot fix": false,
		"":                   false,
	} {
		if got := IsCommand(body, names); got != want {
			t.Errorf("IsCommand(%q) = %v", body, got)
		}
	}
}

func TestMergeNames(t *testing.T) {
	got := MergeNames([]string{"Bot", " "}, nil, []string{"bot", " extra ", "BOT", "other"})
	if want := []string{"Bot", "extra", "other"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := MergeNames(); got != nil {
		t.Fatalf("empty: %v", got)
	}
}
