// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/zip"
	"errors"
	"fmt"
	"github.com/bodgit/sevenzip"
	"io"
	"os"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/rar"
)

// Open reads the archive at path, deciding what it is from its bytes, and
// returns it as a filesystem together with the format it turned out to be.
//
// A multi-volume set is followed from here: the rar driver resolves the rest of
// the set by volume NUMBER within path's directory, so files carrying an id a
// download inserted are still reached and nothing is renamed.
func Open(path string) (filesystem.Filesystem, Format, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, FormatUnknown, err
	}
	format, err := Sniff(f)
	closeErr := f.Close()
	if err != nil {
		return nil, format, fmt.Errorf("%s: %w", path, err)
	}
	if closeErr != nil {
		return nil, format, closeErr
	}
	switch format {
	case FormatZIP:
		// archive/zip answers io/fs.FS, so the driver is an adaptation rather
		// than a decoder: see FromFS. The reader holds the file open, so the
		// closer goes with it.
		zr, err := zip.OpenReader(path)
		if err != nil {
			return nil, format, fmt.Errorf("%s: %w", path, err)
		}
		return &closerFS{Filesystem: FromFS(zr), closer: zr}, format, nil
	case Format7z:
		// sevenzip answers io/fs.FS as well, so the same adapter serves it. It
		// also follows its OWN multi-volume chain when the name ends in .001 --
		// and derives the next one by name, so the trap the rar driver exists to
		// avoid is here too: text inserted before the extension breaks it. That
		// is named in the README rather than papered over, because a reader that
		// stops at volume one is how a file comes out the right length and wrong.
		zr, err := sevenzip.OpenReader(path)
		if err != nil {
			return nil, format, fmt.Errorf("%s: %w", path, err)
		}
		return &closerFS{Filesystem: FromFS(zr), closer: zr}, format, nil
	case FormatTar:
		// A tar's entries are whole, uncompressed and contiguous, so an index
		// buys REAL random access -- see openTar. The file stays open behind it.
		f, err := os.Open(path)
		if err != nil {
			return nil, format, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, format, err
		}
		fsys, err := openTar(f, st.Size(), f)
		if err != nil {
			f.Close()
			return nil, format, fmt.Errorf("%s: %w", path, err)
		}
		return fsys, format, nil
	case FormatRAR:
		fsys, err := rar.Open(path)
		if err != nil {
			return nil, format, fmt.Errorf("%s: %w", path, err)
		}
		return fsys, format, nil
	default:
		// Recognised and not read yet, which is a different sentence from "I do
		// not know what this is" and sends a person somewhere else.
		return nil, format, fmt.Errorf("%s: %s: %w", path, format, ErrNotImplemented)
	}
}

// closerFS carries a decoder's own Close alongside the adapted filesystem: the
// adapter has nothing to close, and whatever opened the file does.
//
// ⛔ It forwards OpenFile BY HAND, and that is not decoration. Embedding a
// filesystem.Filesystem narrows the value to that interface, and every optional
// capability the interface does not name -- Opener first among them -- stops
// being reachable through the wrapper. Extract asks for Opener and got "this
// filesystem hands out no file handles" from a filesystem that hands them out
// perfectly well. A capability lost this way says nothing: the type assertion
// simply fails.
type closerFS struct {
	filesystem.Filesystem
	closer io.Closer
}

// OpenFile keeps the Opener capability reachable through the wrapper.
func (c *closerFS) OpenFile(p string) (filesystem.File, error) {
	o, ok := c.Filesystem.(filesystem.Opener)
	if !ok {
		return nil, errors.New("unarchive: the wrapped filesystem hands out no file handles")
	}
	return o.OpenFile(p)
}

func (c *closerFS) Close() error {
	if err := c.Filesystem.Close(); err != nil {
		return err
	}
	return c.closer.Close()
}
