// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	dotz "github.com/go-compressions/compress"
	"github.com/go-compressions/lzip"
	"github.com/go-compressions/lzo"
	filesystem "github.com/go-filesystems/interface"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

// maxNesting is how many stream wrappers Open will peel.
//
// A wrapper holds one stream and that stream may be another wrapper, so there
// is no natural end: a few hundred bytes of .gz nested on itself expands into
// however much spool the machine has. Four is past anything real -- .tar.gz is
// one, and nobody ships a .tar.gz.xz -- and it fails with a sentence naming the
// limit rather than filling a disk.
const maxNesting = 4

// ErrTooDeeplyNested is returned when a file is wrapped more times than
// maxNesting.
var ErrTooDeeplyNested = errors.New("unarchive: more stream wrappers than this will peel")

// decompressor is the reader a wrapper format is read through. Every one of
// these is pure Go, and every one was already in this module's dependency graph
// before it was imported here.
func decompressor(f Format, r io.Reader) (io.Reader, func() error, error) {
	nothing := func() error { return nil }
	switch f {
	case FormatGzip:
		z, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return z, z.Close, nil
	case FormatBzip2:
		// The standard library decompresses bzip2 and does not compress it,
		// which is all that is wanted here.
		return bzip2.NewReader(r), nothing, nil
	case FormatXZ:
		z, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return z, nothing, nil
	case FormatZstd:
		z, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return z, func() error { z.Close(); return nil }, nil
	case FormatLZ4:
		return lz4.NewReader(r), nothing, nil
	case FormatLZO, FormatLzip:
		// Both refuse at CONSTRUCTION like .Z does, reading their header there,
		// so a stream that is not one is caught here rather than at the first
		// Read. Neither has a Close of its own: the spool's file is the only
		// thing holding anything, and openCompressed closes that.
		newReader := lzo.NewReader
		if f == FormatLzip {
			newReader = lzip.NewReader
		}
		z, err := newReader(r)
		if err != nil {
			return nil, nil, err
		}
		return z, nothing, nil
	case FormatZ:
		// The only wrapper whose reader refuses at CONSTRUCTION: it reads the
		// three-byte header there, so a stream that is not .Z is caught here
		// rather than at the first Read.
		z, err := dotz.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return z, nothing, nil
	}
	return nil, nil, fmt.Errorf("%s: %w", f, ErrUnknownFormat)
}

// wrapperSuffixes map a compressed name to the name of what is inside it.
//
// The name is not how the format was decided -- the bytes did that -- but it is
// the only thing that says what the single file inside should be CALLED, and a
// person who gunzips notes.txt.gz expects notes.txt. Where the name says
// nothing, the archive's own entries do, so this matters only for the
// single-file case.
var wrapperSuffixes = []struct{ from, to string }{
	{".tar.gz", ".tar"}, {".tgz", ".tar"},
	{".tar.bz2", ".tar"}, {".tbz2", ".tar"}, {".tbz", ".tar"},
	{".tar.xz", ".tar"}, {".txz", ".tar"},
	{".tar.zst", ".tar"}, {".tzst", ".tar"},
	{".tar.lz4", ".tar"},
	{".tar.z", ".tar"}, {".taz", ".tar"},
	{".tar.lzo", ".tar"}, {".tzo", ".tar"},
	{".tar.lz", ".tar"},
	{".gz", ""}, {".bz2", ""}, {".xz", ""}, {".zst", ""}, {".lz4", ""},
	{".lzo", ""},
	// ⛔ .lz is lzip, NOT lzip's cousin .lzma and not lzop's .lzo. The three are
	// different framings of two different algorithms and the suffixes are one
	// letter apart.
	{".lz", ""},
	// Lower-cased before matching, so this catches the .Z everybody writes.
	{".z", ""},
}

// InnerName is what the stream inside a compressed wrapper is called: the base
// name with the wrapper's suffix taken off, so notes.txt.gz gives notes.txt and
// archive.tgz gives archive.tar.
//
// It is exported because the command needs the same answer for a different
// question -- what to call the directory it extracts into -- and computing it
// from filepath.Ext there gives "fixture.tar" for fixture.tar.gz, which is a
// directory named after half a suffix.
func InnerName(name string) string { return innerName(name) }

// innerName is what the stream inside a wrapper is called.
func innerName(name string) string {
	base := filepath.Base(name)
	lower := strings.ToLower(base)
	for _, s := range wrapperSuffixes {
		if strings.HasSuffix(lower, s.from) {
			return base[:len(base)-len(s.from)] + s.to
		}
	}
	// A wrapper whose name claims nothing. The stream still has to be called
	// something, and reusing the whole name is better than inventing one: it is
	// what the person typed.
	return base
}

// openCompressed peels one stream wrapper and opens what was inside it.
//
// The stream is spooled to a file first, and that is the format's doing rather
// than a shortcut. Every archive here needs RANDOM access -- a tar index, a zip
// central directory, a 7z header at the end -- and a decompressor gives a
// one-way stream. Reading it twice means decompressing it twice; holding it in
// memory means holding a whole archive in memory. So it goes to a temporary
// file, which is removed when the filesystem is closed.
//
// The spool is then SNIFFED, not assumed. A .gz usually holds a tar and may
// hold anything, including a single ordinary file, and deciding from ".tar.gz"
// would be the extension deciding again.
func openCompressed(path string, format Format, depth int) (filesystem.Filesystem, Format, error) {
	if depth >= maxNesting {
		return nil, format, fmt.Errorf("%s: %d wrappers deep: %w", path, depth, ErrTooDeeplyNested)
	}

	src, err := os.Open(path)
	if err != nil {
		return nil, format, err
	}
	defer src.Close()

	zr, closeZ, err := decompressor(format, src)
	if err != nil {
		return nil, format, fmt.Errorf("%s: %w", path, err)
	}

	// A directory rather than a bare temporary file, so that the stream can keep
	// the name it should have: for a single compressed file that name is the
	// only thing the archive has, and os.DirFS over the directory is then a
	// one-entry filesystem with the right entry in it.
	dir, err := os.MkdirTemp("", "unarchive-spool-")
	if err != nil {
		return nil, format, err
	}
	spool := filepath.Join(dir, innerName(path))
	out, err := os.Create(spool)
	if err != nil {
		os.RemoveAll(dir)
		return nil, format, err
	}
	_, copyErr := io.Copy(out, zr)
	closeErr := out.Close()
	if err := closeZ(); err != nil && copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		os.RemoveAll(dir)
		return nil, format, fmt.Errorf("%s: %w", path, copyErr)
	}
	if closeErr != nil {
		os.RemoveAll(dir)
		return nil, format, closeErr
	}

	inner, innerFormat, err := openAt(spool, depth+1)
	if err != nil {
		if !errors.Is(err, ErrUnknownFormat) {
			os.RemoveAll(dir)
			return nil, format, err
		}
		// Not an archive: a single compressed file, which gunzip would have left
		// beside it. One entry, named by stripping the wrapper's suffix.
		return &spoolFS{Filesystem: FromFS(os.DirFS(dir)), dir: dir}, format, nil
	}
	// The format reported is the WRAPPER's, because that is what the caller
	// handed over. What was inside is the answer to a different question, and
	// saying "tar" for a file called .tar.gz would make a correct answer read
	// like a mistake.
	_ = innerFormat
	return &spoolFS{Filesystem: inner, dir: dir}, format, nil
}

// spoolFS removes the spool when the filesystem is closed.
//
// ⛔ It forwards OpenFile BY HAND, for the reason closerFS gives: embedding a
// filesystem.Filesystem narrows the value to that interface, and every optional
// capability -- Opener first -- stops being reachable through the wrapper. The
// assertion then simply fails, and a filesystem that hands out file handles
// perfectly well reports that it does not.
type spoolFS struct {
	filesystem.Filesystem
	dir string
}

// OpenFile keeps the Opener capability reachable through the wrapper.
func (s *spoolFS) OpenFile(p string) (filesystem.File, error) {
	o, ok := s.Filesystem.(filesystem.Opener)
	if !ok {
		return nil, errors.New("unarchive: the wrapped filesystem hands out no file handles")
	}
	return o.OpenFile(p)
}

func (s *spoolFS) Close() error {
	err := s.Filesystem.Close()
	// The spool goes whether or not the close succeeded: leaving it would leak a
	// whole decompressed archive into the temporary directory, and the caller
	// has no handle on it to clean up with.
	if rmErr := os.RemoveAll(s.dir); err == nil {
		err = rmErr
	}
	return err
}
