// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"strings"
	"testing"
)

// A name becomes a DNS label: lower case, each run of other characters one
// `-`, no `-` at either end, at most 63 characters, else `job` (GST-12).
func TestDNSName(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("0123456789abcdef", 4)
	for _, tc := range []struct{ desc, name, want string }{
		{
			"a runner's name, cut at 63", "FORGEJO-ACTIONS-TASK-42_WORKFLOW-" + hash + "_JOB-build",
			"forgejo-actions-task-42-workflow-" + hash[:30],
		},
		{"already a label", "build-1", "build-1"},
		{"upper case", "Build", "build"},
		{"runs of other characters", "a_b  c./d", "a-b-c-d"},
		{"hyphens are kept", "a--b_-c", "a--b--c"},
		{"leading and trailing junk", "  _-a b-_.  ", "a-b"},
		{"cut at a hyphen", strings.Repeat("a", 62) + "_b", strings.Repeat("a", 62)},
		{"cut after hyphens", strings.Repeat("a", 60) + "---" + "bbb", strings.Repeat("a", 60)},
		{"exactly 63", strings.Repeat("a", 63) + "_", strings.Repeat("a", 63)},
		{"non-ASCII", "Ünïcødé ジョブ x", "n-c-d-x"},
		{"non-ASCII upper case is not lowered", "ÀB", "b"},
		{"maps to nothing", "_.!ジョブ-", "job"},
		{"empty", "", "job"},
	} {
		if got := dnsName(tc.name); got != tc.want {
			t.Errorf("%s: dnsName(%q) = %q, want %q", tc.desc, tc.name, got, tc.want)
		}
	}
}
