// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/sys/unix"
)

// The image cache's directories under <cache> (docs/architecture.md, Image
// cache).
const (
	// layersDir holds one EROFS file per layer, <diffID hex>.erofs. A
	// file's presence is the index: there is no index file to keep in sync.
	layersDir = "layers"
	// tmpDir holds partial writes, renamed into place once complete.
	tmpDir = "tmp"
	// imagesDir holds each image's raw config, <manifest digest hex>.json.
	imagesDir = "images"
	// tagsDir holds each reference's tag file, <sha256 hex of the
	// normalised reference>: the reference and its manifest digest.
	tagsDir = "tags"
	// jobsDir holds each VM's layer link directory, jobs/<id>/<n>/.
	jobsDir = "jobs"
)

// maxPulls is how many layer pulls run at once, plugin-wide. A limit per
// image stops being a limit once several cold images pull together.
const maxPulls = 3

// maxLayers is the most layers an image may have: overlay stacks at most
// 500 lower layers, and the boot root is one of them.
const maxLayers = 499

// Cache is the image cache's real adapter.
type Cache struct {
	log  *slog.Logger
	dir  string
	opts []remote.Option
	// pulls holds a token for each layer pull running, up to maxPulls.
	pulls chan struct{}
	// convert is the layer conversion; tests replace it.
	convert convertFunc
	// decompress turns a layer's blob into its tar stream; tests replace
	// it.
	decompress decompressFunc
	// rename stores a temp file under a layer's name, never replacing
	// one; tests replace it.
	rename func(oldpath, newpath string) error
	// limits bounds each layer's tar stream; tests lower them.
	limits tarLimits

	// tagMu makes record's compare and write of a tag file one step.
	tagMu sync.Mutex

	mu sync.Mutex
	// acquiring counts each job's Acquires in flight, for Release.
	acquiring map[string]*inFlight
	// releaseWaits, when set, is called when Release starts waiting for
	// an Acquire: a test's view of it. Nil outside tests.
	releaseWaits func()
}

// inFlight is one job's Acquires in flight.
type inFlight struct {
	n int
	// idle is closed when n drops to zero.
	idle chan struct{}
}

// New returns the image cache in dir, the plugin's cache directory, which
// exists and is locked, and creates its directories, mode 0700: the cache
// holds image configs, whose environment may carry values. It pulls with the
// plugin user's registry credentials (IMG-3): go-containerregistry's default
// keychain, which reads ~/.docker/config.json or $DOCKER_CONFIG and runs its
// credential helpers. It pulls for linux/<GOARCH> (IMG-2). Plain HTTP is the
// library's own rule: a localhost, loopback or RFC 1918 registry is tried
// over HTTPS, then HTTP; every other registry is HTTPS only.
//
// SHORTCUT(#45): New keeps whatever a crash left in <cache>/tmp/ and
// <cache>/jobs/: temp files and link directories of jobs that no longer
// exist stay until someone deletes them, and the layers they link stay with
// them.
func New(log *slog.Logger, dir string, opts ...remote.Option) (*Cache, error) {
	for _, d := range []string{layersDir, imagesDir, tagsDir, tmpDir, jobsDir} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, fmt.Errorf("the image cache directory %s: %w", dir, err)
		}
	}
	return &Cache{
		log: log,
		dir: dir,
		opts: append([]remote.Option{
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
			remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
		}, opts...),
		pulls:      make(chan struct{}, maxPulls),
		convert:    convert,
		decompress: decompress,
		rename:     renameNoReplace,
		limits:     tarLimits{entries: maxEntries, metadata: maxMetadata},
		acquiring:  map[string]*inFlight{},
	}, nil
}

func (c *Cache) path(elem ...string) string {
	return filepath.Join(append([]string{c.dir}, elem...)...)
}

// Acquire pulls and converts whatever layers of req.Ref the cache lacks, and
// makes the VM's layer link directory, <cache>/jobs/<id>/<n>/. A failed
// Acquire leaves no link directory and no partial file, and fails only this
// call.
func (c *Cache) Acquire(ctx context.Context, req Request) (Image, error) {
	if err := checkName(req.Job); err != nil || req.VM < 0 {
		return Image{}, fmt.Errorf("job %q, VM %d: not a job's VM", req.Job, req.VM)
	}
	defer c.begin(req.Job)()
	ref, err := name.ParseReference(req.Ref)
	if err != nil {
		return Image{}, fmt.Errorf("image %s: %w", req.Ref, err)
	}
	// Every registry request ends with pctx: a layer that fails cancels
	// the others.
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r, err := c.resolve(pctx, ref, req.ForcePull)
	if err != nil {
		return Image{}, fmt.Errorf("pull %s: %w", req.Ref, err)
	}
	diffIDs := r.config.RootFS.DiffIDs
	if len(diffIDs) > maxLayers {
		return Image{}, fmt.Errorf("image %s has %d layers; a VM's root stacks at most %d", req.Ref, len(diffIDs), maxLayers)
	}
	for i, d := range diffIDs {
		if err := checkDiffID(d); err != nil {
			return Image{}, fmt.Errorf("pull %s: layer %d: %w", req.Ref, i, err)
		}
	}
	if err := c.pull(pctx, cancel, diffIDs, c.layerSource(pctx, r)); err != nil {
		return Image{}, fmt.Errorf("pull %s: %w", req.Ref, err)
	}
	linkDir, names, err := c.link(req, diffIDs)
	if err != nil {
		return Image{}, fmt.Errorf("image %s: %w", req.Ref, err)
	}
	if err := c.record(r); err != nil {
		_ = os.RemoveAll(linkDir)
		return Image{}, fmt.Errorf("image %s: %w", req.Ref, err)
	}
	c.log.Info("acquired", "job", req.Job, "vm", req.VM, "image", req.Ref, "digest", r.digest.String(), "layers", len(names))
	return Image{Digest: r.digest.String(), Env: r.config.Config.Env, LinkDir: linkDir, Layers: names}, nil
}

// pull converts each layer the cache lacks, each once, however often the
// image names it. All of them are converted before any is renamed into
// place, so a layer that fails leaves nothing in the cache (IMG-10).
//
// SHORTCUT(#43): two Acquires that need the same missing layer each convert
// it; the second to finish keeps the file the first one stored.
//
// layer returns the image's layer i as the registry serves it; pull calls it
// only for the layers the cache lacks. The layers' reads end with ctx, which
// cancel cancels: the first layer that fails stops the others.
func (c *Cache) pull(ctx context.Context, cancel context.CancelFunc, diffIDs []v1.Hash, layer func(i int) (v1.Layer, error)) error {
	type pending struct {
		diffID v1.Hash
		tmp    string
		err    error
	}
	// Every layer is looked up before any conversion starts, so a lookup
	// that fails returns with nothing running and nothing written.
	var todo []*pending
	var ls []v1.Layer
	seen := map[v1.Hash]bool{}
	for i, d := range diffIDs {
		if seen[d] {
			continue
		}
		seen[d] = true
		if _, err := os.Stat(c.layerPath(d)); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("layer %s: %w", d, err)
		}
		l, err := layer(i)
		if err != nil {
			return fmt.Errorf("layer %s: %w", d, err)
		}
		todo = append(todo, &pending{diffID: d})
		ls = append(ls, l)
	}
	var g sync.WaitGroup
	for i, p := range todo {
		l := ls[i]
		g.Go(func() {
			select {
			case c.pulls <- struct{}{}:
			case <-ctx.Done():
				p.err = ctx.Err()
				return
			}
			defer func() { <-c.pulls }()
			if p.err = ctx.Err(); p.err != nil {
				return
			}
			p.tmp, p.err = c.convertLayer(l, p.diffID)
			if p.err != nil {
				cancel()
			}
		})
	}
	g.Wait()
	// The layer that failed first is the cause; the layers it cancelled
	// report only that.
	err := ctx.Err()
	for _, p := range todo {
		if p.err != nil && (err == nil || errors.Is(err, context.Canceled) && !errors.Is(p.err, context.Canceled)) {
			err = p.err
		}
	}
	if err != nil {
		for _, p := range todo {
			if p.tmp != "" {
				_ = os.Remove(p.tmp)
			}
		}
		return err
	}
	for i, p := range todo {
		if err := c.store(p.tmp, p.diffID); err != nil {
			for _, q := range todo[i:] {
				_ = os.Remove(q.tmp)
			}
			return c.spaceError(err)
		}
		c.log.Info("converted", "layer", p.diffID.String())
	}
	if len(todo) > 0 {
		return syncDir(c.path(layersDir))
	}
	return nil
}

// store renames the complete temp file tmp into place as the layer diffID.
// It never replaces a file already there: a layer another Acquire stored
// first is the same content, and VMs may already link it.
//
// The cache's filesystem must support renameat2's RENAME_NOREPLACE
// (docs/development.md); one that doesn't fails the Acquire, naming the
// cache directory.
func (c *Cache) store(tmp string, diffID v1.Hash) error {
	err := c.rename(tmp, c.layerPath(diffID))
	switch {
	case errors.Is(err, unix.EEXIST):
		return os.Remove(tmp)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.EOPNOTSUPP):
		return fmt.Errorf("the image cache directory %s is on a filesystem without rename's RENAME_NOREPLACE, which the image cache needs: %w", c.dir, err)
	}
	return err
}

// renameNoReplace renames oldpath to newpath unless newpath exists.
func renameNoReplace(oldpath, newpath string) error {
	return unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
}

// link makes the VM's layer link directory: one hard link per layer of its
// image, named by its position, bottom layer first, since an image may hold
// one layer twice. Each layer's mtime is set as it is linked, so its last use
// is recorded. On error the directory is deleted.
func (c *Cache) link(req Request, diffIDs []v1.Hash) (dir string, names []string, err error) {
	jobDir := c.path(jobsDir, req.Job)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return "", nil, err
	}
	vmDir := filepath.Join(jobDir, strconv.Itoa(req.VM))
	if err := os.Mkdir(vmDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("layer link directory: %w", err)
	}
	// vmDir, not dir: an error returns dir empty.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(vmDir)
		}
	}()
	now := time.Now()
	for i, d := range diffIDs {
		src := c.layerPath(d)
		// SHORTCUT(#45): a layer deleted from the cache since it was
		// converted fails the Acquire instead of being converted again
		// (IMG-15).
		if err := os.Chtimes(src, now, now); err != nil {
			return "", nil, fmt.Errorf("layer %s: %w", d, err)
		}
		n := strconv.Itoa(i) + ".erofs"
		if err := os.Link(src, filepath.Join(vmDir, n)); err != nil {
			return "", nil, fmt.Errorf("layer %s: %w", d, err)
		}
		names = append(names, n)
	}
	return vmDir, names, nil
}

// begin counts an Acquire of job in flight until the returned func is called.
func (c *Cache) begin(job string) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.acquiring[job]
	if f == nil {
		f = &inFlight{idle: make(chan struct{})}
		c.acquiring[job] = f
	}
	f.n++
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if f.n--; f.n == 0 {
			close(f.idle)
			delete(c.acquiring, job)
		}
	}
}

// Release deletes the layer link directories of every VM of job, once every
// Acquire of job in flight has returned, so a cancelled Acquire can't finish
// a link directory after it is deleted. A job with none is not an error.
func (c *Cache) Release(ctx context.Context, job string) error {
	if err := checkName(job); err != nil {
		return err
	}
	for {
		c.mu.Lock()
		f := c.acquiring[job]
		hook := c.releaseWaits
		c.mu.Unlock()
		if f == nil {
			break
		}
		if hook != nil {
			hook()
		}
		select {
		case <-f.idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return os.RemoveAll(c.path(jobsDir, job))
}

// checkName refuses a job id that is not one path component.
func checkName(job string) error {
	if job == "" || job == "." || job == ".." || filepath.Base(job) != job {
		return fmt.Errorf("job %q: not a job's environment id", job)
	}
	return nil
}

// syncDir fsyncs dir, so the renames in it survive a power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: a directory of the image cache
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
