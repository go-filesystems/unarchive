// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// source builds the archive these tests start FROM, with the system's tar.
//
// It is deliberately not built with this package's own writers: a round trip
// that starts from our own output could agree with itself about a mistake made
// in both directions, and the whole claim here is interoperability.
func source(t *testing.T) (path string, want map[string]string) {
	t.Helper()
	bin, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("no tar here to build the fixture with")
	}
	want = map[string]string{
		"notes.txt":       "as it was",
		"replaced.txt":    "the old bytes",
		"keep/deep.txt":   "still here",
		"drop/gone.txt":   "will be deleted",
		"keep/binary.dat": strings.Repeat("\x00\xff", 2000),
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
	path = filepath.Join(t.TempDir(), "source.tar")
	cmd := exec.Command(bin, "-cf", path, "-C", src, ".")
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fixture: %v\n%s", err, out)
	}
	return path, want
}

// decompressors are the tool that undoes each stream wrapper, and they are
// spelled out rather than left to tar.
//
// ⛔ `tar -xf` on a .tar.lz4 works on macOS and FAILS on Linux: BSD tar is
// libarchive and sniffs the compression, GNU tar does not read lz4 at all
// without being told. So a test that handed the wrapped file to tar passed here
// for a reason that had nothing to do with the archive, and CI said "This does
// not look like a tar archive" on arm64.
//
// Undoing the wrapper explicitly is also a better witness: it checks the two
// layers separately, so a broken wrapper and a broken tar cannot be confused.
var decompressors = map[Format][]string{
	FormatGzip:  {"gzip", "-dc"},
	FormatXZ:    {"xz", "-dc"},
	FormatZstd:  {"zstd", "-dqc"},
	FormatLZ4:   {"lz4", "-dqc"},
	FormatBzip2: {"bzip2", "-dc"},
}

// extractors are the SYSTEM tools that judge each target format. Reading our
// own output back with our own reader would prove that two halves of this
// module agree with each other, which is not the question an archive is for.
var extractors = map[Format]func(t *testing.T, archive, into string){
	FormatTar: func(t *testing.T, archive, into string) {
		run(t, "tar", "-xf", archive, "-C", into)
	},
	FormatZIP: func(t *testing.T, archive, into string) {
		run(t, "unzip", "-q", archive, "-d", into)
	},
	Format7z: func(t *testing.T, archive, into string) {
		run(t, "7zz", "x", "-bso0", "-bsp0", "-o"+into, archive)
	},
}

// unwrap undoes a stream wrapper with its own tool and returns the plain
// archive's path. A target with no wrapper is returned unchanged.
func unwrap(t *testing.T, archive string, wrapper Format) string {
	t.Helper()
	if wrapper == FormatUnknown {
		return archive
	}
	argv, ok := decompressors[wrapper]
	if !ok {
		t.Fatalf("no tool here undoes %v, so nothing judges it", wrapper)
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		t.Skipf("no %s here to undo the wrapper with", argv[0])
	}
	plain := filepath.Join(t.TempDir(), "unwrapped.tar")
	out, err := os.Create(plain)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command(bin, append(argv[1:], archive)...)
	cmd.Stdout = out
	var errOut strings.Builder
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s could not undo the wrapper: %v\n%s", argv[0], err, errOut.String())
	}
	return plain
}

func run(t *testing.T, tool string, args ...string) {
	t.Helper()
	bin, err := exec.LookPath(tool)
	if err != nil {
		t.Skipf("no %s here to judge the archive with", tool)
	}
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", tool, args, err, out)
	}
}

// TestAnArchiveIsRewrittenOnlyAtTheSeal is the whole layer, end to end, into
// every target it can write.
//
// It asserts three things, and the middle one is the reason the layer exists:
// the changes take effect, the SOURCE is untouched until the seal, and the
// result is read by the system's own tool for that format.
func TestAnArchiveIsRewrittenOnlyAtTheSeal(t *testing.T) {
	for _, target := range []string{
		"out.tar", "out.tar.gz", "out.tgz", "out.tar.xz", "out.tar.zst",
		"out.tar.lz4", "out.tar.bz2", "out.tbz2", "out.zip", "out.7z",
	} {
		t.Run(target, func(t *testing.T) {
			archive, base := source(t)
			before, err := os.Stat(archive)
			if err != nil {
				t.Fatal(err)
			}

			o, format, err := Writable(archive)
			if err != nil {
				t.Fatalf("Writable: %v", err)
			}
			defer o.Close()
			if format != FormatTar {
				t.Errorf("opened %v, want tar", format)
			}

			// Cheap changes. None of these may touch the archive.
			if err := o.WriteFile("replaced.txt", []byte("the new bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := o.MkDir("added", 0o750); err != nil {
				t.Fatal(err)
			}
			if err := o.WriteFile("added/fresh.txt", []byte("brand new"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := o.WriteFile("added/empty.txt", nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := o.DeleteFile("drop/gone.txt"); err != nil {
				t.Fatal(err)
			}

			// The claim of a deferred write layer, asserted before the seal.
			after, err := os.Stat(archive)
			if err != nil {
				t.Fatal(err)
			}
			if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
				t.Errorf("the source changed before the seal: %v/%d -> %v/%d",
					before.ModTime(), before.Size(), after.ModTime(), after.Size())
			}

			out := filepath.Join(t.TempDir(), target)
			if err := SealTo(o, out); err != nil {
				t.Fatalf("SealTo: %v", err)
			}

			// What should be in it now.
			want := map[string]string{
				"notes.txt":       base["notes.txt"],
				"keep/deep.txt":   base["keep/deep.txt"],
				"keep/binary.dat": base["keep/binary.dat"],
				"replaced.txt":    "the new bytes",
				"added/fresh.txt": "brand new",
				"added/empty.txt": "",
			}

			tgt, err := TargetFor(target)
			if err != nil {
				t.Fatal(err)
			}
			into := filepath.Join(t.TempDir(), "extracted")
			if err := os.MkdirAll(into, 0o755); err != nil {
				t.Fatal(err)
			}
			extractors[tgt.Archive](t, unwrap(t, out, tgt.Wrapper), into)

			for name, body := range want {
				got, err := os.ReadFile(filepath.Join(into, name))
				if err != nil {
					t.Errorf("%s: %v", name, err)
					continue

				}
				if string(got) != body {
					t.Errorf("%s: %d bytes, want %d", name, len(got), len(body))
				}
			}
			if _, err := os.Stat(filepath.Join(into, "drop/gone.txt")); err == nil {
				t.Error("drop/gone.txt survived the seal, and it was deleted")
			}
			// An added directory arrives as a directory, which is the thing the
			// two builders spell differently (a trailing slash) and could each
			// get wrong alone.
			fi, err := os.Stat(filepath.Join(into, "added"))
			if err != nil {
				t.Errorf("added/: %v", err)
			} else if !fi.IsDir() {
				t.Error("added came back as a file")
			}
		})
	}
}

// TestTargetForNamesWhatItCannotWrite.
//
// Three sentences, not two: unknown, readable-but-not-writable, and fine. RAR
// has no free writer and bzip2 has no Go compressor in this module's
// dependencies -- a licensing fact and a missing library, and a caller holding
// one of them is in a different situation from a caller who typed nonsense.
func TestTargetForNamesWhatItCannotWrite(t *testing.T) {
	for _, c := range []struct {
		name    string
		archive Format
		wrapper Format
		err     error
	}{
		{"out.tar", FormatTar, FormatUnknown, nil},
		{"out.tar.gz", FormatTar, FormatGzip, nil},
		{"out.TGZ", FormatTar, FormatGzip, nil},
		{"out.tar.xz", FormatTar, FormatXZ, nil},
		{"out.txz", FormatTar, FormatXZ, nil},
		{"out.tar.zst", FormatTar, FormatZstd, nil},
		{"out.tzst", FormatTar, FormatZstd, nil},
		{"out.tar.lz4", FormatTar, FormatLZ4, nil},
		{"out.zip", FormatZIP, FormatUnknown, nil},
		{"out.jar", FormatZIP, FormatUnknown, nil},
		{"out.7z", Format7z, FormatUnknown, nil},
		{"out.tar.bz2", FormatTar, FormatBzip2, nil},
		{"out.tbz2", FormatTar, FormatBzip2, nil},
		{"out.tbz", FormatTar, FormatBzip2, nil},
		{"out.bz2", FormatTar, FormatBzip2, nil},
		// The only format left that is read and not written, and the only one
		// whose reason will not change here: no free writer exists.
		// Writable since go-filesystems/rar shipped a stored-only writer. What
		// it CANNOT do is compress, and that is said through Format.Note rather
		// than by refusing the write.
		{"out.rar", FormatRAR, FormatUnknown, nil},
		{"out.a", FormatAr, FormatUnknown, nil},
		{"out.deb", FormatAr, FormatUnknown, nil},
		{"out.cpio", FormatCpio, FormatUnknown, nil},
		{"out.iso", FormatISO9660, FormatUnknown, nil},
		{"out", FormatUnknown, FormatUnknown, ErrUnknownFormat},
	} {
		got, err := TargetFor(c.name)
		if !errors.Is(err, c.err) {
			t.Errorf("TargetFor(%q) err = %v, want %v", c.name, err, c.err)
			continue
		}
		if c.err != nil {
			continue
		}
		if got.Archive != c.archive || got.Wrapper != c.wrapper {
			t.Errorf("TargetFor(%q) = %v/%v, want %v/%v",
				c.name, got.Archive, got.Wrapper, c.archive, c.wrapper)
		}
	}
}

// TestSealToLeavesAnExistingTargetAloneWhenItFails.
//
// The seal is atomic by RENAME, and this is what that buys: a target that
// already holds something keeps holding it when the write cannot finish.
// Sealing in place would destroy the old archive in order to write the new one,
// and the window is the whole rewrite.
func TestSealToLeavesAnExistingTargetAloneWhenItFails(t *testing.T) {
	archive, _ := source(t)
	o, _, err := Writable(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	// ⛔ A .Z, not a .rar. This case needs a format the package genuinely cannot
	// write, and .rar stopped being one: it now writes, stored. Leaving it here
	// turned a test about an atomic rename into a test that overwrote the file it
	// was asserting about, and it said so -- the "held" bytes came back as a real
	// RAR archive.
	existing := filepath.Join(t.TempDir(), "precious.tar.Z")
	const held = "something that was already here"
	if err := os.WriteFile(existing, []byte(held), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SealTo(o, existing); !errors.Is(err, ErrCannotWrite) {
		t.Errorf("SealTo to a .tar.Z gave %v, want ErrCannotWrite", err)
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != held {
		t.Errorf("the target was touched: %q, want %q", got, held)
	}
}

// TestABuilderRefusesASizeThatDisagreesWithItsBytes.
//
// The contract says the reader delivers exactly size bytes, and tar RELIES on
// it: the length is already in the header when the bytes are written. So a
// mismatch has to fail HERE, with the entry's name in hand, rather than produce
// an archive whose header disagrees with its data -- which every reader reports
// as corruption, of the file, with no clue who wrote it.
func TestABuilderRefusesASizeThatDisagreesWithItsBytes(t *testing.T) {
	// Written directly against the builders, because no Overlay can produce the
	// mismatch: it always knows the size. That is the point -- the check guards
	// the CONTRACT, for whatever implements Builder next.
	for _, kind := range []Format{FormatTar, FormatZIP} {
		t.Run(kind.String(), func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "b-*")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			b, err := builderFor(kind)(f)
			if err != nil {
				t.Fatal(err)
			}
			// Declares 100, delivers 5.
			err = b.AddFile("short.txt", 0o644, 100, strings.NewReader("hello"))
			if err == nil {
				t.Fatal("a short reader was accepted, and the header already said 100")
			}
			if !strings.Contains(err.Error(), "short.txt") {
				t.Errorf("the error does not name the entry: %v", err)
			}
			if !strings.Contains(err.Error(), "100") {
				t.Errorf("the error does not say what was declared: %v", err)
			}
		})
	}
}

// TestADirectorysPermissionsSurviveEveryInputFormat.
//
// ⛔ Found by a `rm -rf` that FAILED in a smoke test, not by this suite: the
// extracted tree could not be deleted, because a 0755 directory had become
// 0555 and nothing could be removed from inside it.
//
// The cause was not in the writers. Both io/fs adapters report every directory
// as `fs.ModeDir | 0555` -- archive/zip's synthesised fileListEntry and
// bodgit/sevenzip's alike -- whatever the archive recorded, so the conversion
// was faithfully carrying a number the adapter invented. tar to tar was correct
// throughout, which is what pointed at the adapters rather than at the builders:
// the tar reader here reads real headers.
//
// The assertion is on the tar HEADER we wrote rather than on an extracted
// directory, because extraction applies the umask and would make this test
// depend on the shell that ran it.
func TestADirectorysPermissionsSurviveEveryInputFormat(t *testing.T) {
	const want = 0o755
	for _, c := range []struct {
		name string
		tool string
		args func(archive, dir string) []string
	}{
		{"zip", "zip", func(a, d string) []string { return []string{"-qr", a, "."} }},
		{"tar", "tar", func(a, d string) []string { return []string{"-cf", a, "."} }},
		{"7z", "7zz", func(a, d string) []string { return []string{"a", "-bso0", "-bsp0", a, "."} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin, err := exec.LookPath(c.tool)
			if err != nil {
				t.Skipf("no %s here", c.tool)
			}
			src := t.TempDir()
			if err := os.MkdirAll(filepath.Join(src, "deep"), want); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(src, "deep"), want); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "deep/b.txt"), []byte("body"), 0o644); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "source."+c.name)
			cmd := exec.Command(bin, c.args(archive, src)...)
			cmd.Dir = src
			cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v\n%s", err, out)
			}

			// The PREMISE: we read 0755 from the archive. Without this the test
			// could pass by writing whatever it read, having read 0555 too.
			o, _, err := Writable(archive)
			if err != nil {
				t.Fatalf("Writable: %v", err)
			}
			defer o.Close()
			st, err := o.Stat("deep")
			if err != nil {
				t.Fatalf("Stat(deep): %v", err)
			}
			// Stat's mode is a POSIX st_mode, so the permission bits are the low
			// nine and the type bits above them are not os.FileMode's.
			if got := os.FileMode(st.Mode() & 0o777); got != want {
				t.Errorf("read mode %v from the archive, want %v: the adapter's "+
					"invented mode is being carried", got, os.FileMode(want))
			}

			target := filepath.Join(t.TempDir(), "out.tar")
			if err := SealTo(o, target); err != nil {
				t.Fatalf("SealTo: %v", err)
			}
			if got := tarDirMode(t, target, "deep/"); got != want {
				t.Errorf("wrote mode %v, want %v", os.FileMode(got), os.FileMode(want))
			}
		})
	}
}

// tarDirMode reads one directory entry's recorded mode out of a tar.
func tarDirMode(t *testing.T, archive, name string) os.FileMode {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("%s holds no entry called %q", archive, name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == name {
			return os.FileMode(h.Mode).Perm()
		}
	}
}
