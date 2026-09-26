// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-diskimages/dmg"
	filesystem "github.com/go-filesystems/interface"
)

// openDMG turns a UDIF image into its sectors and opens the filesystem in them.
//
// A .dmg is a CONTAINER, and treating it as one of this package's images would
// be wrong in a way that happens to work: a UDIF whose sectors are stored raw
// can be read in place, and the same image written sparse or zlib-compressed
// cannot. So the sectors are decoded to a spool and the spool is SNIFFED, the
// way a stream wrapper's contents are -- a .dmg holds HFS+, or APFS, or an
// ISO9660, and which one is not written anywhere in the container.
//
// What it does NOT do is look at a partition map. An image that carries one
// holds a table where a filesystem's magic would be, so the sniff finds nothing
// and says so, naming the DMG it came out of. Splitting that apart is a volume
// manager's job, not an unarchiver's, and go-volumes has one.
func openDMG(path string, depth int) (filesystem.Filesystem, Format, error) {
	if depth >= maxNesting {
		return nil, FormatDMG, fmt.Errorf("%s: %d wrappers deep: %w", path, depth, ErrTooDeeplyNested)
	}

	// dmg writes the sectors to a temporary file of its own choosing. It is moved
	// into a directory this package owns, both so spoolFS can remove the whole
	// thing on Close and so the spool carries a NAME: if what comes out is not an
	// archive the directory is the filesystem, and its one entry should not be
	// called something dmg invented.
	raw, err := dmg.UnpackToTemp(path)
	if err != nil {
		return nil, FormatDMG, fmt.Errorf("%s: %w", path, err)
	}
	dir, err := os.MkdirTemp("", "unarchive-dmg-")
	if err != nil {
		os.Remove(raw)
		return nil, FormatDMG, err
	}
	spool := filepath.Join(dir, innerName(path))
	// Both temporary paths come from os.TempDir(), so this rename cannot be the
	// cross-device kind and there is no copy fallback here on purpose: a fallback
	// nothing can reach is a branch nothing can test.
	if err := os.Rename(raw, spool); err != nil {
		os.Remove(raw)
		os.RemoveAll(dir)
		return nil, FormatDMG, fmt.Errorf("%s: spooling the sectors: %w", path, err)
	}

	inner, _, err := openAt(spool, depth+1)
	if err != nil {
		os.RemoveAll(dir)
		// The error is reported AS a dmg failure, naming the image the caller
		// handed over. Unlike openCompressed, an unrecognised spool is not
		// handed back as a one-entry filesystem: a stream wrapper legitimately
		// holds a single ordinary file, and a UDIF image never does. Its sectors
		// are a filesystem or they are nothing, so "I unpacked it and do not know
		// what this is" is the truth and the useful thing to say -- especially
		// for APFS, which detect names and no driver here opens.
		return nil, FormatDMG, fmt.Errorf("%s: the image unpacked, and its sectors hold no filesystem this opens: %w", path, err)
	}
	// FormatDMG is what is reported, because that is what the caller handed over.
	// The same reasoning as openCompressed: answering "hfsplus" for a file called
	// .dmg makes a correct answer read like a mistake.
	return &spoolFS{Filesystem: inner, dir: dir}, FormatDMG, nil
}
