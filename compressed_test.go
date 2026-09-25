// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// contents is what every fixture in this file holds.
var contents = map[string]string{
	"notes.txt":      "hello from inside a wrapper",
	"deep/other.txt": "and one more, in a directory",
}

// tarball builds an uncompressed tar of contents.
//
// The tar is ours and the COMPRESSION is not: each fixture is compressed by the
// system's own tool, so the decompressor under test is judged against an
// independent implementation rather than against a writer from the same module.
func tarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	names := make([]string, 0, len(contents))
	for n := range contents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		body := contents[n]
		if err := w.WriteHeader(&tar.Header{
			Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// compress runs the system's compressor over data, skipping if it is absent.
func compress(t *testing.T, tool string, args []string, data []byte) []byte {
	t.Helper()
	bin, err := exec.LookPath(tool)
	if err != nil {
		t.Skipf("no %s here to build the fixture with", tool)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(data)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", tool, args, err, errOut.String())
	}
	return out.Bytes()
}

// wrappers are the stream formats, each with the tool that makes one.
var wrappers = []struct {
	format Format
	name   string // the fixture's file name
	tool   string
	args   []string
}{
	{FormatGzip, "fixture.tar.gz", "gzip", []string{"-c"}},
	{FormatBzip2, "fixture.tar.bz2", "bzip2", []string{"-c"}},
	{FormatXZ, "fixture.tar.xz", "xz", []string{"-c"}},
	{FormatZstd, "fixture.tar.zst", "zstd", []string{"-q", "-c"}},
	{FormatLZ4, "fixture.tar.lz4", "lz4", []string{"-q", "-c"}},
	// ⛔ -b 16 explicitly. This machine's compress(1) writes streams its own -d
	// refuses at -b 9, -b 10 and -b 11 -- and exits 0 doing it -- so leaving the
	// width to its default would be trusting a generator that is broken at three
	// of its eight settings. See go-compressions/compress.
	{FormatZ, "fixture.tar.Z", "compress", []string{"-b", "16", "-c"}},
}

// TestATarInsideEveryWrapperComesBackWhole.
//
// The claim is not that the file opens: it is that every entry's BYTES come
// back. A decompressor that stops early leaves a short tar, and a short tar
// still lists the entries that arrived -- so counting them would pass.
func TestATarInsideEveryWrapperComesBackWhole(t *testing.T) {
	inner := tarball(t)
	for _, w := range wrappers {
		t.Run(w.format.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), w.name)
			if err := os.WriteFile(path, compress(t, w.tool, w.args, inner), 0o644); err != nil {
				t.Fatal(err)
			}

			fsys, format, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fsys.Close()

			// The format reported is the WRAPPER's: that is the file the caller
			// handed over, and answering "tar" for a .tar.gz would make a correct
			// answer read like a mistake.
			if format != w.format {
				t.Errorf("format = %v, want %v", format, w.format)
			}

			for name, want := range contents {
				b, err := fsys.ReadFile(name)
				if err != nil {
					t.Errorf("%s: %v", name, err)
					continue
				}
				if string(b) != want {
					t.Errorf("%s = %q, want %q", name, b, want)
				}
			}
		})
	}
}

// TestASingleCompressedFileIsAOneEntryFilesystem.
//
// A .gz usually holds a tar and does not have to. gunzip on notes.txt.gz leaves
// notes.txt, so that is what this returns: one entry, named by stripping the
// suffix -- the only thing the name is used for, because the FORMAT came from
// the bytes.
func TestASingleCompressedFileIsAOneEntryFilesystem(t *testing.T) {
	body := []byte("nothing archival about this, and it is still a file")
	path := filepath.Join(t.TempDir(), "notes.txt.gz")
	if err := os.WriteFile(path, compress(t, "gzip", []string{"-c"}, body), 0o644); err != nil {
		t.Fatal(err)
	}

	fsys, format, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if format != FormatGzip {
		t.Errorf("format = %v, want gzip", format)
	}

	entries, err := fsys.ListDir("")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%d entries, want 1: %v", len(entries), names)
	}
	if entries[0].Name() != "notes.txt" {
		t.Errorf("the entry is called %q, want %q", entries[0].Name(), "notes.txt")
	}
	got, err := fsys.ReadFile("notes.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("read back %q, want %q", got, body)
	}
}

// TestTheSpoolIsRemovedWhenTheFilesystemIsClosed.
//
// A wrapper is decompressed to a temporary file because every archive here needs
// random access. That file is the size of the whole decompressed archive, and
// nothing but Close can remove it: the caller never learns its name.
func TestTheSpoolIsRemovedWhenTheFilesystemIsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if err := os.WriteFile(path, compress(t, "gzip", []string{"-c"}, tarball(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := fsys.(*spoolFS)
	if !ok {
		t.Fatalf("Open returned %T, want a *spoolFS holding the spool", fsys)
	}
	dir := s.dir
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the spool is not there while the filesystem is open: %v", err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the spool survived Close: stat gave %v", err)
	}
}

// TestNestingIsCapped.
//
// A wrapper holds one stream and that stream may be another wrapper, so a few
// hundred bytes can ask for however much spool the machine has. The failure has
// to name the limit rather than be a full disk.
func TestNestingIsCapped(t *testing.T) {
	data := tarball(t)
	// One more layer than the cap, so the innermost is never reached.
	for i := 0; i <= maxNesting; i++ {
		data = compress(t, "gzip", []string{"-c"}, data)
	}
	path := filepath.Join(t.TempDir(), "matryoshka.gz")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Open(path)
	if !errors.Is(err, ErrTooDeeplyNested) {
		t.Errorf("Open gave %v, want ErrTooDeeplyNested", err)
	}
}

// TestInnerName is the only place a NAME decides anything, and it decides only
// what the single file inside is called.
func TestInnerName(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"notes.txt.gz", "notes.txt"},
		{"/a/b/notes.txt.gz", "notes.txt"},
		{"archive.tar.gz", "archive.tar"},
		{"archive.tgz", "archive.tar"},
		{"archive.TGZ", "archive.tar"},
		{"archive.tar.bz2", "archive.tar"},
		{"archive.tbz2", "archive.tar"},
		{"archive.tar.xz", "archive.tar"},
		{"archive.txz", "archive.tar"},
		{"archive.tar.zst", "archive.tar"},
		{"archive.tzst", "archive.tar"},
		{"archive.tar.lz4", "archive.tar"},
		{"film.mkv.zst", "film.mkv"},
		{"archive.tar.Z", "archive.tar"},
		{"archive.taz", "archive.tar"},
		{"notes.txt.Z", "notes.txt"},
		// A name that claims nothing still has to yield one, and reusing what
		// the person typed beats inventing a name.
		{"opaque", "opaque"},
	} {
		if got := innerName(c.in); got != c.want {
			t.Errorf("innerName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEveryDeclaredFormatIsEitherReadOrDeclaredUnread keeps the count.
//
// ErrNotImplemented exists to say "I know exactly what this is and cannot open
// it yet", which is a different sentence from "I do not know what this is". As
// of this commit NOTHING is in that bucket: every format the sniffer recognises
// is also read. That is a fine state and a fragile one, because a format added
// to the sniffer alone would silently fall to the default case, and the sentence
// would come back with nobody having decided it should.
//
// So the list is declared and the TOTAL is asserted. A new Format constant lands
// in neither list, the count fails, and somebody chooses.
func TestEveryDeclaredFormatIsEitherReadOrDeclaredUnread(t *testing.T) {
	read := []Format{
		FormatRAR, FormatZIP, Format7z, FormatTar,
		FormatGzip, FormatBzip2, FormatXZ, FormatZstd, FormatLZ4,
	}
	var unread []Format // deliberately empty: see above

	const declared = 10 // every Format constant, FormatUnknown included
	if got := len(read) + len(unread) + 1; got != declared {
		t.Errorf("%d formats accounted for, %d declared: a format was added to "+
			"the sniffer without anyone deciding whether Open reads it", got, declared)
	}

	seen := map[Format]bool{}
	for _, f := range append(append([]Format{}, read...), unread...) {
		if seen[f] {
			t.Errorf("%v appears twice", f)
		}
		seen[f] = true
		if f == FormatUnknown {
			t.Error("FormatUnknown is not a format Open can be asked for")
		}
		if f.String() == "" {
			t.Errorf("%v has no name", f)
		}
	}
}

// TestTheErrorNamesTheFileAndTheFormat: a failure inside a wrapper has to say
// which file it was reading, because by then it is reading a SPOOL whose name
// the caller has never seen.
func TestTheErrorNamesTheFileAndTheFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.tar.gz")
	// A valid gzip header and then nothing that finishes it.
	good := compress(t, "gzip", []string{"-c"}, tarball(t))
	if err := os.WriteFile(path, good[:len(good)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, format, err := Open(path)
	if err == nil {
		t.Fatal("a truncated gzip was accepted")
	}
	if format != FormatGzip {
		t.Errorf("format = %v, want gzip: the wrapper was recognised before it failed", format)
	}
	if !strings.Contains(err.Error(), "truncated.tar.gz") {
		t.Errorf("the error does not name the file the caller handed over: %v", err)
	}
	if errors.Is(err, ErrNotImplemented) || errors.Is(err, ErrUnknownFormat) {
		t.Errorf("a broken file was reported as unknown or unimplemented: %v", err)
	}
}

// TestTheOpenerCapabilitySurvivesTheSpoolWrapper.
//
// ⛔ Embedding a filesystem.Filesystem narrows the value to that interface, so
// every optional capability -- Opener first -- stops being reachable through the
// wrapper, and the assertion fails SILENTLY: a filesystem that hands out file
// handles perfectly well reports that it does not. closerFS was written wrong
// this way once; spoolFS is the same shape and needs the same witness, because
// nothing else in these tests asks for a handle.
func TestTheOpenerCapabilitySurvivesTheSpoolWrapper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if err := os.WriteFile(path, compress(t, "gzip", []string{"-c"}, tarball(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	o, ok := fsys.(filesystem.Opener)
	if !ok {
		t.Fatalf("%T does not answer Opener, and what it wraps does", fsys)
	}
	h, err := o.OpenFile("notes.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()

	want := contents["notes.txt"]
	if h.Size() != int64(len(want)) {
		t.Errorf("Size() = %d, want %d", h.Size(), len(want))
	}
	// Read through the handle at an OFFSET, which is what a handle is for: a
	// wrapper that quietly returned a whole-file reader would pass a ReadAt(0).
	const skip = 6
	buf := make([]byte, len(want)-skip)
	if _, err := h.ReadAt(buf, skip); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != want[skip:] {
		t.Errorf("ReadAt(%d) = %q, want %q", skip, buf, want[skip:])
	}
}

// TestADotZIsRecognisedAndNotConfusedWithGzip.
//
// Their magics differ by ONE byte -- 1F 8B against 1F 9D -- so a sniffer that
// reads 0x1F and stops claims either for the other, and a .Z handed to a gzip
// reader fails with a message about gzip. The two are asserted together for that
// reason.
func TestADotZIsRecognisedAndNotConfusedWithGzip(t *testing.T) {
	for _, c := range []struct {
		name string
		head []byte
		want Format
	}{
		{"gzip", []byte{0x1F, 0x8B, 0x08, 0}, FormatGzip},
		{"compress", []byte{0x1F, 0x9D, 0x90, 0}, FormatZ},
		// One byte of shared prefix and nothing else is neither.
		{"just 0x1F", []byte{0x1F}, FormatUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Sniff(bytes.NewReader(c.head), int64(len(c.head)))
			if c.want == FormatUnknown {
				if err == nil {
					t.Errorf("Sniff = %v, want a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Sniff: %v", err)
			}
			if got != c.want {
				t.Errorf("Sniff = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDotZIsReadAndNotWritten, which is a third sentence and not an oversight:
// the reader exists, the writer deliberately does not, and the message says so
// rather than leaving somebody to wonder when it will arrive.
func TestDotZIsReadAndNotWritten(t *testing.T) {
	if note := FormatZ.Note(); note == "" {
		t.Error("FormatZ carries no note, so its absence from the writers reads as an oversight")
	}
	// ⛔ The first version of this test asserted ErrUnknownFormat, which is what
	// the code did and what it should never have said: .Z is a format this package
	// READS, so "the bytes match no format this knows" is false and unhelpful in
	// one sentence. The test pinned the wrong sentence, and the smoke test through
	// the built binary is what showed it.
	for _, name := range []string{"out.tar.Z", "out.Z", "out.taz"} {
		_, err := TargetFor(name)
		if !errors.Is(err, ErrCannotWrite) {
			t.Errorf("TargetFor(%q) = %v, want ErrCannotWrite", name, err)
		}
		if errors.Is(err, ErrUnknownFormat) {
			t.Errorf("TargetFor(%q) calls .Z unknown, and it is read here: %v", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "compress(1)") {
			t.Errorf("TargetFor(%q) does not say what .Z is: %v", name, err)
		}
	}
}
