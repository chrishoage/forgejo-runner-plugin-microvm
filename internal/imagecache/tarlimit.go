// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"archive/tar"
	"fmt"
	"io"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// The bounds on one layer's tar stream (docs/architecture.md, Image cache).
// go-erofs keeps every entry's metadata in memory until the layer's file is
// written, and a layer is untrusted: a small one could hold millions of
// entries and exhaust the plugin, ending every job.
const (
	// maxEntries is the most entries a layer may hold: the largest real
	// images hold a few hundred thousand files across all their layers.
	maxEntries = 1_000_000
	// maxMetadata is the most bytes of names, link targets and PAX records
	// (xattrs among them) a layer may hold: maxEntries of them at 256 bytes
	// each, where real paths average well under 100.
	maxMetadata = 256 << 20
)

// tarLimits bounds one layer's tar stream.
type tarLimits struct {
	entries  int
	metadata int64
}

// tarCounter reads a copy of a layer's tar stream, written to it, and fails
// once the stream exceeds its limits: its next Write returns the limit's
// error, so the conversion reading the stream fails with it.
type tarCounter struct {
	pw   *io.PipeWriter
	done chan struct{}
	// err is the limit exceeded, set before done is closed.
	err  error
	once sync.Once
}

// countTar starts counting the tar stream of the layer diffID.
func countTar(diffID v1.Hash, lim tarLimits) *tarCounter {
	pr, pw := io.Pipe()
	t := &tarCounter{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		if t.err = count(pr, diffID, lim); t.err != nil {
			_ = pr.CloseWithError(t.err)
			return
		}
		// What follows the archive's end, or all of it once the headers
		// can't be read, is dropped, so writes never block.
		_, _ = io.Copy(io.Discard, pr)
	}()
	return t
}

func (t *tarCounter) Write(p []byte) (int, error) { return t.pw.Write(p) }

// close ends the stream, waits for the count and returns the limit it found
// exceeded, if any. It may be called more than once.
func (t *tarCounter) close() error {
	t.once.Do(func() { _ = t.pw.Close() })
	<-t.done
	return t.err
}

// count reads r's tar headers until the archive ends or a limit is exceeded.
// A stream that isn't a valid tar is not its error: tarconv parses the same
// bytes with the same archive/tar, stops at the same place, and reports it.
func count(r io.Reader, diffID v1.Hash, lim tarLimits) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("layer %s: reading its tar headers panicked: %v", diffID, p)
		}
	}()
	tr := tar.NewReader(r)
	var entries int
	var metadata int64
	for {
		hdr, err := tr.Next()
		if err != nil {
			return nil
		}
		if entries++; entries > lim.entries {
			return fmt.Errorf("layer %s has more than %d entries, the most the image cache converts in one layer", diffID, lim.entries)
		}
		metadata += int64(len(hdr.Name) + len(hdr.Linkname))
		for k, v := range hdr.PAXRecords {
			metadata += int64(len(k) + len(v))
		}
		if metadata > lim.metadata {
			return fmt.Errorf("layer %s has more than %d bytes of names, link targets and PAX records, the most the image cache converts in one layer", diffID, lim.metadata)
		}
	}
}
