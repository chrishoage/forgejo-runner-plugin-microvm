// SPDX-License-Identifier: GPL-3.0-or-later

package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// SetLevel takes exactly log_level's four names, and refuses any other
// without changing the level.
func TestSetLevel(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError,
	} {
		var v slog.LevelVar
		if err := SetLevel(&v, name); err != nil || v.Level() != want {
			t.Errorf("%s: level %v, err %v; want %v", name, v.Level(), err, want)
		}
	}
	for _, name := range []string{"", "DEBUG", "warning", "trace", "info+2", "-4"} {
		var v slog.LevelVar
		v.Set(slog.LevelWarn)
		err := SetLevel(&v, name)
		if err == nil || !strings.Contains(err.Error(), "debug, info, warn or error") || v.Level() != slog.LevelWarn {
			t.Errorf("%q: level %v, err %v; want an error naming the four and the level kept", name, v.Level(), err)
		}
	}
}

// A logger writes only at its level and above, which can change after it
// is made.
func TestNewFollowsItsLevel(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	var v slog.LevelVar
	log := New(&out, "test", &v)
	log.Debug("hidden")
	if err := SetLevel(&v, "debug"); err != nil {
		t.Fatal(err)
	}
	log.Debug("shown")
	got := out.String()
	if strings.Contains(got, "hidden") || !strings.Contains(got, "level=DEBUG msg=shown program=test") {
		t.Errorf("log:\n%s", got)
	}
}
