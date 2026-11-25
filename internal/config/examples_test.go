// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The configuration reference and its examples (docs/configuration.md), from
// this package's directory.
const (
	referenceFile = "../../docs/configuration.md"
	examplesDir   = "../../docs/examples"
	// examplesGlob matches the example config files; the directory may hold
	// other examples, such as a runner config.
	examplesGlob = "config-*.yaml"
	// pluginExample is the example that sets every plugin-level key.
	pluginExample = "config-plugin-level.yaml"
)

// examples returns the absolute paths of the reference's example config
// files.
func examples(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(examplesDir, examplesGlob))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 3 {
		t.Fatalf("%d example config files (%s) in %s, want at least the minimal, plugin-level and profiles ones", len(paths), examplesGlob, examplesDir)
	}
	for i, p := range paths {
		if paths[i], err = filepath.Abs(p); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

// sourceMissing matches the one error the examples may give with sources
// checked: a mount's or a disk's source that doesn't exist on this host.
var sourceMissing = regexp.MustCompile(`: (mounts|disks): source: ".*" does not exist$`)

// Every example config file passes the checks `runner-microvm check` runs,
// on amd64, the only supported architecture, except that mount and disk
// sources exist: the examples name sources such as /srv/vols/persist.ext4,
// which a test host doesn't have. Every other rule is checked, and with
// sources checked too, a missing source is the only error.
func TestExamplesPassTheChecks(t *testing.T) {
	for _, path := range examples(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := loadOn(path, true, nil, "amd64", false); err != nil {
				t.Fatalf("the checks fail:\n%v", err)
			}
			_, err := loadOn(path, true, nil, "amd64", true)
			if err == nil {
				return
			}
			for _, line := range strings.Split(err.Error(), "\n") {
				if !sourceMissing.MatchString(line) {
					t.Errorf("with sources checked, an error other than a missing source:\n%s", line)
				}
			}
		})
	}
}

// Without its sources checked, a file still fails every other rule: the
// examples' test skips only the sources' existence.
func TestUncheckedSourcesKeepTheOtherRules(t *testing.T) {
	path := writeConfig(t, `profiles:
  p:
    mounts:
      - {source: /no/such/dir, target: /a}
      - {source: relative, target: /c}
    disks:
      - {source: /no/such/disk, target: /a, read_write: true}
      - {source: /no/such/disk, target: /b}
`)
	_, err := loadOn(path, true, nil, "amd64", false)
	if err == nil {
		t.Fatal("loaded, want errors")
	}
	msg := err.Error()
	wantErr(t, path, msg, `line 5: profile "p": mounts: source: "relative" is not an absolute path`)
	wantErr(t, path, msg, `line 7: profile "p": disks: target: "/a" is already a target on line 4`)
	wantErr(t, path, msg, `line 8: profile "p": disks: source: "/no/such/disk" is the same file as the source on line 7`)
	if strings.Contains(msg, "does not exist") {
		t.Errorf("a source's existence was checked:\n%s", msg)
	}
}

// The reference's tables list exactly the keys the decoder accepts: those
// its switches in top, profile and storageEntry name. The table under each
// heading is read from its first column.
func TestReferenceListsEveryKey(t *testing.T) {
	documented := referenceKeys(t)
	accepted := decoderKeys(t)
	for _, k := range []struct{ section, fn string }{
		{"Plugin-level keys", "top"},
		{"Profile keys", "profile"},
		{"Mount and disk entries", "storageEntry"},
	} {
		want := accepted[k.fn]
		if k.fn == "top" {
			// profiles is the section of its own, not a plugin-level key.
			want = slices.DeleteFunc(want, func(s string) bool { return s == "profiles" })
		}
		slices.Sort(want)
		got := slices.Sorted(slices.Values(documented[k.section]))
		if !slices.Equal(got, want) {
			t.Errorf("%s: the reference lists %q, the decoder (%s) accepts %q", k.section, got, k.fn, want)
		}
	}
}

// The plugin-level example sets every plugin-level key the reference lists.
func TestPluginLevelExampleSetsEveryKey(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(examplesDir, pluginExample))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range referenceKeys(t)["Plugin-level keys"] {
		if _, ok := top[key]; !ok {
			t.Errorf("%s doesn't set %s", pluginExample, key)
		}
	}
}

// tableKey matches a table row whose first column is a key in backquotes.
var tableKey = regexp.MustCompile("^\\| `([a-z_]+)` \\|")

// referenceKeys returns the keys in the first column of the reference's
// tables, by the heading they are under.
func referenceKeys(t *testing.T) map[string][]string {
	t.Helper()
	f, err := os.Open(referenceFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	keys := map[string][]string{}
	heading := ""
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if h, ok := strings.CutPrefix(line, "#"); ok {
			heading = strings.TrimSpace(strings.TrimLeft(h, "#"))
			continue
		}
		if m := tableKey.FindStringSubmatch(line); m != nil {
			keys[heading] = append(keys[heading], m[1])
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return keys
}

// decoderKeys returns the string cases of the switches in this package's
// functions top, profile and storageEntry: the keys each decodes.
func decoderKeys(t *testing.T) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	keys := map[string][]string{}
	for _, file := range []string{"config.go", "storage.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !slices.Contains([]string{"top", "profile", "storageEntry"}, fn.Name.Name) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				cc, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, e := range cc.List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						s, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatal(err)
						}
						keys[fn.Name.Name] = append(keys[fn.Name.Name], s)
					}
				}
				return true
			})
		}
	}
	for _, fn := range []string{"top", "profile", "storageEntry"} {
		if len(keys[fn]) == 0 {
			t.Fatalf("found no keys in %s", fn)
		}
	}
	return keys
}
