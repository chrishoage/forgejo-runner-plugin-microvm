// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !payload

package payload

// JobBinary returns ErrNoPayload: this binary was built without the payload
// build tag.
func JobBinary() ([]byte, error) { return nil, ErrNoPayload }

// Agent returns ErrNoPayload: this binary was built without the payload build
// tag.
func Agent() ([]byte, error) { return nil, ErrNoPayload }
