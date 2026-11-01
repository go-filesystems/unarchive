// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/klauspost/compress/zstd"
)

// ErrTooLarge is returned when an archive asks for more than a limit allows: more
// entries, longer paths, a larger dictionary, a bigger spool, a larger ReadFile
// than Limits (or, for Extract, Options) permit. It is raised before the memory
// or the disk is spent. The archive is not necessarily corrupt: raise the limit if
// the size is expected.
var ErrTooLarge = errors.New("unarchive: exceeds the size limit")

// Limits bounds what an untrusted archive may make Open, ReadFile and the stream
// wrappers reserve. A field that is zero or negative keeps its default.
type Limits struct {
	// MaxEntries bounds the paths an index holds (tar, ar, cpio), counting the
	// directories an archive implies.
	MaxEntries int

	// MaxReadFile bounds what ReadFile returns, in bytes. Extract and OpenFile
	// stream and are not affected.
	MaxReadFile int64

	// MaxSpool bounds what a stream wrapper (.gz, .xz, .zst, ...) may decompress
	// to the temporary file it is spooled to.
	MaxSpool int64

	// MaxDictionary bounds the dictionary or window a decoder may reserve: the
	// LZMA2 or LZMA dictionary of an .xz stream or a zip entry (the window grows
	// with the data decoded and ErrTooLarge is returned before it would pass this),
	// a zstd window (refused from the frame header).
	MaxDictionary int64
}

// DefaultLimits is what Open and the readers behind it apply. It is process
// wide: set its fields once at start-up, before opening anything; it is read,
// never written, by this package.
//
// The defaults sit above anything an honest archive needs and below what a few
// dozen hostile bytes can ask for: a 9-byte LZMA header asked for a 1 GiB
// dictionary and a 218-byte zip for a 4 GiB ReadFile.
var DefaultLimits = Limits{
	MaxEntries:    1 << 20,
	MaxReadFile:   1 << 30,
	MaxSpool:      64 << 30,
	MaxDictionary: 256 << 20,
}

// resolved is DefaultLimits with a nonsensical field replaced by the built-in
// default, so that a caller zeroing a field cannot switch a limit off by accident.
func (l Limits) resolved() Limits {
	d := Limits{MaxEntries: 1 << 20, MaxReadFile: 1 << 30, MaxSpool: 64 << 30, MaxDictionary: 256 << 20}
	if l.MaxEntries > 0 {
		d.MaxEntries = l.MaxEntries
	}
	if l.MaxReadFile > 0 {
		d.MaxReadFile = l.MaxReadFile
	}
	if l.MaxSpool > 0 {
		d.MaxSpool = l.MaxSpool
	}
	if l.MaxDictionary > 0 {
		d.MaxDictionary = l.MaxDictionary
	}
	return d
}

// zstdWindowLimit caps the window a zstd frame may ask the decoder to reserve.
// klauspost's own default is 512 MiB (decoder_options.go: MaxWindowSize); zstd
// itself allows far more.
func zstdWindowLimit() zstd.DOption {
	return zstd.WithDecoderMaxWindow(uint64(DefaultLimits.resolved().MaxDictionary))
}

// readHint is how much readBounded reserves before the data has earned it: the
// size an archive declares is a claim.
const readHint = 8 << 20

// readBounded reads an entry that declares size bytes into memory, refusing one
// over max before reserving anything and growing the buffer from what r yields.
// The declared size reached make() unchecked here, in ReadFile for tar and for
// every io/fs archive (zip, 7z): a 218-byte zip declared 4 GiB and got it.
func readBounded(r io.Reader, size, max int64, name string) ([]byte, error) {
	if size > max || size > math.MaxInt {
		return nil, fmt.Errorf("%s declares %d bytes, ReadFile's limit is %d (Extract and OpenFile stream): %w",
			name, size, max, ErrTooLarge)
	}
	var buf bytes.Buffer
	buf.Grow(int(min(max64(size, 0), readHint)) + bytes.MinRead)
	// One byte past the limit tells an entry whose declaration was a lie.
	n, err := io.Copy(&buf, io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, fmt.Errorf("%s yields more than %d bytes, ReadFile's limit: %w", name, max, ErrTooLarge)
	}
	return buf.Bytes(), nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

const (
	// maxEntryPath is the longest path an index accepts: Linux's PATH_MAX. A longer
	// path cannot be extracted anywhere.
	maxEntryPath = 4096

	// maxIndexPathBytes bounds all the paths of one index together.
	maxIndexPathBytes = 256 << 20
)

// indexBudget is what an index may still spend. Every path an index stores is
// charged to it first: a name of 1 MB made of "a/" synthesises 500,000 parent
// directories, each its own string, which is a quarter of a terabyte.
type indexBudget struct {
	entries      int
	pathBytes    int
	maxEntries   int
	maxPathBytes int
}

func newIndexBudget() *indexBudget {
	return &indexBudget{
		maxEntries:   DefaultLimits.resolved().MaxEntries,
		maxPathBytes: maxIndexPathBytes,
	}
}

// charge accounts for one new key of an index.
func (b *indexBudget) charge(p string) error {
	if len(p) > maxEntryPath {
		return fmt.Errorf("a path of %d bytes, the limit is %d: %w", len(p), maxEntryPath, ErrTooLarge)
	}
	if b.entries >= b.maxEntries {
		return fmt.Errorf("more than %d entries and directories: %w", b.maxEntries, ErrTooLarge)
	}
	if b.pathBytes+len(p) > b.maxPathBytes {
		return fmt.Errorf("paths together exceed %d bytes: %w", b.maxPathBytes, ErrTooLarge)
	}
	b.entries++
	b.pathBytes += len(p)
	return nil
}
