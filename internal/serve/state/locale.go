// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"bufio"
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
)

// Locale texts live in .txt files so translated words stay out of the Go
// sources, which the English-only check scans.
//
//go:embed locale/*.txt
var localeFS embed.FS

var sectionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

const fallbackLocale = "en"

// Locale maps text keys to translated text.
type Locale map[string]string

// LoadLocale loads locale/<name>.txt on top of the English file. English is
// the source of every key; see merge for how a translation falls back.
func LoadLocale(name string) (Locale, error) {
	base, err := readLocale(fallbackLocale)
	if err != nil {
		return nil, err
	}
	if name == fallbackLocale {
		return base, nil
	}
	over, err := readLocale(name)
	if err != nil {
		return nil, err
	}
	return merge(base, over), nil
}

// merge lays a translation over the English base. A key the translation
// lacks keeps the English text, and so does a translated text whose
// placeholders differ from the English ones: a missing or misspelled
// placeholder would post a raw "{name}" or drop a value. A key English
// does not have is not used by the service, so it is left out.
func merge(base, over Locale) Locale {
	out := make(Locale, len(base))
	for k, v := range base {
		out[k] = v
		if t, ok := over[k]; ok && slices.Equal(placeholders(t), placeholders(v)) {
			out[k] = t
		}
	}
	return out
}

var placeholderPattern = regexp.MustCompile(`\{[A-Za-z0-9_]+\}`)

// placeholders lists the distinct placeholders of s, sorted.
func placeholders(s string) []string {
	found := placeholderPattern.FindAllString(s, -1)
	slices.Sort(found)
	return slices.Compact(found)
}

func readLocale(name string) (Locale, error) {
	if name == "" || strings.ContainsAny(name, `/\.`) {
		return nil, fmt.Errorf("invalid locale name %q", name)
	}
	raw, err := fs.ReadFile(localeFS, "locale/"+name+".txt")
	if err != nil {
		return nil, fmt.Errorf("unknown locale %q: %w", name, err)
	}
	return parseLocale(name, raw)
}

// parseLocale reads `key = value` lines. A `[section]` line, as in a TOML
// table, makes the keys after it `section.key`; keys before the first
// section line have no prefix.
func parseLocale(name string, raw []byte) (Locale, error) {
	out := Locale{}
	prefix := ""
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section, ok := strings.CutSuffix(strings.TrimPrefix(line, "["), "]")
			if section = strings.TrimSpace(section); !ok || !sectionPattern.MatchString(section) {
				return nil, fmt.Errorf("locale %s line %d: want [section]", name, n)
			}
			prefix = section + "."
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("locale %s line %d: want key = value", name, n)
		}
		key = prefix + key
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("locale %s line %d: duplicate key %q", name, n, key)
		}
		out[key] = strings.ReplaceAll(strings.TrimSpace(value), `\n`, "\n")
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("locale %s: %w", name, err)
	}
	return out, nil
}

// Text returns the text for key with each {name} placeholder replaced; kv
// holds name, value pairs. A missing key returns the key itself, so a gap is
// visible in the posted text instead of silently blank.
func (l Locale) Text(key string, kv ...string) string {
	s, ok := l[key]
	if !ok {
		return key
	}
	if len(kv) < 2 {
		return s
	}
	pairs := make([]string, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, "{"+kv[i]+"}", kv[i+1])
	}
	return strings.NewReplacer(pairs...).Replace(s)
}
