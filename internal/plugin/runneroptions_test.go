// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The keepalive test dials with the options of forgejo-runner's plugin
// client, mirrored in runnerClient. This reads the runner's own source at
// the version in go.mod, so a runner update that changes a default, or
// dials with another option, fails here, not unnoticed in production.
func TestRunnerClientOptions(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-json", "code.forgejo.org/forgejo/runner/v13")
	cmd.WaitDelay = 10 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -m: %v\n%s", err, stderr.String())
	}
	var mod struct{ Dir, Version string }
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(mod.Dir, "act", "plugin", "client.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	src := runnerSource{fset: fset, file: file}
	where := "forgejo-runner " + mod.Version + "'s act/plugin/client.go"

	// The defaults, by name, as numbers.
	for name, want := range map[string]int64{
		"defaultKeepaliveTime":    int64(runnerKeepaliveTime),
		"defaultKeepaliveTimeout": int64(runnerKeepaliveTimeout),
		"defaultMaxMessageSize":   runnerMaxMessageSize,
	} {
		got, err := src.constant(name)
		if err != nil {
			t.Errorf("%s: %v", where, err)
		} else if got != want {
			t.Errorf("%s: %s is %d, the test mirrors %d: update runnerClient's options", where, name, got, want)
		}
	}

	// resolveOptions applies those defaults to the options dial reads.
	defaults := map[string]string{}
	for _, lit := range src.literals("clientOptions") {
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				defaults[src.text(kv.Key)] = src.text(kv.Value)
			}
		}
	}
	for field, want := range map[string]string{
		"keepaliveTime":    "defaultKeepaliveTime",
		"keepaliveTimeout": "defaultKeepaliveTimeout",
		"maxMessageSize":   "defaultMaxMessageSize",
	} {
		if defaults[field] != want {
			t.Errorf("%s: resolveOptions sets %s to %q, want %s", where, field, defaults[field], want)
		}
	}

	// dial's options: the same ones, the keepalive parameters from those
	// options, without PermitWithoutStream.
	dial := src.function("dial")
	if dial == nil {
		t.Fatalf("%s: no function dial", where)
	}
	var withs, calls []string
	ast.Inspect(dial, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch recv := src.text(sel.X); {
		case recv == "grpc" && strings.HasPrefix(sel.Sel.Name, "With"):
			withs = append(withs, sel.Sel.Name)
		case recv == "grpc" && strings.HasPrefix(sel.Sel.Name, "MaxCall"):
			calls = append(calls, sel.Sel.Name+"("+src.text(call.Args[0])+")")
		}
		return true
	})
	if want := []string{"WithTransportCredentials", "WithDefaultCallOptions", "WithKeepaliveParams", "WithContextDialer"}; !slices.Equal(withs, want) {
		t.Errorf("%s: dial's options are %v, the test mirrors %v", where, withs, want)
	}
	if want := []string{"MaxCallRecvMsgSize(cfg.maxMessageSize)", "MaxCallSendMsgSize(cfg.maxMessageSize)"}; !slices.Equal(calls, want) {
		t.Errorf("%s: dial's call options are %v, the test mirrors %v", where, calls, want)
	}
	params := map[string]string{}
	for _, lit := range src.literals("keepalive.ClientParameters") {
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				params[src.text(kv.Key)] = src.text(kv.Value)
			}
		}
	}
	for field, want := range map[string]string{
		"Time":                "cfg.keepaliveTime",
		"Timeout":             "cfg.keepaliveTimeout",
		"PermitWithoutStream": strconv.FormatBool(runnerPermitWithoutStream),
	} {
		if params[field] != want {
			t.Errorf("%s: dial's keepalive.ClientParameters has %s: %q, the test mirrors %q", where, field, params[field], want)
		}
	}
	if len(params) != 3 {
		t.Errorf("%s: dial's keepalive.ClientParameters has the fields %v, the test mirrors Time, Timeout and PermitWithoutStream", where, params)
	}
	if !strings.Contains(src.text(dial), `"passthrough:///"`) {
		t.Errorf("%s: dial no longer dials a unix socket through the passthrough resolver, as the test does", where)
	}
}

// runnerSource is a parsed file of the runner.
type runnerSource struct {
	fset *token.FileSet
	file *ast.File
}

// text prints n as source.
func (s runnerSource) text(n ast.Node) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, s.fset, n); err != nil {
		return "?"
	}
	return b.String()
}

func (s runnerSource) function(name string) *ast.FuncDecl {
	for _, d := range s.file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == name {
			return f
		}
	}
	return nil
}

// literals returns every composite literal of the named type in the file.
func (s runnerSource) literals(typ string) []*ast.CompositeLit {
	var lits []*ast.CompositeLit
	ast.Inspect(s.file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && lit.Type != nil && s.text(lit.Type) == typ {
			lits = append(lits, lit)
		}
		return true
	})
	return lits
}

// constant evaluates the file's constant name, which may be a product of
// integers and the time package's units.
func (s runnerSource) constant(name string) (int64, error) {
	for _, d := range s.file.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			v := sp.(*ast.ValueSpec)
			for i, n := range v.Names {
				if n.Name == name && i < len(v.Values) {
					return s.eval(v.Values[i])
				}
			}
		}
	}
	return 0, fmt.Errorf("no constant %s", name)
}

func (s runnerSource) eval(e ast.Expr) (int64, error) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind == token.INT {
			return strconv.ParseInt(e.Value, 0, 64)
		}
	case *ast.ParenExpr:
		return s.eval(e.X)
	case *ast.Ident:
		return s.constant(e.Name)
	case *ast.SelectorExpr:
		if s.text(e.X) == "time" {
			if d, ok := map[string]time.Duration{
				"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond,
				"Second": time.Second, "Minute": time.Minute, "Hour": time.Hour,
			}[e.Sel.Name]; ok {
				return int64(d), nil
			}
		}
	case *ast.BinaryExpr:
		x, err := s.eval(e.X)
		if err != nil {
			return 0, err
		}
		y, err := s.eval(e.Y)
		if err != nil {
			return 0, err
		}
		if e.Op == token.MUL {
			return x * y, nil
		}
		if e.Op == token.SHL {
			return x << y, nil
		}
	}
	return 0, fmt.Errorf("can't evaluate %s", s.text(e))
}
