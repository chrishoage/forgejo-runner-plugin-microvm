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
	// maxSize is the cap on the allocated size of the cached layers,
	// image configs and tags, in bytes. No layer converts past it, unless
	// it is 0. capped says WithMaxSize set it.
	maxSize int64
	capped  bool

	// storeMu is held by linking, eviction and pruning, and by record's
	// writes of configs and tags, so an eviction can't delete a layer
	// between an Acquire's conversion and its link, or a config while a
	// Start writes it. It also makes record's compare and write of a tag
	// file one step.
	storeMu sync.Mutex
	// pinned counts, for each layer, the Acquires in flight that need it;
	// eviction skips a layer pinned. Held by storeMu.
	pinned map[v1.Hash]int
	// pinnedConfigs counts, for each image config by manifest digest,
	// the Acquires in flight that looked it up; eviction and pruning skip
	// a config pinned. Held by storeMu.
	pinnedConfigs map[v1.Hash]int
	// jobConfigs lists, for each job, the image config of each of its
	// VMs' Acquires that succeeded, by manifest digest: a config is in use
	// from its Acquire to its job's Release, like the layers its VM links,
	// and eviction and pruning skip it. Held by storeMu.
	jobConfigs map[string][]v1.Hash

	// lookups shares one lookup per reference, and layers one pull per
	// layer, between the Acquires that need it at once (IMG-12).
	lookups *sharedCalls[*resolved]
	layers  *sharedCalls[*pulledLayer]
	// running counts the shared calls running, the abandoned ones that
	// still unwind included: tests wait on it.
	running sync.WaitGroup

	mu sync.Mutex
	// acquiring counts each job's Acquires in flight, for Release.
	acquiring map[string]*inFlight
	// afterLookup, when set, is called as Acquire's lookup returns: a
	// test's view of it. Nil outside tests.
	afterLookup func()
	// beforeLink, when set, is called as Acquire starts linking: a test's
	// view of it. Nil outside tests.
	beforeLink func()
	// pruneScanned, when set, is called once pruning has read the
	// configs, before it takes storeMu: a test's view of it. Nil outside
	// tests.
	pruneScanned func()
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

// Option configures the image cache that New returns.
type Option func(*Cache)

// WithMaxSize sets the image cache's size cap: the allocated size, in bytes,
// that eviction keeps its layers, image configs and tags under, and that no
// layer's conversion may pass. New requires it, 0 or more. A cap of 0 is
// taken literally: the cache keeps nothing no VM is using, and then no
// layer's conversion is bounded by it, since every layer is over it.
func WithMaxSize(bytes int64) Option {
	return func(c *Cache) { c.maxSize, c.capped = bytes, true }
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
// New is the cache side of startup cleanup (docs/architecture.md, Failure
// handling and startup cleanup): it deletes everything in <cache>/jobs/ and
// <cache>/tmp/ without inspecting it, then evicts and prunes once. The
// directory lock proves no other plugin uses the cache, and no job process
// outlives the plugin that started it, so every link directory and temp
// file there is a crash's leftover. Eviction and pruning errors are logged,
// as after a Release; a leftover New can't delete fails it.
func New(log *slog.Logger, dir string, opts ...Option) (*Cache, error) {
	for _, d := range []string{layersDir, imagesDir, tagsDir, tmpDir, jobsDir} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, fmt.Errorf("the image cache directory %s: %w", dir, err)
		}
	}
	c := &Cache{
		log: log,
		dir: dir,
		opts: []remote.Option{
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
			remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
		},
		pulls:      make(chan struct{}, maxPulls),
		convert:    convert,
		decompress: decompress,
		rename:     renameNoReplace,
		limits:     tarLimits{entries: maxEntries, metadata: maxMetadata},
		acquiring:  map[string]*inFlight{},
		pinned:     map[v1.Hash]int{},

		pinnedConfigs: map[v1.Hash]int{},
		jobConfigs:    map[string][]v1.Hash{},
	}
	for _, o := range opts {
		o(c)
	}
	if !c.capped {
		return nil, fmt.Errorf("the image cache %s: no size cap", dir)
	}
	if c.maxSize < 0 {
		return nil, fmt.Errorf("the image cache %s: a negative size cap, %d", dir, c.maxSize)
	}
	c.lookups = newSharedCalls(&c.running, func(r *resolved) { r.unpin() })
	c.layers = newSharedCalls(&c.running, (*pulledLayer).discard)
	jobs, err := emptyDir(c.path(jobsDir))
	if err != nil {
		return nil, fmt.Errorf("the image cache directory %s: startup cleanup: %w", dir, err)
	}
	tmp, err := emptyDir(c.path(tmpDir))
	if err != nil {
		return nil, fmt.Errorf("the image cache directory %s: startup cleanup: %w", dir, err)
	}
	if jobs > 0 || tmp > 0 {
		log.Info("deleted crash leftovers", "cache", dir, "jobs", jobs, "temp_files", tmp)
	}
	// The link directories just deleted may have been all that kept
	// layers in use, and a crash between an eviction and its pruning
	// leaves configs with no layer: so prune whatever eviction deletes.
	c.evict(true)
	return c, nil
}

// emptyDir deletes everything in dir, keeping dir, and returns how many
// entries it deleted.
func emptyDir(dir string) (int, error) {
	es, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	for _, e := range es {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return 0, err
		}
	}
	return len(es), nil
}

func (c *Cache) path(elem ...string) string {
	return filepath.Join(append([]string{c.dir}, elem...)...)
}

// Acquire pulls and converts whatever layers of req.Ref the cache lacks, and
// makes the VM's layer link directory, <cache>/jobs/<id>/<n>/. A failed
// Acquire leaves no link directory and fails only this call; a pull it alone
// waited on is cancelled, and deletes its temp file as it unwinds. Acquires
// that need the same reference or layer at once share its lookup and pull
// (IMG-12); a cancelled one returns ctx's error at once.
func (c *Cache) Acquire(ctx context.Context, req Request) (Image, error) {
	if err := checkName(req.Job); err != nil || req.VM < 0 {
		return Image{}, fmt.Errorf("job %q, VM %d: not a job's VM", req.Job, req.VM)
	}
	defer c.begin(req.Job)()
	ref, err := name.ParseReference(req.Ref)
	if err != nil {
		return Image{}, fmt.Errorf("image %s: %w", req.Ref, err)
	}
	// From the lookup to the link, the image's layers are pinned, so an
	// eviction can't delete one this Acquire found cached or stored.
	r, err := c.lookup(ctx, ref, req.ForcePull)
	if err != nil {
		return Image{}, fmt.Errorf("pull %s: %w", req.Ref, err)
	}
	defer r.unpin()
	if c.afterLookup != nil {
		c.afterLookup()
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
	linkDir, names, pulled, err := c.pullAndLink(ctx, req, diffIDs, c.layerSource(ctx, r))
	if err != nil {
		return Image{}, err
	}
	if err := c.record(r); err != nil {
		_ = os.RemoveAll(linkDir)
		return Image{}, fmt.Errorf("image %s: %w", req.Ref, err)
	}
	// Nothing fails from here on: the job holds the config until its
	// Release, and this Acquire's own pin, released as it returns, covers
	// it until then.
	c.useConfig(req.Job, r.digest)
	c.log.Info("acquired", "env", req.Job, "vm", req.VM, "image", req.Ref, "digest", r.digest.String(), "layers", len(names),
		"layers_pulled", pulled)
	return Image{
		Digest: r.digest.String(), Env: r.config.Config.Env, User: r.config.Config.User, WorkingDir: r.config.Config.WorkingDir,
		Entrypoint: r.config.Config.Entrypoint, Cmd: r.config.Config.Cmd, Healthcheck: healthcheck(r.config, r.raw),
		LinkDir: linkDir, Layers: names, Pulled: pulled,
	}, nil
}

// pullAndLink pulls the layers the cache lacks and links them. A layer
// deleted from under the cache since pull looked (IMG-15), by something
// other than the plugin since it is pinned, is pulled and converted again,
// once: one retry covers a single deletion, and a loop could spin against
// something that keeps deleting. A second miss fails, naming the layer and
// the cache directory. It returns how many of the image's layers it pulled,
// each counted once: a layer the retry pulls again was pulled already.
func (c *Cache) pullAndLink(ctx context.Context, req Request, diffIDs []v1.Hash, layer func(i int) (fetchLayer, error)) (string, []string, int, error) {
	pulled := map[v1.Hash]bool{}
	for retry := false; ; retry = true {
		lacked, err := c.pull(ctx, diffIDs, layer, retry)
		for _, d := range lacked {
			pulled[d] = true
		}
		if err != nil {
			return "", nil, 0, fmt.Errorf("pull %s: %w", req.Ref, err)
		}
		if c.beforeLink != nil {
			c.beforeLink()
		}
		linkDir, names, err := c.link(req, diffIDs)
		var missing *missingLayerError
		if errors.As(err, &missing) && !retry {
			c.log.Warn("layer deleted from the cache, converting it again", "env", req.Job, "vm", req.VM, "layer", missing.diffID.String(), "cache", c.dir)
			continue
		}
		if errors.As(err, &missing) {
			return "", nil, 0, fmt.Errorf("image %s: layer %s was deleted from the image cache %s, converted again, and deleted again: %w", req.Ref, missing.diffID, c.dir, missing.err)
		}
		if err != nil {
			return "", nil, 0, fmt.Errorf("image %s: %w", req.Ref, err)
		}
		return linkDir, names, len(pulled), nil
	}
}

// missingLayerError is link's error for a layer missing from <cache>/layers/.
type missingLayerError struct {
	diffID v1.Hash
	err    error
}

func (e *missingLayerError) Error() string { return fmt.Sprintf("layer %s: %v", e.diffID, e.err) }
func (e *missingLayerError) Unwrap() error { return e.err }

// lookup is resolve, shared by the Acquires that look ref up at once. The
// lookup runs in a context of its own, and ends when the last of them leaves
// it. Each Acquire gets a copy, so the copies' later changes are their own,
// with a pin of its own on the image's layers, which it unpins. The shared
// result's pin, taken as resolve read the cache, is held until every Acquire
// has its own, so no eviction finds the layers unpinned in between.
func (c *Cache) lookup(ctx context.Context, ref name.Reference, force bool) (*resolved, error) {
	// A force_pull lookup never hands its caller a cached tag, so it
	// shares only with other force_pull lookups.
	key := ref.Name()
	if force {
		key += " force"
	}
	r, release, err := c.lookups.do(ctx, key, func(ctx context.Context) (*resolved, error) {
		return c.resolve(ctx, ref, force)
	})
	defer release()
	if err != nil {
		return nil, err
	}
	rr := *r
	rr.unpin = c.pin(r.digest, r.config.RootFS.DiffIDs)
	return &rr, nil
}

// fetchLayer is where an image's layer comes from, and how to fetch it.
type fetchLayer struct {
	// source is the layer's blob, registry/repository@digest.
	source string
	// fetch returns the layer as the registry serves it. Its reads end
	// with ctx, the context of the layer's shared pull.
	fetch func(ctx context.Context) (v1.Layer, error)
}

// pullError is a failed pull's error, with the source it pulled from.
type pullError struct {
	source string
	err    error
}

func (e *pullError) Error() string { return e.err.Error() }
func (e *pullError) Unwrap() error { return e.err }

// fromSource reports whether err is the failure of a pull from source.
func fromSource(err error, source string) bool {
	var pe *pullError
	return errors.As(err, &pe) && pe.source == source
}

// pulledLayer is a shared pull's result: a converted layer in tmp, until
// the first Acquire that needs it stores it.
type pulledLayer struct {
	mu     sync.Mutex
	tmp    string
	stored bool
	// size is the converted layer's size in bytes, and took how long its
	// fetch and conversion took, from when it had its place among the
	// pulls.
	size int64
	took time.Duration
}

// discard deletes the temp file once nobody holds the pull: every Acquire
// that waited on it stored it or failed.
func (p *pulledLayer) discard() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stored && p.tmp != "" {
		_ = os.Remove(p.tmp)
	}
}

// pull converts each layer the cache lacks, each once, however often the
// image names it. All of them are converted before any is renamed into
// place, so a layer that fails leaves nothing in the cache (IMG-10).
// It returns the layers it pulled: those the cache lacked, each once.
//
// Each layer's pull is shared with every Acquire that needs that layer
// while it runs (IMG-12), and runs in a context of its own. A cancelled ctx
// returns its error at once; the first layer that fails stops this
// Acquire's waits on the others. A pull ends when its last waiter leaves.
//
// A shared pull fetches from the source of the Acquire that started it. One
// that fails from another source is tried once more from this Acquire's
// own, shared with the other Acquires retrying from that source: an image
// may name another image's layer's diffID for a blob of its own, and must
// not fail the Starts that join its pull. A failure from this Acquire's own
// source is final.
//
// layer returns how to fetch the image's layer i; pull calls it only for
// the layers the cache lacks, all of them before any pull starts.
//
// A retry, after a layer was deleted from under the cache, shares its pulls
// only with other retries: a pull another Acquire still holds may have
// stored the layer that was deleted, and joining it would store nothing.
func (c *Cache) pull(ctx context.Context, diffIDs []v1.Hash, layer func(i int) (fetchLayer, error), retry bool) ([]v1.Hash, error) {
	type pending struct {
		diffID  v1.Hash
		fetch   fetchLayer
		p       *pulledLayer
		release func()
		err     error
	}
	// Every layer is looked up before any pull starts, so a lookup that
	// fails returns with nothing running and nothing written.
	var todo []*pending
	seen := map[v1.Hash]bool{}
	for i, d := range diffIDs {
		if seen[d] {
			continue
		}
		seen[d] = true
		if _, err := os.Stat(c.layerPath(d)); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("layer %s: %w", d, err)
		}
		f, err := layer(i)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", d, err)
		}
		todo = append(todo, &pending{diffID: d, fetch: f})
	}
	// wctx ends this Acquire's waits: the first layer that fails cancels
	// it.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var g sync.WaitGroup
	for _, p := range todo {
		g.Go(func() {
			fn := func(pctx context.Context) (*pulledLayer, error) {
				return c.pullLayer(pctx, p.diffID, p.fetch)
			}
			key := p.diffID.String()
			if retry {
				key += " retry"
			}
			p.p, p.release, p.err = c.layers.do(wctx, key, fn)
			if p.err != nil && wctx.Err() == nil && !fromSource(p.err, p.fetch.source) {
				p.release()
				p.p, p.release, p.err = c.layers.do(wctx, key+" "+p.fetch.source, fn)
			}
			if p.err != nil {
				cancel()
			}
		})
	}
	g.Wait()
	defer func() {
		for _, p := range todo {
			p.release()
		}
	}()
	// The layer that failed first is the cause; the waits it cancelled
	// report only that.
	err := ctx.Err()
	for _, p := range todo {
		if p.err != nil && (err == nil || errors.Is(err, context.Canceled) && !errors.Is(p.err, context.Canceled)) {
			err = p.err
		}
	}
	if err != nil {
		return nil, err
	}
	lacked := make([]v1.Hash, 0, len(todo))
	for _, p := range todo {
		stored, err := c.store(p.p, p.diffID)
		if err != nil {
			return nil, c.spaceError(err)
		}
		lacked = append(lacked, p.diffID)
		if stored {
			c.log.Info("pulled and converted", "layer", p.diffID.String(), "size", p.p.size, "took", p.p.took.Round(time.Millisecond))
		}
	}
	if len(todo) > 0 {
		return lacked, syncDir(c.path(layersDir))
	}
	return nil, nil
}

// pullLayer is one layer's shared pull: it fetches the layer and converts
// it into a temp file, in ctx, the pull's own context. A layer stored since
// its Acquires looked it up is not pulled again. Its errors name the source
// it pulled from.
func (c *Cache) pullLayer(ctx context.Context, diffID v1.Hash, from fetchLayer) (pl *pulledLayer, err error) {
	defer func() {
		if err != nil {
			err = &pullError{source: from.source, err: err}
		}
	}()
	if _, err := os.Stat(c.layerPath(diffID)); err == nil {
		return &pulledLayer{stored: true}, nil
	}
	// A shared pull takes one place, whatever its number of waiters.
	select {
	case c.pulls <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.pulls }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	begin := time.Now()
	l, err := from.fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("layer %s: %w", diffID, err)
	}
	tmp, err := c.convertLayer(l, diffID)
	if err != nil {
		return nil, err
	}
	took := time.Since(begin)
	fi, err := os.Stat(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	return &pulledLayer{tmp: tmp, size: fi.Size(), took: took}, nil
}

// store renames p's complete temp file into place as the layer diffID,
// unless an Acquire sharing p stored it first, and reports whether it did.
// It never replaces a file already there: a layer stored first is the same
// content, and VMs may already link it.
//
// The cache's filesystem must support renameat2's RENAME_NOREPLACE
// (docs/development.md); one that doesn't fails the Acquire, naming the
// cache directory.
func (c *Cache) store(p *pulledLayer, diffID v1.Hash) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stored {
		return false, nil
	}
	err := c.rename(p.tmp, c.layerPath(diffID))
	switch {
	case err == nil:
	case errors.Is(err, unix.EEXIST):
		if err := os.Remove(p.tmp); err != nil {
			return false, err
		}
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.EOPNOTSUPP):
		return false, fmt.Errorf("the image cache directory %s is on a filesystem without rename's RENAME_NOREPLACE, which the image cache needs: %w", c.dir, err)
	default:
		return false, err
	}
	p.stored = true
	return err == nil, nil
}

// renameNoReplace renames oldpath to newpath unless newpath exists.
func renameNoReplace(oldpath, newpath string) error {
	return unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
}

// link makes the VM's layer link directory: one hard link per layer of its
// image, named by its position, bottom layer first, since an image may hold
// one layer twice. Each layer's mtime is set as it is linked, so its last use
// is recorded. On error the directory is deleted; a layer missing from the
// cache is a *missingLayerError. It holds c.storeMu, so no eviction runs
// while it links.
func (c *Cache) link(req Request, diffIDs []v1.Hash) (dir string, names []string, err error) {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
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
		// A layer missing here was deleted from under the cache:
		// eviction skips pinned layers.
		if err := os.Chtimes(src, now, now); errors.Is(err, fs.ErrNotExist) {
			return "", nil, &missingLayerError{diffID: d, err: err}
		} else if err != nil {
			return "", nil, fmt.Errorf("layer %s: %w", d, err)
		}
		n := strconv.Itoa(i) + ".erofs"
		if err := os.Link(src, filepath.Join(vmDir, n)); errors.Is(err, fs.ErrNotExist) {
			return "", nil, &missingLayerError{diffID: d, err: err}
		} else if err != nil {
			return "", nil, fmt.Errorf("layer %s: %w", d, err)
		}
		names = append(names, n)
	}
	return vmDir, names, nil
}

// useConfig marks the image config cached under digest as in use by job,
// until job's Release.
func (c *Cache) useConfig(job string, digest v1.Hash) {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	c.jobConfigs[job] = append(c.jobConfigs[job], digest)
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
// a link directory after it is deleted, and ends job's use of its image
// configs. A job with none is not an error. Then it evicts: its layers and
// configs may no longer be in use. An eviction's errors
// are logged, not returned: they are no job's.
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
	err := os.RemoveAll(c.path(jobsDir, job))
	c.storeMu.Lock()
	delete(c.jobConfigs, job)
	c.storeMu.Unlock()
	c.evict(false)
	return err
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
