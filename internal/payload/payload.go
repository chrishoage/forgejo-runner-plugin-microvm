// SPDX-License-Identifier: GPL-3.0-or-later

// Package payload holds the job binary and the agent that runner-microvm
// embeds. With the payload build tag, embedded.go embeds them by exact file
// name, so a build without them fails to compile. Without the tag, stub.go
// returns ErrNoPayload, so plain go build, go vet, go test and lint need no
// prebuilt payload. `just build` builds both programs into this directory
// first.
package payload

import "errors"

// ErrNoPayload is returned by a binary built without the payload build tag.
var ErrNoPayload = errors.New("payload: built without the payload build tag")
