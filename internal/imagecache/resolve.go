// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// tagFile is what <cache>/tags/<sha256 hex of the reference> holds: the
// normalised reference and the manifest digest it resolved to. A tag has
// one, and so does the digest of a multi-platform index, which resolves to
// its platform image's manifest digest.
type tagFile struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

// maxConfigSize is the largest image config the image cache takes. A config
// is held in memory and kept in <cache>/images/, and a registry is untrusted;
// real configs are a few KiB to a few hundred KiB, so 8 MiB is generous.
const maxConfigSize = 8 << 20

// resolved is an image whose manifest digest and config are known, from the
// cache or the registry.
type resolved struct {
	ref    name.Reference
	digest v1.Hash
	// raw is the image config as the registry served it.
	raw    []byte
	config *v1.ConfigFile
	// remote is the image as the registry serves it, by digest; nil until
	// Acquire needs the registry.
	remote v1.Image
	// cachedConfig is set when raw came from <cache>/images/.
	cachedConfig bool
	// tag is set when ref needs a tag file naming digest: a tag, or the
	// digest of an index.
	tag bool
	// seenTag is ref's tag file as resolve read it, before any registry
	// request; nil when there was none. record writes the tag file only
	// while it still holds this, so a slower Acquire can't put back an
	// older digest that a later lookup replaced.
	seenTag []byte
}

// resolve finds ref's manifest digest and config. A tag the cache holds, with
// its config, and a digest whose config the cache holds, need no registry
// request; with force, a tag is looked up again (IMG-6). The digest of an
// index, which never changes, resolves once through a tag file of its own. Each reference is
// normalised to go-containerregistry's fully qualified name, so node:22 and
// index.docker.io/library/node:22 are one tag (IMG-1).
func (c *Cache) resolve(ctx context.Context, ref name.Reference, force bool) (*resolved, error) {
	r := &resolved{ref: ref}
	cached := func(h v1.Hash) (bool, error) {
		raw, err := os.ReadFile(c.imagePath(h))
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		r.digest, r.raw, r.cachedConfig = h, raw, true
		return true, nil
	}
	var found bool
	var err error
	switch ref := ref.(type) {
	case name.Digest:
		h, herr := v1.NewHash(ref.DigestStr())
		if herr == nil {
			herr = checkDigest(h)
		}
		if herr != nil {
			return nil, herr
		}
		// An index's digest names no config of its own: its tag file
		// names its platform image's.
		if found, err = cached(h); !found && err == nil {
			if r.seenTag, err = c.readTagFile(ref); err == nil {
				found, err = c.cachedTag(ref, r.seenTag, cached)
			}
		}
	case name.Tag:
		r.tag = true
		// With force, the tag file is read only for record to compare.
		if r.seenTag, err = c.readTagFile(ref); err == nil && !force {
			found, err = c.cachedTag(ref, r.seenTag, cached)
		}
	}
	if err != nil {
		return nil, err
	}
	if !found {
		// The registry resolves a tag, and an index to the host's
		// platform (IMG-2).
		img, err := remote.Image(ref, c.remoteOpts(ctx)...)
		if err != nil {
			return nil, err
		}
		if r.digest, err = img.Digest(); err != nil {
			return nil, err
		}
		if err := checkDigest(r.digest); err != nil {
			return nil, err
		}
		r.remote = img
		// A digest reference needs a tag file only when it names an
		// index.
		if d, ok := ref.(name.Digest); ok {
			r.tag = d.DigestStr() != r.digest.String()
		}
		if found, err = cached(r.digest); err != nil {
			return nil, err
		}
	}
	if !found {
		if r.raw, err = fetchConfig(r.remote); err != nil {
			return nil, fmt.Errorf("config of %s: %w", r.digest, err)
		}
	}
	if r.config, err = v1.ParseConfigFile(bytes.NewReader(r.raw)); err != nil {
		return nil, fmt.Errorf("config of %s: %w", r.digest, err)
	}
	return r, nil
}

// fetchConfig returns img's raw config, refusing one over maxConfigSize
// before fetching it by the size its manifest declares, and bounding the read
// in case the registry sends more.
func fetchConfig(img v1.Image) ([]byte, error) {
	mf, err := img.Manifest()
	if err != nil {
		return nil, err
	}
	if s := mf.Config.Size; s < 0 || s > maxConfigSize {
		return nil, fmt.Errorf("its manifest gives it %d bytes; the image cache takes at most %d", s, maxConfigSize)
	}
	l, err := partial.ConfigLayer(img)
	if err != nil {
		return nil, err
	}
	rc, err := l.Compressed()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	// go-containerregistry checks the digest at EOF.
	raw, err := io.ReadAll(io.LimitReader(rc, maxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxConfigSize {
		return nil, fmt.Errorf("over %d bytes, the most the image cache takes", maxConfigSize)
	}
	return raw, nil
}

// layerSource returns, for each index into the image's layers, the layer as
// the registry serves it, by its blob digest. The first call fetches the
// manifest by digest, never by tag, and checks that it names the config
// Acquire holds.
func (c *Cache) layerSource(ctx context.Context, r *resolved) func(i int) (v1.Layer, error) {
	var m *v1.Manifest
	return func(i int) (v1.Layer, error) {
		if m == nil {
			if r.remote == nil {
				img, err := remote.Image(r.ref.Context().Digest(r.digest.String()), c.remoteOpts(ctx)...)
				if err != nil {
					return nil, err
				}
				r.remote = img
			}
			mf, err := r.remote.Manifest()
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(r.raw)
			if mf.Config.Digest.Algorithm != "sha256" || mf.Config.Digest.Hex != hex.EncodeToString(sum[:]) {
				return nil, fmt.Errorf("manifest %s names config %s, not the one cached", r.digest, mf.Config.Digest)
			}
			if len(mf.Layers) != len(r.config.RootFS.DiffIDs) {
				return nil, fmt.Errorf("manifest %s has %d layers, its config %d", r.digest, len(mf.Layers), len(r.config.RootFS.DiffIDs))
			}
			m = mf
		}
		return r.remote.LayerByDigest(m.Layers[i].Digest)
	}
}

// record writes the image's config and tag that the cache lacks. Acquire
// calls it only once every layer is linked, so pruning, which deletes configs
// with no cached layer, can't delete a config a cold Acquire just wrote.
//
// The tag file is written only while it holds what resolve saw (none, or
// the same file): otherwise an Acquire that resolved since, force_pull for
// example, wrote a newer digest, and since tags never expire an older one
// put back would stick.
func (c *Cache) record(r *resolved) error {
	if !r.cachedConfig {
		if err := c.writeFile(imagesDir, r.digest.Hex+".json", r.raw); err != nil {
			return err
		}
	}
	if !r.tag {
		return nil
	}
	b, err := json.Marshal(tagFile{Reference: r.ref.Name(), Digest: r.digest.String()})
	if err != nil {
		return err
	}
	// Only this process writes tag files, so a lock of its own makes the
	// compare and the write one step.
	c.tagMu.Lock()
	defer c.tagMu.Unlock()
	cur, err := c.readTagFile(r.ref)
	if err != nil || bytes.Equal(cur, b) || !bytes.Equal(cur, r.seenTag) {
		return err
	}
	return c.writeFile(tagsDir, filepath.Base(c.tagPath(r.ref)), b)
}

// cachedTag looks the digest that ref's tag file b names up through cached;
// b is nil when the cache has no tag file.
func (c *Cache) cachedTag(ref name.Reference, b []byte, cached func(v1.Hash) (bool, error)) (bool, error) {
	if b == nil {
		return false, nil
	}
	h, err := c.parseTag(ref, b)
	if err != nil {
		return false, err
	}
	return cached(h)
}

// readTagFile returns ref's tag file, or nil when the cache has none.
func (c *Cache) readTagFile(ref name.Reference) ([]byte, error) {
	b, err := os.ReadFile(c.tagPath(ref))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// parseTag returns the digest that b, ref's tag file, names.
func (c *Cache) parseTag(ref name.Reference, b []byte) (v1.Hash, error) {
	path := c.tagPath(ref)
	var tf tagFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return v1.Hash{}, fmt.Errorf("tag file %s: %w", path, err)
	}
	if tf.Reference != ref.Name() {
		return v1.Hash{}, fmt.Errorf("tag file %s names %q, not %q", path, tf.Reference, ref.Name())
	}
	digest, err := v1.NewHash(tf.Digest)
	if err == nil {
		err = checkDigest(digest)
	}
	if err != nil {
		return v1.Hash{}, fmt.Errorf("tag file %s: %w", path, err)
	}
	return digest, nil
}

// writeFile writes b to <cache>/<dir>/<name>: a temp file, fsync, rename
// into place, and fsync of the directory, so a crash leaves the old file or
// the new one, never a partial one. Mode 0600, from os.CreateTemp.
func (c *Cache) writeFile(dir, name string, b []byte) (err error) {
	f, err := os.CreateTemp(c.path(tmpDir), name+"-*")
	if err != nil {
		return c.spaceError(err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			err = c.spaceError(err)
		}
	}()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), c.path(dir, name)); err != nil {
		return err
	}
	return syncDir(c.path(dir))
}

// tagPath is the tag file of ref, named by the sha256 of its normalised
// name, so a long repository path can't outgrow NAME_MAX and no escaping
// rule is the cache's own.
func (c *Cache) tagPath(ref name.Reference) string {
	sum := sha256.Sum256([]byte(ref.Name()))
	return c.path(tagsDir, hex.EncodeToString(sum[:]))
}

// imagePath is where the config of the image with manifest digest is cached.
func (c *Cache) imagePath(digest v1.Hash) string {
	return c.path(imagesDir, digest.Hex+".json")
}

func (c *Cache) remoteOpts(ctx context.Context) []remote.Option {
	return append([]remote.Option{remote.WithContext(ctx)}, c.opts...)
}

// checkDigest refuses a manifest digest that can't name a cached image
// config. A digest reference comes from the workflow, and becomes a file
// name.
func checkDigest(h v1.Hash) error {
	if !isSHA256Hex(h) {
		return fmt.Errorf("image digest %q: not a sha256 digest", h)
	}
	return nil
}
