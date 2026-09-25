// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/zip"
	"compress/bzip2"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// The zip compression methods this package handles beyond the two archive/zip has
// of its own.
//
// Only bzip2 and LZMA are exercised by fixtures here, and that is the honest
// split: 7-Zip writes those two into a zip and does not write xz or zstd ones, so
// no tool on this machine can produce them. Those two carry NO zip-specific
// framing -- the entry is the bare stream -- while LZMA does, and it is the one
// that could be wrong. PPMd is refused by name; see registerZipMethods.
//
// ⛔ A zip is not a deflate container. archive/zip reads STORED and DEFLATE and
// nothing else, so a zip written by 7-Zip or WinZip with any other method fails
// with "zip: unsupported compression algorithm" -- on a file every other
// unarchiver opens. Each decoder below was already in this module's dependency
// graph before it was wired here.
const (
	zipMethodBzip2 = 12
	zipMethodLZMA  = 14
	zipMethodZstd  = 93
	zipMethodXZ    = 95
	zipMethodPPMd  = 98
)

// registerZipMethods teaches a *zip.Reader the methods above.
//
// It takes the register function rather than the reader so that both the path and
// the split reader wire the same set: a method taught to one and not the other is
// the kind of difference nobody notices until a split archive uses it.
//
// ⛔ The parameter is typed zip.Decompressor, not a structurally identical
// func(io.Reader) io.ReadCloser. Go matches a NAMED function type by name, so the
// obvious spelling does not compile -- the same rule that made an anonymous
// interface unsatisfiable elsewhere in this package, in its function form.
func registerZipMethods(register func(uint16, zip.Decompressor)) {
	register(zipMethodBzip2, func(r io.Reader) io.ReadCloser {
		return io.NopCloser(bzip2.NewReader(r))
	})
	register(zipMethodXZ, func(r io.Reader) io.ReadCloser {
		return errReadCloser(func() (io.Reader, error) { return xz.NewReader(r) })
	})
	register(zipMethodZstd, func(r io.Reader) io.ReadCloser {
		return errReadCloser(func() (io.Reader, error) {
			z, err := zstd.NewReader(r)
			if err != nil {
				return nil, err
			}
			return z.IOReadCloser(), nil
		})
	})
	register(zipMethodLZMA, func(r io.Reader) io.ReadCloser {
		return errReadCloser(func() (io.Reader, error) { return newZipLZMA(r) })
	})
	// ⛔ PPMd is REFUSED BY NAME rather than decoded, and the reason is a variant
	// mismatch that looks like a bug if it is not said out loud.
	//
	// A zip's method 98 is PPMd variant I. The PPMd in this module's dependency
	// graph -- stangelandcl/ppmd, which bodgit/sevenzip uses -- says in its own
	// first line that it is "PPMD variant H with 7-zip extensions", which is what
	// a .7z carries. Wiring it here decoded the model into "invalid SummFreq <
	// count" on a real 7-Zip archive; a decoder that agreed with itself for longer
	// would have produced plausible rubbish instead.
	//
	// Leaving it unregistered gives "zip: unsupported compression algorithm",
	// which is true and sends nobody anywhere. This says which variant is missing.
	register(zipMethodPPMd, func(io.Reader) io.ReadCloser {
		return errReadCloser(func() (io.Reader, error) {
			return nil, fmt.Errorf("zip ppmd (method %d) is PPMd variant I, and the "+
				"PPMd available here is variant H, the one .7z uses: %w",
				zipMethodPPMd, ErrNotImplemented)
		})
	})
}

// errReadCloser defers a constructor that can fail until the first Read, because
// zip.Decompressor cannot return an error.
//
// The alternative is to panic or to return a reader that silently yields nothing,
// and a silently empty entry is the failure that gets mistaken for an empty file.
func errReadCloser(open func() (io.Reader, error)) io.ReadCloser {
	return &lazyReader{open: open}
}

type lazyReader struct {
	open func() (io.Reader, error)
	r    io.Reader
	err  error
}

func (l *lazyReader) Read(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	if l.r == nil {
		l.r, l.err = l.open()
		if l.err != nil {
			return 0, l.err
		}
	}
	return l.r.Read(p)
}

func (l *lazyReader) Close() error {
	if c, ok := l.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// newZipLZMA reads the LZMA framing a zip puts around the raw stream.
//
// A zip's LZMA entry begins with its OWN four-byte preamble -- two version bytes
// and a two-byte property length -- and then the properties. The ordinary .lzma
// header that decoders expect is those properties followed by an eight-byte
// uncompressed size, which is not here: the size lives in the zip's own header.
// So the header is rebuilt with the size marked UNKNOWN, which makes the decoder
// stop at the end-of-stream marker instead.
func newZipLZMA(r io.Reader) (io.Reader, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("zip lzma: preamble: %w", err)
	}
	propLen := binary.LittleEndian.Uint16(head[2:4])
	if propLen == 0 || propLen > 64 {
		return nil, fmt.Errorf("zip lzma: %d property bytes: %w", propLen, ErrUnknownFormat)
	}
	props := make([]byte, propLen)
	if _, err := io.ReadFull(r, props); err != nil {
		return nil, fmt.Errorf("zip lzma: properties: %w", err)
	}
	var size [8]byte
	for i := range size {
		size[i] = 0xFF // unknown: stop at the end marker
	}
	return lzma.NewReader(io.MultiReader(
		newByteReader(props), newByteReader(size[:]), r))
}

// newByteReader is bytes.NewReader without the import, kept next to its only use.
func newByteReader(b []byte) io.Reader { return &byteSlice{b: b} }

type byteSlice struct{ b []byte }

func (s *byteSlice) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

var _ = errors.Is
