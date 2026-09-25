// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// There is no pure-Go 7z WRITER, so the fixture is made by the 7-Zip binary
// when one is present and the test skips when it is not -- the same shape
// go-filesystems/iso9660 uses for genisoimage.
//
// Skipping is honest and it is also a gap: on a machine without 7-Zip this
// package's 7z support is compiled and not exercised. That is why the coverage
// floor in CI is where it is, and why it says so.
func sevenZip(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"7zz", "7z", "7za"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no 7-Zip binary here to write the fixture with; install one to run this")
	return ""
}

// TestInterop7zIsOpenedAndExtracted reads an archive written by the reference
// implementation, which is the only judge that counts for a format whose
// specification is its own source.
func TestInterop7zIsOpenedAndExtracted(t *testing.T) {
	bin := sevenZip(t)
	dir := t.TempDir()

	// The tree to pack, made here so the expected bytes are the test's own.
	src := filepath.Join(dir, "src")
	files := map[string]string{
		"notes.txt":           "hello",
		"photos/one.jpg":      "aaaa",
		"photos/2024/two.jpg": "bbbbbb",
	}
	for p, body := range files {
		full := filepath.Join(src, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(dir, "holiday.7z")
	cmd := exec.Command(bin, "a", "-bso0", "-bsp0", archive, ".")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("7-Zip could not write the fixture: %v\n%s", err, out)
	}

	fsys, format, err := Open(archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if format != Format7z {
		t.Errorf("format = %q, want 7z", format)
	}

	dest := filepath.Join(dir, "out")
	res, err := Extract(fsys, dest, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Files != len(files) {
		t.Errorf("%d files extracted, want %d", res.Files, len(files))
	}
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
}
