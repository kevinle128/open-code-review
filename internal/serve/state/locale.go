// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package state

import (
	"bufio"
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

// Locale texts live in .txt files so translated words stay out of the Go
// sources, which the English-only check scans.
//
//go:embed locale/*.txt
var localeFS embed.FS

const fallbackLocale = "en"

// Locale maps text keys to translated text.
type Locale map[string]string

// LoadLocale loads locale/<name>.txt on top of the English file, so a key
// missing from a translation falls back to English.
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
	for k, v := range over {
		base[k] = v
	}
	return base, nil
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

func parseLocale(name string, raw []byte) (Locale, error) {
	out := Locale{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("locale %s line %d: want key = value", name, n)
		}
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
