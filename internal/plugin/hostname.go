// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import "strings"

// maxLabel is a DNS label's longest (RFC 1035).
const maxLabel = 63

// fallbackName is a VM's name when its source maps to nothing.
const fallbackName = "job"

// dnsName makes name a VM's name on the job network, its hostname, a DNS
// label (GST-12): lower-cased (ASCII), each run of characters outside
// a-z0-9- replaced by one `-`, leading and trailing `-` trimmed, cut to 63
// characters, any `-` left at the cut trimmed again, and `job` when nothing
// remains. Every VM's name goes through it: the job's container's comes
// from the runner's job name, a service's will be its workflow key, so the
// rule can't drift between them.
func dnsName(name string) string {
	var b strings.Builder
	junk := false
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			junk = true
			continue
		}
		if junk {
			b.WriteByte('-')
			junk = false
		}
		b.WriteRune(r)
	}
	if junk {
		b.WriteByte('-')
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > maxLabel {
		s = strings.TrimRight(s[:maxLabel], "-")
	}
	if s == "" {
		return fallbackName
	}
	return s
}
