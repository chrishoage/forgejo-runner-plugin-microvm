// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
)

// decompressFunc returns the tar stream of a layer's blob r, decoded in the
// goroutine that reads it.
type decompressFunc func(r io.Reader) (io.ReadCloser, error)

var (
	gzipMagic = []byte{0x1f, 0x8b}
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// decompress is the real decompression: gzip, zstd or none, told apart by
// the blob's first bytes as go-containerregistry does, since a registry's
// media type can't be trusted to match. go-containerregistry's own
// Uncompressed decodes zstd in the library's goroutines, where a panic would
// end the plugin; here zstd runs with a concurrency of 1, which decodes in
// the reader's goroutine, so convertLayer recovers a panic (IMG-11).
func decompress(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(len(zstdMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	switch {
	case bytes.HasPrefix(magic, gzipMagic):
		return gzip.NewReader(br)
	case bytes.HasPrefix(magic, zstdMagic):
		zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		return zr.IOReadCloser(), nil
	}
	return io.NopCloser(br), nil
}
