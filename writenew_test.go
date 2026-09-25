// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestTheFormatsWeNowWriteAreReadByTheirOwnTools.
//
// Each of these is read back by the program that owns the format -- ar(1),
// cpio(1), and 7-Zip or hdiutil for an ISO -- rather than by this package. A round
// trip through our own reader would prove that two halves of one module agree,
// which is not what writing an archive is for.
func TestTheFormatsWeNowWriteAreReadByTheirOwnTools(t *testing.T) {
	for _, c := range []struct {
		target string
		tool   string
		// longNames says whether the format can carry a name past sixteen
		// characters. ISO 9660 cannot without Rock Ridge or Joliet, which this
		// builder does not write -- so demanding it there would be demanding
		// something of the format rather than of the code.
		longNames bool
		// read returns the contents of one entry, as the tool sees it.
		read func(t *testing.T, bin, archive, entry string) []byte
	}{
		{"out.a", "ar", true, func(t *testing.T, bin, archive, entry string) []byte {
			// ar prints a member to stdout with p.
			out, err := exec.Command(bin, "p", archive, entry).Output()
			if err != nil {
				t.Fatalf("ar p: %v", err)
			}
			return out
		}},
		{"out.cpio", "cpio", true, func(t *testing.T, bin, archive, entry string) []byte {
			into := t.TempDir()
			f, err := os.Open(archive)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			cmd := exec.Command(bin, "-i", "-d")
			cmd.Dir = into
			cmd.Stdin = f
			var errOut bytes.Buffer
			cmd.Stderr = &errOut
			if err := cmd.Run(); err != nil {
				t.Fatalf("cpio -i: %v\n%s", err, errOut.String())
			}
			b, err := os.ReadFile(filepath.Join(into, entry))
			if err != nil {
				t.Fatalf("%s: %v", entry, err)
			}
			return b
		}},
		{"out.iso", "7zz", false, func(t *testing.T, bin, archive, entry string) []byte {
			into := t.TempDir()
			if o, err := exec.Command(bin, "x", "-bso0", "-bsp0", "-o"+into, archive).CombinedOutput(); err != nil {
				t.Fatalf("7zz x: %v\n%s", err, o)
			}
			// ISO 9660 without Rock Ridge upper-cases and may pad names, so the
			// tree is searched for the CONTENT rather than a spelling the format is
			// entitled to change.
			var found []byte
			err := filepath.Walk(into, func(p string, fi os.FileInfo, err error) error {
				if err != nil || fi.IsDir() {
					return err
				}
				if !strings.EqualFold(strings.TrimSuffix(fi.Name(), ";1"), entry) {
					return nil
				}
				found, err = os.ReadFile(p)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if found == nil {
				t.Fatalf("nothing in the extracted image is called %q", entry)
			}
			return found
		}},
	} {
		t.Run(c.target, func(t *testing.T) {
			bin, err := exec.LookPath(c.tool)
			if err != nil {
				t.Skipf("no %s here to read the result with", c.tool)
			}
			// A source archive to seal FROM, built by the system's tar.
			archive, want := source(t)
			o, _, err := Writable(archive)
			if err != nil {
				t.Fatalf("Writable: %v", err)
			}
			defer o.Close()

			// ⛔ A name past sixteen characters, added here because NOTHING in the
			// source fixture has one -- so ar's long-name path never ran, and an
			// ablation that removed it stayed green.
			const longName = "a-name-that-is-well-past-sixteen-characters.txt"
			want[longName] = "and a body behind a long name"
			if err := o.WriteFile(longName, []byte(want[longName]), 0o600); err != nil {
				t.Fatal(err)
			}

			out := filepath.Join(t.TempDir(), c.target)
			if err := SealTo(o, out); err != nil {
				t.Fatalf("SealTo: %v", err)
			}
			if st, err := os.Stat(out); err != nil {
				t.Fatal(err)
			} else if st.Size() == 0 {
				t.Fatal("the target is empty")
			}

			// ar is flat, so only root-level entries are comparable there -- and
			// both spellings of a name are checked, the field and the "#1/" form.
			entries := []string{"notes.txt"}
			if c.longNames {
				entries = append(entries, longName)
			}
			for _, entry := range entries {
				if got := c.read(t, bin, out, entry); !bytes.Equal(got, []byte(want[entry])) {
					t.Errorf("%s read back %q for %s, want %q", c.tool, got, entry, want[entry])
				}
			}
		})
	}
}

// TestAnArArchiveWeWroteHasNoDirectoryMembers.
//
// ar is FLAT. AddDir has nowhere to put a directory, so it records nothing --
// and the archive must therefore contain no member named after one, which is the
// difference between "dropped deliberately" and "written as a zero-length file
// that ar lists and nobody can use".
func TestAnArArchiveWeWroteHasNoDirectoryMembers(t *testing.T) {
	bin, err := exec.LookPath("ar")
	if err != nil {
		t.Skip("no ar here")
	}
	archive, _ := source(t)
	o, _, err := Writable(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	out := filepath.Join(t.TempDir(), "flat.a")
	if err := SealTo(o, out); err != nil {
		t.Fatalf("SealTo: %v", err)
	}
	listed, err := exec.Command(bin, "t", out).Output()
	if err != nil {
		t.Fatalf("ar t: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(listed)), "\n") {
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, "/") || line == "keep" || line == "drop" {
			t.Errorf("the archive lists %q, and ar has no directories", line)
		}
	}
	if !strings.Contains(string(listed), "notes.txt") {
		t.Errorf("the archive does not list notes.txt:\n%s", listed)
	}
}

// TestTheModesWeWriteAreTheModesTheToolsSee.
//
// A member's mode is OCTAL in an ar header whose sizes are DECIMAL, and a cpio
// entry's TYPE lives in the high bits of its mode: both are plain digits that a
// reader accepts in either base, so a wrong one is a plausible mode rather than an
// error. Ablations that wrote the mode in decimal, and that left the POSIX type
// bits out, both stayed green until this existed.
func TestTheModesWeWriteAreTheModesTheToolsSee(t *testing.T) {
	t.Run("ar", func(t *testing.T) {
		bin, err := exec.LookPath("ar")
		if err != nil {
			t.Skip("no ar here")
		}
		out := sealSource(t, "modes.a")
		listed, err := exec.Command(bin, "tv", out).Output()
		if err != nil {
			t.Fatalf("ar tv: %v", err)
		}
		// ar tv prints the permissions as a string: rw-r--r-- for 0644.
		if !strings.Contains(string(listed), "rw-r--r--") {
			t.Errorf("no 0644 member in:\n%s", listed)
		}
		if strings.Contains(string(listed), "r--r--r--") {
			t.Errorf("a member came out 0444, which is what 0644 read as decimal "+
				"gives:\n%s", listed)
		}
	})

	t.Run("cpio", func(t *testing.T) {
		bin, err := exec.LookPath("cpio")
		if err != nil {
			t.Skip("no cpio here")
		}
		out := sealSource(t, "modes.cpio")
		into := t.TempDir()
		f, err := os.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cmd := exec.Command(bin, "-i", "-d")
		cmd.Dir = into
		cmd.Stdin = f
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		if err := cmd.Run(); err != nil {
			t.Fatalf("cpio -i: %v\n%s", err, errOut.String())
		}
		// The type bits are what make a directory a directory. Without them every
		// entry arrives as a file, and "keep" becomes a zero-length one.
		fi, err := os.Stat(filepath.Join(into, "keep"))
		if err != nil {
			t.Fatalf("keep: %v", err)
		}
		if !fi.IsDir() {
			t.Error("keep came back as a file, so the POSIX type bits were not written")
		}
		reg, err := os.Stat(filepath.Join(into, "notes.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if reg.IsDir() {
			t.Error("notes.txt came back as a directory")
		}

		// ⛔ A regular file's type bits cannot be checked through extraction: a
		// mode with NO type at all still arrives as a file, because that is what
		// cpio(1) makes of a type it does not recognise. So the headers are read.
		//
		// Every entry's mode must carry S_IFREG or S_IFDIR. An archive whose files
		// say neither is accepted by cpio today and is not a cpio archive.
		checked := cpioCheckTypes(t, out)
		if checked == 0 {
			t.Error("no entry headers were read, so nothing was checked")
		}
	})
}

// sealSource seals the standard source fixture into name and returns its path.
func sealSource(t *testing.T, name string) string {
	t.Helper()
	archive, _ := source(t)
	o, _, err := Writable(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	out := filepath.Join(t.TempDir(), name)
	if err := SealTo(o, out); err != nil {
		t.Fatalf("SealTo: %v", err)
	}
	return out
}

// cpioCheckTypes walks a newc archive's headers and reports how many entries it
// checked, failing for any whose mode carries no POSIX file type.
func cpioCheckTypes(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hex := func(off, n int) int64 {
		v, err := strconv.ParseInt(string(raw[off:off+n]), 16, 64)
		if err != nil {
			t.Fatalf("field at %d: %v", off, err)
		}
		return v
	}
	checked := 0
	for off := 0; off+110 <= len(raw); {
		if string(raw[off:off+6]) != cpioNewc {
			break
		}
		mode := hex(off+14, 8)
		fileSize := hex(off+54, 8)
		nameSize := hex(off+94, 8)
		if off+110+int(nameSize) > len(raw) {
			t.Fatalf("name at %d runs past the end", off)
		}
		name := strings.TrimRight(string(raw[off+110:off+110+int(nameSize)]), "\x00")
		if name != cpioTrailer {
			switch mode & 0o170000 {
			case 0o100000, 0o040000:
			default:
				t.Errorf("%s: mode %o carries no POSIX file type", name, mode)
			}
			checked++
		}
		dataOff := cpioRound4(int64(off+110) + nameSize)
		off = int(cpioRound4(dataOff + fileSize))
	}
	return checked
}
