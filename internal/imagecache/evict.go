// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"cmp"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// evictable is a file eviction may delete: a layer or an image config.
type evictable struct {
	// kind is "layer" or "config", as logged.
	kind string
	// id is the layer's diffID or the config's manifest digest.
	id   v1.Hash
	path string
	// size is the file's allocated size, in bytes.
	size int64
	// used is the file's mtime: its last use.
	used time.Time
}

// pin marks the image config cached under digest, and the layers diffIDs,
// as needed by an Acquire in flight until the returned func is called, so
// eviction skips them between the Acquire's look at the cache and its link
// and record: from then on the links mark the layers in use. Pruning keeps
// the config of an image with a layer pinned, and so its tag.
func (c *Cache) pin(digest v1.Hash, diffIDs []v1.Hash) func() {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	return c.pinLocked(digest, diffIDs)
}

// pinLocked is pin with c.storeMu held. Its returned func takes c.storeMu,
// and does nothing after its first call.
func (c *Cache) pinLocked(digest v1.Hash, diffIDs []v1.Hash) func() {
	c.pinnedConfigs[digest]++
	for _, d := range diffIDs {
		c.pinned[d]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.storeMu.Lock()
			defer c.storeMu.Unlock()
			if c.pinnedConfigs[digest]--; c.pinnedConfigs[digest] == 0 {
				delete(c.pinnedConfigs, digest)
			}
			for _, d := range diffIDs {
				if c.pinned[d]--; c.pinned[d] == 0 {
					delete(c.pinned, d)
				}
			}
		})
	}
}

// configInUse reports whether an Acquire in flight has looked up the image
// config cached under digest, or a job not yet released acquired it.
// c.storeMu is held.
func (c *Cache) configInUse(digest v1.Hash) bool {
	if c.pinnedConfigs[digest] > 0 {
		return true
	}
	for _, held := range c.jobConfigs {
		if slices.Contains(held, digest) {
			return true
		}
	}
	return false
}

// evict deletes least-recently-used layers and image configs not in use,
// oldest mtime first, until the allocated size of the cached layers, configs
// and tags fits the cap, and then, if it deleted any or always is set,
// prunes the image configs and tags (IMG-14). A layer is in use while a VM
// links it or an Acquire in flight pins it, and a config from its Acquire's
// lookup to its job's Release: neither is ever deleted, even over the cap.
// Errors are logged: they are no job's.
func (c *Cache) evict(always bool) {
	c.storeMu.Lock()
	deleted, err := c.evictLRU()
	c.storeMu.Unlock()
	if err != nil {
		c.log.Error("eviction", "err", err)
	}
	if !deleted && !always {
		return
	}
	if err := c.prune(); err != nil {
		c.log.Error("pruning", "err", err)
	}
}

// evictLRU is evict's deletion of layers and configs, and reports whether
// it deleted any. It reads only directories and file metadata. c.storeMu is
// held.
func (c *Cache) evictLRU() (bool, error) {
	var errs []error
	var total int64
	var free []evictable
	// scan adds up the allocated size of the files in dir that name
	// parses, and calls add for each.
	scan := func(dir string, name func(string) (v1.Hash, bool), add func(evictable, *syscall.Stat_t)) {
		es, err := os.ReadDir(c.path(dir))
		if err != nil {
			errs = append(errs, err)
			return
		}
		for _, e := range es {
			id, ok := name(e.Name())
			if !ok {
				continue
			}
			fi, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok || !fi.Mode().IsRegular() {
				continue
			}
			f := evictable{id: id, path: c.path(dir, e.Name()), size: st.Blocks * 512, used: fi.ModTime()}
			total += f.size
			if add != nil {
				add(f, st)
			}
		}
	}
	scan(layersDir, layerName, func(f evictable, st *syscall.Stat_t) {
		// One link is the cache's own name: no layer link directory
		// holds it.
		if st.Nlink == 1 && c.pinned[f.id] == 0 {
			f.kind = "layer"
			free = append(free, f)
		}
	})
	scan(imagesDir, configName, func(f evictable, _ *syscall.Stat_t) {
		if !c.configInUse(f.id) {
			f.kind = "config"
			free = append(free, f)
		}
	})
	// A tag is never evicted itself: pruning deletes it after its config.
	scan(tagsDir, func(n string) (v1.Hash, bool) {
		h := v1.Hash{Algorithm: "sha256", Hex: n}
		return h, isSHA256Hex(h)
	}, nil)
	if total <= c.maxSize {
		return false, errors.Join(errs...)
	}
	slices.SortFunc(free, func(a, b evictable) int {
		return cmp.Or(a.used.Compare(b.used), strings.Compare(a.path, b.path))
	})
	deleted := false
	for _, f := range free {
		if total <= c.maxSize {
			break
		}
		if err := os.Remove(f.path); err != nil {
			errs = append(errs, err)
			continue
		}
		total -= f.size
		deleted = true
		if f.kind == "layer" {
			c.log.Info("evicted", "layer", f.id.String(), "size", f.size)
		} else {
			c.log.Info("evicted", "config", f.id.String(), "size", f.size)
		}
	}
	return deleted, errors.Join(errs...)
}

// scannedFile is a config or tag file as pruning read it, without
// c.storeMu.
type scannedFile struct {
	path string
	fi   fs.FileInfo
}

// unchanged reports whether the file at s.path is still the one pruning
// read: every write renames a new file into place.
func (s scannedFile) unchanged() (bool, error) {
	fi, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(fi, s.fi) && fi.ModTime().Equal(s.fi.ModTime()) && fi.Size() == s.fi.Size(), nil
}

// scanFile reads the file at p, with its metadata as it read it.
func scanFile(p string) (scannedFile, []byte, error) {
	f, err := os.Open(p) //nolint:gosec // G304: a file in <cache>/images/ or <cache>/tags/
	if err != nil {
		return scannedFile{}, nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return scannedFile{}, nil, err
	}
	b, err := io.ReadAll(f)
	return scannedFile{path: p, fi: fi}, b, err
}

// prune deletes every image config with no cached layer left and not in
// use, then every tag whose config is gone. An image with some of its
// layers left, or whose config is in use, keeps both: its next Start
// fetches the missing layers by digest.
//
// A config or tag file whose content a lookup refuses (parseConfig,
// parseTagFile) is deleted too, and logged, as a lookup deletes it. One it
// can't read is kept, and named in the error. A file in <cache>/tags/ not
// named like a tag file is not the cache's, and is kept.
//
// It reads and parses the files without c.storeMu, so a Start's link and
// record never wait on the parse of every config, which may be up to
// maxConfigSize each; under c.storeMu it only checks that each file is
// still the one it read, and deletes it. A file rewritten since is left for
// the next pruning.
func (c *Cache) prune() error {
	var errs []error
	type config struct {
		scannedFile
		digest  v1.Hash
		diffIDs []v1.Hash
		// damaged is parseConfig's error.
		damaged error
	}
	es, err := os.ReadDir(c.path(imagesDir))
	if err != nil {
		return err
	}
	var configs []config
	for _, e := range es {
		digest, ok := configName(e.Name())
		if !ok {
			continue
		}
		s, raw, err := scanFile(c.imagePath(digest))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cf := config{scannedFile: s, digest: digest}
		if cfg, err := parseConfig(raw); err != nil {
			cf.damaged = err
		} else {
			cf.diffIDs = cfg.RootFS.DiffIDs
		}
		configs = append(configs, cf)
	}
	if c.pruneScanned != nil {
		c.pruneScanned()
	}
	c.storeMu.Lock()
	for _, cf := range configs {
		same, err := cf.unchanged()
		switch {
		case err != nil || !same:
		case cf.damaged != nil:
			err = c.deleteDamaged(cf.path, cf.damaged)
		case c.configInUse(cf.digest):
		default:
			var cached bool
			if cached, err = c.anyLayerCached(cf.diffIDs); err == nil && !cached {
				if err = os.Remove(cf.path); err == nil {
					c.log.Info("pruned", "config", cf.digest.String())
				}
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	c.storeMu.Unlock()

	type tag struct {
		scannedFile
		tf     tagFile
		digest v1.Hash
		// damaged is parseTagFile's error.
		damaged error
	}
	es, err = os.ReadDir(c.path(tagsDir))
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var tags []tag
	for _, e := range es {
		if !isSHA256Hex(v1.Hash{Algorithm: "sha256", Hex: e.Name()}) {
			continue
		}
		s, b, err := scanFile(c.path(tagsDir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		t := tag{scannedFile: s}
		t.tf, t.digest, t.damaged = parseTagFile(e.Name(), b)
		tags = append(tags, t)
	}
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	for _, t := range tags {
		same, err := t.unchanged()
		switch {
		case err != nil || !same:
		case t.damaged != nil:
			err = c.deleteDamaged(t.path, t.damaged)
		default:
			if _, err = os.Stat(c.imagePath(t.digest)); errors.Is(err, fs.ErrNotExist) {
				if err = os.Remove(t.path); err == nil {
					c.log.Info("pruned", "tag", t.tf.Reference)
				}
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deleteDamaged deletes the config or tag file at p, whose content
// parseConfig or parseTagFile refused with err, and logs it. It returns
// only a failure to delete. c.storeMu is held.
func (c *Cache) deleteDamaged(p string, err error) error {
	if rerr := os.Remove(p); rerr != nil {
		return rerr
	}
	c.log.Warn("deleted a damaged file", "path", p, "err", err)
	return nil
}

// anyLayerCached reports whether the cache holds any of the layers diffIDs,
// or an Acquire in flight pins one. c.storeMu is held.
func (c *Cache) anyLayerCached(diffIDs []v1.Hash) (bool, error) {
	for _, d := range diffIDs {
		if checkDiffID(d) != nil {
			continue
		}
		if c.pinned[d] > 0 {
			return true, nil
		}
		_, err := os.Stat(c.layerPath(d))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// configName returns the manifest digest that a file name in
// <cache>/images/ names, and false for any other file.
func configName(n string) (v1.Hash, bool) {
	hex, ok := strings.CutSuffix(n, ".json")
	h := v1.Hash{Algorithm: "sha256", Hex: hex}
	return h, ok && isSHA256Hex(h)
}

// layerName returns the diffID that a file name in <cache>/layers/ names,
// and false for any other file.
func layerName(n string) (v1.Hash, bool) {
	hex, ok := strings.CutSuffix(n, ".erofs")
	h := v1.Hash{Algorithm: "sha256", Hex: hex}
	return h, ok && isSHA256Hex(h)
}
