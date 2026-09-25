// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"io"
	iofs "io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// record is one entry of an archive whose data sits contiguous and uncompressed
// in the file.
//
// That shape is what ar, cpio and tar share: once the offset is known, a read is
// a read, with no decompression and no cost that depends on where in the archive
// the entry sits.
type record struct {
	name   string        // slash-separated, cleaned, no leading "./"
	mode   iofs.FileMode // permissions AND type bits
	size   int64
	offset int64 // where the data begins in the ReaderAt
	link   string
	mtime  time.Time
}

// newIndexFS presents records pointing into ra as an io/fs.FS.
//
// It exists so that a new contiguous format needs a PARSER and nothing else. The
// filesystem side -- Stat, ListDir, ReadFile, OpenFile, the read-only refusals --
// already exists in FromFS and is already tested; writing a second copy of it per
// format is how five slightly different answers to "what is a directory" get into
// one package.
//
// Directories that no record mentions are SYNTHESISED. cpio usually lists its
// directories and ar has none at all, so a member called "usr/bin/tool" has to
// put usr and usr/bin somewhere, or a walk of the root finds nothing.
func newIndexFS(ra io.ReaderAt, recs []record) iofs.FS {
	f := &indexFS{ra: ra, byName: map[string]*record{}, kids: map[string][]string{}}
	for i := range recs {
		r := recs[i]
		r.name = indexClean(r.name)
		if r.name == "" || r.name == "." {
			continue
		}
		f.byName[r.name] = &r
		f.link(r.name)
	}
	// Synthesise the parents nothing declared, after the real records, so a
	// declared directory keeps its own mode.
	for name := range f.byName {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if _, ok := f.byName[dir]; !ok {
				f.byName[dir] = &record{name: dir, mode: iofs.ModeDir | 0o755}
				f.link(dir)
			}
		}
	}
	return f
}

// indexClean is the one spelling of a name this index uses.
func indexClean(name string) string {
	name = strings.TrimPrefix(strings.ReplaceAll(name, `\`, "/"), "./")
	name = strings.TrimSuffix(name, "/")
	name = path.Clean("/" + name)
	return strings.TrimPrefix(name, "/")
}

type indexFS struct {
	ra     io.ReaderAt
	byName map[string]*record
	kids   map[string][]string
}

// link files a name under its parent, once.
func (f *indexFS) link(name string) {
	dir := path.Dir(name)
	if dir == name {
		return
	}
	if !slices.Contains(f.kids[dir], name) {
		f.kids[dir] = append(f.kids[dir], name)
	}
}

func (f *indexFS) Open(name string) (iofs.File, error) {
	if name == "." {
		return &indexDir{fs: f, name: "."}, nil
	}
	r, ok := f.byName[indexClean(name)]
	if !ok {
		return nil, &iofs.PathError{Op: "open", Path: name, Err: iofs.ErrNotExist}
	}
	if r.mode.IsDir() {
		return &indexDir{fs: f, name: r.name, rec: r}, nil
	}
	return &indexFile{rec: r, SectionReader: io.NewSectionReader(f.ra, r.offset, r.size)}, nil
}

// ReadDir answers iofs.ReadDirFS, which is what the adapter's ListDir uses.
func (f *indexFS) ReadDir(name string) ([]iofs.DirEntry, error) {
	dir := "."
	if name != "." && name != "" {
		dir = indexClean(name)
		if r, ok := f.byName[dir]; !ok || !r.mode.IsDir() {
			return nil, &iofs.PathError{Op: "readdir", Path: name, Err: iofs.ErrNotExist}
		}
	}
	names := slices.Clone(f.kids[dir])
	slices.Sort(names)
	out := make([]iofs.DirEntry, 0, len(names))
	for _, n := range names {
		out = append(out, iofs.FileInfoToDirEntry(indexInfo{f.byName[n]}))
	}
	return out, nil
}

// indexFile is a regular entry. It IS an io.ReaderAt, which is what lets the
// adapter hand out a real seekable handle instead of re-reading from the start.
type indexFile struct {
	*io.SectionReader
	rec *record
}

func (f *indexFile) Stat() (iofs.FileInfo, error) { return indexInfo{f.rec}, nil }
func (f *indexFile) Close() error                 { return nil }

// indexDir is a directory, real or synthesised.
type indexDir struct {
	fs   *indexFS
	name string
	rec  *record
	read int
}

func (d *indexDir) Stat() (iofs.FileInfo, error) {
	if d.rec != nil {
		return indexInfo{d.rec}, nil
	}
	return indexInfo{&record{name: d.name, mode: iofs.ModeDir | 0o755}}, nil
}

func (d *indexDir) Read([]byte) (int, error) {
	return 0, &iofs.PathError{Op: "read", Path: d.name, Err: iofs.ErrInvalid}
}

func (d *indexDir) Close() error { return nil }

func (d *indexDir) ReadDir(n int) ([]iofs.DirEntry, error) {
	all, err := d.fs.ReadDir(d.name)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		d.read = len(all)
		return all, nil
	}
	if d.read >= len(all) {
		return nil, io.EOF
	}
	end := min(d.read+n, len(all))
	out := all[d.read:end]
	d.read = end
	return out, nil
}

// indexInfo is a record seen as an iofs.FileInfo.
type indexInfo struct{ rec *record }

func (i indexInfo) Name() string { return path.Base(i.rec.name) }
func (i indexInfo) Size() int64  { return i.rec.size }
func (i indexInfo) Mode() iofs.FileMode {
	return i.rec.mode
}
func (i indexInfo) ModTime() time.Time { return i.rec.mtime }
func (i indexInfo) IsDir() bool        { return i.rec.mode.IsDir() }
func (i indexInfo) Sys() any           { return i.rec }

// ReadLink answers iofs.ReadLinkFS, so a symlink's target survives -- cpio
// carries them and the adapter asks for them.
func (f *indexFS) ReadLink(name string) (string, error) {
	r, ok := f.byName[indexClean(name)]
	if !ok {
		return "", &iofs.PathError{Op: "readlink", Path: name, Err: iofs.ErrNotExist}
	}
	if r.mode&iofs.ModeSymlink == 0 {
		return "", &iofs.PathError{Op: "readlink", Path: name, Err: iofs.ErrInvalid}
	}
	return r.link, nil
}

// Lstat answers the other half of iofs.ReadLinkFS.
func (f *indexFS) Lstat(name string) (iofs.FileInfo, error) {
	r, ok := f.byName[indexClean(name)]
	if !ok {
		return nil, &iofs.PathError{Op: "lstat", Path: name, Err: iofs.ErrNotExist}
	}
	return indexInfo{r}, nil
}
