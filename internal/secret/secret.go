// SPDX-License-Identifier: GPL-3.0-or-later

// Package secret holds the secret value type, which implements
// slog.LogValuer and redacts itself, so a secret logged by accident shows no
// value (LOG-2).
package secret

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
)

// Redacted is what a Value shows wherever it is logged or printed.
const Redacted = "[REDACTED]"

// Value is a secret string. It shows only Redacted, however it is logged or
// printed: through slog, which calls LogValue, through fmt with any verb,
// %#v included, and as text or JSON, which slog's handlers use for a value
// held in a struct, map or slice that slog doesn't resolve. Reveal is the
// only way to its value.
//
// The value is behind a pointer: fmt can't call the methods of a struct's
// unexported field, and prints a Value held in one field by field, which
// then shows only the pointer's address.
type Value struct {
	p *string
}

// New returns s as a secret.
func New(s string) Value { return Value{p: &s} }

// Reveal returns the value, for the one place that must use it. The zero
// Value's is empty.
func (v Value) Reveal() string {
	if v.p == nil {
		return ""
	}
	return *v.p
}

// LogValue implements slog.LogValuer.
func (Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// String implements fmt.Stringer.
func (Value) String() string { return Redacted }

// GoString implements fmt.GoStringer, for %#v.
func (Value) GoString() string { return Redacted }

// Format implements fmt.Formatter, so no verb reaches the value: %x and %q
// would otherwise format the struct's field.
func (Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// MarshalText implements encoding.TextMarshaler, which slog's text handler
// uses first for a value of kind Any.
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// MarshalJSON implements json.Marshaler, which slog's JSON handler uses.
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// Env is an environment whose values are secrets, by variable name. Names
// are not secret, and show.
type Env map[string]Value

// EnvOf returns env with each value a secret. A nil env gives an empty Env.
func EnvOf(env map[string]string) Env {
	e := make(Env, len(env))
	for k, v := range env {
		e[k] = New(v)
	}
	return e
}

// Reveal returns e's values in the clear, for the one place that must use
// them.
func (e Env) Reveal() map[string]string {
	out := make(map[string]string, len(e))
	for k, v := range e {
		out[k] = v.Reveal()
	}
	return out
}

// Environ returns e as KEY=VALUE entries sorted by name, as a process's
// environment takes them.
func (e Env) Environ() []string {
	out := make([]string, 0, len(e))
	for _, k := range slices.Sorted(maps.Keys(e)) {
		out = append(out, k+"="+e[k].Reveal())
	}
	return out
}

// LogValue implements slog.LogValuer: a group of each name, sorted, with
// its value redacted.
func (e Env) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, len(e))
	for _, k := range slices.Sorted(maps.Keys(e)) {
		attrs = append(attrs, slog.String(k, Redacted))
	}
	return slog.GroupValue(attrs...)
}
