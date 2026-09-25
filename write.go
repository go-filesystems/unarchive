// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	gobzip2 "github.com/go-compressions/bzip2"
	"github.com/go-filesystems/overlay"
	szip "github.com/go-filesystems/sevenzip"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

// ErrCannotWrite is returned for a format this package reads and does not write.
//
// A third sentence alongside the other two: "I know what this is, I can read it,
// and I cannot produce one". Only RAR is left in it, and for a reason that is not
// going to change here: no free writer exists.
//
// bzip2 used to be in it too, because the standard library decompresses bzip2 and
// nothing compressed it. That was a missing LIBRARY rather than a property of the
// format, so the library was written -- go-compressions/bzip2 -- and the sentence
// stopped being true. Worth saying, because the two reasons look identical from
// the outside and only one of them is permanent.
var ErrCannotWrite = errors.New("unarchive: this format is read but not written")

// Writable opens an archive and returns it as a filesystem that can be CHANGED,
// without rewriting the archive.
//
// Changes go into a temporary cache; the archive underneath is untouched until
// SealTo. So renaming one entry in a 4 GiB archive costs a rename, not 4 GiB of
// rewriting, and an interrupted session leaves the original exactly as it was.
//
// The caller closes the returned Overlay, which discards the cache. Closing it
// does NOT write anything back: that is SealTo's job, and keeping them apart is
// what makes "I changed my mind" free.
func Writable(path string) (*overlay.Overlay, Format, error) {
	base, format, err := Open(path)
	if err != nil {
		return nil, format, err
	}
	o, err := overlay.New(base)
	if err != nil {
		base.Close()
		return nil, format, err
	}
	return o, format, nil
}

// WriteTarget is what a target name resolves to: an archive format, and an
// optional stream wrapper around it.
type WriteTarget struct {
	Archive Format // FormatTar, FormatZIP or Format7z
	Wrapper Format // FormatUnknown, or one of the stream formats
}

// String names the pair the way a file name spells it.
func (t WriteTarget) String() string {
	if t.Wrapper == FormatUnknown {
		return t.Archive.String()
	}
	return t.Archive.String() + "+" + t.Wrapper.String()
}

// writeSuffixes map an output name to what to write. Longest first, so
// ".tar.gz" is not read as ".gz" holding a lone file.
var writeSuffixes = []struct {
	suffix  string
	archive Format
	wrapper Format
}{
	{".tar.gz", FormatTar, FormatGzip}, {".tgz", FormatTar, FormatGzip},
	{".tar.xz", FormatTar, FormatXZ}, {".txz", FormatTar, FormatXZ},
	{".tar.zst", FormatTar, FormatZstd}, {".tzst", FormatTar, FormatZstd},
	{".tar.lz4", FormatTar, FormatLZ4},
	{".tar.bz2", FormatTar, FormatBzip2}, {".tbz2", FormatTar, FormatBzip2},
	{".tbz", FormatTar, FormatBzip2},
	{".tar", FormatTar, FormatUnknown},
	{".7z", Format7z, FormatUnknown},
	{".a", FormatAr, FormatUnknown}, {".deb", FormatAr, FormatUnknown},
	{".cpio", FormatCpio, FormatUnknown},
	{".iso", FormatISO9660, FormatUnknown},
	{".zip", FormatZIP, FormatUnknown},
	{".jar", FormatZIP, FormatUnknown},
	{".bz2", FormatTar, FormatBzip2},
	// Read but not written, named so the error can say WHICH. Leaving them out
	// entirely makes TargetFor answer ErrUnknownFormat -- "the bytes match no
	// format this knows" for a format this package reads perfectly well, which is
	// both false and unhelpful in one sentence.
	{".rar", FormatRAR, FormatUnknown},
	// ⛔ LOWER CASE. TargetFor lower-cases the name before matching, so ".Z" here
	// never matches anything -- which is how the first attempt at this left the
	// message unchanged while the table looked right.
	{".tar.z", FormatTar, FormatZ}, {".taz", FormatTar, FormatZ},
	{".z", FormatTar, FormatZ},
}

// TargetFor says what to write into a file with this name.
//
// ⛔ This is the one place in the package where the NAME decides, and it has to:
// an output file has no bytes yet to read. Everywhere else a format comes from
// the content, because an extension is a claim by whoever last renamed the file;
// here the extension is the only statement of intent there is.
func TargetFor(name string) (WriteTarget, error) {
	lower := strings.ToLower(filepath.Base(name))
	for _, s := range writeSuffixes {
		if !strings.HasSuffix(lower, s.suffix) {
			continue
		}
		t := WriteTarget{Archive: s.archive, Wrapper: s.wrapper}
		switch {
		case s.archive == FormatRAR:
			return t, fmt.Errorf("%s: rar: %w", name, ErrCannotWrite)
		case s.wrapper == FormatZ:
			// General sentence first, specific one after -- the same order Open
			// uses for a Note, and for the same reason: the other way round left
			// the sentinel trailing after a paragraph that had said more.
			return t, fmt.Errorf("%s: %w: %s", name, ErrCannotWrite, FormatZ.Note())
		}
		return t, nil
	}
	return WriteTarget{}, fmt.Errorf("%s: %w", name, ErrUnknownFormat)
}

// SealTo writes everything the overlay holds into target, once.
//
// The format comes from target's name -- see TargetFor -- and the write is
// atomic: the archive is built beside target and RENAMED over it, so an
// interrupted seal leaves whatever was there before untouched rather than
// half-replaced.
func SealTo(o *overlay.Overlay, target string) error {
	t, err := TargetFor(target)
	if err != nil {
		return err
	}
	if t.Wrapper == FormatUnknown {
		return o.SealToFile(target, builderFor(t.Archive))
	}

	// A compressed target needs the archive to exist before it can be
	// compressed: every builder here writes backwards at some point -- a zip's
	// central directory, a 7z's header offset -- and a compressed stream cannot
	// seek. So the archive is built to a spool and then squeezed into place.
	dir := filepath.Dir(target)
	spool, err := os.CreateTemp(dir, "."+filepath.Base(target)+".building-")
	if err != nil {
		return err
	}
	spoolName := spool.Name()
	defer func() {
		spool.Close()
		os.Remove(spoolName)
	}()
	b, err := builderFor(t.Archive)(spool)
	if err != nil {
		return err
	}
	if err := o.Seal(b); err != nil {
		return err
	}
	if c, ok := b.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return err
		}
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return compressInto(target, t.Wrapper, spool)
}

// compressInto writes r through the wrapper into target, by rename.
func compressInto(target string, wrapper Format, r io.Reader) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".sealing-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	w, finish, err := compressor(wrapper, tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	// The compressor's last block is written by its Close, so finishing it
	// before the Sync is not an ordering detail: a file synced first and
	// finished after is missing its end.
	if err := finish(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

// compressor is the writer a wrapper format is written through.
func compressor(f Format, w io.Writer) (io.Writer, func() error, error) {
	switch f {
	case FormatGzip:
		z := gzip.NewWriter(w)
		return z, z.Close, nil
	case FormatXZ:
		z, err := xz.NewWriter(w)
		if err != nil {
			return nil, nil, err
		}
		return z, z.Close, nil
	case FormatZstd:
		z, err := zstd.NewWriter(w)
		if err != nil {
			return nil, nil, err
		}
		return z, z.Close, nil
	case FormatLZ4:
		z := lz4.NewWriter(w)
		return z, z.Close, nil
	case FormatBzip2:
		z := gobzip2.NewWriter(w)
		return z, z.Close, nil
	}
	return nil, nil, fmt.Errorf("%s: %w", f, ErrCannotWrite)
}

// builderFor is the maker SealToFile wants for an archive format.
func builderFor(f Format) func(io.WriteSeeker) (overlay.Builder, error) {
	return func(w io.WriteSeeker) (overlay.Builder, error) {
		switch f {
		case FormatTar:
			return &tarBuilder{w: tar.NewWriter(w)}, nil
		case FormatZIP:
			return &zipBuilder{w: zip.NewWriter(w)}, nil
		case Format7z:
			return szip.NewWriter(w)
		case FormatAr:
			return newArBuilder(w)
		case FormatCpio:
			return newCpioBuilder(w), nil
		case FormatISO9660:
			// The volume id is what a mounted image is CALLED, and eleven
			// characters is all ISO 9660 allows without an extension this builder
			// does not write.
			return newISOBuilder(w, "UNARCHIVE"), nil
		}
		return nil, fmt.Errorf("%s: %w", f, ErrCannotWrite)
	}
}

// tarBuilder is the one that needs the size, and the reason the contract carries
// it: a tar entry's length goes in its header, before its bytes.
type tarBuilder struct{ w *tar.Writer }

func (b *tarBuilder) AddDir(name string, perm fs.FileMode) error {
	return b.w.WriteHeader(&tar.Header{
		// A tar directory's name ends in a slash by convention, and readers use
		// it: an entry with Typeflag set and no slash is accepted by Go and
		// confuses bsdtar's listing.
		Name:     ensureSlash(name),
		Mode:     int64(perm.Perm()),
		Typeflag: tar.TypeDir,
	})
}

func (b *tarBuilder) AddFile(name string, perm fs.FileMode, size int64, r io.Reader) error {
	if err := b.w.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     int64(perm.Perm()),
		Size:     size,
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	// CopyN rather than Copy, and the difference is the whole contract: the
	// header already said size, so writing one byte more or fewer produces an
	// archive whose header disagrees with its data. CopyN fails instead, here,
	// where the name is still in hand.
	n, err := io.CopyN(b.w, r, size)
	if err != nil {
		return fmt.Errorf("%s: wrote %d of the %d bytes declared: %w", name, n, size, err)
	}
	return nil
}

func (b *tarBuilder) Close() error { return b.w.Close() }

// zipBuilder writes the format everything opens.
type zipBuilder struct{ w *zip.Writer }

func (b *zipBuilder) AddDir(name string, perm fs.FileMode) error {
	h := &zip.FileHeader{Name: ensureSlash(name)}
	h.SetMode(perm.Perm() | fs.ModeDir)
	_, err := b.w.CreateHeader(h)
	return err
}

func (b *zipBuilder) AddFile(name string, perm fs.FileMode, size int64, r io.Reader) error {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(perm.Perm())
	w, err := b.w.CreateHeader(h)
	if err != nil {
		return err
	}
	n, err := io.CopyN(w, r, size)
	if err != nil {
		return fmt.Errorf("%s: wrote %d of the %d bytes declared: %w", name, n, size, err)
	}
	return nil
}

func (b *zipBuilder) Close() error { return b.w.Close() }

// ensureSlash gives a directory name the trailing slash both formats expect.
func ensureSlash(name string) string {
	if strings.HasSuffix(name, "/") {
		return name
	}
	return path.Clean(name) + "/"
}
