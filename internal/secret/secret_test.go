// SPDX-License-Identifier: GPL-3.0-or-later

package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

const value = "hunter2-0123456789abcdef"

// logged returns what slog's text and JSON handlers write for one line
// carrying args.
func logged(args ...any) string {
	var out bytes.Buffer
	slog.New(slog.NewTextHandler(&out, nil)).Info("line", args...)
	slog.New(slog.NewJSONHandler(&out, nil)).Info("line", args...)
	return out.String()
}

// A Value logged through slog, directly or held in a struct, map or slice,
// or printed with any fmt verb, shows the redaction and never the value.
func TestValueRedacts(t *testing.T) {
	t.Parallel()
	v := New(value)
	type held struct {
		Name string
		V    Value
		p    Value
	}
	for name, out := range map[string]string{
		"attr":       logged("v", v),
		"slog.Any":   logged(slog.Any("v", v)),
		"struct":     logged("v", held{Name: "n", V: v, p: v}),
		"pointer":    logged("v", &held{V: v}),
		"map":        logged("v", map[string]Value{"K": v}),
		"slice":      logged("v", []Value{v}),
		"group":      logged(slog.Group("g", "v", v)),
		"fmt":        fmt.Sprintf("%v %+v %#v %s %q %x %X %d %10s", v, v, v, v, v, v, v, v, v),
		"fmt struct": fmt.Sprintf("%v %+v %#v", held{V: v, p: v}, held{V: v, p: v}, held{V: v, p: v}),
		"error":      fmt.Errorf("failed with %v", v).Error(),
	} {
		if strings.Contains(out, value) || !strings.Contains(out, Redacted) {
			t.Errorf("%s: got %q, want the redaction and not the value", name, out)
		}
	}
	if b, err := json.Marshal(map[string]Value{"K": v}); err != nil || string(b) != `{"K":"[REDACTED]"}` {
		t.Errorf("json: %s, %v", b, err)
	}
	if got := v.Reveal(); got != value {
		t.Errorf("Reveal() = %q, want %q", got, value)
	}
}

// An Env logs its names with every value redacted, and gives its values
// back only through Reveal and Environ.
func TestEnv(t *testing.T) {
	t.Parallel()
	plain := map[string]string{"B": value, "A": value + "-a", "PATH": "/bin"}
	e := EnvOf(plain)
	for name, out := range map[string]string{
		"attr":  logged("env", e),
		"slice": logged("envs", []Env{e}),
		"fmt":   fmt.Sprintf("%v %+v %#v", e, e, e),
	} {
		if strings.Contains(out, value) || strings.Contains(out, "/bin") || !strings.Contains(out, Redacted) {
			t.Errorf("%s: got %q, want the redaction and no value", name, out)
		}
	}
	if out := logged("env", e); !strings.Contains(out, "env.A=[REDACTED] env.B=[REDACTED] env.PATH=[REDACTED]") {
		t.Errorf("the text handler wrote %q, want each name, sorted, with its value redacted", out)
	}
	got := e.Reveal()
	if len(got) != len(plain) {
		t.Errorf("Reveal() = %v, want %v", got, plain)
	}
	for k, v := range plain {
		if got[k] != v {
			t.Errorf("Reveal()[%s] = %q, want %q", k, got[k], v)
		}
	}
	if got, want := e.Environ(), []string{"A=" + value + "-a", "B=" + value, "PATH=/bin"}; !slices.Equal(got, want) {
		t.Errorf("Environ() = %q, want %q", got, want)
	}
	if e := EnvOf(nil); e == nil || len(e) != 0 || len(e.Environ()) != 0 {
		t.Errorf("EnvOf(nil) = %#v, want an empty Env", e)
	}
}
