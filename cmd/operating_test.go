// SPDX-License-Identifier: GPL-3.0-or-later

package cmd_test

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

const (
	// operatorGuide is docs/operating.md, from this package's directory.
	operatorGuide = "../docs/operating.md"
	// libMinimums is the libraries' minimum versions, the one source the
	// guide, the release notes and the dev shell take them from.
	libMinimums = "../nix/lib-minimums.json"
)

// Every libkrun and libkrunfw version the operator guide names is that
// library's minimum in nix/lib-minimums.json: the requirement, the release
// tags it fetches and the file names it shows. Each pattern must match at
// least once, so a guide reworded past one fails too rather than passing
// unchecked.
func TestOperatorGuideMinimums(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(libMinimums)
	if err != nil {
		t.Fatal(err)
	}
	var minimums map[string]string
	if err := json.Unmarshal(data, &minimums); err != nil {
		t.Fatalf("%s: %v", libMinimums, err)
	}
	guide, err := os.ReadFile(operatorGuide)
	if err != nil {
		t.Fatal(err)
	}
	const v = `(\d+\.\d+\.\d+)`
	for _, c := range []struct {
		lib     string
		pattern string
	}{
		{"libkrun", `\blibkrun ` + v + ` or later`},
		{"libkrun", `--branch v` + v + ` https://github\.com/libkrun/libkrun\.git`},
		{"libkrun", `\blibkrun\.so\.` + v},
		{"libkrunfw", `\blibkrunfw ` + v + ` or later`},
		{"libkrunfw", `libkrunfw/releases/download/v` + v + `/`},
		{"libkrunfw", `\blibkrunfw\.so\.` + v},
	} {
		want, ok := minimums[c.lib]
		if !ok {
			t.Fatalf("%s has no minimum for %s", libMinimums, c.lib)
		}
		matches := regexp.MustCompile(c.pattern).FindAllSubmatch(guide, -1)
		if len(matches) == 0 {
			t.Errorf("%s: nothing matches %s, where the guide names %s's version", operatorGuide, c.pattern, c.lib)
		}
		for _, m := range matches {
			if got := string(m[1]); got != want {
				t.Errorf("%s: %q names %s %s, but its minimum in %s is %s", operatorGuide, m[0], c.lib, got, libMinimums, want)
			}
		}
	}
}
