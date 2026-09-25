// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"sort"

	filesystem "github.com/go-filesystems/interface"
)

// ErrReadOnly is returned by every mutating method of a filesystem this package
// builds. An archive is read-only.
var ErrReadOnly = errors.New("unarchive: archive is read-only")

// FromFS presents any io/fs.FS as a filesystem.Filesystem.
//
// It exists because the standard library already answers fs.FS for some of
// these formats -- a *zip.Reader is one -- so the driver for them is an
// adaptation and not a decoder. Writing a second zip reader to satisfy a
// different interface would be a duplicate of something already correct.
//
// ⛔ Random access costs what the underlying fs.FS charges. An fs.File is a
// stream: unless it also answers io.ReaderAt, reaching an offset means reading
// what comes before it, and reading BACKWARDS means opening the entry again.
// A zip entry is stored on its own, so a fresh open is cheap; an entry inside a
// solidly-compressed archive is not, and that is the format's shape rather than
// this adapter's.
func FromFS(fsys iofs.FS) filesystem.Filesystem { return &fsWrap{fsys: fsys} }

type fsWrap struct{ fsys iofs.FS }

func (w *fsWrap) Close() error { return nil }

func (w *fsWrap) ListDir(dir string) ([]filesystem.DirEntry, error) {
	if dir == "" {
		dir = "."
	}
	entries, err := iofs.ReadDir(w.fsys, dir)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.DirEntry, 0, len(entries))
	for i, e := range entries {
		var ftype uint8
		if e.IsDir() {
			ftype = fileTypeDir
		}
		out = append(out, filesystem.NewDirEntry(uint64(i+1), e.Name(), ftype))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (w *fsWrap) Stat(p string) (filesystem.Stat, error) {
	st, err := iofs.Stat(w.fsys, w.name(p))
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(posixMode(st.Mode()), uint64(st.Size()), 0), nil
}

// posixMode is the st_mode a Stat carries. os.FileMode keeps its type bits at
// the top of a uint32, so narrowing one loses every one of them -- a directory
// that cannot say it is a directory.
func posixMode(m os.FileMode) uint16 {
	perm := uint16(m.Perm())
	switch {
	case m.IsDir():
		return 0o040000 | perm
	case m&os.ModeSymlink != 0:
		return 0o120000 | perm
	default:
		return 0o100000 | perm
	}
}

func (w *fsWrap) ReadFile(p string) ([]byte, error) { return iofs.ReadFile(w.fsys, w.name(p)) }

func (w *fsWrap) ReadLink(p string) (string, error) {
	if rl, ok := w.fsys.(iofs.ReadLinkFS); ok {
		return rl.ReadLink(w.name(p))
	}
	return "", errors.New("unarchive: this archive reports no link targets")
}

func (w *fsWrap) OpenFile(p string) (filesystem.File, error) {
	name := w.name(p)
	st, err := iofs.Stat(w.fsys, name)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, errors.New("unarchive: not a regular file")
	}
	return &fsHandle{fsys: w.fsys, name: name, size: st.Size()}, nil
}

// name turns this package's spelling of a path into fs.FS's: the root is "."
// there and "" here.
func (w *fsWrap) name(p string) string {
	if p == "" {
		return "."
	}
	return path.Clean(p)
}

func (w *fsWrap) WriteFile(string, []byte, os.FileMode) error { return ErrReadOnly }
func (w *fsWrap) MkDir(string, os.FileMode) error             { return ErrReadOnly }
func (w *fsWrap) DeleteFile(string) error                     { return ErrReadOnly }
func (w *fsWrap) DeleteDir(string) error                      { return ErrReadOnly }
func (w *fsWrap) Rename(string, string) error                 { return ErrReadOnly }

// fsHandle reads one entry, keeping its stream and position so that reading
// forwards does not start over.
type fsHandle struct {
	fsys iofs.FS
	name string
	size int64
	f    iofs.File
	pos  int64
}

func (h *fsHandle) Size() int64 { return h.size }

func (h *fsHandle) Close() error {
	if h.f != nil {
		err := h.f.Close()
		h.f = nil
		return err
	}
	return nil
}

func (h *fsHandle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("unarchive: negative offset")
	}
	if off >= h.size {
		return 0, io.EOF
	}
	// An entry that answers io.ReaderAt needs none of the rest: ask it.
	if h.f == nil {
		f, err := h.fsys.Open(h.name)
		if err != nil {
			return 0, err
		}
		h.f, h.pos = f, 0
	}
	if ra, ok := h.f.(io.ReaderAt); ok {
		return ra.ReadAt(p, off)
	}
	if off < h.pos {
		// Backwards: the stream cannot rewind, so open it again.
		if err := h.Close(); err != nil {
			return 0, err
		}
		f, err := h.fsys.Open(h.name)
		if err != nil {
			return 0, err
		}
		h.f, h.pos = f, 0
	}
	if off > h.pos {
		if _, err := io.CopyN(io.Discard, h.f, off-h.pos); err != nil {
			return 0, err
		}
		h.pos = off
	}
	n, err := io.ReadFull(h.f, p)
	h.pos += int64(n)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}
