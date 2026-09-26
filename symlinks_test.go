// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/tar"
	"bytes"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// Two archives holding the same three symbolic links, written by the tools that
// own the formats -- macOS `tar` and `cpio -o -H newc`:
//
//	link-to-real   -> real.txt      relative, beside it
//	sub/link-up    -> ../real.txt   relative, climbing one level
//	link-absolute  -> /etc/passwd   absolute, and well outside any destination
//
//go:embed testdata/gnutar-links.tar testdata/cpio-links.cpio
var linkArchives embed.FS

func linkFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := linkArchives.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestASymlinkComesOutASymlink.
//
// ⛔ Every one of them came out as an EMPTY REGULAR FILE until today: walk asked
// only whether an entry was a directory, so a link fell through to extractFile,
// which wrote the zero bytes a link's body contains. No error, and Files counted
// it. ReadLink had the target the whole time.
//
// Both formats are checked because they arrive by different routes -- tar has a
// driver of its own, cpio comes through the index and FromFS -- and the type byte
// was missing from both.
func TestASymlinkComesOutASymlink(t *testing.T) {
	for _, fixture := range []string{"gnutar-links.tar", "cpio-links.cpio"} {
		t.Run(fixture, func(t *testing.T) {
			fsys, format, err := Open(linkFixture(t, fixture))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fsys.Close()

			dest := t.TempDir()
			res, err := Extract(fsys, dest, Options{})
			if err != nil {
				t.Fatalf("Extract (%s): %v", format, err)
			}
			if res.Links != 3 {
				t.Errorf("Links = %d, want 3", res.Links)
			}
			// ⛔ And NOT counted as files. A count that lumps them together would
			// pass just as well against the old behaviour, which is exactly what
			// made the defect invisible.
			if res.Files != 1 {
				t.Errorf("Files = %d, want 1 (real.txt alone)", res.Files)
			}

			for name, want := range map[string]string{
				"link-to-real":  "real.txt",
				"sub/link-up":   filepath.FromSlash("../real.txt"),
				"link-absolute": filepath.FromSlash("/etc/passwd"),
			} {
				p := filepath.Join(dest, filepath.FromSlash(name))
				fi, err := os.Lstat(p)
				if err != nil {
					t.Errorf("Lstat %s: %v", name, err)
					continue
				}
				if fi.Mode()&os.ModeSymlink == 0 {
					t.Errorf("%s is %v, not a symbolic link: this is the defect, and "+
						"an empty regular file is what it looked like", name, fi.Mode())
					continue
				}
				got, err := os.Readlink(p)
				if err != nil {
					t.Errorf("Readlink %s: %v", name, err)
					continue
				}
				if got != want {
					t.Errorf("%s -> %q, want %q", name, got, want)
				}
			}
		})
	}
}

// TestAnAbsoluteTargetIsWrittenAsRecorded pins the decision, so it is changed on
// purpose if it is changed.
//
// Creating a link writes nothing outside dest -- a target is a string in an inode
// -- and what would write outside is closed separately, by the two tests below.
// Refusing the target instead would make an RPM or a .deb unextractable, and
// rewriting it would produce a tree meaning something other than what was packed.
func TestAnAbsoluteTargetIsWrittenAsRecorded(t *testing.T) {
	fsys, _, err := Open(linkFixture(t, "gnutar-links.tar"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()
	dest := t.TempDir()
	if _, err := Extract(fsys, dest, Options{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got, err := os.Readlink(filepath.Join(dest, "link-absolute"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != filepath.FromSlash("/etc/passwd") {
		t.Errorf("target = %q, want it left exactly as the archive recorded it", got)
	}
}

// nestedTar writes a symbolic link and then an entry underneath it -- the shape a
// symlink extraction attack takes.
func nestedTar(t *testing.T, linkTarget string) string {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	body := []byte("PWNED\n")
	for _, h := range []*tar.Header{
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: linkTarget, Mode: 0o777},
		{Name: "link/pwned", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))},
	} {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "nested.tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAnEntryNestedUnderALinkIsRefusedAtOpen.
//
// Nothing was ever written outside here -- walk descends only into directories, so
// the entry under the link was never reached. What it WAS is silent: Files 0, err
// nil, and a file the archive held simply gone. That is why this is refused at
// Open: by extraction time there is nothing left to report it with.
func TestAnEntryNestedUnderALinkIsRefusedAtOpen(t *testing.T) {
	outside := t.TempDir()
	_, _, err := Open(nestedTar(t, outside))
	if err == nil {
		t.Fatal("an archive listing a file beneath a symbolic link was accepted")
	}
	if !errors.Is(err, ErrNestedUnderNonDirectory) {
		t.Errorf("err = %v, want ErrNestedUnderNonDirectory", err)
	}
	// It names both ends, because "malformed" alone sends nobody anywhere.
	for _, want := range []string{`"link"`, `"pwned"`} {
		if !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Errorf("err = %v, want it to name %s", err, want)
		}
	}
}

// TestAWriteNeverGoesThroughALinkAlreadyThere is the other half, and the one that
// is actually about escaping: a link sitting in the destination before extraction
// starts, at a path the archive writes.
//
// O_TRUNC follows a symbolic link, so overwriting one writes to whatever it points
// at -- which for an attacker is any file the process can reach.
func TestAWriteNeverGoesThroughALinkAlreadyThere(t *testing.T) {
	const held = "the bytes that must survive"

	for _, c := range []struct {
		name      string
		overwrite bool
	}{
		{"refused outright", false},
		{"replaced, not followed", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			outsideDir := t.TempDir()
			victim := filepath.Join(outsideDir, "victim.txt")
			if err := os.WriteFile(victim, []byte(held), 0o600); err != nil {
				t.Fatal(err)
			}

			fsys, _, err := Open(linkFixture(t, "gnutar-links.tar"))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fsys.Close()

			// real.txt is a plain file in the archive. A link of that name is put
			// in the destination first, pointing at the victim.
			dest := t.TempDir()
			if err := os.Symlink(victim, filepath.Join(dest, "real.txt")); err != nil {
				t.Fatal(err)
			}

			_, err = Extract(fsys, dest, Options{Overwrite: c.overwrite})
			switch {
			case c.overwrite && err != nil:
				t.Fatalf("Extract: %v", err)
			case !c.overwrite && !errors.Is(err, ErrExists):
				t.Fatalf("Extract = %v, want ErrExists", err)
			}

			// ⛔ THE assertion, both ways round: the file outside is untouched.
			got, err := os.ReadFile(victim)
			if err != nil {
				t.Fatalf("the victim is gone: %v", err)
			}
			if string(got) != held {
				t.Errorf("the victim was written through the link: %q", got)
			}
			if c.overwrite {
				// …and the destination holds a real file now, not the link.
				fi, err := os.Lstat(filepath.Join(dest, "real.txt"))
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode()&os.ModeSymlink != 0 {
					t.Error("the link is still there, so nothing was written at all")
				}
			}
		})
	}
}

// TestADirectoryIsNotWrittenThroughALinkEither. MkdirAll walks THROUGH a link to a
// directory, so this is the same hole one level up -- and resolve() cannot see it,
// because the path it checked was perfectly well behaved.
func TestADirectoryIsNotWrittenThroughALinkEither(t *testing.T) {
	outside := t.TempDir()

	fsys, _, err := Open(linkFixture(t, "gnutar-links.tar"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()

	// sub is a directory in the archive. A link of that name is put there first.
	dest := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, "sub")); err != nil {
		t.Fatal(err)
	}

	_, err = Extract(fsys, dest, Options{Overwrite: true})
	if err == nil {
		t.Fatal("a directory was created through a symbolic link")
	}
	if !errors.Is(err, ErrEscapes) {
		t.Errorf("err = %v, want ErrEscapes", err)
	}
	// Nothing of the archive reached the far side of the link.
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("%d entries were written outside", len(entries))
	}
}

// TestALinkDeclaredWithNoTargetIsRefused. A driver that says fileTypeSymlink and
// cannot say where to has nothing this can write; a file of no bytes would be the
// old defect wearing the new code.
func TestALinkDeclaredWithNoTargetIsRefused(t *testing.T) {
	err := extractSymlink("", filepath.Join(t.TempDir(), "x"), Options{})
	if !errors.Is(err, ErrIncomplete) {
		t.Errorf("err = %v, want ErrIncomplete", err)
	}
}

// declaresALinkWithNoTarget is a driver that says fileTypeSymlink and then fails
// to say where to -- a truncated header, or a reader that lost the field.
type declaresALinkWithNoTarget struct{ filesystem.Filesystem }

func (declaresALinkWithNoTarget) ReadLink(string) (string, error) {
	return "", errors.New("the link target was not recorded")
}

// TestADeclaredLinkIsBelievedEvenWhenReadLinkFails.
//
// ⛔ The two halves of isSymlink answer different questions, and this is the one
// that matters. There is no shared sentinel for "not a link" across the org's
// drivers, so a ReadLink error cannot be read as a no -- and treating it as one
// would put an empty regular file where a link belongs, which is precisely the
// defect this whole change removes.
//
// So a driver that DECLARES the type is believed, and the entry is refused rather
// than written.
func TestADeclaredLinkIsBelievedEvenWhenReadLinkFails(t *testing.T) {
	e := filesystem.NewDirEntry(1, "link", fileTypeSymlink)
	link, ok := isSymlink(declaresALinkWithNoTarget{}, "/link", e)
	if !ok {
		t.Fatal("an entry declared as a symbolic link was reported as something else, " +
			"so it would be written as a file with no bytes in it")
	}
	if link != "" {
		t.Errorf("target = %q, want empty: nothing was recorded", link)
	}
	// …and the empty target is then an error rather than an empty file.
	if err := extractSymlink(link, filepath.Join(t.TempDir(), "link"), Options{}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("extractSymlink = %v, want ErrIncomplete", err)
	}
}

// TestALinkIsNotSilentlyReplaced. Extracting twice over the same destination
// follows extractFile's rule and not a looser one: refused unless asked.
func TestALinkIsNotSilentlyReplaced(t *testing.T) {
	fsys, _, err := Open(linkFixture(t, "gnutar-links.tar"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()

	dest := t.TempDir()
	if _, err := Extract(fsys, dest, Options{}); err != nil {
		t.Fatalf("first Extract: %v", err)
	}
	// Premise: it is a link now, so the second pass is about a link and not about
	// a file that happens to be there.
	fi, err := os.Lstat(filepath.Join(dest, "link-to-real"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the first pass did not write a link: %v %v", fi, err)
	}

	if _, err := Extract(fsys, dest, Options{}); !errors.Is(err, ErrExists) {
		t.Errorf("second Extract = %v, want ErrExists", err)
	}
	// And with Overwrite it goes through, leaving a link again.
	if _, err := Extract(fsys, dest, Options{Overwrite: true}); err != nil {
		t.Fatalf("Extract with Overwrite: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(dest, "link-to-real")); err != nil || got != "real.txt" {
		t.Errorf("after Overwrite the link is %q, %v", got, err)
	}
}

// TestANonDirectoryWithNoChildrenIsFine. The guard is about entries LISTED beneath
// something; a file with an empty child list is every ordinary file in every
// archive, and refusing those would refuse everything.
func TestANonDirectoryWithNoChildrenIsFine(t *testing.T) {
	kids := map[string][]string{"notes.txt": {}, ".": {"notes.txt"}}
	if err := refuseNestingUnderANonDirectory(kids, func(p string) bool { return p == "." }); err != nil {
		t.Errorf("a file with no children was refused: %v", err)
	}
}

// TestADanglingLinkInTheWayIsSeen.
//
// ⛔ Lstat, not Stat. Stat follows the link, so it answers "nothing there" for a
// link pointing at a file that does not exist -- and the code then goes on to
// os.Symlink over a name that is taken, which fails with a bare EEXIST from the
// operating system instead of this package's own ErrExists.
//
// A dangling link is the ordinary case, not a contrived one: a half-finished
// extraction leaves them, and so does an archive whose target was never packed.
func TestADanglingLinkInTheWayIsSeen(t *testing.T) {
	fsys, _, err := Open(linkFixture(t, "gnutar-links.tar"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsys.Close()

	dest := t.TempDir()
	// Points at nothing, at a name the archive writes.
	if err := os.Symlink(filepath.Join(dest, "nowhere"), filepath.Join(dest, "link-to-real")); err != nil {
		t.Fatal(err)
	}
	// Premise: it dangles. Stat must fail where Lstat succeeds, or this tests
	// nothing about which one is used.
	if _, err := os.Stat(filepath.Join(dest, "link-to-real")); err == nil {
		t.Fatal("the link does not dangle, so Stat and Lstat agree here")
	}
	if _, err := os.Lstat(filepath.Join(dest, "link-to-real")); err != nil {
		t.Fatalf("Lstat cannot see it either: %v", err)
	}

	_, err = Extract(fsys, dest, Options{})
	if !errors.Is(err, ErrExists) {
		t.Errorf("Extract = %v, want this package's ErrExists rather than a bare "+
			"EEXIST from os.Symlink", err)
	}

	// And with Overwrite the dangling link is replaced by the archive's.
	if _, err := Extract(fsys, dest, Options{Overwrite: true}); err != nil {
		t.Fatalf("Extract with Overwrite: %v", err)
	}
	if got, _ := os.Readlink(filepath.Join(dest, "link-to-real")); got != "real.txt" {
		t.Errorf("link-to-real -> %q, want the archive's target", got)
	}
}

// TestBothDriversDeclareTheSymlinkType.
//
// The type byte is a contract with every consumer, not only with Extract -- which
// has a fallback and therefore cannot notice a driver that stops setting it. So it
// is asserted where it is published.
//
// ⛔ Both routes, because they are different code: tar has a driver of its own and
// cpio arrives through the index and the io/fs adapter. Neither set it.
func TestBothDriversDeclareTheSymlinkType(t *testing.T) {
	for _, fixture := range []string{"gnutar-links.tar", "cpio-links.cpio"} {
		t.Run(fixture, func(t *testing.T) {
			fsys, _, err := Open(linkFixture(t, fixture))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fsys.Close()
			entries, err := fsys.ListDir(".")
			if err != nil {
				t.Fatalf("ListDir: %v", err)
			}
			want := map[string]uint8{
				"link-to-real":  fileTypeSymlink,
				"link-absolute": fileTypeSymlink,
				"real.txt":      0, // this driver family does not declare DT_REG
				"sub":           fileTypeDir,
			}
			seen := 0
			for _, e := range entries {
				w, ok := want[e.Name()]
				if !ok {
					continue
				}
				seen++
				if e.FileType() != w {
					t.Errorf("%s: FileType = %d, want %d", e.Name(), e.FileType(), w)
				}
			}
			if seen != len(want) {
				t.Errorf("saw %d of the %d expected entries: the fixture changed and "+
					"this now checks less than it says", seen, len(want))
			}
		})
	}
}
