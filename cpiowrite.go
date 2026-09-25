// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"fmt"
	"io"
	iofs "io/fs"
)

// cpioBuilder writes an SVR4 ASCII cpio -- the "newc" format, which is what an
// initramfs is and what every cpio still in use reads.
//
// newc is chosen over the octal "odc" because odc's fields are six octal digits:
// a file over 8 MiB does not fit, and neither does an inode number over 262143.
// newc's eight hex digits reach 4 GiB, and the whole point of writing one is that
// somebody else can read it.
type cpioBuilder struct {
	w     io.Writer
	pos   int64 // bytes written, because every field is padded relative to the START
	ino   int64
	err   error
	ended bool
}

func newCpioBuilder(w io.Writer) *cpioBuilder { return &cpioBuilder{w: w, ino: 1} }

func (b *cpioBuilder) AddDir(name string, perm iofs.FileMode) error {
	// nlink is 2 for a directory by convention -- itself and its entry in its
	// parent. Readers do not check it, and writing 1 makes an archive that looks
	// wrong to anybody who does.
	return b.entry(name, uint32(perm.Perm())|0o040000, 2, 0, nil)
}

func (b *cpioBuilder) AddFile(name string, perm iofs.FileMode, size int64, r io.Reader) error {
	return b.entry(name, uint32(perm.Perm())|0o100000, 1, size, r)
}

// entry writes one header, its name and its data, each padded to four bytes.
//
// ⛔ The padding is counted from the START of the archive, not from the start of
// the field. A writer that rounds the field length instead puts every later header
// one to three bytes out, and the result reads as a corrupt archive somewhere
// else entirely.
func (b *cpioBuilder) entry(name string, mode uint32, nlink int, size int64, r io.Reader) error {
	if b.err != nil {
		return b.err
	}
	name = indexClean(name)
	if name == "" {
		return nil
	}
	nameBytes := append([]byte(name), 0)
	h := fmt.Sprintf("%s%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		cpioNewc,
		b.ino, // ino
		mode,  // mode, with the POSIX type bits
		0, 0,  // uid, gid
		nlink,      // nlink
		0,          // mtime
		size,       // filesize
		0, 0, 0, 0, // dev major/minor, rdev major/minor
		len(nameBytes), // namesize, INCLUDING the terminating NUL
		0,              // check, unused by newc
	)
	b.ino++
	if err := b.write([]byte(h)); err != nil {
		return err
	}
	if err := b.write(nameBytes); err != nil {
		return err
	}
	if err := b.pad(); err != nil {
		return err
	}
	if size > 0 {
		if _, err := io.CopyN(b.w, r, size); err != nil {
			b.err = fmt.Errorf("cpio: %s: %w", name, err)
			return b.err
		}
		b.pos += size
	}
	return b.pad()
}

// Close writes the trailer every cpio ends with: an entry named TRAILER!!! of no
// bytes. Without it, readers that stop at the trailer read on into whatever
// follows.
func (b *cpioBuilder) Close() error {
	if b.err != nil || b.ended {
		return b.err
	}
	b.ended = true
	// nlink 1, mode 0: the trailer is not a file and carries nothing.
	if err := b.entry(cpioTrailer, 0, 1, 0, nil); err != nil {
		return err
	}
	// An initramfs is padded to a block, and a plain cpio need not be. Nothing is
	// added here: a reader that keeps going past the trailer is reading what the
	// CALLER put there, which is its business.
	return b.err
}

func (b *cpioBuilder) write(p []byte) error {
	n, err := b.w.Write(p)
	b.pos += int64(n)
	if err != nil {
		b.err = err
	}
	return err
}

// pad writes zeros up to the next multiple of four.
func (b *cpioBuilder) pad() error {
	if n := cpioRound4(b.pos) - b.pos; n > 0 {
		return b.write(make([]byte, n))
	}
	return nil
}
