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

// TestOnlySomeFormatsCarryANote. A Note is for a format whose plain sentence
// would mislead; every other format's is the right one, and a Note on all of them
// would be noise nobody reads.
//
// The list is exhaustive on purpose, and the count is asserted: a format added
// upstream falls into neither column, the count fails, and somebody decides
// rather than the default deciding for them.
func TestOnlySomeFormatsCarryANote(t *testing.T) {
	// Every format whose bare sentence says everything there is to say.
	plain := []Format{
		FormatUnknown, FormatZIP, Format7z, FormatTar, FormatAr, FormatCpio,
		FormatGzip, FormatBzip2, FormatXZ, FormatZstd, FormatLZ4,
		FormatLZO, FormatLzip,
		FormatISO9660, FormatSquashFS, FormatHFSPlus, FormatDMG,
		FormatXAR, FormatCAB, FormatRPM, FormatWARC,
	}
	// And every format that needs more than that, with what the note is FOR.
	noted := map[Format]string{
		FormatPtar: "recognised and not unpacked, which would read as a promise",
		FormatZ:    "read and not written, and nobody should be making a new one",
		FormatRAR: "written, but with no compression, and finding that out from " +
			"the file size afterwards is too late",
	}

	for _, f := range plain {
		if note := f.Note(); note != "" {
			t.Errorf("%v carries a note: %q", f, note)
		}
	}
	for f, why := range noted {
		if f.Note() == "" {
			t.Errorf("%v carries no note, and it needs one: %s", f, why)
		}
	}

	// ⛔ The count. Without it the two lists above drift from the constants and
	// a new format quietly joins neither.
	if n := len(plain) + len(noted); n != formatsInThisPackage {
		t.Errorf("this test accounts for %d formats and the package has %d: a "+
			"format was added and nobody said which column it belongs in",
			n, formatsInThisPackage)
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
