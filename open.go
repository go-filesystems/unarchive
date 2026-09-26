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

	"github.com/go-filesystems/hfsplus"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/iso9660"
	"github.com/go-filesystems/rar"
	"github.com/go-filesystems/squashfs"
)

// Open reads the archive at path, deciding what it is from its bytes, and
// returns it as a filesystem together with the format it turned out to be.
//
// A multi-volume set is followed from here: the rar driver resolves the rest of
// the set by volume NUMBER within path's directory, so files carrying an id a
// download inserted are still reached and nothing is renamed.
func Open(path string) (filesystem.Filesystem, Format, error) {
	return openAt(path, 0)
}

// openAt is Open, carrying how many stream wrappers have already been peeled.
func openAt(path string, depth int) (filesystem.Filesystem, Format, error) {
	// A numbered set is read as ONE file, before anything looks at the bytes: the
	// bytes of part one are the beginning of a whole archive and say nothing
	// about the rest existing.
	parts, err := splitParts(path)
	if err != nil {
		return nil, FormatUnknown, err
	}
	if len(parts) > 1 {
		return openSplit(parts)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, FormatUnknown, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, FormatUnknown, err
	}
	format, err := Sniff(f, size)
	if err != nil {
		f.Close()
		return nil, format, fmt.Errorf("%s: %w", path, err)
	}
	// An image is read from the handle that is already open, so that one stays;
	// every other driver is given the path and opens its own.
	if format.Image() {
		fsys, err := openImage(f, size, format)
		if err != nil {
			f.Close()
			return nil, format, fmt.Errorf("%s: %w", path, err)
		}
		return fsys, format, nil
	}
	if err := f.Close(); err != nil {
		return nil, format, err
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
		registerZipMethods(zr.RegisterDecompressor)
		// The modes come from the zip's OWN headers, because the io/fs view
		// reports every directory as 0555 -- see FromFSWithModes.
		modes := make(map[string]os.FileMode, len(zr.File))
		for _, f := range zr.File {
			modes[f.Name] = f.Mode()
		}
		return &closerFS{Filesystem: FromFSWithModes(zr, modes), closer: zr}, format, nil
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
		modes := make(map[string]os.FileMode, len(zr.File))
		for _, f := range zr.File {
			modes[f.Name] = f.FileInfo().Mode()
		}
		return &closerFS{Filesystem: FromFSWithModes(zr, modes), closer: zr}, format, nil
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
	case FormatGzip, FormatBzip2, FormatXZ, FormatZstd, FormatLZ4, FormatZ:
		// A wrapper holds ONE stream, and what is in it is another question:
		// usually a tar, sometimes a single ordinary file, occasionally another
		// archive entirely. openCompressed peels it and asks again.
		return openCompressed(path, format, depth)
	case FormatDMG:
		// A container, not an image: peeled like a wrapper and what comes out is
		// sniffed. See openDMG.
		return openDMG(path, depth)
	case FormatAr, FormatCpio:
		// Both are contiguous and uncompressed, like tar, so they get the same
		// treatment: index once, then a read is a read. The file stays open
		// behind the index.
		f, err := os.Open(path)
		if err != nil {
			return nil, format, err
		}
		size, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			f.Close()
			return nil, format, err
		}
		fsys, err := openContiguous(f, size, f, format)
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
		// not know what this is" and sends a person somewhere else -- and for a
		// format with a Note, somewhere more precise still.
		if note := format.Note(); note != "" {
			// General sentence first, specific one after: the other order put
			// "recognised, but this format is not read yet" AFTER a paragraph
			// that had already said more, so the sentinel read like a trailing
			// afterthought.
			return nil, format, fmt.Errorf("%s: %w: %s is %s", path, ErrNotImplemented, format, note)
		}
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

// openImage hands a filesystem image to the org's driver for it.
//
// Nothing is decoded and nothing is spooled: an image already IS a filesystem, so
// the driver reads the file in place. The handle outlives this call because the
// driver adopts it -- both of these close what they were opened with.
func openImage(f *os.File, size int64, format Format) (filesystem.Filesystem, error) {
	fsys, err := openImageAt(f, size, format)
	if err != nil {
		return nil, err
	}
	// ⛔ NOT wrapped in closerFS. Both drivers take ownership of an io.ReaderAt
	// that is also an io.Closer and forward Close to it, so wrapping would close
	// the file twice -- the second one failing with "file already closed", from
	// Close, long after anything could be done about it.
	return fsys, nil
}

// openImageAt is openImage over any reader, for a split where there is no single
// file to hand over.
func openImageAt(r io.ReaderAt, size int64, format Format) (filesystem.Filesystem, error) {
	switch format {
	case FormatISO9660:
		return iso9660.OpenReader(r, size)
	case FormatSquashFS:
		return squashfs.OpenReader(r, size)
	case FormatHFSPlus:
		return hfsplus.OpenReader(r, size)
	}
	return nil, fmt.Errorf("%s: %w", format, ErrNotImplemented)
}

// openSplit reads a numbered set as one archive.
//
// Everything here is given an io.ReaderAt rather than a path, because a split has
// no single path -- which is also why RAR is not among them: this package's rar
// driver follows a volume set by NAME, deliberately, and a plain numbered split is
// a different thing it has no entry point for. Saying so beats concatenating into
// a spool the size of the whole set.
func openSplit(parts []string) (filesystem.Filesystem, Format, error) {
	j, err := openJoined(parts)
	if err != nil {
		return nil, FormatUnknown, err
	}
	format, err := Sniff(j, j.Size())
	if err != nil {
		j.Close()
		return nil, format, fmt.Errorf("%s (%d parts): %w", parts[0], len(parts), err)
	}
	fail := func(err error) (filesystem.Filesystem, Format, error) {
		j.Close()
		return nil, format, fmt.Errorf("%s (%d parts): %w", parts[0], len(parts), err)
	}
	switch format {
	case FormatZIP:
		zr, err := zip.NewReader(j, j.Size())
		if err != nil {
			return fail(err)
		}
		registerZipMethods(zr.RegisterDecompressor)
		modes := make(map[string]os.FileMode, len(zr.File))
		for _, zf := range zr.File {
			modes[zf.Name] = zf.Mode()
		}
		return &closerFS{Filesystem: FromFSWithModes(zr, modes), closer: j}, format, nil
	case Format7z:
		zr, err := sevenzip.NewReader(j, j.Size())
		if err != nil {
			return fail(err)
		}
		modes := make(map[string]os.FileMode, len(zr.File))
		for _, zf := range zr.File {
			modes[zf.Name] = zf.FileInfo().Mode()
		}
		return &closerFS{Filesystem: FromFSWithModes(zr, modes), closer: j}, format, nil
	case FormatTar, FormatAr, FormatCpio:
		fsys, err := openContiguous(j, j.Size(), j, format)
		if err != nil {
			return fail(err)
		}
		return fsys, format, nil
	}
	// Asked as a QUESTION rather than listed again. This used to name iso9660
	// and squashfs a second time, so Image() and this switch were two doors to
	// one semantics: adding a driver to one left a split of it falling through
	// to "not implemented", which is a wrong answer rather than a missing one.
	if format.Image() {
		fsys, err := openImageAt(j, j.Size(), format)
		if err != nil {
			return fail(err)
		}
		return &closerFS{Filesystem: fsys, closer: j}, format, nil
	}
	return fail(fmt.Errorf("%s across %d parts: %w", format, len(parts), ErrNotImplemented))
}

// openContiguous indexes one of the formats whose entries sit whole and
// uncompressed in the file.
func openContiguous(ra io.ReaderAt, size int64, closer io.Closer, format Format) (filesystem.Filesystem, error) {
	switch format {
	case FormatAr:
		return openAr(ra, size, closer)
	case FormatCpio:
		return openCpio(ra, size, closer)
	case FormatTar:
		return openTar(ra, size, closer)
	}
	return nil, fmt.Errorf("%s: %w", format, ErrNotImplemented)
}
