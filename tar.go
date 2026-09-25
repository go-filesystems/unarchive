// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// openTar indexes a tar and answers it as a filesystem with REAL random access.
//
// A tar stores its entries whole, uncompressed and contiguous, so once the
// offset of each one is known a read is a read -- no decompression, no
// re-scanning, no cost that depends on where in the archive the entry sits.
// That is unusual among archive formats and worth taking: zip pays a fresh open
// per entry and a solid RAR pays the whole block.
//
// archive/tar does not say where an entry's data begins, so the bytes are
// counted: Next consumes exactly the header, including any pax or GNU extended
// records, so the count when it returns IS the data offset. USTAR, pax and GNU
// all come through the standard library's own reader, which means a long name or
// a large size carried in a pax record is already resolved by the time it is
// seen here.
func openTar(ra io.ReaderAt, size int64, closer io.Closer) (filesystem.Filesystem, error) {
	fs := &tarFS{ra: ra, closer: closer, byPath: map[string]*tarEntry{}, kids: map[string][]string{}}
	counter := &countingReader{r: io.NewSectionReader(ra, 0, size)}
	tr := tar.NewReader(counter)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		fs.add(h, counter.n)
	}
	return fs, nil
}

// countingReader says how many bytes have been read through it, which is how
// the offset of an entry's data is learned.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type tarEntry struct {
	path       string
	name       string
	dir        bool
	size       int64
	offset     int64
	mode       uint16
	linkTarget string
}

type tarFS struct {
	ra     io.ReaderAt
	closer io.Closer
	byPath map[string]*tarEntry
	kids   map[string][]string
}

// add records one header and every directory above it that the tar did not
// name. A tar is a list of paths, not a tree, and whether a parent appears is
// up to whatever wrote it.
func (f *tarFS) add(h *tar.Header, offset int64) {
	p := tarClean(h.Name)
	if p == "" || p == "." {
		return
	}
	isDir := h.Typeflag == tar.TypeDir || strings.HasSuffix(h.Name, "/")
	f.byPath[p] = &tarEntry{
		path:       p,
		name:       path.Base(p),
		dir:        isDir,
		size:       h.Size,
		offset:     offset,
		mode:       posixMode(h.FileInfo().Mode()),
		linkTarget: h.Linkname,
	}
	for parent := path.Dir(p); ; parent = path.Dir(parent) {
		if _, seen := f.byPath[parent]; !seen && parent != "." {
			f.byPath[parent] = &tarEntry{
				path: parent, name: path.Base(parent), dir: true, mode: 0o040755,
			}
		}
		f.kids[parent] = appendUnique(f.kids[parent], p)
		if parent == "." {
			break
		}
		p = parent
	}
}

func appendUnique(list []string, s string) []string {
	for _, e := range list {
		if e == s {
			return list
		}
	}
	return append(list, s)
}

// tarClean normalises a tar's own spelling of a path.
func tarClean(name string) string {
	name = strings.TrimSuffix(strings.ReplaceAll(name, `\`, "/"), "/")
	if name == "" {
		return ""
	}
	return path.Clean("/" + name)[1:]
}

func (f *tarFS) Close() error {
	if f.closer != nil {
		return f.closer.Close()
	}
	return nil
}

func (f *tarFS) lookup(p string) (*tarEntry, error) {
	e, ok := f.byPath[tarClean(p)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return e, nil
}

func (f *tarFS) ListDir(dir string) ([]filesystem.DirEntry, error) {
	cd := tarClean(dir)
	if cd == "" {
		cd = "."
	} else {
		e, err := f.lookup(cd)
		if err != nil {
			return nil, err
		}
		if !e.dir {
			return nil, errors.New("unarchive: not a directory")
		}
	}
	kids := append([]string(nil), f.kids[cd]...)
	sort.Strings(kids)
	out := make([]filesystem.DirEntry, 0, len(kids))
	for i, k := range kids {
		e := f.byPath[k]
		if e == nil {
			continue
		}
		var ftype uint8
		if e.dir {
			ftype = fileTypeDir
		}
		out = append(out, filesystem.NewDirEntry(uint64(i+1), e.name, ftype))
	}
	return out, nil
}

func (f *tarFS) Stat(p string) (filesystem.Stat, error) {
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(e.mode, uint64(e.size), 0), nil
}

func (f *tarFS) ReadLink(p string) (string, error) {
	e, err := f.lookup(p)
	if err != nil {
		return "", err
	}
	if e.linkTarget == "" {
		return "", errors.New("unarchive: not a symbolic link")
	}
	return e.linkTarget, nil
}

func (f *tarFS) ReadFile(p string) ([]byte, error) {
	h, err := f.OpenFile(p)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	buf := make([]byte, h.Size())
	n, err := h.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// OpenFile hands back a section of the archive. Nothing is decompressed and
// nothing is re-scanned: this is the whole advantage of the format.
func (f *tarFS) OpenFile(p string) (filesystem.File, error) {
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	if e.dir {
		return nil, errors.New("unarchive: not a regular file")
	}
	return &tarHandle{SectionReader: io.NewSectionReader(f.ra, e.offset, e.size)}, nil
}

// tarHandle is an io.SectionReader, which already answers ReadAt and Size.
type tarHandle struct{ *io.SectionReader }

func (tarHandle) Close() error { return nil }

func (f *tarFS) WriteFile(string, []byte, os.FileMode) error { return ErrReadOnly }
func (f *tarFS) MkDir(string, os.FileMode) error             { return ErrReadOnly }
func (f *tarFS) DeleteFile(string) error                     { return ErrReadOnly }
func (f *tarFS) DeleteDir(string) error                      { return ErrReadOnly }
func (f *tarFS) Rename(string, string) error                 { return ErrReadOnly }
