// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ptarFile builds the smallest thing PlakarKorp's own storage driver would
// accept as a container: the magic, some body, and the six little-endian
// int64 offsets it writes at Close.
//
// It is built from the FORMAT rather than from a real .ptar, because making a
// real one needs plakar, and what is under test here is recognition.
func ptarFile(body []byte) []byte {
	var b bytes.Buffer
	b.WriteString("_PLATAR_")
	b.Write(body)
	for _, v := range []int64{8, int64(len(body)), 8, 0, 8, 0} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	return b.Bytes()
}

// TestAPtarIsRecognisedAndSaysWhatItIs.
//
// ⛔ The magic is _PLATAR_, not _PTAR_. That was read out of PlakarKorp's own
// storage driver, and guessing it from the extension -- the thing this package
// exists not to do -- would have got it wrong.
//
// Recognition is the deliverable here, and it is worth having on its own: without
// it a .ptar answers "the bytes match no format this knows", which sends somebody
// looking for a corrupt file. It is not unpacked, and the message says why rather
// than implying that somebody will get round to it.
func TestAPtarIsRecognisedAndSaysWhatItIs(t *testing.T) {
	img := ptarFile([]byte("whatever a kloset puts here"))

	got, err := Sniff(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Sniff: %v", err)
	}
	if got != FormatPtar {
		t.Fatalf("Sniff = %v, want ptar", got)
	}

	path := filepath.Join(t.TempDir(), "backup.ptar")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	_, format, err := Open(path)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Open = %v, want ErrNotImplemented", err)
	}
	if format != FormatPtar {
		t.Errorf("format = %v, want ptar", format)
	}
	if errors.Is(err, ErrUnknownFormat) {
		t.Errorf("reported as unknown, which sends somebody hunting a corrupt file: %v", err)
	}
	// The message has to name the tool that CAN open it. "not read yet" alone
	// suggests waiting, and waiting is the wrong answer for this one.
	for _, want := range []string{"plakar", "encrypted", "backup.ptar"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestOnlyPtarCarriesANote. A Note is for a format whose "not read yet" would
// mislead; every other format's plain sentence is the right one, and a Note on
// all of them would be noise nobody reads.
func TestOnlyPtarCarriesANote(t *testing.T) {
	for _, f := range []Format{
		FormatUnknown, FormatRAR, FormatZIP, Format7z, FormatTar, FormatGzip,
		FormatBzip2, FormatXZ, FormatZstd, FormatLZ4, FormatISO9660,
		FormatSquashFS,
	} {
		if note := f.Note(); note != "" {
			t.Errorf("%v carries a note: %q", f, note)
		}
	}
	if FormatPtar.Note() == "" {
		t.Error("ptar carries no note, and its bare sentence is the misleading one")
	}
}

// TestPtarDoesNotShadowTar.
//
// Both signatures are in the same table and tar's sits at offset 257, so a
// container whose body happens to hold "ustar" there must still be read as the
// outer format. Order in that table is the only thing deciding it.
func TestPtarDoesNotShadowTar(t *testing.T) {
	// A ptar whose body is long enough to reach offset 257, with "ustar" planted
	// exactly where tar's magic lives.
	body := make([]byte, 400)
	copy(body[257-8:], "ustar")
	img := ptarFile(body)
	got, err := Sniff(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Sniff: %v", err)
	}
	if got != FormatPtar {
		t.Errorf("Sniff = %v, want ptar: tar's deep magic won over an outer format's", got)
	}

	// And the converse: a real tar is still a tar.
	tarHead := make([]byte, 300)
	copy(tarHead[257:], "ustar")
	got, err = Sniff(bytes.NewReader(tarHead), int64(len(tarHead)))
	if err != nil {
		t.Fatalf("Sniff: %v", err)
	}
	if got != FormatTar {
		t.Errorf("Sniff = %v, want tar", got)
	}
}
