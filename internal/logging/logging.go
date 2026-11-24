// SPDX-License-Identifier: GPL-3.0-or-later

// Package logging sets up log/slog the same way in all three programs: the
// text handler (logfmt) on stderr, at the level the plugin-level key
// log_level names.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

// levels are log_level's values: the plugin's, which it passes to every job
// process, VM child and agent, so all three programs follow one setting.
var levels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// SetLevel sets v to the level log_level calls name: debug, info, warn or
// error. Any other name, the empty one included, is an error and leaves v
// as it was.
func SetLevel(v *slog.LevelVar, name string) error {
	l, ok := levels[name]
	if !ok {
		return fmt.Errorf("log level %q: not debug, info, warn or error", name)
	}
	v.Set(l)
	return nil
}

// New returns a text-handler logger writing to w at level, each line tagged
// with the program that wrote it.
func New(w io.Writer, program string, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})).With("program", program)
}

// Setup makes a logger on stderr for program the default, and returns it
// with its level, info until the program reads log_level and sets it.
func Setup(program string) (*slog.Logger, *slog.LevelVar) {
	v := new(slog.LevelVar)
	l := New(os.Stderr, program, v)
	slog.SetDefault(l)
	return l, v
}
