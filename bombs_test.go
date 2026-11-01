// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	filesystem "github.com/go-filesystems/interface"
	"github.com/klauspost/compress/zstd"
)

// Memory bombs. Each case was measured BEFORE the fix, on the commit before it,
// and the figure is in the comment above it.

const oneMiB = 1 << 20

func allocated(f func()) uint64 {
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// withLimits sets DefaultLimits for one test and restores it.
func withLimits(t *testing.T, l Limits) {
	t.Helper()
	old := DefaultLimits
	DefaultLimits = l
	t.Cleanup(func() { DefaultLimits = old })
}

func arHeader(name string, size int) []byte {
	return []byte(fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", size))
}

// Before: a 1.4 MB ar with a 220 KB string table and 20,000 long-named members
// allocated 2,490,065,968 bytes: every member copied the rest of the table
// (string(names[idx:])). Now 11 MB.
func TestBombArStringTable(t *testing.T) {
	const n = 20000
	var table bytes.Buffer
	for i := 0; i < n; i++ {
		fmt.Fprintf(&table, "name%05d/\n", i)
	}
	var ar bytes.Buffer
	ar.WriteString(arMagic)
	ar.Write(arHeader("//", table.Len()))
	ar.Write(table.Bytes())
	for i := 0; i < n; i++ {
		ar.Write(arHeader(fmt.Sprintf("/%d", i*10), 0))
	}
	var fsys any
	var err error
	got := allocated(func() { fsys, err = openAr(bytes.NewReader(ar.Bytes()), int64(ar.Len()), nil) })
	if err != nil {
		t.Fatal(err)
	}
	if got > 64*oneMiB {
		t.Errorf("allocated %d bytes for a %d-byte archive", got, ar.Len())
	}
	_ = fsys
}

// Before: a 400 KB ar holding one member with a 400,001-byte BSD name ran past
// 4 m 40 s (killed), synthesising 200,000 parent directories with paths of up
// to 200 KB each. Now refused in under a millisecond.
func TestBombArDeepName(t *testing.T) {
	name := strings.Repeat("a/", 200000) + "x"
	var ar bytes.Buffer
	ar.WriteString(arMagic)
	ar.Write(arHeader(fmt.Sprintf("#1/%d", len(name)), len(name)))
	ar.WriteString(name)
	var err error
	got := allocated(func() { _, err = openAr(bytes.NewReader(ar.Bytes()), int64(ar.Len()), nil) })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got >= 4*oneMiB {
		t.Errorf("allocated %d bytes", got)
	}
}

func TestIndexPathLengthIsExact(t *testing.T) {
	at := strings.Repeat("x", maxEntryPath)
	if _, err := newIndexFS(nil, []record{{name: at, mode: 0o644}}); err != nil {
		t.Errorf("a %d-byte path: %v", maxEntryPath, err)
	}
	if _, err := newIndexFS(nil, []record{{name: at + "x", mode: 0o644}}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a %d-byte path: %v, want ErrTooLarge", maxEntryPath+1, err)
	}
	// An implied parent is charged as well.
	if _, err := newIndexFS(nil, []record{{name: at + "x/y", mode: 0o644}}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a path whose parent is too long: %v, want ErrTooLarge", err)
	}
}

func TestIndexEntryLimitIsExact(t *testing.T) {
	recs := func(n int) []record {
		out := make([]record, n)
		for i := range out {
			out[i] = record{name: fmt.Sprintf("f%d", i), mode: 0o644}
		}
		return out
	}
	withLimits(t, Limits{MaxEntries: 10})
	if _, err := newIndexFS(nil, recs(10)); err != nil {
		t.Errorf("10 entries with MaxEntries=10: %v", err)
	}
	if _, err := newIndexFS(nil, recs(11)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("11 entries: %v, want ErrTooLarge", err)
	}
	// Implied directories count: "a/b/c" is three paths.
	if _, err := newIndexFS(nil, []record{{name: "a/b/c", mode: 0o644}}); err != nil {
		t.Errorf("three paths with MaxEntries=10: %v", err)
	}
	withLimits(t, Limits{MaxEntries: 2})
	if _, err := newIndexFS(nil, []record{{name: "a/b/c", mode: 0o644}}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("three paths with MaxEntries=2: %v, want ErrTooLarge", err)
	}
}

func TestIndexPathBudgetIsExact(t *testing.T) {
	for _, tc := range []struct {
		budget int
		ok     bool
	}{{6, true}, {5, false}} {
		b := &indexBudget{maxEntries: 100, maxPathBytes: tc.budget}
		var err error
		for _, p := range []string{"ab", "cd", "ef"} {
			err = errors.Join(err, b.charge(p))
		}
		if tc.ok && err != nil {
			t.Errorf("budget %d: %v", tc.budget, err)
		}
		if !tc.ok && !errors.Is(err, ErrTooLarge) {
			t.Errorf("budget %d: %v, want ErrTooLarge", tc.budget, err)
		}
	}
}

func TestADuplicateNameIsListedOnceAndKeepsTheLaterRecord(t *testing.T) {
	recs := []record{
		{name: "d/x", size: 1, mode: 0o644},
		{name: "d/x", size: 2, mode: 0o644},
		{name: "d/y", size: 3, mode: 0o644},
		{name: "d", mode: iofs.ModeDir | 0o700},
		{name: "d/x", size: 4, mode: 0o644},
	}
	fsys, err := newIndexFS(bytes.NewReader(make([]byte, 16)), recs)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := iofs.ReadDir(fsys, "d")
	if err != nil || len(ents) != 2 || ents[0].Name() != "x" || ents[1].Name() != "y" {
		t.Fatalf("ReadDir(d) = %v, %v", ents, err)
	}
	st, _ := ents[0].Info()
	if st.Size() != 4 {
		t.Errorf("the later record must win: size %d", st.Size())
	}
	if d, _ := iofs.Stat(fsys, "d"); d.Mode().Perm() != 0o700 {
		t.Errorf("a declared directory keeps its own mode: %v", d.Mode())
	}
}

func TestANegativeDeclaredSizeIsRefused(t *testing.T) {
	if _, err := newIndexFS(nil, []record{{name: "a", size: -1, mode: 0o644}}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("err = %v, want ErrIncomplete", err)
	}
	if err := (&tarFS{budget: newIndexBudget()}).add(&tar.Header{Name: "a", Size: -1}, 0); !errors.Is(err, ErrIncomplete) {
		t.Errorf("tar: err = %v, want ErrIncomplete", err)
	}
}

func tarOf(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Before: 1000 tar entries 1000 directories deep (a 3 MB tar) allocated
// 435,161,568 bytes in 2.8 s; the path budget stops it at 107 MB in 0.5 s.
func TestBombTarDeepPaths(t *testing.T) {
	deep := strings.Repeat("a/", 1000)
	names := make([]string, 1000)
	for i := range names {
		names[i] = fmt.Sprintf("%d/%sf", i, deep)
	}
	raw := tarOf(t, names...)
	var err error
	n := allocated(func() { _, err = openTar(bytes.NewReader(raw), int64(len(raw)), nil) })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if n > 200*oneMiB {
		t.Errorf("allocated %d bytes", n)
	}
}

func TestTarEntryLimitAndOrder(t *testing.T) {
	// Every name twice, in two directories, one of them implied.
	raw := tarOf(t, "d/b", "d/a", "d/b", "d/c/e", "z")
	withLimits(t, Limits{MaxEntries: 6})
	fsys, err := openTar(bytes.NewReader(raw), int64(len(raw)), nil)
	if err != nil {
		t.Fatalf("six paths with MaxEntries=6: %v", err)
	}
	ents, err := fsys.ListDir("d")
	if err != nil || len(ents) != 3 || ents[0].Name() != "a" || ents[1].Name() != "b" || ents[2].Name() != "c" {
		t.Errorf("ListDir(d) = %v, %v", ents, err)
	}
	withLimits(t, Limits{MaxEntries: 5})
	if _, err := openTar(bytes.NewReader(raw), int64(len(raw)), nil); !errors.Is(err, ErrTooLarge) {
		t.Errorf("six paths with MaxEntries=5: %v, want ErrTooLarge", err)
	}
}

func TestTarReadFileCeilingIsExact(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "a", Mode: 0o644, Size: 10, Typeflag: tar.TypeReg})
	tw.Write([]byte("0123456789"))
	tw.Close()
	for _, tc := range []struct {
		max int64
		ok  bool
	}{{10, true}, {9, false}} {
		withLimits(t, Limits{MaxReadFile: tc.max})
		fsys, err := openTar(bytes.NewReader(buf.Bytes()), int64(buf.Len()), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fsys.ReadFile("a")
		if tc.ok && (err != nil || string(got) != "0123456789") {
			t.Errorf("MaxReadFile=%d: %q, %v", tc.max, got, err)
		}
		if !tc.ok && !errors.Is(err, ErrTooLarge) {
			t.Errorf("MaxReadFile=%d: %v, want ErrTooLarge", tc.max, err)
		}
	}
}

// Before: a 218-byte zip declaring 4 GiB made ReadFile allocate 4,294,975,728
// bytes, because io/fs.ReadFile reserves the size Stat reports.
func TestBombZipReadFileDeclaredSize(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "a", Method: zip.Store, CompressedSize64: 10, UncompressedSize64: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("0123456789"))
	zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var rerr error
	n := allocated(func() { _, rerr = FromFS(zr).ReadFile("a") })
	if !errors.Is(rerr, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", rerr)
	}
	if n >= oneMiB {
		t.Errorf("allocated %d bytes", n)
	}
}

// lyingFS declares size 0 for a file that holds twenty bytes.
type lyingFS struct{}

type lyingFile struct{ r io.Reader }

type lyingInfo struct{}

func (lyingFS) Open(string) (iofs.File, error) {
	return &lyingFile{bytes.NewReader(bytes.Repeat([]byte("x"), 20))}, nil
}
func (f *lyingFile) Read(p []byte) (int, error)   { return f.r.Read(p) }
func (f *lyingFile) Close() error                 { return nil }
func (f *lyingFile) Stat() (iofs.FileInfo, error) { return lyingInfo{}, nil }
func (lyingInfo) Name() string                    { return "a" }
func (lyingInfo) Size() int64                     { return 0 }
func (lyingInfo) Mode() iofs.FileMode             { return 0o644 }
func (lyingInfo) ModTime() time.Time              { return time.Time{} }
func (lyingInfo) IsDir() bool                     { return false }
func (lyingInfo) Sys() any                        { return nil }

func TestFromFSReadFileCeilingIsExact(t *testing.T) {
	m := fstest.MapFS{"a": {Data: []byte("0123456789")}}
	for _, tc := range []struct {
		max int64
		ok  bool
	}{{10, true}, {9, false}} {
		withLimits(t, Limits{MaxReadFile: tc.max})
		got, err := FromFS(m).ReadFile("a")
		if tc.ok && (err != nil || string(got) != "0123456789") {
			t.Errorf("MaxReadFile=%d: %q, %v", tc.max, got, err)
		}
		if !tc.ok && !errors.Is(err, ErrTooLarge) {
			t.Errorf("MaxReadFile=%d: %v, want ErrTooLarge", tc.max, err)
		}
	}
	// A declaration of 0 does not excuse twenty bytes.
	withLimits(t, Limits{MaxReadFile: 19})
	if _, err := FromFS(lyingFS{}).ReadFile("a"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an entry yielding more than it declared: %v, want ErrTooLarge", err)
	}
	withLimits(t, Limits{MaxReadFile: 20})
	if got, err := FromFS(lyingFS{}).ReadFile("a"); err != nil || len(got) != 20 {
		t.Errorf("at the limit: %d bytes, %v", len(got), err)
	}
	if _, err := FromFS(fstest.MapFS{}).ReadFile("missing"); err == nil {
		t.Error("a missing file must be an error")
	}
}

type statFailFS struct{}
type statFailFile struct{}

func (statFailFS) Open(string) (iofs.File, error) { return statFailFile{}, nil }
func (statFailFile) Read([]byte) (int, error)     { return 0, io.EOF }
func (statFailFile) Close() error                 { return nil }
func (statFailFile) Stat() (iofs.FileInfo, error) { return nil, errors.New("stat failed") }

func TestFromFSReadFileStatFailure(t *testing.T) {
	if _, err := FromFS(statFailFS{}).ReadFile("a"); err == nil {
		t.Error("a failing Stat must be reported")
	}
}

// Before: nine bytes (an LZMA preamble naming a 1 GiB dictionary) made a zip
// entry allocate 1,073,769,696 bytes. xz's lzma.NewReader treats DictCap as a
// limit on the header and defaults it to 2 GiB.
func TestBombZipLZMADictionary(t *testing.T) {
	head := func(dict uint32) []byte {
		var in bytes.Buffer
		in.Write([]byte{0, 0, 5, 0}) // versions, 5 property bytes
		in.WriteByte(0x5d)
		binary.Write(&in, binary.LittleEndian, dict)
		return in.Bytes()
	}
	withLimits(t, Limits{MaxDictionary: 1 << 20})
	var err error
	n := allocated(func() { _, err = newZipLZMA(bytes.NewReader(head(1 << 30))) })
	over := func(err error) bool { return err != nil && strings.Contains(err.Error(), "exceeds configured dictionary") }
	if !over(err) {
		t.Fatalf("a 1 GiB dictionary must be refused as too large: %v", err)
	}
	if n >= oneMiB {
		t.Errorf("allocated %d bytes", n)
	}
	if _, err := newZipLZMA(bytes.NewReader(head(1<<20 + 1))); !over(err) {
		t.Errorf("one byte over the limit must be refused: %v", err)
	}
	// Exactly the limit gets past the dictionary check and then meets the end of a
	// stream that holds only a header: EOF, not a refusal.
	if _, err := newZipLZMA(bytes.NewReader(head(1 << 20))); over(err) {
		t.Errorf("exactly the limit must not be refused: %v", err)
	}
}

// A zstd frame whose window is over the limit is refused instead of reserved.
func TestZstdWindowIsLimited(t *testing.T) {
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf, zstd.WithWindowSize(1<<20), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	zw.Write(bytes.Repeat([]byte("abcdefgh"), 1<<19)) // 4 MiB, so the frame needs its window
	zw.Close()
	read := func() error {
		r, closeZ, err := decompressor(FormatZstd, bytes.NewReader(buf.Bytes()))
		if err != nil {
			return err
		}
		defer closeZ()
		_, err = io.Copy(io.Discard, r)
		return err
	}
	withLimits(t, Limits{MaxDictionary: 1 << 22})
	if err := read(); err != nil {
		t.Errorf("a window under the limit: %v", err)
	}
	withLimits(t, Limits{MaxDictionary: 1 << 19})
	if err := read(); err == nil {
		t.Error("a window over the limit must be refused")
	}
}

// A stream wrapper decompresses to disk; the ceiling stops a gzip bomb filling it.
func TestBombSpoolCeilingIsExact(t *testing.T) {
	dir := t.TempDir()
	gz := func(n int) string {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(bytes.Repeat([]byte("z"), n))
		zw.Close()
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt.gz", n))
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	withLimits(t, Limits{MaxSpool: 100})
	fsys, _, err := Open(gz(100))
	if err != nil {
		t.Fatalf("exactly the ceiling: %v", err)
	}
	fsys.Close()
	if _, _, err := Open(gz(101)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one byte over: %v, want ErrTooLarge", err)
	}
	// No spool is left behind by the refusal.
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "unarchive-spool-*"))
	for _, d := range left {
		if st, err := os.Stat(d); err == nil && time.Since(st.ModTime()) < time.Minute {
			t.Errorf("spool directory %s survived a refused open", d)
		}
	}
}

func extractTree() filesystem.Filesystem {
	return FromFS(fstest.MapFS{
		"a":       {Data: []byte("0123456789")},
		"b":       {Data: []byte("0123456789")},
		"d/e/f/g": {Data: []byte("x")},
	})
}

// Extract had no ceiling on the bytes it wrote, the entries it made or the depth
// it recursed to: the first fills a disk, the last is a fatal stack overflow.
func TestExtractByteCeilingIsExact(t *testing.T) {
	// a and b are ten bytes each, d/e/f/g one: 21 in all.
	res, err := Extract(extractTree(), t.TempDir(), Options{MaxBytes: 21})
	if err != nil || res.Bytes != 21 {
		t.Fatalf("exactly the ceiling: %+v, %v", res, err)
	}
	if _, err := Extract(extractTree(), t.TempDir(), Options{MaxBytes: 20}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one byte over: %v, want ErrTooLarge", err)
	}
	if res, err := Extract(extractTree(), t.TempDir(), Options{MaxBytes: -1}); err != nil || res.Bytes != 21 {
		t.Errorf("a negative ceiling removes it: %+v, %v", res, err)
	}
}

func TestExtractEntryCeilingIsExact(t *testing.T) {
	// d, d/e, d/e/f, a, b, d/e/f/g: six.
	if _, err := Extract(extractTree(), t.TempDir(), Options{MaxEntries: 6}); err != nil {
		t.Errorf("exactly the ceiling: %v", err)
	}
	if _, err := Extract(extractTree(), t.TempDir(), Options{MaxEntries: 5}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one over: %v, want ErrTooLarge", err)
	}
	if _, err := Extract(extractTree(), t.TempDir(), Options{MaxEntries: -1}); err != nil {
		t.Errorf("a negative ceiling removes it: %v", err)
	}
}

func TestExtractDepthCeilingIsExact(t *testing.T) {
	// The root is depth 0; d/e/f, the deepest directory, is listed at depth 3.
	if _, err := Extract(extractTree(), t.TempDir(), Options{MaxDepth: 3}); err != nil {
		t.Errorf("exactly the ceiling: %v", err)
	}
	if _, err := Extract(extractTree(), t.TempDir(), Options{MaxDepth: 2}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one level over: %v, want ErrTooLarge", err)
	}
	if _, err := Extract(extractTree(), t.TempDir(), Options{MaxDepth: -1}); err != nil {
		t.Errorf("a negative ceiling removes it: %v", err)
	}
}

func TestDefaultsApplyWhenAFieldIsZero(t *testing.T) {
	if limit(0, 7) != 7 || limit(-3, 7) != -1 || limit(5, 7) != 5 {
		t.Error("limit: zero is the default, negative is none, positive is itself")
	}
	if got := (Limits{}).resolved(); got.MaxEntries != 1<<20 || got.MaxReadFile != 1<<30 ||
		got.MaxSpool != 64<<30 || got.MaxDictionary != 256<<20 {
		t.Errorf("zero Limits resolved to %+v", got)
	}
	if got := (Limits{MaxEntries: 3, MaxReadFile: 4, MaxSpool: 5, MaxDictionary: 6}).resolved(); got != (Limits{3, 4, 5, 6}) {
		t.Errorf("explicit Limits changed: %+v", got)
	}
}

// bytesFile is a filesystem.File over memory.
type bytesFile struct{ *bytes.Reader }

func (bytesFile) Close() error  { return nil }
func (f bytesFile) Size() int64 { return f.Reader.Size() }

func TestCopyEntryStopsAtTheLimit(t *testing.T) {
	data := []byte("0123456789")
	for _, tc := range []struct {
		limit   int64
		want    int64
		tooMany bool
	}{{-1, 10, false}, {10, 10, false}, {11, 10, false}, {9, 9, true}, {0, 0, true}} {
		var out bytes.Buffer
		n, err := copyEntry(&out, bytesFile{bytes.NewReader(data)}, tc.limit)
		if n != tc.want || int64(out.Len()) != tc.want {
			t.Errorf("limit %d: wrote %d (%d in the buffer), want %d", tc.limit, n, out.Len(), tc.want)
		}
		if tc.tooMany != errors.Is(err, ErrTooLarge) {
			t.Errorf("limit %d: err = %v, want ErrTooLarge=%v", tc.limit, err, tc.tooMany)
		}
	}
}

// FuzzOpenContiguous indexes arbitrary bytes as ar, tar and cpio under small
// limits and reads what it finds.
func FuzzOpenContiguous(f *testing.F) {
	var ar bytes.Buffer
	ar.WriteString(arMagic)
	ar.Write(arHeader("a/", 3))
	ar.WriteString("abc\n")
	f.Add(ar.Bytes())
	f.Add([]byte(arMagic))
	f.Add(tarOfFuzz("d/a", "d/b", "c"))
	f.Add([]byte("070701"))
	f.Fuzz(func(t *testing.T, data []byte) {
		old := DefaultLimits
		DefaultLimits = Limits{MaxEntries: 1000, MaxReadFile: oneMiB, MaxSpool: oneMiB, MaxDictionary: oneMiB}
		defer func() { DefaultLimits = old }()
		for _, open := range []func() (filesystem.Filesystem, error){
			func() (filesystem.Filesystem, error) { return openAr(bytes.NewReader(data), int64(len(data)), nil) },
			func() (filesystem.Filesystem, error) { return openTar(bytes.NewReader(data), int64(len(data)), nil) },
			func() (filesystem.Filesystem, error) { return openCpio(bytes.NewReader(data), int64(len(data)), nil) },
		} {
			fsys, err := open()
			if err != nil {
				continue
			}
			var walk func(dir string, depth int)
			walk = func(dir string, depth int) {
				ents, err := fsys.ListDir(dir)
				if err != nil || depth > 16 {
					return
				}
				for _, e := range ents {
					p := strings.TrimPrefix(dir+"/"+e.Name(), "/")
					if e.FileType() == fileTypeDir {
						walk(p, depth+1)
						continue
					}
					_, _ = fsys.ReadFile(p)
				}
			}
			walk("", 0)
		}
	})
}

func tarOfFuzz(names ...string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Typeflag: tar.TypeReg})
	}
	tw.Close()
	return buf.Bytes()
}
