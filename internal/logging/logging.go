// SPDX-License-Identifier: GPL-3.0-or-later

// Package logging sets up log/slog the same way in all three programs: the
// text handler (logfmt) on stderr.
package logging

import (
	"io"
	"log/slog"
	"os"
)

// New returns a text-handler logger writing to w, each line tagged with the
// program that wrote it.
func New(w io.Writer, program string) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, nil)).With("program", program)
}

// Setup makes a logger on stderr for program the default, and returns it.
func Setup(program string) *slog.Logger {
	l := New(os.Stderr, program)
	slog.SetDefault(l)
	return l
}
