// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"fmt"
	"io"
	iofs "io/fs"
	"strings"
)

// arBuilder writes an ar archive: a static library, or a .deb.
//
// ⛔ It writes the BSD convention, not SysV, and that was decided by MEASUREMENT.
// SysV ends a short name with a slash so trailing spaces can be part of it; BSD
// does not, and puts a long name at the start of the member's own data behind a
// "#1/<length>" marker.
//
// Written SysV, `ar t` on macOS listed every member with its slash attached --
// "notes.txt/" -- and `ar p archive notes.txt` then found nothing. BSD is what the
// ar on this platform reads natively, and GNU ar and libarchive read it too, so it
// is the convention that is understood everywhere rather than the one that is
// merely older.
//
// It also needs no string table, which is why this streams: SysV puts long names
// in a "//" member that must PRECEDE every entry, so a SysV writer has to buffer
// the whole archive before it can emit the first byte.
type arBuilder struct {
	w   io.Writer
	err error
}

// newArBuilder starts an archive.
func newArBuilder(w io.Writer) (*arBuilder, error) {
	if _, err := io.WriteString(w, arMagic); err != nil {
		return nil, err
	}
	return &arBuilder{w: w}, nil
}

// AddDir records nothing, because ar is FLAT and has nowhere to put it.
//
// Refusing would make it impossible to seal any overlay that has a directory,
// which is every overlay; storing one would put a member in the archive that no ar
// reads back as a directory, and that every listing shows as a file of no bytes.
func (b *arBuilder) AddDir(string, iofs.FileMode) error { return b.err }

// AddFile writes one member, streaming its contents.
func (b *arBuilder) AddFile(name string, perm iofs.FileMode, size int64, r io.Reader) error {
	if b.err != nil {
		return b.err
	}
	name = indexClean(name)
	if name == "" {
		return nil
	}
	field, prefix := name, ""
	if len(name) > 16 || strings.ContainsAny(name, " /") {
		// The name goes in the data, NUL-padded to a multiple of four, and the
		// size field counts it. macOS's ar writes "#1/44" for a 42-character name,
		// which is where the padding was read from.
		padded := (len(name) + 3) &^ 3
		prefix = name + strings.Repeat("\x00", padded-len(name))
		field = fmt.Sprintf("#1/%d", padded)
	}
	if err := b.header(field, perm, size+int64(len(prefix))); err != nil {
		return err
	}
	if prefix != "" {
		if err := b.writeAll([]byte(prefix)); err != nil {
			return err
		}
	}
	if _, err := io.CopyN(b.w, r, size); err != nil {
		b.err = fmt.Errorf("ar: %s: %w", name, err)
		return b.err
	}
	// Members start on an EVEN offset, and the pad is a newline that the size does
	// not count. Without it every header after an odd-sized member is garbage.
	if (size+int64(len(prefix)))%2 == 1 {
		return b.writeAll([]byte("\n"))
	}
	return nil
}

// Close has nothing to finish: ar has no trailer.
func (b *arBuilder) Close() error { return b.err }

// header writes one 60-byte member header.
//
// ⛔ The mode is OCTAL and the size is DECIMAL, in the same header, both as plain
// digits. Writing either in the other base produces a header every reader accepts
// and misreads.
func (b *arBuilder) header(name string, perm iofs.FileMode, size int64) error {
	h := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n", name, 0, 0, 0, perm.Perm(), size)
	if len(h) != arHeaderLen {
		return fmt.Errorf("ar: %q made a %d-byte header, want %d: %w",
			name, len(h), arHeaderLen, ErrUnknownFormat)
	}
	return b.writeAll([]byte(h))
}

func (b *arBuilder) writeAll(p []byte) error {
	if _, err := b.w.Write(p); err != nil {
		b.err = err
	}
	return b.err
}
