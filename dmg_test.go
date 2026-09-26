// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two images written by hdiutil, both UDZO:
//
//   - hdiutil-iso-in-udzo.dmg wraps an ISO 9660 filesystem, itself written by
//     `hdiutil makehybrid`. Nothing in the fixture was produced here, and
//     5 KiB of container unpacks to 900 KiB of image.
//   - hdiutil-nofs-in-udzo.dmg wraps 1 MiB of ordinary bytes -- a UDIF whose
//     sectors are not a filesystem at all, which is the case the DMG path has
//     to refuse rather than hand back as a one-entry directory.
//
// They are embedded, so every lane reads them. The ISO test beside this one
// skips where xorriso is absent, which is all four emulated lanes; this one
// therefore also happens to be the only place the iso9660 driver is exercised
// there.
//
//   - hfsplus-in-udzo.dmg wraps an HFS+ volume, which is what a .dmg held
//     before APFS. Its container is hdiutil's; the VOLUME was written by
//     go-filesystems/hfsplus's own pure-Go formatter, because authoring one with
//     hdiutil means attaching a block device and this machine does not do that.
//     So this case proves the ROUTE -- bytes carrying the HFS+ signature reach
//     the hfsplus driver and the files come back -- and NOT that the driver is
//     right about HFS+, which is its own repository's business. The volume is
//     not entirely unjudged either: `hdiutil imageinfo` on this image names its
//     one partition "whole disk (Apple_HFS : 0)", so Apple's own tool agrees
//     about what is in there.
//
//go:embed testdata/hdiutil-iso-in-udzo.dmg testdata/hdiutil-nofs-in-udzo.dmg
//go:embed testdata/hfsplus-in-udzo.dmg
var dmgImages embed.FS

func dmgFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := dmgImages.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestADMGIsRecognisedByItsTrailerNotItsHead.
//
// The koly block is the LAST 512 bytes, so a sniffer reading only the head
// cannot see it. This asserts the premise as well as the answer: the head of
// this image matches nothing, so recognising it is the trailer probe's doing and
// not a signature that happens to sit at offset zero.
func TestADMGIsRecognisedByItsTrailerNotItsHead(t *testing.T) {
	path := dmgFixture(t, "hdiutil-iso-in-udzo.dmg")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	trailer := make([]byte, 4)
	if _, err := f.ReadAt(trailer, st.Size()-udifTrailerLen); err != nil {
		t.Fatalf("read trailer: %v", err)
	}
	if !bytes.Equal(trailer, udifTrailerMagic) {
		t.Fatalf("the fixture does not end in a koly block (it ends in %q): "+
			"there would be nothing here for the trailer probe to find", trailer)
	}

	// Premise, the other way round: with the trailer out of reach, nothing else
	// recognises these bytes. If something did, the test below would pass whether
	// the probe existed or not.
	head := make([]byte, 512)
	if _, err := f.ReadAt(head, 0); err != nil {
		t.Fatalf("read head: %v", err)
	}
	if got, err := Sniff(bytes.NewReader(head), int64(len(head))); err == nil {
		t.Fatalf("the head alone sniffs as %s, so this fixture cannot show that "+
			"the trailer is what names a UDIF image", got)
	}

	got, err := Sniff(f, st.Size())
	if err != nil {
		t.Fatalf("Sniff: %v", err)
	}
	if got != FormatDMG {
		t.Errorf("Sniff = %s, want %s", got, FormatDMG)
	}
}

// TestADMGUnpacksToTheFilesystemInsideIt walks the whole route: the trailer names
// a UDIF, dmg decodes its zlib runs into a spool, the spool is sniffed as an ISO,
// and the iso9660 driver reads the files out.
func TestADMGUnpacksToTheFilesystemInsideIt(t *testing.T) {
	path := dmgFixture(t, "hdiutil-iso-in-udzo.dmg")

	fsys, format, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()

	// The format reported is the CONTAINER's, deliberately: iso9660 would be a
	// correct answer to a different question and would read like a mistake next
	// to a file called .dmg.
	if format != FormatDMG {
		t.Errorf("format = %s, want %s", format, FormatDMG)
	}

	for want, body := range map[string]string{
		"hello.txt":      "the bytes an unarchived dmg must give back\n",
		"sub/nested.txt": "nested\n",
	} {
		got, err := fsys.ReadFile(want)
		if err != nil {
			t.Errorf("ReadFile(%q): %v", want, err)
			continue
		}
		if string(got) != body {
			t.Errorf("ReadFile(%q) = %q, want %q", want, got, body)
		}
	}
}

// TestADMGUnpacksAnHFSPlusVolume is the same route over the filesystem a .dmg
// usually holds, and the only place openImageAt's hfsplus branch is reached: a
// Sniff test names the format but never opens anything.
func TestADMGUnpacksAnHFSPlusVolume(t *testing.T) {
	path := dmgFixture(t, "hfsplus-in-udzo.dmg")

	fsys, format, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if format != FormatDMG {
		t.Errorf("format = %s, want %s", format, FormatDMG)
	}

	for want, body := range map[string]string{
		"/hello.txt":      "the bytes an unarchived dmg must give back\n",
		"/sub/nested.txt": "nested\n",
	} {
		got, err := fsys.ReadFile(want)
		if err != nil {
			t.Errorf("ReadFile(%q): %v", want, err)
			continue
		}
		if string(got) != body {
			t.Errorf("ReadFile(%q) = %q, want %q", want, got, body)
		}
	}
}

// TestADMGSpoolGoesWhenTheFilesystemCloses.
//
// A .dmg unpacks to a whole disk image, so a leaked spool is not a stray file
// but a megabyte per open. The spool lives in a directory of its own, and the
// test counts those directories rather than trusting that Close was called.
func TestADMGSpoolGoesWhenTheFilesystemCloses(t *testing.T) {
	before := dmgSpools(t)

	path := dmgFixture(t, "hdiutil-iso-in-udzo.dmg")
	fsys, _, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	during := dmgSpools(t)
	if during <= before {
		t.Fatalf("%d spool directories before the open and %d during it: the test "+
			"cannot show that Close removes one it never saw appear", before, during)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if after := dmgSpools(t); after != before {
		t.Errorf("%d spool directories after Close, want %d as before the open",
			after, before)
	}
}

func dmgSpools(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read the temporary directory: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "unarchive-dmg-") {
			n++
		}
	}
	return n
}

// TestADMGWhoseSectorsAreNotAFilesystemIsRefused.
//
// A stream wrapper legitimately holds one ordinary file, and openCompressed
// hands that back as a one-entry directory. A UDIF image never does: its
// sectors are a filesystem or they are nothing. So this says so, rather than
// presenting a megabyte of raw sectors as an archive of one file -- and it
// leaves no spool behind while doing it.
func TestADMGWhoseSectorsAreNotAFilesystemIsRefused(t *testing.T) {
	before := dmgSpools(t)
	path := dmgFixture(t, "hdiutil-nofs-in-udzo.dmg")

	fsys, format, err := Open(path)
	if err == nil {
		fsys.Close()
		t.Fatalf("Open succeeded on a UDIF holding no filesystem, as %s", format)
	}
	if format != FormatDMG {
		t.Errorf("format = %s, want %s: the container was still recognised", format, FormatDMG)
	}
	if !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("err = %v, want it to wrap ErrUnknownFormat", err)
	}
	if !strings.Contains(err.Error(), filepath.Base(path)) {
		t.Errorf("err = %v, want it to name the image the caller handed over", err)
	}
	if after := dmgSpools(t); after != before {
		t.Errorf("%d spool directories after the refusal, want %d: the sectors were "+
			"unpacked and left behind", after, before)
	}
}

// TestADMGIsNotAnImage pins the distinction the DMG path turns on. An image is
// read in place from the handle Open already has; a container is peeled to a
// spool. Calling a .dmg an image would hand its koly trailer to a filesystem
// driver.
func TestADMGIsNotAnImage(t *testing.T) {
	if FormatDMG.Image() {
		t.Error("FormatDMG.Image() is true: its sectors are compressed and may be " +
			"stored out of order, so there is nothing to read in place")
	}
	if !FormatHFSPlus.Image() {
		t.Error("FormatHFSPlus.Image() is false, but hfsplus reads a volume in place")
	}
}

// TestSniffNamesHFSPlus. detect already recognised HFS+ and this package
// answered "hfsplus: not implemented", which was true and is no longer.
func TestSniffNamesHFSPlus(t *testing.T) {
	// The HFS+ volume header sits at offset 1024 and starts with "H+" for the
	// format and "HX" for HFSX. 1024+512 of zeros with the signature in place is
	// not a mountable volume and does not need to be: what is under test is which
	// format the bytes are CALLED.
	img := make([]byte, 2048)
	copy(img[1024:], []byte{'H', '+', 0x00, 0x04})
	got, err := Sniff(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Sniff: %v", err)
	}
	if got != FormatHFSPlus {
		t.Errorf("Sniff = %s, want %s", got, FormatHFSPlus)
	}
}

// TestADMGWhoseSectorsAreDamagedIsRefused.
//
// The container is intact -- the trailer is there and the blkx table parses --
// and a compressed run's bytes are not. That is a different failure from "these
// sectors hold no filesystem": it happens while decoding, before there is
// anything to sniff, and it must not leave a half-written spool behind either.
func TestADMGWhoseSectorsAreDamagedIsRefused(t *testing.T) {
	before := dmgSpools(t)

	img, err := dmgImages.ReadFile("testdata/hdiutil-iso-in-udzo.dmg")
	if err != nil {
		t.Fatal(err)
	}
	damaged := append([]byte(nil), img...)
	// Well inside the data fork: past the 512-byte first sector and a long way
	// from both the plist and the koly trailer at the end.
	damaged[1000] ^= 0xFF

	// Premise: the container still parses, so what fails below is the decode of a
	// run and not the reading of the table.
	if !bytes.Equal(damaged[len(damaged)-udifTrailerLen:], img[len(img)-udifTrailerLen:]) {
		t.Fatal("the trailer was touched: this would test the sniff, not the decode")
	}

	path := filepath.Join(t.TempDir(), "damaged.dmg")
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}

	fsys, format, err := Open(path)
	if err == nil {
		fsys.Close()
		t.Fatalf("Open succeeded on a UDIF with a damaged run, as %s", format)
	}
	if format != FormatDMG {
		t.Errorf("format = %s, want %s", format, FormatDMG)
	}
	if !strings.Contains(err.Error(), "damaged.dmg") {
		t.Errorf("err = %v, want it to name the image the caller handed over", err)
	}
	if after := dmgSpools(t); after != before {
		t.Errorf("%d spool directories after the refusal, want %d", after, before)
	}
}

// TestSniffIsNotFooledByAFileTooShortToHoldATrailer. hasUDIFTrailer answers
// false for anything it cannot read, and a file of a few bytes is the case that
// says so without needing a broken reader.
func TestSniffIsNotFooledByAFileTooShortToHoldATrailer(t *testing.T) {
	if hasUDIFTrailer(bytes.NewReader([]byte("koly")), 4) {
		t.Error("four bytes spelling koly were taken for a UDIF image: a trailer " +
			"lives at the END, and there is no room for one here")
	}
}
