// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Six fixtures, each written by the tool that owns its format, and each read back
// by that tool before it was kept:
//
//	xar-cli.xar             /usr/bin/xar        xar -tf
//	gcab.cab                gcab 1.6           cabextract -l
//	lzop.tar.lzo            lzop 1.04          lzop -dc | tar tf -
//	lzip.tar.lz             lzip 1.26          lzip -dc | tar tf -
//	rootfiles-el9-zstd.rpm  rpmbuild, Rocky 9  (from go-filesystems/rpm, License: Public Domain)
//	warcio.warc             warcio             (from go-filesystems/warc)
//
// Embedded, because the emulated lanes run a test binary with no testdata/ beside
// it -- and because none of these tools exists on a Linux runner either, so a test
// that shelled out would skip everywhere that matters.
//
//go:embed testdata/xar-cli.xar testdata/gcab.cab
//go:embed testdata/lzop.tar.lzo testdata/lzip.tar.lz
//go:embed testdata/rootfiles-el9-zstd.rpm testdata/warcio.warc
var newFormats embed.FS

func newFormatFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := newFormats.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestTheNewFormatsOpenAndReadBack walks each one end to end: the bytes are
// sniffed as the right format, and named files come back with the right contents.
//
// The bodies are asserted, never the entry count. A count agrees with an archive
// that lists everything and reads nothing.
func TestTheNewFormatsOpenAndReadBack(t *testing.T) {
	const body = "the bytes an unarchived archive must give back\n"
	for _, c := range []struct {
		fixture string
		format  Format
		want    map[string]string
	}{
		{"xar-cli.xar", FormatXAR, map[string]string{
			"hello.txt":      body,
			"sub/nested.txt": "nested\n",
		}},
		{"gcab.cab", FormatCAB, map[string]string{
			"hello.txt":      body,
			"sub/nested.txt": "nested\n",
		}},
		// A .tar.lzo and a .tar.lz are WRAPPERS: what comes back is the tar
		// inside, so the paths are the tar's and the format reported is the
		// wrapper's.
		{"lzop.tar.lzo", FormatLZO, map[string]string{
			"hello.txt":      body,
			"sub/nested.txt": "nested\n",
		}},
		{"lzip.tar.lz", FormatLzip, map[string]string{
			"hello.txt":      body,
			"sub/nested.txt": "nested\n",
		}},
		{"rootfiles-el9-zstd.rpm", FormatRPM, map[string]string{
			// Two of the five real files in the payload, by length rather than by
			// content: they are somebody else's bytes and pinning them here would
			// be pinning a copy.
			"/usr/share/rootfiles/.bash_logout": "",
			"/usr/share/rootfiles/.bashrc":      "",
		}},
		{"warcio.warc", FormatWARC, map[string]string{
			"/00001-response-one.html": "",
			"/00002-request-one.html":  "",
		}},
	} {
		t.Run(string(c.format), func(t *testing.T) {
			path := newFormatFixture(t, c.fixture)

			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			st, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			got, err := Sniff(f, st.Size())
			f.Close()
			if err != nil {
				t.Fatalf("Sniff: %v", err)
			}
			if got != c.format {
				t.Fatalf("Sniff = %s, want %s", got, c.format)
			}

			fsys, format, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fsys.Close()
			if format != c.format {
				t.Errorf("Open format = %s, want %s", format, c.format)
			}
			for name, want := range c.want {
				b, err := fsys.ReadFile(name)
				if err != nil {
					t.Errorf("ReadFile(%q): %v", name, err)
					continue
				}
				switch {
				case want != "" && string(b) != want:
					t.Errorf("ReadFile(%q) = %q, want %q", name, b, want)
				case want == "" && len(b) == 0:
					t.Errorf("ReadFile(%q) came back empty", name)
				}
			}
		})
	}
}

// countingCloser records that it was closed, and how often.
type countingCloser struct{ closes int }

func (c *countingCloser) Close() error { c.closes++; return nil }

// TestAReaderArchiveClosesTheHandleItWasGiven.
//
// ⛔ This is the one thing about these four drivers that is easy to get wrong and
// impossible to notice. They are opened from the handle Open already has, exactly
// like an image -- and unlike an image they do NOT close it:
//
//	func (f *FS) Close() error { return nil }
//
// every one of them, measured by reading each one's Close. iso9660 and squashfs
// forward Close to what they were given, so wrapping THOSE closes the file twice
// and complains about it, late, from Close. Not wrapping THESE leaks the file --
// once per open, and nothing ever complains.
//
// So the close is watched rather than assumed. Twice over: exactly once, because
// a wrapper that forwarded twice would be the other half of the same defect.
func TestAReaderArchiveClosesTheHandleItWasGiven(t *testing.T) {
	for _, c := range []struct {
		fixture string
		format  Format
	}{
		{"xar-cli.xar", FormatXAR},
		{"gcab.cab", FormatCAB},
		{"rootfiles-el9-zstd.rpm", FormatRPM},
		{"warcio.warc", FormatWARC},
	} {
		t.Run(string(c.format), func(t *testing.T) {
			b, err := newFormats.ReadFile("testdata/" + c.fixture)
			if err != nil {
				t.Fatal(err)
			}
			closer := &countingCloser{}
			fsys, err := openReaderArchive(bytes.NewReader(b), int64(len(b)), closer, c.format)
			if err != nil {
				t.Fatalf("openReaderArchive: %v", err)
			}

			// Premise: the driver itself has not closed anything. If it had, this
			// wrapper would be the double-close half of the pair and the count
			// below would be right for the wrong reason.
			if closer.closes != 0 {
				t.Fatalf("%d closes before anyone asked for one", closer.closes)
			}
			if err := fsys.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if closer.closes != 1 {
				t.Errorf("%d closes, want exactly 1: %s", closer.closes,
					map[bool]string{
						true:  "the handle was never closed, so a real open leaks the file",
						false: "the handle was closed more than once",
					}[closer.closes == 0])
			}
		})
	}
}

// TestAnImageDriverIsNotWrappedTheSameWay is the other half, and it is why the two
// have separate doors. These adopt the reader they are given, so the wrapper above
// would close it a second time.
func TestAnImageDriverIsNotWrappedTheSameWay(t *testing.T) {
	for _, f := range []Format{FormatISO9660, FormatSquashFS, FormatHFSPlus} {
		if f.ReaderArchive() {
			t.Errorf("%s answers to ReaderArchive, and its driver closes what it "+
				"is given: wrapping it closes the file twice", f)
		}
		if !f.Image() {
			t.Errorf("%s does not answer to Image, so nothing would open it", f)
		}
	}
	for _, f := range []Format{FormatXAR, FormatCAB, FormatRPM, FormatWARC} {
		if f.Image() {
			t.Errorf("%s answers to Image, and openImage does not close the handle: "+
				"a real open would leak the file", f)
		}
		if !f.ReaderArchive() {
			t.Errorf("%s does not answer to ReaderArchive, so nothing would open it", f)
		}
	}
}

// TestTheNoteReachesAnyoneWritingARar. The write goes out stored, and the format
// is the reason rather than this package: RAR's compression is proprietary and its
// licence forecloses deriving it.
func TestTheNoteReachesAnyoneWritingARar(t *testing.T) {
	target, err := TargetFor("out.rar")
	if err != nil {
		t.Fatalf("TargetFor(out.rar) = %v, want a writable target", err)
	}
	if target.Archive != FormatRAR {
		t.Fatalf("archive = %s, want %s", target.Archive, FormatRAR)
	}
	note := FormatRAR.Note()
	if !strings.Contains(note, "no compression") {
		t.Errorf("note = %q, and it has to say that the archive goes out "+
			"uncompressed", note)
	}
	if !strings.Contains(note, "CRC-32") {
		t.Errorf("note = %q, and it should say what a stored entry DOES carry, "+
			"or it reads as though nothing can be checked", note)
	}
}

// TestEveryReaderArchiveHasADriver closes the drift between the predicate and the
// switch. ReaderArchive says a format is opened through openReaderArchiveAt; if
// the switch has no case for it, Open answers "not implemented" for a format this
// package advertises. Two doors to one semantics, which openSplit already had once.
func TestEveryReaderArchiveHasADriver(t *testing.T) {
	// Every format in the package, so a new one cannot slip past by not being
	// listed here either.
	all := []Format{
		FormatUnknown, FormatRAR, FormatZIP, Format7z, FormatTar, FormatAr,
		FormatCpio, FormatGzip, FormatBzip2, FormatXZ, FormatZstd, FormatLZ4,
		FormatZ, FormatLZO, FormatLzip, FormatISO9660, FormatSquashFS,
		FormatHFSPlus, FormatDMG, FormatXAR, FormatCAB, FormatRPM, FormatWARC,
		FormatPtar,
	}
	if len(all) != formatsInThisPackage {
		t.Fatalf("this test knows %d formats and the package has %d", len(all), formatsInThisPackage)
	}

	// A four-byte reader: every driver refuses it, and what matters is WHICH
	// refusal. ErrNotImplemented means no case exists; anything else means the
	// driver was reached and disliked the bytes, which is the answer wanted here.
	tiny := bytes.NewReader([]byte("____"))
	reached := 0
	for _, f := range all {
		_, err := openReaderArchiveAt(tiny, 4, f)
		switch {
		case f.ReaderArchive():
			if errors.Is(err, ErrNotImplemented) {
				t.Errorf("%s answers to ReaderArchive and openReaderArchiveAt has no "+
					"case for it, so Open would call it unimplemented", f)
			}
			reached++
		default:
			if !errors.Is(err, ErrNotImplemented) {
				t.Errorf("%s does not answer to ReaderArchive, and "+
					"openReaderArchiveAt opened it anyway: err = %v", f, err)
			}
		}
	}
	if reached != 4 {
		t.Errorf("%d formats answer to ReaderArchive, want 4: xar, cab, rpm, warc", reached)
	}
}

// TestOpenReaderArchiveDoesNotCloseWhatItCouldNotOpen. On failure the caller still
// holds the handle and closes it -- openAt does, right there. Closing it here too
// would be a double close, which is the defect this door exists to avoid in the
// other direction.
func TestOpenReaderArchiveDoesNotCloseWhatItCouldNotOpen(t *testing.T) {
	closer := &countingCloser{}
	_, err := openReaderArchive(bytes.NewReader([]byte("not a cabinet")), 13, closer, FormatCAB)
	if err == nil {
		t.Fatal("thirteen bytes of prose opened as a cabinet")
	}
	if closer.closes != 0 {
		t.Errorf("%d closes on a failed open: the caller closes the handle it still "+
			"holds, and closing it here makes that a double close", closer.closes)
	}
}
