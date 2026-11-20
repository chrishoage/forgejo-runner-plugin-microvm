// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Cache is the image cache's real adapter.
//
// SHORTCUT(#5): it caches nothing. Every Acquire pulls the whole image and
// flattens it into the VM's root directory.
type Cache struct {
	log  *slog.Logger
	opts []remote.Option
}

// New returns a cache that pulls with the plugin user's registry
// credentials, for the host's platform.
func New(log *slog.Logger, opts ...remote.Option) *Cache {
	return &Cache{log: log, opts: append([]remote.Option{
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
	}, opts...)}
}

// Acquire pulls req.Ref and flattens its filesystem into req.Root, which must
// not exist yet.
func (c *Cache) Acquire(ctx context.Context, req Request) (Image, error) {
	ref, err := name.ParseReference(req.Ref)
	if err != nil {
		return Image{}, fmt.Errorf("image %s: %w", req.Ref, err)
	}
	img, err := remote.Image(ref, append([]remote.Option{remote.WithContext(ctx)}, c.opts...)...)
	if err != nil {
		return Image{}, fmt.Errorf("pull %s: %w", req.Ref, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return Image{}, fmt.Errorf("pull %s: %w", req.Ref, err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		return Image{}, fmt.Errorf("pull %s: config: %w", req.Ref, err)
	}
	fs := mutate.Extract(img)
	err = extract(fs, req.Root)
	// Closing the reader ends Extract's goroutine, also when extract
	// stopped early.
	err = errors.Join(err, fs.Close())
	if err != nil {
		return Image{}, fmt.Errorf("pull %s: %w", req.Ref, err)
	}
	c.log.Info("pulled", "job", req.Job, "vm", req.VM, "image", req.Ref, "digest", digest.String())
	return Image{Digest: digest.String(), Env: config.Config.Env}, nil
}

// Release does nothing: the plugin deletes each VM's root directory with the
// job's state directory.
func (c *Cache) Release(context.Context, string) error { return nil }
