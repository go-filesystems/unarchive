// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// TestAnArArchiveReadsThroughTheSystemsOwnAr.
//
// The fixture is built by ar(1), so the member headers, the padding and the long
// name spelling are whichever this machine's ar uses rather than whichever this
// parser finds convenient. That matters here more than usual: ar bolted long
// names on TWICE, incompatibly, and a reader that knows only one reports names
// like "/48" without failing.
func TestAnArArchiveReadsThroughTheSystemsOwnAr(t *testing.T) {
	bin, err := exec.LookPath("ar")
	if err != nil {
		t.Skip("no ar here to build the fixture with")
	}
	// An odd-sized member, so the next header is only found if the pad byte is
	// skipped; and a name past sixteen characters, so the long-name path runs.
	want := map[string]string{
		"short.txt": "odd",
		"a-name-that-is-well-past-sixteen-characters.txt": "and a body",
		"third.bin": strings.Repeat("x", 100),
	}
	dir := t.TempDir()
	var args []string
	archive := filepath.Join(t.TempDir(), "lib.a")
	// ⛔ rcS, not rc. On macOS `ar rc` runs ranlib, which WARNS that a text file
	// is "not a mach-o file", drops every member, writes an archive holding only
	// the symbol table -- and exits 0. A fixture generator that reports success
	// without doing the work is the very thing this package exists to catch, and
	// it caught the parser first: three members missing, and the parser blamed.
	// S means "no symbol table", which is also what a real .deb has.
	args = append(args, "rcS", archive)
	for name, body := range want {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ar: %v\n%s", err, out)
	}
	// The PREMISE: ar actually stored them. Without this the test blames the
	// parser for an archive that never held the files.
	if st, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	} else if st.Size() < 200 {
		t.Fatalf("%s is only %d bytes: this ar dropped the members, so the "+
			"fixture is wrong rather than the parser", archive, st.Size())
	}

	fsys, format, err := Open(archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := fsys.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if format != FormatAr {
		t.Errorf("format = %v, want ar", format)
	}

	entries, err := fsys.ListDir("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	for name, body := range want {
		got, err := fsys.ReadFile(name)
		if err != nil {
			t.Errorf("%s: %v (the archive holds %v)", name, err, names)
			continue
		}
		if string(got) != body {
			t.Errorf("%s: %d bytes, want %d", name, len(got), len(body))
		}
	}
	// The mode is OCTAL in a header whose sizes are DECIMAL, and both are plain
	// digits: "100644" read as decimal gives 0o444 instead of 0o644, which is a
	// plausible mode and the wrong one. Nothing else here would notice.
	for name := range want {
		st, err := fsys.Stat(name)
		if err != nil {
			t.Errorf("Stat(%s): %v", name, err)
			continue
		}
		if got := st.Mode() & 0o777; got != 0o644 {
			t.Errorf("%s: mode %o, want 644 (a decimal read of the octal field "+
				"gives 444)", name, got)
		}
	}

	// A symbol table is not a file anybody asked for, so it must not appear.
	for _, n := range names {
		if n == "/" || n == "//" || strings.HasPrefix(n, "__.SYMDEF") {
			t.Errorf("the symbol table %q is listed as a member", n)
		}
	}
}

// TestADebIsAnAr: the case this earns its keep on. A .deb is an ar of three
// members, and the middle two are themselves archives this package reads.
func TestADebIsAnAr(t *testing.T) {
	bin, err := exec.LookPath("ar")
	if err != nil {
		t.Skip("no ar here")
	}
	dir := t.TempDir()
	// The three members a real .deb has, in the order dpkg requires.
	if err := os.WriteFile(filepath.Join(dir, "debian-binary"), []byte("2.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, inner := range []string{"control.tar", "data.tar"} {
		payload := filepath.Join(dir, "payload")
		if err := os.MkdirAll(payload, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(payload, inner+".txt"), []byte("inside "+inner), 0o644); err != nil {
			t.Fatal(err)
		}
		tarBin, err := exec.LookPath("tar")
		if err != nil {
			t.Skip("no tar here")
		}
		cmd := exec.Command(tarBin, "-cf", filepath.Join(dir, inner), ".")
		cmd.Dir = payload
		cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("tar: %v\n%s", err, out)
		}
		os.RemoveAll(payload)
	}
	deb := filepath.Join(t.TempDir(), "pkg.deb")
	// rcS: no symbol table, which is what a real .deb carries, and what stops
	// macOS's ranlib from dropping every member (see the test above).
	cmd := exec.Command(bin, "rcS", deb, "debian-binary", "control.tar", "data.tar")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ar: %v\n%s", err, out)
	}

	fsys, format, err := Open(deb)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if format != FormatAr {
		t.Errorf("format = %v, want ar", format)
	}
	got, err := fsys.ReadFile("debian-binary")
	if err != nil {
		t.Fatalf("debian-binary: %v", err)
	}
	if string(got) != "2.0\n" {
		t.Errorf("debian-binary = %q, want \"2.0\\n\"", got)
	}
	// And a member that is itself a tar comes out whole, so a second pass over it
	// works -- which is what somebody unpacking a .deb actually does.
	inner, err := fsys.ReadFile("data.tar")
	if err != nil {
		t.Fatalf("data.tar: %v", err)
	}
	tmp := filepath.Join(t.TempDir(), "data.tar")
	if err := os.WriteFile(tmp, inner, 0o644); err != nil {
		t.Fatal(err)
	}
	inner2, f2, err := Open(tmp)
	if err != nil {
		t.Fatalf("the extracted data.tar does not open: %v", err)
	}
	defer inner2.Close()
	if f2 != FormatTar {
		t.Errorf("the inner member is %v, want tar", f2)
	}
	if b, err := inner2.ReadFile("data.tar.txt"); err != nil || string(b) != "inside data.tar" {
		t.Errorf("inner file = %q, %v", b, err)
	}
}

// cpioVariants are the ASCII formats cpio(1) can write here.
var cpioVariants = []string{"newc", "odc"}

// TestACpioReadsThroughTheSystemsOwnCpio, over each ASCII variant.
//
// newc pads both the name and the data to four bytes and odc pads NOTHING, which
// is the difference that puts every later header out by one to three bytes if it
// is got wrong -- and reads as a corrupt archive somewhere else entirely.
func TestACpioReadsThroughTheSystemsOwnCpio(t *testing.T) {
	bin, err := exec.LookPath("cpio")
	if err != nil {
		t.Skip("no cpio here to build the fixture with")
	}
	for _, variant := range cpioVariants {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			want := map[string]string{
				"a.txt":           "odd",   // 3 bytes: the padding case
				"deep/b.txt":      "four",  // 4 bytes: already aligned
				"deep/c.txt":      "fivee", // 5 bytes
				"deep/more/d.bin": strings.Repeat("y", 300),
			}
			for name, body := range want {
				p := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			archive := filepath.Join(t.TempDir(), "arch.cpio")
			out, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			var list bytes.Buffer
			for name := range want {
				fmt.Fprintln(&list, name)
			}
			fmt.Fprintln(&list, "deep")
			cmd := exec.Command(bin, "-o", "-H", variant)
			cmd.Dir = dir
			cmd.Stdin = &list
			cmd.Stdout = out
			var errOut bytes.Buffer
			cmd.Stderr = &errOut
			if err := cmd.Run(); err != nil {
				out.Close()
				t.Skipf("this cpio will not write %s: %v\n%s", variant, err, errOut.String())
			}
			out.Close()

			fsys, format, err := Open(archive)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() {
				if err := fsys.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()
			if format != FormatCpio {
				t.Errorf("format = %v, want cpio", format)
			}
			for name, body := range want {
				got, err := fsys.ReadFile(name)
				if err != nil {
					t.Errorf("%s: %v", name, err)
					continue
				}
				if string(got) != body {
					t.Errorf("%s: %d bytes, want %d", name, len(got), len(body))
				}
			}
			// The directory the fixture named must be a directory, and the ones it
			// did not name must have been synthesised.
			for _, d := range []string{"deep", "deep/more"} {
				st, err := fsys.Stat(d)
				if err != nil {
					t.Errorf("%s: %v", d, err)
					continue
				}
				if st.Mode()&0o170000 != 0o040000 {
					t.Errorf("%s: mode %o is not a directory", d, st.Mode())
				}
			}
			// And a handle, at an offset, which is what the extractor uses.
			o, ok := fsys.(filesystem.Opener)
			if !ok {
				t.Fatalf("%T hands out no handles", fsys)
			}
			h, err := o.OpenFile("deep/more/d.bin")
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer h.Close()
			buf := make([]byte, 10)
			if _, err := h.ReadAt(buf, 290); err != nil {
				t.Fatalf("ReadAt: %v", err)
			}
			if string(buf) != strings.Repeat("y", 10) {
				t.Errorf("ReadAt(290) = %q", buf)
			}
		})
	}
}

// TestOldBinaryCpioIsRefusedBySaying: the fourth variant stores its magic as a
// 16-bit word in the writer's byte order, so the same bytes mean different things
// on different machines. Guessing is worse than refusing, and the refusal says
// which of the three sentences it is.
func TestOldBinaryCpioIsRefusedBySaying(t *testing.T) {
	// 0o070707 as a little-endian uint16 is 0xC7 0x71.
	img := append([]byte{0xC7, 0x71}, make([]byte, 200)...)
	path := filepath.Join(t.TempDir(), "old.cpio")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	// It is not even sniffed as cpio -- the ASCII magics do not match -- so this
	// records what actually happens rather than what would be nice.
	_, _, err := Open(path)
	if !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("Open = %v, want ErrUnknownFormat: old binary cpio has no ASCII magic", err)
	}
}

// TestIndexFSSynthesisesParentsAndKeepsDeclaredModes.
//
// A member called "usr/bin/tool" has to put usr and usr/bin somewhere or a walk
// of the root finds nothing. A DECLARED directory must keep its own mode, which
// means the synthesis has to run after the records and not instead of them.
func TestIndexFSSynthesisesParentsAndKeepsDeclaredModes(t *testing.T) {
	data := []byte("0123456789")
	fsys := newIndexFS(bytes.NewReader(data), []record{
		{name: "usr/bin/tool", mode: 0o755, size: 4, offset: 2},
		{name: "etc", mode: iofs.ModeDir | 0o700},
		{name: "etc/conf", mode: 0o600, size: 3, offset: 0},
	})

	for _, c := range []struct {
		name string
		dir  bool
		perm iofs.FileMode
	}{
		{"usr", true, 0o755},     // synthesised
		{"usr/bin", true, 0o755}, // synthesised
		{"etc", true, 0o700},     // declared, and its mode kept
		{"usr/bin/tool", false, 0o755},
	} {
		st, err := iofs.Stat(fsys, c.name)
		if err != nil {
			t.Errorf("Stat(%q): %v", c.name, err)
			continue
		}
		if st.IsDir() != c.dir {
			t.Errorf("%s: IsDir() = %v, want %v", c.name, st.IsDir(), c.dir)
		}
		if got := st.Mode().Perm(); got != c.perm {
			t.Errorf("%s: perm %v, want %v", c.name, got, c.perm)
		}
	}
	// The data is a section of the reader, not a copy.
	b, err := iofs.ReadFile(fsys, "usr/bin/tool")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "2345" {
		t.Errorf("read %q, want %q", b, "2345")
	}
	// And io/fs's own checker, which is stricter than anything written here.
	if err := iofs.WalkDir(fsys, ".", func(p string, d iofs.DirEntry, err error) error {
		return err
	}); err != nil {
		t.Errorf("WalkDir: %v", err)
	}
}
