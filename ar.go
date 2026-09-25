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

// arMagic opens every ar archive: a static library, and a .deb.
const arMagic = "!<arch>\n"

// arHeaderLen is the fixed size of a member header.
const arHeaderLen = 60

// openAr indexes an ar archive.
//
// ar is the simplest archive format still in use and it holds a Debian package:
// a .deb is an ar of debian-binary, control.tar.* and data.tar.*. It is FLAT --
// no directories, no paths -- which is why long names needed bolting on twice,
// incompatibly, and both spellings are read here:
//
//	SysV/GNU   a "//" member holds a string table; a member named "/123"
//	           takes its name from offset 123 of it. Short names end in "/".
//	BSD        a member named "#1/13" carries thirteen bytes of name at the
//	           START of its own data, which the data offset then skips.
//
// Which one a file uses depends on the ar that wrote it, not on what is in it, so
// a reader that knows only one silently reports names like "/48".
func openAr(ra io.ReaderAt, size int64, closer io.Closer) (filesystem.Filesystem, error) {
	head := make([]byte, len(arMagic))
	if _, err := ra.ReadAt(head, 0); err != nil {
		return nil, err
	}
	if string(head) != arMagic {
		return nil, fmt.Errorf("ar: %w", ErrUnknownFormat)
	}

	var recs []record
	var names []byte // the SysV string table, once seen
	off := int64(len(arMagic))
	for off+arHeaderLen <= size {
		h := make([]byte, arHeaderLen)
		if _, err := ra.ReadAt(h, off); err != nil {
			return nil, fmt.Errorf("ar: member header at %d: %w", off, err)
		}
		if string(h[58:60]) != "`\n" {
			return nil, fmt.Errorf("ar: member header at %d does not end in its magic: %w",
				off, ErrUnknownFormat)
		}
		name := strings.TrimRight(string(h[0:16]), " ")
		msize, err := strconv.ParseInt(strings.TrimSpace(string(h[48:58])), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ar: member size at %d: %w", off, err)
		}
		if msize < 0 || off+arHeaderLen+msize > size {
			return nil, fmt.Errorf("ar: member at %d says %d bytes, past the end: %w",
				off, msize, ErrIncomplete)
		}
		mtime, _ := strconv.ParseInt(strings.TrimSpace(string(h[16:28])), 10, 64)
		// The mode is OCTAL and the sizes are DECIMAL, in the same header. Reading
		// either in the other base gives a plausible number.
		mode, _ := strconv.ParseUint(strings.TrimSpace(string(h[40:48])), 8, 32)

		data := off + arHeaderLen
		dsize := msize
		switch {
		case name == "//":
			// The string table itself is not a member anyone asked for.
			names = make([]byte, msize)
			if _, err := ra.ReadAt(names, data); err != nil {
				return nil, fmt.Errorf("ar: string table: %w", err)
			}
			off = data + msize + msize%2
			continue
		case name == "/" || name == "/SYM64/":
			// The symbol table of a static library: not a file either.
			off = data + msize + msize%2
			continue
		case strings.HasPrefix(name, "#1/"):
			n, err := strconv.Atoi(name[3:])
			if err != nil || int64(n) > msize {
				return nil, fmt.Errorf("ar: BSD long name at %d: %w", off, ErrUnknownFormat)
			}
			buf := make([]byte, n)
			if _, err := ra.ReadAt(buf, data); err != nil {
				return nil, fmt.Errorf("ar: BSD long name at %d: %w", off, err)
			}
			// NUL-padded to a multiple of four by some ars, so the name stops at
			// the first NUL rather than filling the field.
			name = string(buf)
			if i := strings.IndexByte(name, 0); i >= 0 {
				name = name[:i]
			}
			data += int64(n)
			dsize -= int64(n)
		case strings.HasPrefix(name, "/"):
			idx, err := strconv.Atoi(name[1:])
			if err != nil || idx < 0 || idx >= len(names) {
				return nil, fmt.Errorf("ar: long name %q has no entry in the string table: %w",
					name, ErrUnknownFormat)
			}
			end := strings.IndexAny(string(names[idx:]), "/\n")
			if end < 0 {
				end = len(names) - idx
			}
			name = string(names[idx : idx+end])
		default:
			// SysV ends a short name with a slash so trailing spaces can be part
			// of it. BSD does not, so the slash is stripped only when it is there.
			name = strings.TrimSuffix(name, "/")
		}

		if name != "" {
			recs = append(recs, record{
				name:   name,
				mode:   iofs.FileMode(mode & 0o777),
				size:   dsize,
				offset: data,
				mtime:  time.Unix(mtime, 0),
			})
		}
		// Members are padded to an EVEN offset with a newline, and the padding is
		// not counted in the size. Skipping it is what keeps the next header
		// aligned; without it every member after an odd-sized one is garbage.
		off = off + arHeaderLen + msize + msize%2
	}
	return &closerFS{Filesystem: FromFS(newIndexFS(ra, recs)), closer: nopCloser(closer)}, nil
}

// nopCloser gives closerFS something to close when the caller kept the handle.
func nopCloser(c io.Closer) io.Closer {
	if c != nil {
		return c
	}
	return closerFunc(func() error { return nil })
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
