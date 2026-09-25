// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// makeZip writes a real zip the test can then be made to open the ordinary way.
// The point is that nothing here is a double: archive/zip writes it and
// archive/zip reads it back through the adapter, so what is proven is the whole
// path a caller uses.
func makeZip(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAZipIsOpenedListedAndExtracted is the first end-to-end run in this
// package: a real archive, a real decoder, and the extraction check that gives
// the package its reason to exist.
func TestAZipIsOpenedListedAndExtracted(t *testing.T) {
	src := makeZip(t, "holiday.zip", map[string]string{
		"notes.txt":           "hello",
		"photos/one.jpg":      "aaaa",
		"photos/2024/two.jpg": "bbbbbb",
	})

	fsys, format, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if format != FormatZIP {
		t.Errorf("format = %q, want zip", format)
	}

	// The name said .zip and so did the bytes; the bytes are what decided.
	// Renamed, it still opens.
	renamed := filepath.Join(filepath.Dir(src), "holiday.rar")
	if err := os.Link(src, renamed); err == nil {
		fs2, f2, err := Open(renamed)
		if err != nil {
			t.Errorf("a zip named .rar was refused: %v", err)
		} else {
			fs2.Close()
			if f2 != FormatZIP {
				t.Errorf("a zip named .rar was read as %q", f2)
			}
		}
	}

	entries, err := List(fsys)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var files, dirs int
	for _, e := range entries {
		if e.Dir {
			dirs++
			continue
		}
		files++
	}
	if files != 3 {
		t.Errorf("%d files listed, want 3: %+v", files, entries)
	}
	if dirs != 2 {
		t.Errorf("%d directories listed, want photos and photos/2024", dirs)
	}

	dest := t.TempDir()
	res, err := Extract(fsys, dest, Options{Overwrite: true})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Files != 3 {
		t.Errorf("%d files extracted, want 3", res.Files)
	}
	for p, want := range map[string]string{
		"notes.txt":           "hello",
		"photos/one.jpg":      "aaaa",
		"photos/2024/two.jpg": "bbbbbb",
	} {
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

// TestAnEmptyZipIsStillAZip: PK\x05\x06 is an archive holding nothing, and
// "nothing to extract" must not read as "I do not know what this is".
func TestAnEmptyZipIsStillAZip(t *testing.T) {
	src := makeZip(t, "empty.zip", nil)
	fsys, format, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if format != FormatZIP {
		t.Errorf("format = %q, want zip", format)
	}
	entries, err := List(fsys)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("an empty zip listed %d entries", len(entries))
	}
	res, err := Extract(fsys, t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Files != 0 {
		t.Errorf("%d files from an empty zip", res.Files)
	}
}

// TestTheAdapterIsReadOnly: the mutating half answers by name, as everywhere
// else in this org.
func TestTheAdapterIsReadOnly(t *testing.T) {
	src := makeZip(t, "x.zip", map[string]string{"a.txt": "x"})
	fsys, _, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	for name, err := range map[string]error{
		"WriteFile":  fsys.WriteFile("a.txt", nil, 0),
		"MkDir":      fsys.MkDir("d", 0),
		"DeleteFile": fsys.DeleteFile("a.txt"),
		"DeleteDir":  fsys.DeleteDir("d"),
		"Rename":     fsys.Rename("a.txt", "b.txt"),
	} {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s gave %v, want ErrReadOnly", name, err)
		}
	}
	st, err := fsys.Stat("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	// POSIX st_mode: a regular file, with its permission bits.
	if st.Mode()&0o170000 != 0o100000 {
		t.Errorf("Stat mode %#o does not say regular file", st.Mode())
	}
}
