// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"fmt"
	"io"
	iofs "io/fs"

	"github.com/go-filesystems/iso9660"
)

// isoBuilder writes an ISO 9660 image through go-filesystems/iso9660's own
// builder.
//
// ⛔ It holds every entry IN MEMORY, and that is upstream's shape rather than a
// choice made here: iso9660.Builder.AddFile takes a []byte, and WriteTo needs an
// io.WriterAt, so nothing can be streamed through it. A 700 MB image costs 700 MB
// of memory.
//
// It is wired anyway because an .iso is a distribution format people are handed
// and asked to open, and because the alternative -- not offering it while the
// driver sits in the same organisation -- is worse. The cost is stated where
// somebody will meet it rather than discovered.
type isoBuilder struct {
	b   *iso9660.Builder
	w   io.WriteSeeker
	err error
}

func newISOBuilder(w io.WriteSeeker, volumeID string) *isoBuilder {
	return &isoBuilder{b: iso9660.NewBuilder(volumeID), w: w}
}

func (i *isoBuilder) AddDir(name string, _ iofs.FileMode) error {
	if i.err != nil {
		return i.err
	}
	// ISO 9660 carries no POSIX mode without Rock Ridge, which this builder does
	// not write, so the perm is dropped rather than half-stored.
	i.err = i.b.AddDir(indexClean(name))
	return i.err
}

func (i *isoBuilder) AddFile(name string, _ iofs.FileMode, size int64, r io.Reader) error {
	if i.err != nil {
		return i.err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		i.err = fmt.Errorf("iso9660: %s: %w", name, err)
		return i.err
	}
	i.err = i.b.AddFile(indexClean(name), data)
	return i.err
}

// Close lays the image out and writes it.
//
// The image is built into memory first because WriteTo wants an io.WriterAt and
// the seal is handed an io.WriteSeeker: the two are one method apart, and
// bridging them with a buffer is honest about what is already true of this
// builder.
func (i *isoBuilder) Close() error {
	if i.err != nil {
		return i.err
	}
	var buf writerAtBuffer
	if err := i.b.WriteTo(&buf); err != nil {
		return err
	}
	_, err := i.w.Write(buf.b)
	return err
}

// writerAtBuffer is an io.WriterAt over a growing slice.
type writerAtBuffer struct{ b []byte }

func (w *writerAtBuffer) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("iso9660: negative offset %d", off)
	}
	if need := off + int64(len(p)); need > int64(len(w.b)) {
		grown := make([]byte, need)
		copy(grown, w.b)
		w.b = grown
	}
	return copy(w.b[off:], p), nil
}

var _ = bytes.MinRead
