// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPaxCarriesWhatUstarCannot.
//
// PAX is not a separate format to open -- it is tar, and its magic sits at the
// same offset -- so "we read PAX" was true here by inheritance and pinned by
// nothing. What distinguishes it is what USTAR CANNOT hold: a path longer than
// the 100-byte name field, and metadata outside ASCII. PAX puts those in an
// extended header record; a reader that ignores those records still opens the
// archive and gives back a TRUNCATED name.
//
// The fixture is built by the system's tar, not by archive/tar, because
// archive/tar is also the reader under test: a producer and consumer from one
// library agree about their own encoding whether or not it is right.
func TestPaxCarriesWhatUstarCannot(t *testing.T) {
	bin, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("no tar here to build a PAX fixture with")
	}

	// 120 characters, so the name cannot fit USTAR's field at all.
	long := strings.Repeat("a", 120) + ".txt"
	const accented = "un répertoire accentué/fichier-éàü.txt"
	want := map[string]string{
		long:     "a name longer than ustar can hold",
		accented: "and one whose bytes are not ascii",
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

	archive := filepath.Join(t.TempDir(), "pax.tar")
	cmd := exec.Command(bin, "--format=pax", "-cf", archive, "-C", src, ".")
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1") // no AppleDouble forks
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this tar will not write PAX: %v\n%s", err, out)
	}

	// The PREMISE: this really is a PAX archive. A tar that quietly wrote USTAR
	// and dropped the long-named file would leave a test that passes while
	// exercising nothing -- which is exactly what `tar --format=ustar` does with
	// this fixture, silently, and it was the first thing tried here.
	if !hasPaxHeader(t, archive) {
		t.Fatalf("%s carries no PAX extended header, so this test would prove nothing", archive)
	}

	fsys, format, err := Open(archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if format != FormatTar {
		t.Errorf("format = %v, want tar: PAX is tar, and is recognised as tar", format)
	}

	for name, body := range want {
		b, err := fsys.ReadFile(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(b) != body {
			t.Errorf("%s = %q, want %q", name, b, body)
		}
	}

	// And the long name is whole. A reader that dropped the extended header
	// would hand back the truncated one, which still READS -- so the length is
	// asserted, not just the lookup.
	entries, err := fsys.ListDir("")
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "aaa") {
			found = e.Name()
		}
	}
	if found != long {
		t.Errorf("the long entry is %q (%d chars), want %q (%d)",
			found, len(found), long, len(long))
	}
}

// hasPaxHeader says whether any block in the tar is a PAX extended header.
//
// tar is a sequence of 512-byte blocks, and a header block's type is one byte at
// offset 156. 'x' marks the record PAX puts a long name or a non-ASCII field in,
// so its presence is what makes an archive PAX rather than USTAR wearing the
// same magic.
func hasPaxHeader(t *testing.T, path string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off+512 <= len(b); off += 512 {
		if b[off+156] == 'x' || b[off+156] == 'X' {
			return true
		}
	}
	return false
}
