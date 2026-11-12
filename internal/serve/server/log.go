// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package server

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"

	"github.com/alibaba/open-code-review/internal/serve/forge"
)

// NewLogger returns the JSON logger the service writes to w. Secrets never
// reach it: tokens only live in headers built inside the forges and in the
// environment of the git fetch process.
func NewLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

func withRequest(l *slog.Logger, requestID string) *slog.Logger {
	return l.With("request_id", requestID)
}

func withPR(l *slog.Logger, ref forge.PRRef) *slog.Logger {
	return l.With("pr", ref.Key())
}

// newRequestID is used when a delivery carries no id of its own.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
