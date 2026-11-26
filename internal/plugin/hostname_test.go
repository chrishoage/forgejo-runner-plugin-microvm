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

// A service's key becomes its name by the Docker backend's alias rule:
// lower case, each character outside a-z0-9- one `_`, with no cut.
func TestServiceName(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 100)
	for _, tc := range []struct{ desc, key, want string }{
		{"already a name", "redis", "redis"},
		{"upper case and a dot", "My.DB", "my_db"},
		{"underscores are kept", "my_db", "my_db"},
		{"hyphens are kept", "-a--b-", "-a--b-"},
		{"each character, not each run", "a  b./c", "a__b__c"},
		{"no length cut", long, long},
		{"non-ASCII, one _ each", "Ünï ジョ", "_n____"},
		{"non-ASCII upper case is not lowered", "ÀB", "_b"},
		{"invalid UTF-8, one _ per byte", "a\xff\xfeb", "a__b"},
		{"empty", "", ""},
	} {
		if got := serviceName(tc.key); got != tc.want {
			t.Errorf("%s: serviceName(%q) = %q, want %q", tc.desc, tc.key, got, tc.want)
		}
	}
}
