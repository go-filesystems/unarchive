// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"errors"
	"testing"
)

// at builds a head with magic placed at offset, so a signature that is not at
// the start is tested where it actually lives.
func at(offset int, magic string, total int) []byte {
	b := make([]byte, total)
	copy(b[offset:], magic)
	return b
}

// TestSniffReadsTheBytesNotTheName covers every signature, and the one that
// cannot be guessed from a name: tar's identity sits 257 bytes in.
func TestSniffReadsTheBytesNotTheName(t *testing.T) {
	for _, c := range []struct {
		name string
		head []byte
		want Format
	}{
		{"RAR5", at(0, "Rar!\x1a\x07\x01\x00", 300), FormatRAR},
		{"RAR 4", at(0, "Rar!\x1a\x07\x00", 300), FormatRAR},
		{"7z", at(0, "7z\xbc\xaf\x27\x1c", 300), Format7z},
		{"xz", at(0, "\xfd7zXZ\x00", 300), FormatXZ},
		{"zstd", at(0, "\x28\xb5\x2f\xfd", 300), FormatZstd},
		{"zip", at(0, "PK\x03\x04", 300), FormatZIP},
		{"an empty zip is still a zip", at(0, "PK\x05\x06", 300), FormatZIP},
		{"gzip", at(0, "\x1f\x8b", 300), FormatGzip},
		{"bzip2", at(0, "BZh", 300), FormatBzip2},
		// The case that makes extension-guessing look reasonable and is the
		// reason it is not: nothing at the start says "tar".
		{"tar, 257 bytes in", at(257, "ustar", 300), FormatTar},
		{"nothing at all", make([]byte, 300), FormatUnknown},
		{"shorter than any magic", []byte{'P'}, FormatUnknown},
		{"empty", nil, FormatUnknown},
	} {
		got, err := Sniff(bytes.NewReader(c.head))
		if c.want == FormatUnknown {
			if !errors.Is(err, ErrUnknownFormat) {
				t.Errorf("%s: err = %v, want ErrUnknownFormat", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: Sniff = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRAR5IsNotReadAsRAR4 is an ordering trap, not a signature: RAR5's magic is
// RAR4's plus one byte, so whichever rule is tried first wins. Tried the wrong
// way round, every RAR5 archive is opened as the older format -- which reads
// its header differently and fails somewhere else entirely, where nothing
// points back here.
func TestRAR5IsNotReadAsRAR4(t *testing.T) {
	rar5 := at(0, "Rar!\x1a\x07\x01\x00", 300)
	var first, second int = -1, -1
	for i, s := range signatures {
		if s.format != FormatRAR {
			continue
		}
		if len(s.magic) == 8 && first < 0 {
			first = i
		}
		if len(s.magic) == 7 && second < 0 {
			second = i
		}
	}
	if first < 0 || second < 0 {
		t.Fatal("both RAR signatures should be present")
	}
	if first > second {
		t.Error("the shorter RAR signature is tried first, so every RAR5 archive " +
			"is claimed by the RAR4 rule")
	}
	// And the whole thing, through the door a caller uses.
	if got, err := Sniff(bytes.NewReader(rar5)); err != nil || got != FormatRAR {
		t.Errorf("Sniff(RAR5) = %q, %v", got, err)
	}
}

// TestCompressedNamesTheWrappers: a gzip holds ONE stream, which is usually a
// tar and might be anything, so a caller has to know it is looking at a wrapper
// before it can decide what to do next.
func TestCompressedNamesTheWrappers(t *testing.T) {
	for _, f := range []Format{FormatGzip, FormatBzip2, FormatXZ, FormatZstd} {
		if !f.Compressed() {
			t.Errorf("%s is a stream wrapper and does not say so", f)
		}
	}
	for _, f := range []Format{FormatRAR, FormatZIP, Format7z, FormatTar, FormatUnknown} {
		if f.Compressed() {
			t.Errorf("%s is not a stream wrapper", f)
		}
	}
	if FormatUnknown.String() != "unknown" {
		t.Errorf("FormatUnknown prints as %q", FormatUnknown)
	}
	if FormatRAR.String() != "rar" {
		t.Errorf("FormatRAR prints as %q", FormatRAR)
	}
}
