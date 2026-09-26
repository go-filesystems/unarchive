// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"fmt"
	"io"

	"github.com/go-filesystems/cab"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/rpm"
	"github.com/go-filesystems/warc"
	"github.com/go-filesystems/xar"
)

// ReaderArchive says whether a format's driver reads it in place through an
// io.ReaderAt, the way an image is read, while being an ARCHIVE and not a
// filesystem.
//
// The distinction is not academic. Both kinds are opened from the handle Open
// already has and neither is spooled, so they look alike from here -- but they
// differ about who CLOSES that handle, and getting it the wrong way round fails
// in opposite directions.
func (f Format) ReaderArchive() bool {
	switch f {
	case FormatXAR, FormatCAB, FormatRPM, FormatWARC:
		return true
	}
	return false
}

// openReaderArchive opens one of those, and wraps it so the handle is closed.
//
// ⛔ The opposite of openImage, and the difference is measured rather than
// assumed: iso9660 and squashfs forward Close to the io.ReaderAt they were
// given, so wrapping THEM closes the file twice. These four are
//
//	func (f *FS) Close() error { return nil }
//
// every one of them, so NOT wrapping them leaks the file -- once per open, in
// silence, which is the half of this pair that no error message ever mentions.
//
// Six drivers, one signature, two ownership rules. The signature does not say
// which, so each one's Close was read.
// The closer is a PARAMETER rather than the handle itself, which is what
// openContiguous already does and for the same reason: it is the only way a test
// can watch the close happen. A test that opened a real file could only assert
// that Close returned nil, which it does whether the handle was forwarded or
// dropped -- and counting the process's open descriptors is not portable (/dev/fd
// cannot be listed on macOS, so that test would skip on half the lanes and prove
// nothing on the machine it was written on).
func openReaderArchive(ra io.ReaderAt, size int64, closer io.Closer, format Format) (filesystem.Filesystem, error) {
	fsys, err := openReaderArchiveAt(ra, size, format)
	if err != nil {
		return nil, err
	}
	return &closerFS{Filesystem: fsys, closer: closer}, nil
}

// openReaderArchiveAt is openReaderArchive over any reader, for a split where
// there is no single file to hand over. The caller owns the reader.
func openReaderArchiveAt(r io.ReaderAt, size int64, format Format) (filesystem.Filesystem, error) {
	switch format {
	case FormatXAR:
		return xar.OpenReader(r, size)
	case FormatCAB:
		return cab.OpenReader(r, size)
	case FormatRPM:
		return rpm.OpenReader(r, size)
	case FormatWARC:
		return warc.OpenReader(r, size)
	}
	return nil, fmt.Errorf("%s: %w", format, ErrNotImplemented)
}
