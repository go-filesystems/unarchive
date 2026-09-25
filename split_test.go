// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// splitFile cuts path into numbered parts of at most n bytes and removes the
// original, returning the first part's name.
//
// Done here rather than with split(1) so the WIDTH is under the test's control:
// split's own naming differs between GNU and BSD, and this is about reading a
// set, not about spelling one.
func splitFile(t *testing.T, path string, n int) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= n {
		t.Fatalf("%s is %d bytes: splitting it into %d-byte parts gives one part, "+
			"and a single part is not a split", path, len(data), n)
	}
	for i := 0; i*n < len(data); i++ {
		part := fmt.Sprintf("%s.%03d", path, i+1)
		if err := os.WriteFile(part, data[i*n:min((i+1)*n, len(data))], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return path + ".001"
}

// TestASplitArchiveReadsAsOne, over every format whose reader takes an
// io.ReaderAt.
//
// The parts are deliberately SMALL, so the boundaries fall inside the archive's
// own structures rather than politely between them: a zip's central directory is
// at the end, a 7z's header offset points backwards from the start, and a read
// that spans two parts is the normal case.
func TestASplitArchiveReadsAsOne(t *testing.T) {
	for _, c := range []struct {
		name   string
		build  func(t *testing.T, dir string) string
		format Format
	}{
		{"zip", func(t *testing.T, dir string) string {
			return buildWith(t, dir, "zip", "source.zip", func(a, d string) []string {
				return []string{"-qr", a, "."}
			})
		}, FormatZIP},
		{"tar", func(t *testing.T, dir string) string {
			return buildWith(t, dir, "tar", "source.tar", func(a, d string) []string {
				return []string{"-cf", a, "."}
			})
		}, FormatTar},
		{"7z", func(t *testing.T, dir string) string {
			return buildWith(t, dir, "7zz", "source.7z", func(a, d string) []string {
				return []string{"a", "-bso0", "-bsp0", a, "."}
			})
		}, Format7z},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			want := splitCorpus(t, dir)
			archive := c.build(t, dir)
			first := splitFile(t, archive, 700)

			fsys, format, err := Open(first)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() {
				if err := fsys.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()
			if format != c.format {
				t.Errorf("format = %v, want %v", format, c.format)
			}
			for name, body := range want {
				got, err := fsys.ReadFile(name)
				if err != nil {
					t.Errorf("%s: %v", name, err)
					continue
				}
				if !bytes.Equal(got, []byte(body)) {
					t.Errorf("%s: %d bytes, want %d", name, len(got), len(body))
				}
			}
		})
	}
}

// splitCorpus writes the files the fixtures hold, big enough that the archive
// needs several parts.
func splitCorpus(t *testing.T, dir string) map[string]string {
	t.Helper()
	want := map[string]string{
		"a.txt":        "a short one",
		"deep/big.bin": string(filler(4000)),
		"deep/b.txt":   "and another",
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
	return want
}

// buildWith runs a system archiver over dir and returns the archive's path.
func buildWith(t *testing.T, dir, tool, name string, args func(archive, dir string) []string) string {
	t.Helper()
	bin, err := exec.LookPath(tool)
	if err != nil {
		t.Skipf("no %s here to build the fixture with", tool)
	}
	archive := filepath.Join(t.TempDir(), name)
	cmd := exec.Command(bin, args(archive, dir)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", tool, err, out)
	}
	return archive
}

// TestSevenZipsOwnVolumesStillWork.
//
// 7-Zip's -v produces exactly this shape, and its own reader follows the chain by
// NAME. Both paths now exist, so this checks the one that matters: whichever runs,
// the bytes come back. The generic split path is in fact the more robust of the
// two, because following by name breaks when a download inserts text before the
// extension -- which is the trap the rar driver exists to avoid.
func TestSevenZipsOwnVolumesStillWork(t *testing.T) {
	bin, err := exec.LookPath("7zz")
	if err != nil {
		t.Skip("no 7zz here")
	}
	dir := t.TempDir()
	want := splitCorpus(t, dir)
	archive := filepath.Join(t.TempDir(), "vol.7z")
	cmd := exec.Command(bin, "a", "-bso0", "-bsp0", "-v800b", archive, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("7zz: %v\n%s", err, out)
	}
	first := archive + ".001"
	if _, err := os.Stat(first); err != nil {
		t.Skipf("7zz did not split the fixture: %v", err)
	}
	if _, err := os.Stat(archive + ".002"); err != nil {
		t.Skip("7zz produced a single volume, so there is no set to read")
	}

	fsys, format, err := Open(first)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if format != Format7z {
		t.Errorf("format = %v, want 7z", format)
	}
	for name, body := range want {
		got, err := fsys.ReadFile(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, []byte(body)) {
			t.Errorf("%s: %d bytes, want %d", name, len(got), len(body))
		}
	}
}

// TestOnlyAFirstPartStartsASet.
//
// Handed part three, Open must read THAT file and fail on it, rather than quietly
// assembling a set the caller did not name. A tool that guesses which set a file
// belongs to will one day guess a set that is not there.
func TestOnlyAFirstPartStartsASet(t *testing.T) {
	for _, c := range []struct {
		name  string
		parts int
	}{{"single file, no number", 0}, {"a set, asked for part two", 2}} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			switch c.parts {
			case 0:
				p := filepath.Join(dir, "plain.bin")
				if err := os.WriteFile(p, []byte("nothing"), 0o644); err != nil {
					t.Fatal(err)
				}
				got, err := splitParts(p)
				if err != nil || got != nil {
					t.Errorf("splitParts(%q) = %v, %v; want nil, nil", p, got, err)
				}
			default:
				for i := 1; i <= 3; i++ {
					p := filepath.Join(dir, fmt.Sprintf("set.bin.%03d", i))
					if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				second := filepath.Join(dir, "set.bin.002")
				got, err := splitParts(second)
				if err != nil || got != nil {
					t.Errorf("splitParts(part two) = %v, %v; want nil, nil", got, err)
				}
				first := filepath.Join(dir, "set.bin.001")
				got, err = splitParts(first)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 3 {
					t.Errorf("splitParts(part one) found %d parts, want 3: %v", len(got), got)
				}
			}
		})
	}
}

// TestASetStopsAtTheFirstHole.
//
// Parts 1, 2 and 4 must be read as a set of TWO rather than three: concatenating
// across a hole gives a file of a plausible size whose middle is wrong, and every
// reader then reports corruption of an archive that is intact.
func TestASetStopsAtTheFirstHole(t *testing.T) {
	dir := t.TempDir()
	for _, i := range []int{1, 2, 4} {
		p := filepath.Join(dir, fmt.Sprintf("set.bin.%03d", i))
		if err := os.WriteFile(p, []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := splitParts(filepath.Join(dir, "set.bin.001"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("found %d parts, want 2 (the set stops before the hole): %v", len(got), got)
	}
}

// TestJoinedReadsAcrossBoundaries, on the reader alone.
//
// Every offset and every length, over parts of different sizes, compared with the
// concatenation. A read that spans a boundary is the normal case here, and the
// loop that handles it is the kind that returns short by one and looks right.
func TestJoinedReadsAcrossBoundaries(t *testing.T) {
	dir := t.TempDir()
	pieces := [][]byte{[]byte("abcde"), []byte("fg"), []byte("hijklmno"), []byte("p")}
	var whole []byte
	var parts []string
	for i, piece := range pieces {
		p := filepath.Join(dir, fmt.Sprintf("j.bin.%03d", i+1))
		if err := os.WriteFile(p, piece, 0o644); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
		whole = append(whole, piece...)
	}
	j, err := openJoined(parts)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if j.Size() != int64(len(whole)) {
		t.Fatalf("Size() = %d, want %d", j.Size(), len(whole))
	}

	for off := 0; off <= len(whole); off++ {
		for n := 1; n <= len(whole)+1; n++ {
			buf := make([]byte, n)
			got, err := j.ReadAt(buf, int64(off))
			wantN := min(n, len(whole)-off)
			if wantN < 0 {
				wantN = 0
			}
			if got != wantN {
				t.Fatalf("ReadAt(%d bytes, off %d) read %d, want %d", n, off, got, wantN)
			}
			if !bytes.Equal(buf[:got], whole[off:off+got]) {
				t.Fatalf("ReadAt(%d bytes, off %d) = %q, want %q",
					n, off, buf[:got], whole[off:off+got])
			}
			if wantN < n && !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt(%d bytes, off %d) short by %d but err = %v, want io.EOF",
					n, off, n-wantN, err)
			}
			if wantN == n && err != nil {
				t.Fatalf("ReadAt(%d bytes, off %d) filled the buffer but err = %v", n, off, err)
			}
		}
	}
	if _, err := j.ReadAt(make([]byte, 1), -1); err == nil {
		t.Error("a negative offset was accepted")
	}
}

// filler is deterministic data, compressible enough that an archiver does not
// simply store it and varied enough to fill several parts.
func filler(n int) []byte {
	b := make([]byte, n)
	x := uint32(1)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
		if i%5 == 0 {
			b[i] = 'z'
		}
	}
	return b
}

// TestPartAtAgreesWithALinearSearch.
//
// ReadAt walks forward from partAt's answer and skips parts that end before the
// offset, so too LOW is only slow -- an ablation making it always return 0 passes,
// correctly. Too HIGH skips data, and nothing else here would catch it. So it is
// pinned against the obvious implementation, over every offset.
func TestPartAtAgreesWithALinearSearch(t *testing.T) {
	dir := t.TempDir()
	sizes := []int{5, 1, 8, 1, 3}
	var parts []string
	for i, n := range sizes {
		p := filepath.Join(dir, fmt.Sprintf("p.bin.%03d", i+1))
		if err := os.WriteFile(p, bytes.Repeat([]byte{byte('a' + i)}, n), 0o644); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
	}
	j, err := openJoined(parts)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	linear := func(off int64) int {
		for i := len(j.starts) - 1; i >= 0; i-- {
			if j.starts[i] <= off {
				return i
			}
		}
		return 0
	}
	for off := int64(0); off < j.Size(); off++ {
		if got, want := j.partAt(off), linear(off); got != want {
			t.Errorf("partAt(%d) = %d, want %d", off, got, want)
		}
	}
}
