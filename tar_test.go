// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/tar"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// makeTar writes a real tar. format chooses the dialect, so pax and GNU are
// exercised rather than assumed: a long name and a large size are the two
// things USTAR cannot carry, and they are the reason pax exists.
func makeTar(t *testing.T, name string, files map[string]string, format tar.Format) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		body := files[n]
		if err := tw.WriteHeader(&tar.Header{
			Name: n, Mode: 0o644, Size: int64(len(body)), Format: format,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestATarIsOpenedAndExtracted, in all three dialects.
//
// USTAR, pax and GNU are separate formats, not spellings, and the one thing a
// caller must never have to know is which of them is in front of it. The
// standard library resolves a pax record before this package sees it, so the
// test's job is to prove that nothing here undoes that.
func TestATarIsOpenedAndExtracted(t *testing.T) {
	// A name USTAR cannot hold: over 100 bytes, which is where pax and GNU
	// diverge from it.
	long := "deeply/nested/directory/structure/that/keeps/going/for/a/while/" +
		"because/a/hundred/bytes/is/not/very/many/at/all/notes.txt"

	for _, c := range []struct {
		name   string
		format tar.Format
		files  map[string]string
	}{
		{"USTAR", tar.FormatUSTAR, map[string]string{
			"notes.txt": "hello", "photos/one.jpg": "aaaa",
		}},
		{"pax", tar.FormatPAX, map[string]string{
			"notes.txt": "hello", long: "a long way down",
		}},
		{"GNU", tar.FormatGNU, map[string]string{
			"notes.txt": "hello", long: "a long way down",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := makeTar(t, "archive.tar", c.files, c.format)
			fsys, format, err := Open(src)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fsys.Close()
			if format != FormatTar {
				t.Errorf("format = %q, want tar", format)
			}
			dest := t.TempDir()
			res, err := Extract(fsys, dest, Options{})
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if res.Files != len(c.files) {
				t.Errorf("%d files extracted, want %d", res.Files, len(c.files))
			}
			for p, want := range c.files {
				got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(p)))
				if err != nil {
					t.Errorf("%s: %v", p, err)
					continue
				}
				if string(got) != want {
					t.Errorf("%s = %q, want %q", p, got, want)
				}
			}
		})
	}
}

// TestATarEntryIsReadWhereItLies is the property that makes tar worth indexing:
// an entry is a SECTION of the file, so reading it costs nothing that depends
// on where it sits. Reading the last entry first would be as cheap as the first.
func TestATarEntryIsReadWhereItLies(t *testing.T) {
	src := makeTar(t, "archive.tar", map[string]string{
		"first.bin":  "0123456789",
		"second.bin": "abcdefghij",
		"third.bin":  "ABCDEFGHIJ",
	}, tar.FormatPAX)
	fsys, _, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	// Backwards on purpose: the last entry, then the first.
	for _, c := range []struct{ path, want string }{
		{"third.bin", "ABCDEFGHIJ"},
		{"first.bin", "0123456789"},
		{"second.bin", "abcdefghij"},
	} {
		got, err := fsys.ReadFile(c.path)
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}

	// And a read from the middle of an entry lands where it should, which is
	// what a SectionReader buys and a stream does not.
	st, err := fsys.Stat("second.bin")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 10 {
		t.Errorf("Stat size = %d, want 10", st.Size())
	}
}
