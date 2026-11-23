// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
)

// label is a parsed label argument: `[//]<image>[;profile=<name>]`.
type label struct {
	// image is the label's image; empty when the job must bring its own
	// container: image.
	image string
	// profile is the profile the label names, or config.DefaultProfile.
	profile string
}

// parseLabel parses a label argument, or fails with InvalidArgument naming
// it. It strips one leading `//`, then splits at the first `;`, which no OCI
// reference contains. After the `;`, only `profile=<name>` is allowed, once,
// with a name; matching is exact and case-sensitive.
func parseLabel(arg string) (label, error) {
	image, opts, found := strings.Cut(strings.TrimPrefix(arg, "//"), ";")
	if !found {
		return label{image: image, profile: config.DefaultProfile}, nil
	}
	if opts == "" {
		return label{}, labelError(arg, `nothing follows the ";": only profile=<name> may`)
	}
	opt, more, _ := strings.Cut(opts, ";")
	name, isProfile := strings.CutPrefix(opt, "profile=")
	switch {
	case !isProfile:
		return label{}, labelError(arg, "%q: only profile=<name> may follow the image", opt)
	case name == "":
		return label{}, labelError(arg, "profile= names no profile")
	case strings.Contains(opts, ";"):
		return label{}, labelError(arg, "%q follows the profile: only one profile=<name> may follow the image", ";"+more)
	}
	return label{image: image, profile: name}, nil
}

// labelError is InvalidArgument naming the label argument.
func labelError(arg, format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, "label %q: "+format, append([]any{arg}, args...)...)
}
