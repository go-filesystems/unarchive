package unarchive

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func auditExtract(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	fsys, format, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	out := filepath.Join(dir, "out")
	res, err := Extract(fsys, out, Options{})
	t.Logf("format %s, Extract result %+v, err %v", format, res, err)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return out
}

// TestAuditTarHardlink: a tar holding a regular file and a HARD link to it
// (typeflag '1'), which is what `tar c` writes for any two names of one inode.
// The link must come out holding the file's bytes, or the extraction must fail.
func TestAuditTarHardlink(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	body := []byte("hello hardlink\n")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Name: "data/a.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
	_, err := tw.Write(body)
	must(err)
	must(tw.WriteHeader(&tar.Header{Name: "links/b.txt", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "data/a.txt"}))
	must(tw.Close())

	out := auditExtract(t, "hl.tar", b.Bytes())
	link := filepath.Join(out, "links", "b.txt")
	li, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the hard link was not extracted: %v", err)
	}
	t.Logf("links/b.txt on disk: mode %v", li.Mode())
	if li.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(link)
		t.Logf("it is a SYMBOLIC link to %q", target)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("Extract reported success, and links/b.txt cannot be read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("links/b.txt holds %q, want %q", got, body)
	}
}

func auditNewc(b *bytes.Buffer, name string, ino, mode, nlink int, data []byte) {
	fmt.Fprintf(b, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		ino, mode, 0, 0, nlink, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
	b.WriteString(name)
	b.WriteByte(0)
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
	b.Write(data)
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

// TestAuditCpioNewcHardlink: in the newc format every link of a multiply-linked
// inode but the LAST is written with filesize 0, and the data follows the last
// one (cpio(5), "New ASCII Format"; this is what GNU cpio and rpm write). The
// first name must come out with the bytes, or the extraction must fail.
func TestAuditCpioNewcHardlink(t *testing.T) {
	var b bytes.Buffer
	body := []byte("#!/bin/sh\necho the real program\n")
	auditNewc(&b, "bin/first", 42, 0o100755, 2, nil)
	auditNewc(&b, "bin/second", 42, 0o100755, 2, body)
	auditNewc(&b, "TRAILER!!!", 0, 0, 1, nil)

	out := auditExtract(t, "hl.cpio", b.Bytes())
	for _, n := range []string{"second", "first"} {
		got, err := os.ReadFile(filepath.Join(out, "bin", n))
		if err != nil {
			t.Fatalf("bin/%s: %v", n, err)
		}
		t.Logf("bin/%s: %d bytes", n, len(got))
		if !bytes.Equal(got, body) {
			t.Errorf("bin/%s holds %d bytes, want the %d bytes of its inode; Extract reported success", n, len(got), len(body))
		}
	}
}

// TestAuditTarGNUSparse: an old-GNU sparse entry (typeflag 'S') stores 5 bytes
// and declares a real size of 4101: 4096 zeros, then "HELLO". The entry after it
// is another member's secret.
func TestAuditTarGNUSparse(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Name: "sparse.bin", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg, Format: tar.FormatGNU}))
	_, err := tw.Write([]byte("HELLO"))
	must(err)
	secret := bytes.Repeat([]byte("SECRET-OF-ANOTHER-MEMBER "), 200)
	must(tw.WriteHeader(&tar.Header{Name: "other.txt", Mode: 0o600, Size: int64(len(secret)), Typeflag: tar.TypeReg, Format: tar.FormatGNU}))
	_, err = tw.Write(secret)
	must(err)
	must(tw.Close())
	raw := b.Bytes()

	// Patch header 0 into an old-GNU sparse header.
	h := raw[:512]
	h[156] = 'S'
	copy(h[386:], fmt.Sprintf("%011o\x00%011o\x00", 4096, 5)) // sparse[0]: offset, numbytes
	h[482] = 0                                                // isextended
	copy(h[483:], fmt.Sprintf("%011o\x00", 4101))             // realsize
	copy(h[148:156], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:156], fmt.Sprintf("%06o\x00 ", sum))

	// Oracle: archive/tar itself.
	tr := tar.NewReader(bytes.NewReader(raw))
	hd, err := tr.Next()
	must(err)
	want := make([]byte, hd.Size)
	if _, err := tr.Read(want); err != nil && len(want) == 0 {
		t.Fatal(err)
	}
	want = want[:0]
	tr = tar.NewReader(bytes.NewReader(raw))
	_, err = tr.Next()
	must(err)
	var wb bytes.Buffer
	_, err = wb.ReadFrom(tr)
	must(err)
	want = wb.Bytes()
	if len(want) != 4101 || string(want[4096:]) != "HELLO" {
		t.Fatalf("the fixture is wrong: archive/tar reads %d bytes", len(want))
	}

	out := auditExtract(t, "sparse.tar", raw)
	got, err := os.ReadFile(filepath.Join(out, "sparse.bin"))
	must(err)
	t.Logf("sparse.bin: %d bytes on disk, first 16: %q", len(got), got[:min(16, len(got))])
	if bytes.Contains(got, []byte("SECRET-OF-ANOTHER-MEMBER")) {
		t.Errorf("sparse.bin contains the bytes of other.txt")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("sparse.bin differs from what archive/tar decodes (4096 zeros + HELLO); Extract reported success")
	}
}

// patchOldGNUSparse turns a plain header into an old-GNU sparse one whose single
// data run sits at offset realsize-n, and fixes the checksum.
func patchOldGNUSparse(h []byte, realsize, n int64) {
	h[156] = 'S'
	copy(h[386:], fmt.Sprintf("%011o\x00%011o\x00", realsize-n, n))
	h[482] = 0
	copy(h[483:], fmt.Sprintf("%011o\x00", realsize))
	copy(h[148:156], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:156], fmt.Sprintf("%06o\x00 ", sum))
}
