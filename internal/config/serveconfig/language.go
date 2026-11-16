// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

import (
	"slices"
	"strings"
)

// Output language of each platform: the language of the posted comments
// and of the text the engines write for its PRs.
const (
	EnvGitHubLanguage = "OCR_SERVE_GITHUB_LANGUAGE"
	EnvGitLabLanguage = "OCR_SERVE_GITLAB_LANGUAGE"

	LanguageEnglish    = "English"
	LanguageVietnamese = "Vietnamese"

	DefaultGitHubLanguage = LanguageEnglish
	DefaultGitLabLanguage = LanguageVietnamese
)

// languageLocales maps each accepted language to the locale of the posted
// text, which must exist under internal/serve/state/locale.
var languageLocales = map[string]string{
	LanguageEnglish:    "en",
	LanguageVietnamese: "vi",
}

// LocaleOf returns the locale of an accepted language, or "" for any other.
func LocaleOf(language string) string {
	return languageLocales[language]
}

// language reads an output language setting. The value matches without
// regard to case and is returned in its canonical spelling.
func (l *loader) language(key, def string) string {
	v := l.str(key, def)
	for name := range languageLocales {
		if strings.EqualFold(v, name) {
			return name
		}
	}
	names := make([]string, 0, len(languageLocales))
	for name := range languageLocales {
		names = append(names, name)
	}
	slices.Sort(names)
	l.fail("%s must be one of %s, got %q", key, strings.Join(names, ", "), v)
	return def
}
