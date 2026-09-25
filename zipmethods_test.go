// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// zipMethods are the compression methods 7-Zip can put in a zip, with the number
// the format records for each.
var zipMethods = []struct {
	name   string
	method uint16
}{
	{"Deflate", 8},
	{"BZip2", zipMethodBzip2},
	{"LZMA", zipMethodLZMA},
}

// TestEveryZipMethodWeClaimIsActuallyUsed.
//
// ⛔ The PREMISE first, because the obvious version of this test is vacuous. On a
// thirty-byte file 7-Zip STORES the entry whatever -mm says, so a corpus of small
// files passes every method while only Store ever runs -- which is exactly what my
// first attempt did, reporting all four green while three were broken.
//
// So the payload is large and compressible, and the method the archive RECORDS is
// read out of the local header and asserted before anything is decoded.
func TestEveryZipMethodWeClaimIsActuallyUsed(t *testing.T) {
	bin, err := exec.LookPath("7zz")
	if err != nil {
		t.Skip("no 7zz here to build the fixtures with")
	}
	body := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 2000)

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "big.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, m := range zipMethods {
		t.Run(m.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "z.zip")
			cmd := exec.Command(bin, "a", "-tzip", "-mm="+m.name, "-bso0", "-bsp0", archive, ".")
			cmd.Dir = src
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Skipf("this 7zz will not write %s: %v\n%s", m.name, err, out)
			}
			// The premise: the entry really uses the method. The local header's
			// method field is two bytes at offset 8.
			head, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			if len(head) < 10 {
				t.Fatalf("%s is %d bytes", archive, len(head))
			}
			if got := binary.LittleEndian.Uint16(head[8:10]); got != m.method {
				t.Fatalf("7zz recorded method %d, not %d: this fixture does not "+
					"exercise %s, so the test would pass without reading it",
					got, m.method, m.name)
			}

			fsys, format, err := Open(archive)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fsys.Close()
			if format != FormatZIP {
				t.Errorf("format = %v, want zip", format)
			}
			got, err := fsys.ReadFile("big.txt")
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Errorf("read back %d bytes, want %d (first difference at %d)",
					len(got), len(body), firstDiff(got, body))
			}
		})
	}
}

// TestAZipMethodWeCannotDecodeSaysSo: the failure has to name the entry and the
// method, because "unsupported compression algorithm" alone leaves somebody
// guessing which of a thousand files stopped them.
func TestAZipMethodWeCannotDecodeSaysSo(t *testing.T) {
	// Method 99 is WinZip's AES wrapper, which this does not decode.
	var b bytes.Buffer
	b.Write([]byte("PK\x03\x04"))
	b.Write(make([]byte, 4))
	_ = binary.Write(&b, binary.LittleEndian, uint16(99))
	b.Write(make([]byte, 100))
	path := filepath.Join(t.TempDir(), "aes.zip")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(path); err == nil {
		t.Error("a truncated zip with an unknown method was accepted")
	} else if !strings.Contains(err.Error(), "aes.zip") {
		t.Errorf("the error does not name the file: %v", err)
	}
}

// firstDiff says where two byte slices part company, which is the number that
// says WHERE a decoder went wrong rather than merely that it did.
func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestAZipPPMdEntrySaysWhichVariantIsMissing.
//
// Method 98 is PPMd variant I. The PPMd in this module's graph is variant H, the
// one .7z uses, and wiring it here produced "invalid SummFreq < count" on a real
// archive -- a model error, which is the polite form of the failure. A decoder
// that stayed consistent for longer would have produced plausible rubbish.
//
// So it is refused, and the refusal names the variant: "unsupported compression
// algorithm" is true and sends nobody anywhere.
func TestAZipPPMdEntrySaysWhichVariantIsMissing(t *testing.T) {
	bin, err := exec.LookPath("7zz")
	if err != nil {
		t.Skip("no 7zz here")
	}
	body := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 2000)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "big.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "ppmd.zip")
	cmd := exec.Command(bin, "a", "-tzip", "-mm=PPMd", "-bso0", "-bsp0", archive, ".")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this 7zz will not write PPMd: %v\n%s", err, out)
	}
	head, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(head[8:10]); got != zipMethodPPMd {
		t.Skipf("7zz recorded method %d, not PPMd", got)
	}

	fsys, _, err := Open(archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	_, err = fsys.ReadFile("big.txt")
	if err == nil {
		t.Fatal("a PPMd entry was decoded, and the variant available here cannot")
	}
	for _, want := range []string{"variant I", "variant H", "98"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}
