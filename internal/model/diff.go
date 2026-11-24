// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package model

// Diff represents a single file change in a git diff.
type Diff struct {
	OldPath        string `json:"old_path"`
	NewPath        string `json:"new_path"`
	Diff           string `json:"diff"`
	NewFileContent string `json:"new_file_content"`
	IsBinary       bool   `json:"is_binary"`
	IsDeleted      bool   `json:"is_deleted"`
	IsNew          bool   `json:"is_new"`
	IsRenamed      bool   `json:"is_renamed"`
	Insertions     int64  `json:"insertions"`
	Deletions      int64  `json:"deletions"`
	// StatBytes is the size of this file's unified diff when the patch text
	// itself is not kept. TokensCounted means TokenCount was already computed
	// from that patch. None of the three is serialized: a saved diff keeps
	// its patch text, and these fields only exist during selection.
	StatBytes      int64 `json:"-"`
	TokensCounted  bool  `json:"-"`
	TokenCount     int   `json:"-"`
	DiscardedPatch bool  `json:"-"`
}
