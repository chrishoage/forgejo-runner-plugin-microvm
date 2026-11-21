// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/continuity/tarconv"
	erofs "github.com/erofs/go-erofs"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// HardLinkError is a layer that hard-links a file from an earlier layer,
// which the image cache does not support (IMG-10): each layer is its own
// EROFS file, and a hard link can't cross two of them.
type HardLinkError struct {
	// Layer is the layer's diffID.
	Layer string
	// Err is tarconv's error, which names the link.
	Err error
}

func (e *HardLinkError) Error() string {
	return fmt.Sprintf("layer %s hard-links a file from an earlier layer, which the image cache does not support: %v", e.Layer, e.Err)
}

func (e *HardLinkError) Unwrap() error { return e.Err }

// tarconvHardLink is the start of the text tarconv's error has when a hard
// link's target never appears in the layer. tarconv has no typed error for
// it, so the text is matched; a test pins it, so a tarconv upgrade that
// changes the wording fails CI.
const tarconvHardLink = "unresolved hard link "

// convertFunc writes the EROFS file of the uncompressed layer tar in r to
// out. tmpDir holds its scratch files.
type convertFunc func(out io.Writer, r io.Reader, tmpDir string) error

// convert is the real conversion: tarconv.Apply into go-erofs's writer,
// uncompressed. OCI whiteouts and opaque directories become overlay
// whiteouts and opaque markers (IMG-4).
func convert(out io.Writer, r io.Reader, tmpDir string) error {
	fw := &failWriter{w: out}
	w := erofs.Create(fw, erofs.WithTempDir(tmpDir))
	if err := tarconv.Apply(w, r); err != nil {
		// Close releases the writer's spool file; with fw failing, it
		// writes nothing.
		fw.fail = true
		_ = w.Close()
		return err
	}
	return w.Close()
}

// failWriter passes writes to w until fail is set.
type failWriter struct {
	w    io.Writer
	fail bool
}

var errAbandoned = errors.New("abandoned")

func (f *failWriter) Write(p []byte) (int, error) {
	if f.fail {
		return 0, errAbandoned
	}
	return f.w.Write(p)
}

// convertLayer converts l into a temp file in the cache's tmp directory and
// returns its path. The layer's uncompressed content must hash to diffID,
// since the file is cached under that name and shared by every image that
// names it, and must stay within c.limits. The blob is decompressed in this
// goroutine, so a panic in decompression or conversion fails only this layer
// (IMG-11). On error nothing is left in tmp.
func (c *Cache) convertLayer(l v1.Layer, diffID v1.Hash) (path string, err error) {
	tmpDir := c.path(tmpDir)
	f, err := os.CreateTemp(tmpDir, diffID.Hex+"-*.erofs")
	if err != nil {
		return "", c.spaceError(err)
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("layer %s: the conversion panicked: %v", diffID, p)
		}
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			err = c.spaceError(err)
		}
	}()
	blob, err := l.Compressed()
	if err != nil {
		return "", fmt.Errorf("layer %s: %w", diffID, err)
	}
	defer func() { _ = blob.Close() }()
	rc, err := c.decompress(blob)
	if err != nil {
		return "", fmt.Errorf("layer %s: %w", diffID, err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	tc := countTar(diffID, c.limits)
	// Deferred too, for a panic: the counter's goroutine always ends.
	defer func() { _ = tc.close() }()
	src := io.TeeReader(rc, io.MultiWriter(h, tc))
	if err := c.convert(f, src, tmpDir); err != nil {
		// A layer over a limit fails tarconv's read with that limit;
		// the limit is the cause.
		if lerr := tc.close(); lerr != nil {
			return "", lerr
		}
		if strings.Contains(err.Error(), tarconvHardLink) {
			return "", &HardLinkError{Layer: diffID.String(), Err: err}
		}
		return "", fmt.Errorf("layer %s: %w", diffID, err)
	}
	// tarconv reads the stream to its end; whatever it left counts too.
	if _, err := io.Copy(io.Discard, src); err != nil {
		return "", errors.Join(tc.close(), fmt.Errorf("layer %s: %w", diffID, err))
	}
	if err := tc.close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != diffID.Hex {
		return "", fmt.Errorf("layer %s: its content has the digest sha256:%s", diffID, got)
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// spaceError names the cache directory when err is a full disk. Nothing
// evicts in an emergency: the size cap is the operator's tool.
func (c *Cache) spaceError(err error) error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return fmt.Errorf("the image cache directory %s is out of space: %w", c.dir, err)
	}
	return err
}

// layerPath is where the layer with diffID is cached.
func (c *Cache) layerPath(diffID v1.Hash) string {
	return filepath.Join(c.dir, layersDir, diffID.Hex+".erofs")
}

// checkDiffID refuses a diffID that can't name a cached layer. It comes from
// the image config, which is untrusted, and it becomes a file name.
func checkDiffID(h v1.Hash) error {
	if h.Algorithm != "sha256" || len(h.Hex) != 64 || strings.Trim(h.Hex, "0123456789abcdef") != "" {
		return fmt.Errorf("layer diffID %q: not a sha256 digest", h)
	}
	return nil
}
