// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"testing"
	"time"
)

// The test-only ready timeout is empty in a shipped binary, which takes
// NewServer's default; a test's build sets a positive duration, and any
// other value stops startup.
func TestParseReadyTimeout(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "10s", want: 10 * time.Second},
		{in: "1500ms", want: 1500 * time.Millisecond},
		{in: "soon", wantErr: true},
		{in: "10", wantErr: true},
		{in: "0s", wantErr: true},
		{in: "-3s", wantErr: true},
	} {
		got, err := parseReadyTimeout(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseReadyTimeout(%q) = %v, %v; want %v, error %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}
