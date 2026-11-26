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
// remains. The job's container's name comes from the runner's job name; a
// service's VM's hostname comes from its name, through the same function, so
// the rule can't drift between them.
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

// serviceName is a service's name on the job network, from its key in the
// workflow's services: the Docker backend's network alias rule, so a
// workflow written for it names its services the same here: lower-cased
// (ASCII), every character outside a-z0-9- replaced by `_`, with no length
// cut. Unlike a hostname it may hold `_`, which glibc, musl, Go and Node all
// resolve from /etc/hosts. An empty key maps to an empty name, which Create
// refuses before it gets here.
func serviceName(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			r = '_'
		}
		b.WriteRune(r)
	}
	return b.String()
}
