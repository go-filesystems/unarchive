package unarchive

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestTarHardlinkToMissingTargetRefused: a hard link whose target is not an
// earlier regular file is refused rather than turned into a dangling entry.
func TestTarHardlinkToMissingTargetRefused(t *testing.T) {
	for name, target := range map[string]string{"absent": "nope.txt", "directory": "d", "symlink": "sym"} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		if err := tw.WriteHeader(&tar.Header{Name: "sym", Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "l", Typeflag: tar.TypeLink, Linkname: target}); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		if _, err := openTar(bytes.NewReader(b.Bytes()), int64(b.Len()), nil); !errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: err = %v, want ErrIncomplete", name, err)
		}
	}
}

// TestTarHardlinkChain: a link to a link resolves to the first name's bytes.
func TestTarHardlinkChain(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	body := []byte("chain")
	tw.WriteHeader(&tar.Header{Name: "a", Mode: 0o644, Size: int64(len(body))})
	tw.Write(body)
	tw.WriteHeader(&tar.Header{Name: "b", Typeflag: tar.TypeLink, Linkname: "a"})
	tw.WriteHeader(&tar.Header{Name: "c", Typeflag: tar.TypeLink, Linkname: "b"})
	tw.Close()
	fsys, err := openTar(bytes.NewReader(b.Bytes()), int64(b.Len()), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		got, err := fsys.ReadFile(n)
		if err != nil || !bytes.Equal(got, body) {
			t.Errorf("%s: %q, %v", n, got, err)
		}
	}
}

// TestTarSparseOverLimitRefused: a sparse entry declaring more than
// MaxReadFile is refused, not expanded.
func TestTarSparseOverLimitRefused(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "s", Mode: 0o644, Size: 5, Format: tar.FormatGNU})
	tw.Write([]byte("HELLO"))
	tw.Close()
	raw := b.Bytes()
	patchOldGNUSparse(raw[:512], 1<<32, 5)
	old := DefaultLimits
	DefaultLimits = Limits{MaxReadFile: 1 << 20}
	defer func() { DefaultLimits = old }()
	if _, err := openTar(bytes.NewReader(raw), int64(len(raw)), nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// TestTarSparseLimitEdge: an entry expanding to exactly MaxReadFile is served, one
// byte more is refused.
func TestTarSparseLimitEdge(t *testing.T) {
	for _, tc := range []struct {
		real int64
		ok   bool
	}{{4096, true}, {4097, false}} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		tw.WriteHeader(&tar.Header{Name: "s", Mode: 0o644, Size: 5, Format: tar.FormatGNU})
		tw.Write([]byte("HELLO"))
		tw.Close()
		raw := b.Bytes()
		patchOldGNUSparse(raw[:512], tc.real, 5)
		old := DefaultLimits
		DefaultLimits = Limits{MaxReadFile: 4096}
		_, err := openTar(bytes.NewReader(raw), int64(len(raw)), nil)
		DefaultLimits = old
		if (err == nil) != tc.ok {
			t.Errorf("real %d: err = %v, want ok=%v", tc.real, err, tc.ok)
		}
	}
}

// TestTarSparseTruncated: a sparse entry whose stored data is cut off is an
// error, not a short file.
func TestTarSparseTruncated(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "s", Mode: 0o644, Size: 5, Format: tar.FormatGNU})
	tw.Write([]byte("HELLO"))
	tw.Close()
	raw := b.Bytes()
	patchOldGNUSparse(raw[:512], 100, 5)
	raw = raw[:512+3]
	_, err := openTar(bytes.NewReader(raw), int64(len(raw)), nil)
	if err == nil {
		t.Fatal("a truncated sparse entry was accepted")
	}
	// The error must come from expanding THIS entry, not from reading the header.
	if !strings.Contains(err.Error(), `sparse entry "s"`) {
		t.Fatalf("err = %v, want it to name the sparse entry", err)
	}
}

// TestTarPaxSparse10: a GNU sparse 1.0 entry (a pax record, regular typeflag) is
// expanded too.
func TestTarPaxSparse10(t *testing.T) {
	// archive/tar's Writer cannot emit GNU.sparse records or an 'x' header, so the
	// blocks are written by hand.
	var b bytes.Buffer
	m := make([]byte, 512)
	copy(m, "1\n2\n5\n")
	stored := append(m, []byte("HELLO")...)
	x := paxRec("GNU.sparse.major", "1") + paxRec("GNU.sparse.minor", "0") +
		paxRec("GNU.sparse.name", "p") + paxRec("GNU.sparse.realsize", "7")
	b.Write(rawTarHeader("PaxHeaders/p", 'x', len(x)))
	b.WriteString(x)
	b.Write(make([]byte, (512-len(x)%512)%512))
	b.Write(rawTarHeader("p", '0', len(stored)))
	b.Write(stored)
	b.Write(make([]byte, (512-len(stored)%512)%512))
	b.Write(rawTarHeader("q", '0', 3))
	b.WriteString("SEC")
	b.Write(make([]byte, 509+1024))
	fsys, err := openTar(bytes.NewReader(b.Bytes()), int64(b.Len()), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fsys.ReadFile("p")
	if err != nil || string(got) != "\x00\x00HELLO" {
		t.Fatalf("p = %q, %v", got, err)
	}
}

// paxRec spells one pax extended-header record, "len key=value\n".
func paxRec(k, v string) string {
	l := len(k) + len(v) + 3
	for {
		s := fmt.Sprintf("%d %s=%s\n", l, k, v)
		if len(s) == l {
			return s
		}
		l = len(s)
	}
}

// rawTarHeader is a ustar header block, checksummed.
func rawTarHeader(name string, typeflag byte, size int) []byte {
	h := make([]byte, 512)
	copy(h, name)
	copy(h[100:], "0000644\x00")
	copy(h[108:], "0000000\x00")
	copy(h[116:], "0000000\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	copy(h[136:], "00000000000\x00")
	h[156] = typeflag
	copy(h[257:], "ustar\x0000")
	copy(h[148:156], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

// TestTarSparseUnnamedEntry: a sparse header naming the archive root indexes
// nothing, and must not be dereferenced.
func TestTarSparseUnnamedEntry(t *testing.T) {
	var b bytes.Buffer
	h := rawTarHeader(".", 'S', 5)
	copy(h[257:], "ustar  \x00") // the GNU magic; 'S' is not a ustar typeflag
	patchOldGNUSparse(h, 5, 5)
	b.Write(h)
	b.WriteString("HELLO")
	b.Write(make([]byte, 507+1024))
	if _, err := openTar(bytes.NewReader(b.Bytes()), int64(b.Len()), nil); err != nil {
		t.Fatal(err)
	}
}
