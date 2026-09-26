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
			if len(head) < 2 || cpioBinaryOrder(head[:2]) == nil {
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

// TestABigEndianBinaryCpioIsReadToo.
//
// ⛔ Nothing on this machine writes one: cpio(1) here emits little-endian words,
// so the fixture is BUILT BY HAND from the same header layout, with every 16-bit
// word byte-swapped. That is stated rather than hidden -- a fixture of our own
// making cannot fail the way a real archive can.
//
// It is worth having anyway, because the whole reason the byte order is DETECTED
// rather than assumed is that archives written on the other kind of machine exist,
// and a reader that handles only one silently misreads every field of the other.
func TestABigEndianBinaryCpioIsReadToo(t *testing.T) {
	const name = "swapped.txt"
	body := []byte("written on a big-endian machine")

	var b bytes.Buffer
	put := func(v uint16) { _ = binary.Write(&b, binary.BigEndian, v) }
	entry := func(n string, mode uint16, data []byte) {
		nameBytes := append([]byte(n), 0)
		put(cpioBinaryMagic)         // magic
		put(0)                       // dev
		put(1)                       // ino
		put(mode)                    // mode
		put(0)                       // uid
		put(0)                       // gid
		put(1)                       // nlink
		put(0)                       // rdev
		put(0)                       // mtime, HIGH word first
		put(0)                       // mtime, low
		put(uint16(len(nameBytes)))  // namesize, with the NUL
		put(uint16(len(data) >> 16)) // filesize, HIGH word first
		put(uint16(len(data)))       // filesize, low
		b.Write(nameBytes)
		if len(nameBytes)%2 == 1 {
			b.WriteByte(0)
		}
		b.Write(data)
		if len(data)%2 == 1 {
			b.WriteByte(0)
		}
	}
	entry(name, 0o100644, body)
	entry(cpioTrailer, 0, nil)

	path := filepath.Join(t.TempDir(), "be.cpio")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// The premise: these bytes really are the big-endian spelling, and NOT the
	// little-endian one -- otherwise the test proves nothing about the detection.
	if binary.LittleEndian.Uint16(b.Bytes()[:2]) == cpioBinaryMagic {
		t.Fatal("the fixture reads as little-endian too, so the detection is untested")
	}

	fsys, format, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	if format != FormatCpio {
		t.Errorf("format = %v, want cpio", format)
	}
	got, err := fsys.ReadFile(name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("read back %q, want %q", got, body)
	}
}
