// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package serveconfig

// DefaultExcludes are review exclude globs that `ocr serve` adds to OCR's
// built-in excludes. A webhook review runs on every push under a time limit,
// and these files are agent and tool output, lock files, data or markup that
// take long to review and rarely hold a defect. OCR_SERVE_EXCLUDE and the
// serve.json exclude key append to this list; nothing removes from it.
//
// "**/.*/**/*" skips every directory whose name starts with a dot, at any
// depth. The final "/*" is needed: doublestar lets a trailing "/**" match
// nothing, so "**/.*/**" would also skip dotfiles such as .eslintrc.js.
var DefaultExcludes = []string{
	"**/.*/**/*",
	"**/.mcp.json",
	"**/_bmad-output/**",
	"**/claude-design/**",
	"**/plans/**",
	"**/third_party/**",
	"**/docs/**/*.html",
	"**/*.lock",
	"**/yarn.lock",
	"**/go.sum",
	"**/Cargo.lock",
	"**/composer.lock",
	"**/*.map",
	"**/locales/**/*.json",
	"**/i18n/**/*.json",
	"**/*.html",
	"**/*.htm",
	"**/*.css",
}

// MaxDataChangedLines is the serve limit for one JSON or YAML file: a diff
// that changes more lines than this is skipped.
const MaxDataChangedLines = 1000
