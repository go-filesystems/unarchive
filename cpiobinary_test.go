// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestTheOldBinaryCpioReadsThroughTheSystemsOwnCpio, in both variants cpio(1)
// offers.
//
// ⛔ `bin` and `pwb` produce BYTE-IDENTICAL headers on this machine -- compared
// field by field before this was written -- so one implementation serves both.
// libarchive lists them separately, which is why the comparison was worth making
// rather than assuming two formats.
func TestTheOldBinaryCpioReadsThroughTheSystemsOwnCpio(t *testing.T) {
	bin, err := exec.LookPath("cpio")
	if err != nil {
		t.Skip("no cpio here to build the fixture with")
	}
	for _, variant := range []string{"bin", "pwb"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			// An odd-sized name and an odd-sized body, because the binary format
			// pads to an EVEN offset and not to four: a corpus whose lengths are
			// all even never exercises the pad.
			want := map[string]string{
				"a.txt":      "odd",
				"deep/b.txt": "fourx",
				"deep/c.bin": string(filler(3001)),
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
			archive := filepath.Join(t.TempDir(), "old.cpio")
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

			// The PREMISE: the archive really is the binary variant, not an ASCII
			// one cpio fell back to. The magic is a WORD, so it is two bytes.
			head, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			// 0o070707 as a 16-bit word, either way round: 0o070707 little-endian
			// or 0o143561, which is the same two bytes reversed.
			isBinary := len(head) >= 2 &&
				(binary.LittleEndian.Uint16(head[:2]) == 0o070707 ||
					binary.LittleEndian.Uint16(head[:2]) == 0o143561)
			if !isBinary {
				t.Fatalf("%s does not begin with a binary cpio magic (% x): this "+
					"fixture exercises nothing", archive, head[:min(4, len(head))])
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
					t.Errorf("%s: %d bytes, want %d (first difference at %d)",
						name, len(got), len(body), firstDiff(got, []byte(body)))
				}
			}
			if st, err := fsys.Stat("deep"); err != nil {
				t.Errorf("deep: %v", err)
			} else if st.Mode()&0o170000 != 0o040000 {
				t.Errorf("deep: mode %o is not a directory", st.Mode())
			}
		})
	}
}

// The hand-built big-endian archive that used to sit here has moved to
// go-filesystems/cpio, which owns the parser now and carries bin-swapped.cpio --
// byte-swapped from cpio(1)'s own output rather than written from scratch, so it is
// less of a fixture of our own making than this one was.
//
// What stays here is the test above, which is about the ROUTE: bytes carrying a
// binary cpio magic reach the parser and the files come back. The byte order, the
// field widths and the even-length padding are the parser's business and are tested
// where the parser lives.
