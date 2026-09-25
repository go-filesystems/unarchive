// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// TestAnISOOpensLikeAnyOtherArchive.
//
// A filesystem image is an archive in every way that matters to somebody holding
// one: a single file containing a tree, handed around to be unpacked. The
// interesting part is that nothing here decodes it -- go-filesystems already owns
// the driver, and this only has to recognise the bytes and hand them over.
//
// The fixture is built by xorriso, which is the reference-grade tool for the
// format, so the driver is judged against an image this project did not produce.
func TestAnISOOpensLikeAnyOtherArchive(t *testing.T) {
	bin, err := exec.LookPath("xorriso")
	if err != nil {
		t.Skip("no xorriso here to build an ISO with")
	}
	want := map[string]string{
		"notes.txt":      "a body in the root",
		"deep/other.txt": "and one in a directory",
	}
	src := t.TempDir()
	for name, body := range want {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := filepath.Join(t.TempDir(), "disc.iso")
	out, err := exec.Command(bin, "-as", "mkisofs", "-quiet", "-J", "-r",
		"-o", image, src).CombinedOutput()
	if err != nil {
		t.Skipf("xorriso would not build the fixture: %v\n%s", err, out)
	}

	fsys, format, err := Open(image)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// ⛔ Checked, not deferred and dropped. These drivers ADOPT the handle they
	// were opened with and forward Close to it, so a wrapper that also closes it
	// closes it twice -- and the second failure comes back from Close, which a
	// `defer fsys.Close()` throws away. An ablation that added the wrapper stayed
	// green for exactly that reason.
	defer func() {
		if err := fsys.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if format != FormatISO9660 {
		t.Errorf("format = %v, want iso9660", format)
	}
	if !format.Image() {
		t.Error("Image() says an iso9660 is not an image")
	}

	for name, body := range want {
		got, err := fsys.ReadFile(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(got) != body {
			t.Errorf("%s = %q, want %q", name, got, body)
		}
	}

	// And the capability the extractor needs: a handle, read at an offset.
	o, ok := fsys.(filesystem.Opener)
	if !ok {
		t.Fatalf("%T hands out no file handles, so Extract would stream nothing", fsys)
	}
	h, err := o.OpenFile("notes.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	const skip = 2
	buf := make([]byte, len(want["notes.txt"])-skip)
	if _, err := h.ReadAt(buf, skip); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != want["notes.txt"][skip:] {
		t.Errorf("ReadAt(%d) = %q, want %q", skip, buf, want["notes.txt"][skip:])
	}
}

// TestAnISOExtractsThroughTheCommandPath, because Open answering is not the same
// as the whole tool working: the extractor asks for capabilities the drivers
// satisfy separately.
func TestAnISOExtractsThroughTheCommandPath(t *testing.T) {
	bin, err := exec.LookPath("xorriso")
	if err != nil {
		t.Skip("no xorriso here to build an ISO with")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("extracted"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(t.TempDir(), "disc.iso")
	if out, err := exec.Command(bin, "-as", "mkisofs", "-quiet", "-o", image, src).CombinedOutput(); err != nil {
		t.Skipf("xorriso would not build the fixture: %v\n%s", err, out)
	}

	fsys, _, err := Open(image)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fsys.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	into := filepath.Join(t.TempDir(), "out")
	res, err := Extract(fsys, into, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res == nil {
		t.Fatal("Extract returned no result")
	}
	// ISO 9660 upper-cases and pads names unless Rock Ridge or Joliet is there,
	// and this fixture has neither: the entry is A.TXT;1 or A.TXT. So the test
	// looks for the CONTENT rather than pinning a spelling the format is entitled
	// to change.
	var found bool
	err = filepath.Walk(into, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if string(b) == "extracted" {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("nothing in the extracted tree holds the file's contents")
	}
}

// TestSquashFSIsRoutedToItsDriver.
//
// ⛔ This does NOT prove squashfs works, and says so. There is no mksquashfs on
// this machine -- go-filesystems/squashfs's own tests note the same and build
// synthetic images in-process -- so no fixture a real tool produced exists to
// judge against.
//
// What it does prove is the ROUTE: bytes carrying the squashfs magic are
// recognised as squashfs and handed to the squashfs driver. The image is
// deliberately truncated after its magic, so the driver is the thing that
// refuses it: an ErrUnknownFormat or an ErrNotImplemented here would mean the
// wiring is absent, and those are the failures this catches.
func TestSquashFSIsRoutedToItsDriver(t *testing.T) {
	// "hsqs" little-endian, then nothing that finishes a superblock.
	img := make([]byte, 128)
	binary.LittleEndian.PutUint32(img, 0x73717368)
	path := filepath.Join(t.TempDir(), "truncated.squashfs")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}

	format, err := Sniff(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Sniff: %v", err)
	}
	if format != FormatSquashFS {
		t.Fatalf("Sniff = %v, want squashfs", format)
	}

	_, format, err = Open(path)
	if err == nil {
		t.Fatal("a 128-byte squashfs was accepted")
	}
	if format != FormatSquashFS {
		t.Errorf("format = %v, want squashfs: the route was not taken", format)
	}
	if errors.Is(err, ErrUnknownFormat) {
		t.Errorf("reported as unknown, so squashfs is not wired: %v", err)
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Errorf("reported as unimplemented, so no driver was reached: %v", err)
	}
}
