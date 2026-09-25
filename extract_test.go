// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// fakeFS is an archive written by the test. What is under test here is the
// extraction -- where bytes land, what is refused, and what happens when an
// entry does not deliver what its header promised -- none of which is decoding.
type fakeFS struct {
	files map[string]string // path -> contents
	// declared overrides the size a Stat reports, so an entry can promise more
	// than it holds. That is the whole defect, in one field.
	declared map[string]int64
}

func (f *fakeFS) Close() error { return nil }

func (f *fakeFS) ListDir(dir string) ([]filesystem.DirEntry, error) {
	seen := map[string]bool{}
	var out []filesystem.DirEntry
	for p := range f.files {
		if dir != "" && !strings.HasPrefix(p, dir+"/") {
			continue
		}
		rest := p
		if dir != "" {
			rest = p[len(dir)+1:]
		}
		name := rest
		var ftype uint8
		if i := strings.Index(rest, "/"); i >= 0 {
			name, ftype = rest[:i], fileTypeDir
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, filesystem.NewDirEntry(0, name, ftype))
	}
	return out, nil
}

func (f *fakeFS) Stat(p string) (filesystem.Stat, error) {
	body, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	size := int64(len(body))
	if d, ok := f.declared[p]; ok {
		size = d
	}
	return filesystem.NewStat(0o100644, uint64(size), 0), nil
}

func (f *fakeFS) OpenFile(p string) (filesystem.File, error) {
	body, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	size := int64(len(body))
	if d, ok := f.declared[p]; ok {
		size = d
	}
	return &fakeFile{body: body, size: size}, nil
}

func (f *fakeFS) ReadFile(p string) ([]byte, error) {
	body, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(body), nil
}

func (f *fakeFS) ReadLink(string) (string, error)             { return "", errors.New("no links") }
func (f *fakeFS) WriteFile(string, []byte, os.FileMode) error { return errors.New("read-only") }
func (f *fakeFS) MkDir(string, os.FileMode) error             { return errors.New("read-only") }
func (f *fakeFS) DeleteFile(string) error                     { return errors.New("read-only") }
func (f *fakeFS) DeleteDir(string) error                      { return errors.New("read-only") }
func (f *fakeFS) Rename(string, string) error                 { return errors.New("read-only") }

type fakeFile struct {
	body string
	size int64
}

func (f *fakeFile) Size() int64  { return f.size }
func (f *fakeFile) Close() error { return nil }

func (f *fakeFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.body)) {
		return 0, io.EOF
	}
	n := copy(p, f.body[off:])
	if off+int64(n) >= int64(len(f.body)) {
		return n, io.EOF
	}
	return n, nil
}

func TestExtractWritesTheTreeAndCountsIt(t *testing.T) {
	fs := &fakeFS{files: map[string]string{
		"notes.txt":         "hello",
		"film/2024/one.mkv": "aaaa",
		"film/2024/two.mkv": "bbbbbb",
	}}
	dest := t.TempDir()
	res, err := Extract(fs, dest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 {
		t.Errorf("%d files, want 3", res.Files)
	}
	if res.Bytes != int64(len("hello")+len("aaaa")+len("bbbbbb")) {
		t.Errorf("%d bytes written", res.Bytes)
	}
	for p, want := range map[string]string{
		"notes.txt":         "hello",
		"film/2024/one.mkv": "aaaa",
		"film/2024/two.mkv": "bbbbbb",
	} {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
}

// TestAnEntryShorterThanItsHeaderIsAnError is the defect, reproduced in
// miniature: the header promises sixteen bytes and the archive holds four.
//
// Written and reported as a success, the file is short or -- when whatever
// produced the archive padded it -- exactly the right length with a tail of
// nothing, and every size check afterwards agrees. So it is an error.
func TestAnEntryShorterThanItsHeaderIsAnError(t *testing.T) {
	fs := &fakeFS{
		files:    map[string]string{"truncated.bin": "0123"},
		declared: map[string]int64{"truncated.bin": 16},
	}
	dest := t.TempDir()
	_, err := Extract(fs, dest, Options{})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Extract gave %v, want ErrIncomplete", err)
	}
	if !strings.Contains(err.Error(), "4 of 16") {
		t.Errorf("the error reads %q; it should say how much of how much", err)
	}
}

// TestAnEntryCannotLeaveTheDestination: an archive is somebody else's list of
// names, and ".." is a name. Every extractor that trusted one has had this bug.
func TestAnEntryCannotLeaveTheDestination(t *testing.T) {
	for _, name := range []string{
		"../escaped.txt",
		"../../etc/passwd",
		"a/../../escaped.txt",
	} {
		dest := t.TempDir()
		fs := &fakeFS{files: map[string]string{name: "x"}}
		_, err := Extract(fs, dest, Options{})
		if !errors.Is(err, ErrEscapes) {
			t.Errorf("%q gave %v, want ErrEscapes", name, err)
		}
		// And nothing was written outside, which is the claim that matters.
		if _, statErr := os.Stat(filepath.Join(filepath.Dir(dest), "escaped.txt")); statErr == nil {
			t.Errorf("%q wrote a file outside the destination", name)
		}
	}
	// resolve is the guard; check it directly too, including the case whose
	// pieces are each harmless.
	absDest := t.TempDir()
	if _, err := resolve(absDest, "a/../b.txt"); err != nil {
		t.Errorf("a path that stays inside was refused: %v", err)
	}
	if _, err := resolve(absDest, path.Join("..", "out.txt")); !errors.Is(err, ErrEscapes) {
		t.Errorf("resolve let %q through", "../out.txt")
	}
}

// TestAnExistingFileIsNotOverwrittenByDefault: an extraction that replaces what
// is there by default destroys data on a mistyped destination, and the mistake
// does not come back.
func TestAnExistingFileIsNotOverwrittenByDefault(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &fakeFS{files: map[string]string{"notes.txt": "theirs"}}

	if _, err := Extract(fs, dest, Options{}); !errors.Is(err, ErrExists) {
		t.Errorf("Extract gave %v, want ErrExists", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "notes.txt")); string(got) != "mine" {
		t.Errorf("the existing file now reads %q", got)
	}

	if _, err := Extract(fs, dest, Options{Overwrite: true}); err != nil {
		t.Fatalf("with Overwrite: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "notes.txt")); string(got) != "theirs" {
		t.Errorf("with Overwrite the file reads %q, want the archive's", got)
	}
}

// TestListRefusesWhatExtractWouldRefuse: the point of looking before extracting
// is to be told the bad news EARLY. A list that walks happily through an entry
// Extract would reject teaches a person the archive is fine.
func TestListRefusesWhatExtractWouldRefuse(t *testing.T) {
	fs := &fakeFS{files: map[string]string{
		"notes.txt":         "hello",
		"film/2024/one.mkv": "aaaa",
	}}
	got, err := List(fs)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range got {
		if e.Dir {
			paths = append(paths, e.Path+"/")
			continue
		}
		paths = append(paths, e.Path)
	}
	// Directories are named as well as their contents, and a size comes with a
	// file: this is what -n prints.
	want := map[string]bool{"notes.txt": true, "film/": true, "film/2024/": true, "film/2024/one.mkv": true}
	if len(paths) != len(want) {
		t.Errorf("List gave %v, want %d entries", paths, len(want))
	}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("List gave an unexpected %q", p)
		}
	}
	for _, e := range got {
		if !e.Dir && e.Path == "notes.txt" && e.Size != 5 {
			t.Errorf("notes.txt listed as %d bytes, want 5", e.Size)
		}
	}

	bad := &fakeFS{files: map[string]string{"a/../../escaped.txt": "x"}}
	if _, err := List(bad); !errors.Is(err, ErrEscapes) {
		t.Errorf("List walked through a traversal: %v", err)
	}
}
