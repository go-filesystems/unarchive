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
	"io"
)

// Format is an archive format this package can recognise.
type Format string

const (
	FormatUnknown Format = ""
	FormatRAR     Format = "rar"   // RAR 1.5-4.x and RAR5, single or multi-volume
	FormatZIP     Format = "zip"   // PKZIP, including the jar/epub/odf family
	Format7z      Format = "7z"    // 7-Zip
	FormatTar     Format = "tar"   // POSIX tar: v7, USTAR, PAX and GNU alike
	FormatGzip    Format = "gzip"  // a stream wrapper, usually around a tar
	FormatBzip2   Format = "bzip2" // likewise
	FormatXZ      Format = "xz"    // likewise
	FormatZstd    Format = "zstd"  // likewise
)

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
	{FormatZIP, 0, []byte("PK\x03\x04")},
	{FormatZIP, 0, []byte("PK\x05\x06")}, // an empty archive is still an archive
	{FormatZIP, 0, []byte("PK\x07\x08")}, // spanned
	{FormatGzip, 0, []byte{0x1F, 0x8B}},
	{FormatBzip2, 0, []byte("BZh")},
	// tar carries no magic at the start: its identity sits 257 bytes in, which
	// is why a tar is so often guessed at by extension instead of read.
	{FormatTar, 257, []byte("ustar")},
}

// sniffLen is how much of a file Sniff needs.
const sniffLen = 262

// Sniff names the format of the archive in r, reading only its head.
func Sniff(r io.ReaderAt) (Format, error) {
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
	return FormatUnknown, ErrUnknownFormat
}

// Compressed says whether a format is a stream wrapper rather than an archive
// of its own: gzip holds one stream, which is usually a tar and might be
// anything.
func (f Format) Compressed() bool {
	switch f {
	case FormatGzip, FormatBzip2, FormatXZ, FormatZstd:
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
