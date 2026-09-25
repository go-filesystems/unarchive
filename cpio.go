// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"fmt"
	"io"
	iofs "io/fs"
	"strconv"
	"strings"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// The cpio variants this reads, by the six bytes each begins its headers with.
//
// All three are ASCII, which is what makes them readable without knowing the
// machine that wrote them. The fourth variant -- "old binary" -- stores a 16-bit
// magic in the writer's own byte order, so the same file means different things on
// different machines; it is recognised and refused rather than guessed at.
const (
	cpioNewc    = "070701" // SVR4, the initramfs one: fields are 8 hex digits
	cpioCRC     = "070702" // the same, with a checksum field that is filled in
	cpioODC     = "070707" // POSIX.1 "odc": fields are 6 octal digits
	cpioTrailer = "TRAILER!!!"
)

// openCpio indexes a cpio archive.
//
// cpio is what an initramfs is, and what an RPM carries inside it. Unlike tar it
// has no padding to a block size in the ASCII variants -- except newc, which pads
// both the name and the data to four bytes, and forgetting either puts every
// subsequent header one to three bytes out.
func openCpio(ra io.ReaderAt, size int64, closer io.Closer) (filesystem.Filesystem, error) {
	var recs []record
	off := int64(0)
	for off+6 <= size {
		magic := make([]byte, 6)
		if _, err := ra.ReadAt(magic, off); err != nil {
			return nil, fmt.Errorf("cpio: header at %d: %w", off, err)
		}
		switch string(magic) {
		case cpioNewc, cpioCRC:
			r, next, err := cpioReadNewc(ra, size, off)
			if err != nil {
				return nil, err
			}
			if r == nil {
				return cpioDone(ra, recs, closer)
			}
			recs = append(recs, *r)
			off = next
		case cpioODC:
			r, next, err := cpioReadODC(ra, size, off)
			if err != nil {
				return nil, err
			}
			if r == nil {
				return cpioDone(ra, recs, closer)
			}
			recs = append(recs, *r)
			off = next
		default:
			if off == 0 {
				return nil, fmt.Errorf("cpio: %q is not an ASCII cpio magic "+
					"(old binary cpio is byte-order dependent and not read here): %w",
					magic, ErrNotImplemented)
			}
			// Trailing padding after the trailer is normal: an initramfs is padded
			// to a block. Anything else here is a malformed archive, and stopping
			// with what was read beats refusing the whole thing.
			return cpioDone(ra, recs, closer)
		}
	}
	return cpioDone(ra, recs, closer)
}

func cpioDone(ra io.ReaderAt, recs []record, closer io.Closer) (filesystem.Filesystem, error) {
	if len(recs) == 0 {
		return nil, fmt.Errorf("cpio: no entries: %w", ErrUnknownFormat)
	}
	return &closerFS{Filesystem: FromFS(newIndexFS(ra, recs)), closer: nopCloser(closer)}, nil
}

// cpioReadNewc reads one SVR4 header. A nil record means the trailer was reached.
func cpioReadNewc(ra io.ReaderAt, size, off int64) (*record, int64, error) {
	const headerLen = 110
	if off+headerLen > size {
		return nil, 0, fmt.Errorf("cpio: header at %d runs past the end: %w", off, ErrIncomplete)
	}
	h := make([]byte, headerLen)
	if _, err := ra.ReadAt(h, off); err != nil {
		return nil, 0, err
	}
	field := func(i int) (int64, error) {
		return strconv.ParseInt(string(h[6+i*8:6+i*8+8]), 16, 64)
	}
	mode, err := field(1)
	if err != nil {
		return nil, 0, fmt.Errorf("cpio: mode at %d: %w", off, err)
	}
	mtime, _ := field(5)
	fileSize, err := field(6)
	if err != nil {
		return nil, 0, fmt.Errorf("cpio: size at %d: %w", off, err)
	}
	nameSize, err := field(11)
	if err != nil {
		return nil, 0, fmt.Errorf("cpio: name size at %d: %w", off, err)
	}
	if nameSize <= 0 || off+headerLen+nameSize > size {
		return nil, 0, fmt.Errorf("cpio: name at %d is %d bytes: %w", off, nameSize, ErrIncomplete)
	}
	nameBuf := make([]byte, nameSize)
	if _, err := ra.ReadAt(nameBuf, off+headerLen); err != nil {
		return nil, 0, err
	}
	name := strings.TrimRight(string(nameBuf), "\x00")
	// ⛔ Both the name and the data are padded to a multiple of four, and the
	// padding is counted from the START of the header rather than from the field.
	// Rounding the wrong origin puts every later header one to three bytes out,
	// which reads as a corrupt archive somewhere else entirely.
	dataOff := cpioRound4(off + headerLen + nameSize)
	next := cpioRound4(dataOff + fileSize)
	if name == cpioTrailer {
		return nil, next, nil
	}
	if dataOff+fileSize > size {
		return nil, 0, fmt.Errorf("cpio: %s says %d bytes, past the end: %w",
			name, fileSize, ErrIncomplete)
	}
	return cpioRecord(ra, name, mode, fileSize, dataOff, mtime, next)
}

// cpioReadODC reads one POSIX.1 octal header.
func cpioReadODC(ra io.ReaderAt, size, off int64) (*record, int64, error) {
	const headerLen = 76
	if off+headerLen > size {
		return nil, 0, fmt.Errorf("cpio: header at %d runs past the end: %w", off, ErrIncomplete)
	}
	h := make([]byte, headerLen)
	if _, err := ra.ReadAt(h, off); err != nil {
		return nil, 0, err
	}
	// Field widths differ from newc's: 6 for most, 11 for mtime and filesize.
	oct := func(start, width int) (int64, error) {
		return strconv.ParseInt(strings.TrimSpace(string(h[start:start+width])), 8, 64)
	}
	mode, err := oct(18, 6)
	if err != nil {
		return nil, 0, fmt.Errorf("cpio: mode at %d: %w", off, err)
	}
	nameSize, err := oct(59, 6)
	if err != nil {
		return nil, 0, fmt.Errorf("cpio: name size at %d: %w", off, err)
	}
	mtime, _ := oct(48, 11)
	fileSize, err := oct(65, 11)
	if err != nil {
		return nil, 0, fmt.Errorf("cpio: size at %d: %w", off, err)
	}
	if nameSize <= 0 || off+headerLen+nameSize > size {
		return nil, 0, fmt.Errorf("cpio: name at %d is %d bytes: %w", off, nameSize, ErrIncomplete)
	}
	nameBuf := make([]byte, nameSize)
	if _, err := ra.ReadAt(nameBuf, off+headerLen); err != nil {
		return nil, 0, err
	}
	name := strings.TrimRight(string(nameBuf), "\x00")
	// odc pads NOTHING, which is the difference from newc that matters most.
	dataOff := off + headerLen + nameSize
	next := dataOff + fileSize
	if name == cpioTrailer {
		return nil, next, nil
	}
	if dataOff+fileSize > size {
		return nil, 0, fmt.Errorf("cpio: %s says %d bytes, past the end: %w",
			name, fileSize, ErrIncomplete)
	}
	return cpioRecord(ra, name, mode, fileSize, dataOff, mtime, next)
}

// cpioRecord turns one parsed header into a record, reading a symlink's target.
// ⛔ next is a PARAMETER. It used to be returned as 0 from here while the callers
// did `return cpioRecord(...)`, so the next offset came back zero and the outer
// loop read header one for ever -- the suite reported a ten-minute TIMEOUT rather
// than a failure. Same shape as the joined reader's loop the same afternoon:
// a position that the data is allowed to leave unchanged.
func cpioRecord(ra io.ReaderAt, name string, mode, fileSize, dataOff, mtime, next int64) (*record, int64, error) {
	r := &record{
		name:   name,
		mode:   cpioMode(mode),
		size:   fileSize,
		offset: dataOff,
		mtime:  time.Unix(mtime, 0),
	}
	// A symlink's target IS its data, and the size is the target's length. It has
	// to be read now, because a record carries the target and not an offset.
	if r.mode&iofs.ModeSymlink != 0 && fileSize > 0 && fileSize < 1<<16 {
		buf := make([]byte, fileSize)
		if _, err := ra.ReadAt(buf, dataOff); err != nil {
			return nil, 0, err
		}
		r.link = strings.TrimRight(string(buf), "\x00")
		r.size = 0
	}
	return r, next, nil
}

// cpioMode turns a POSIX st_mode into an iofs.FileMode.
//
// ⛔ The type is in the HIGH bits of the octal value, not in iofs.FileMode's own
// places: S_IFDIR is 0o040000 while iofs.ModeDir is 1<<31. Narrowing one into the
// other without this switch leaves every directory looking like a file with
// peculiar permissions.
func cpioMode(m int64) iofs.FileMode {
	mode := iofs.FileMode(m & 0o777)
	switch m & 0o170000 {
	case 0o040000:
		mode |= iofs.ModeDir
	case 0o120000:
		mode |= iofs.ModeSymlink
	case 0o010000:
		mode |= iofs.ModeNamedPipe
	case 0o020000:
		mode |= iofs.ModeDevice | iofs.ModeCharDevice
	case 0o060000:
		mode |= iofs.ModeDevice
	case 0o140000:
		mode |= iofs.ModeSocket
	}
	return mode
}

// cpioRound4 rounds up to the next multiple of four.
func cpioRound4(n int64) int64 { return (n + 3) &^ 3 }
