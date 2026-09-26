// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package unarchive opens an archive without being told what it is, and
// extracts it while checking that what came out is what the archive promised.
//
// # Why the bytes and not the name
//
// A format is recognised by what is IN the file. An extension is a claim by
// whoever last renamed it: a .rar that is really a zip, a video saved as .mp4
// that is an MPEG transport stream, a volume renamed by a download manager --
// all ordinary, and all wrong if the name decides.
//
// # Why the check
//
// An extractor that reports success is not the same as an extractor that got
// everything. libarchive, which is bsdtar and therefore macOS, does not follow
// a multi-volume RAR chain: it writes the first volume, pads the remainder with
// ZEROS to the declared length and exits 0. The result is exactly the right
// SIZE, which is what every check afterwards looks at. This package compares
// what was written against what the header declared, per entry, and says so
// when they differ.
package unarchive

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/go-filesystems/detect"
)

// Format is an archive format this package can recognise.
type Format string

const (
	FormatUnknown Format = ""
	FormatRAR     Format = "rar"   // RAR 1.5-4.x and RAR5, single or multi-volume
	FormatZIP     Format = "zip"   // PKZIP, including the jar/epub/odf family
	Format7z      Format = "7z"    // 7-Zip
	FormatTar     Format = "tar"   // POSIX tar: v7, USTAR, PAX and GNU alike
	FormatAr      Format = "ar"    // the static-library and .deb container
	FormatCpio    Format = "cpio"  // an initramfs, and an RPM's payload
	FormatGzip    Format = "gzip"  // a stream wrapper, usually around a tar
	FormatBzip2   Format = "bzip2" // likewise
	FormatXZ      Format = "xz"    // likewise
	FormatZstd    Format = "zstd"  // likewise
	FormatLZ4     Format = "lz4"   // likewise
	// compress(1)'s .Z, read and not written -- see Format.Note.
	FormatZ Format = "compress"
	// Filesystem IMAGES, which are archives in every way that matters here: one
	// file holding a tree, handed around to be unpacked. They are read through
	// the org's own drivers rather than anything written here.
	//
	// The line is drawn at formats people DISTRIBUTE. You download an .iso and
	// you ship a .squashfs; you do not pass somebody an ext4 image expecting them
	// to unpack it. go-filesystems has drivers for a dozen more and this opens
	// two, on purpose.
	FormatISO9660  Format = "iso9660"
	FormatSquashFS Format = "squashfs"
	// Recognised and deliberately not unpacked. See Format.Note.
	FormatPtar Format = "ptar"
)

// Why brotli is not here. It has no signature: a brotli stream begins with the
// first bits of its own data, so there is nothing to recognise it BY. The only
// way to open one is to be told, by an extension or a flag, and this package
// decides from the bytes. A format that cannot be sniffed does not belong in a
// sniffer, and pretending otherwise would mean guessing brotli for every file
// nothing else claimed.

// ErrUnknownFormat is returned when nothing recognises the bytes.
var ErrUnknownFormat = errors.New("unarchive: the bytes match no format this knows")

// ErrNotImplemented is returned for a format that is recognised and not yet
// read. Telling the two apart matters to somebody holding a file: "I do not
// know what this is" and "I know exactly what this is and cannot open it yet"
// send them to different places.
var ErrNotImplemented = errors.New("unarchive: recognised, but this format is not read yet")

// signature is one magic-byte rule: bytes that must appear at offset.
type signature struct {
	format Format
	offset int
	magic  []byte
}

// signatures are tried in order, longest and most specific first.
//
// RAR5 must precede RAR4: RAR5's magic is RAR4's plus a byte, so a shorter
// match would claim it and the archive would be read as the older format.
var signatures = []signature{
	{FormatRAR, 0, []byte("Rar!\x1a\x07\x01\x00")}, // RAR5
	{FormatRAR, 0, []byte("Rar!\x1a\x07\x00")},     // RAR 1.5-4.x
	{Format7z, 0, []byte{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}},
	{FormatXZ, 0, []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}},
	{FormatZstd, 0, []byte{0x28, 0xB5, 0x2F, 0xFD}},
	{FormatLZ4, 0, []byte{0x04, 0x22, 0x4D, 0x18}}, // the LZ4 frame format
	{FormatZIP, 0, []byte("PK\x03\x04")},
	{FormatZIP, 0, []byte("PK\x05\x06")}, // an empty archive is still an archive
	{FormatZIP, 0, []byte("PK\x07\x08")}, // spanned
	{FormatGzip, 0, []byte{0x1F, 0x8B}},
	// One byte apart from gzip's, which is why both are listed together: reading
	// 0x1F and stopping would claim either for the other.
	{FormatZ, 0, []byte{0x1F, 0x9D}},
	{FormatBzip2, 0, []byte("BZh")},
	// ⛔ The magic is _PLATAR_, not _PTAR_. Read out of PlakarKorp's own storage
	// driver rather than guessed from the extension, which is the whole doctrine
	// of this file and would have got it wrong here.
	{FormatPtar, 0, []byte("_PLATAR_")},
	{FormatAr, 0, []byte("!<arch>\n")},
	// cpio's three ASCII variants. The fourth, "old binary", puts a 16-bit magic
	// in the writer's own byte order -- the same file meaning different things on
	// different machines -- so it is not listed and openCpio says why.
	{FormatCpio, 0, []byte("070701")},
	{FormatCpio, 0, []byte("070702")},
	{FormatCpio, 0, []byte("070707")},
	// The old binary variant's magic is 0o070707 as a 16-bit WORD, so it is two
	// bytes and there are two of them: the same value on a little-endian machine
	// and on a big-endian one. Listing only one reads half the archives in the
	// world and refuses the other half.
	{FormatCpio, 0, []byte{0xC7, 0x71}},
	{FormatCpio, 0, []byte{0x71, 0xC7}},
	// tar carries no magic at the start: its identity sits 257 bytes in, which
	// is why a tar is so often guessed at by extension instead of read.
	{FormatTar, 257, []byte("ustar")},
}

// sniffLen is how much of a file the archive signatures need.
const sniffLen = 262

// Sniff names the format of the archive or filesystem image in r.
//
// The size is needed for the image half and not for the archive half: an
// archive's identity is in its first 262 bytes, while ISO 9660's is at offset
// 32769 and a probe that deep has to know where the file ends, or a truncated
// image turns into a read past the end.
//
// The image probing is go-filesystems/detect's, not a second table written here.
// It is bounded through safeio, it handles both endiannesses of squashfs, and it
// knows a dozen filesystems -- so duplicating the two entries this package opens
// would mean re-deriving something deliberately hardened, and would drift from it.
func Sniff(r io.ReaderAt, size int64) (Format, error) {
	head := make([]byte, sniffLen)
	n, err := r.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return FormatUnknown, err
	}
	head = head[:n]
	for _, s := range signatures {
		end := s.offset + len(s.magic)
		if len(head) < end {
			continue
		}
		if bytes.Equal(head[s.offset:end], s.magic) {
			return s.format, nil
		}
	}
	// Nothing archival. It may still be an image, which is a different prober's
	// question.
	switch t, err := detect.Detect(r, size); {
	case err != nil:
		// detect's own "I do not know" is this package's, so the caller sees one
		// sentinel rather than two that mean the same thing.
		return FormatUnknown, ErrUnknownFormat
	case t == detect.ISO9660:
		return FormatISO9660, nil
	case t == detect.SquashFS:
		return FormatSquashFS, nil
	default:
		// A filesystem detect knows and this package does not open: say WHICH,
		// because "unknown" would be false and unhelpful in the same breath.
		return FormatUnknown, fmt.Errorf("%s: %w", t, ErrNotImplemented)
	}
}

// Note returns a sentence about a format this package recognises and does not
// unpack, or "" for the rest.
//
// It exists because "recognised, not read yet" is true of ptar and MISLEADING
// on its own: it suggests somebody will get round to it. What a .ptar is makes
// that the wrong expectation, and the person holding one is better served by
// being told which tool opens it than by waiting.
func (f Format) Note() string {
	if f == FormatZ {
		// Says what the sentinel does not. "read but not written" already carries
		// the fact; repeating it here made the message contradict its own
		// brevity.
		return "the LZW of compress(1). A new .Z is a file nobody should be " +
			"making: gzip, xz and zstd all compress better and are read everywhere"
	}
	if f == FormatPtar {
		return "a plakar Kloset archive: a content-addressed repository of " +
			"snapshots and deduplicated chunks, encrypted unless it was made " +
			"with -plaintext. Open it with plakar, which has the key"
	}
	return ""
}

// Image says whether a format is a filesystem image rather than an archive.
//
// The distinction is not cosmetic: an image is already a filesystem, so there is
// nothing to decode and nothing to spool -- Open hands back a driver reading the
// file in place.
func (f Format) Image() bool {
	switch f {
	case FormatISO9660, FormatSquashFS:
		return true
	}
	return false
}

// Compressed says whether a format is a stream wrapper rather than an archive
// of its own: gzip holds one stream, which is usually a tar and might be
// anything.
func (f Format) Compressed() bool {
	switch f {
	case FormatGzip, FormatBzip2, FormatXZ, FormatZstd, FormatLZ4, FormatZ:
		return true
	}
	return false
}

// String is the format's name, or "unknown".
func (f Format) String() string {
	if f == FormatUnknown {
		return "unknown"
	}
	return string(f)
}
