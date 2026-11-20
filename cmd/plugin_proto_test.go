// SPDX-License-Identifier: GPL-3.0-or-later

package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The plugin serves its own copy of forgejo-runner's plugin protocol
// (proto/plugin/v1alpha/plugin.proto). It must be byte for byte the
// runner's at the version in go.mod, so a runner update that changes the
// protocol fails here, not in production.
func TestVendoredPluginProtocol(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", forgejoRunner+"/v13")
	cmd.WaitDelay = 10 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go mod download %s: %v\n%s", forgejoRunner, err, stderr.String())
	}
	var mod struct{ Dir, Version string }
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatal(err)
	}
	theirs, err := os.ReadFile(filepath.Join(mod.Dir, "act", "plugin", "proto", "v1alpha", "plugin.proto"))
	if err != nil {
		t.Fatal(err)
	}
	ours, err := os.ReadFile(filepath.Join("..", "proto", "plugin", "v1alpha", "plugin.proto"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ours, theirs) {
		t.Errorf("proto/plugin/v1alpha/plugin.proto differs from forgejo-runner %s's: copy it again and run `just generate`", mod.Version)
	}
}
