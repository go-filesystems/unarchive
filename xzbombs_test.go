// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	cxz "github.com/go-compressions/xz"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// Dictionary bombs for the xz and LZMA decode paths. They must be run under
// GOMEMLIMIT (and ulimit -v on Linux): see the bombs job in .github/workflows/ci.yml.
//
// Measured BEFORE, with ulikunitz/xz v0.5.17 decoding: a 44-byte xz stream declaring
// 1.5 GiB allocated 1,610,663,976 bytes (the dictionary is allocated whole from the
// Block Header, DictCap is a minimum, and nothing bounded the xz path: the .tar.xz
// wrapper and zip method 95 took no limit at all).
// The encoders below are ulikunitz's: the encode side stays on it.

// xzOf compresses data into an .xz stream with ulikunitz's writer.
func xzOf(t testing.TB, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withDictProp rewrites the LZMA2 dictionary property of the first block header of
// a stream (one filter, no sizes) and fixes the header's CRC32. Property 37 declares
// 1.5 GiB, 40 declares 4 GiB - 1 (xz-file-format.txt, 5.3.1).
func withDictProp(t testing.TB, stream []byte, prop byte) []byte {
	t.Helper()
	b := append([]byte(nil), stream...)
	if b[14] != 0x21 || b[15] != 1 {
		t.Fatalf("unexpected block header layout: % x", b[12:20])
	}
	b[16] = prop
	end := 12 + (int(b[12])+1)*4
	binary.LittleEndian.PutUint32(b[end-4:], crc32.ChecksumIEEE(b[12:end-4]))
	return b
}

// randomBytes is incompressible, so a stream of n of them decodes to n bytes of
// window use.
func randomBytes(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// lzmaAloneOf compresses data into a .lzma stream that ends with an end marker, with
// the dictionary the header declares set to dict. Returns the 13-byte header's
// properties byte and the stream.
func lzmaAloneOf(t testing.TB, data []byte, dict uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	cfg := lzma.WriterConfig{DictCap: 1 << 16, EOSMarker: true, Size: -1}
	w, err := cfg.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) < 13 || !bytes.Equal(b[5:13], bytes.Repeat([]byte{0xFF}, 8)) {
		t.Fatalf("unexpected .lzma header: % x", b[:min(len(b), 13)])
	}
	binary.LittleEndian.PutUint32(b[1:5], dict)
	return b
}

// zipLZMAEntry turns a .lzma stream into a zip method 14 entry: the zip preamble
// (versions, property length), the five property bytes, the stream without its
// size field.
func zipLZMAEntry(lz []byte) []byte {
	return append(append([]byte{0, 0, 5, 0}, lz[:5]...), lz[13:]...)
}

func drain(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

// The planted xz bomb: valid streams declaring 1.5 GiB and 4 GiB - 1 over a few
// bytes. The decoder must allocate for the data, not the header.
func TestBombXZDeclaredDictionary(t *testing.T) {
	want := bytes.Repeat([]byte("planted bomb: a huge dictionary over a few bytes. "), 20)
	for _, prop := range []byte{37, 40} {
		s := withDictProp(t, xzOf(t, want), prop)
		var got []byte
		var err error
		n := allocated(func() {
			var r io.Reader
			if r, _, err = decompressor(FormatXZ, bytes.NewReader(s)); err == nil {
				got, err = drain(r)
			}
		})
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("prop %d: %v", prop, err)
		}
		t.Logf("prop %d: %d bytes allocated for a %d-byte stream", prop, n, len(s))
		if n >= oneMiB {
			t.Errorf("prop %d: %d bytes allocated", prop, n)
		}
	}
}

// Limits.MaxDictionary is a ceiling on the window the DATA fills: exactly the limit
// decodes, one byte more is ErrTooLarge, whatever the header declares, on the .tar.xz
// wrapper path (decompressor), through Open (spool) and in a zip entry (method 95).
func TestBombXZCeilingIsExact(t *testing.T) {
	const limit = 1 << 18
	withLimits(t, Limits{MaxDictionary: limit})
	decode := func(stream []byte) ([]byte, error) {
		r, _, err := decompressor(FormatXZ, bytes.NewReader(stream))
		if err != nil {
			return nil, err
		}
		return drain(r)
	}
	exact, over := randomBytes(limit), randomBytes(limit+1)
	for _, prop := range []byte{40, 18} { // 4 GiB - 1, 2 MiB
		if got, err := decode(withDictProp(t, xzOf(t, exact), prop)); err != nil || !bytes.Equal(got, exact) {
			t.Fatalf("prop %d: data of exactly the limit: %v", prop, err)
		}
		_, err := decode(withDictProp(t, xzOf(t, over), prop))
		if !errors.Is(err, ErrTooLarge) || !errors.Is(err, cxz.ErrTooLarge) {
			t.Fatalf("prop %d: one byte over: %v, want ErrTooLarge wrapping the decoder's", prop, err)
		}
	}

	// A benign first stream protects nothing: the second one is refused.
	two := append(xzOf(t, []byte("benign")), withDictProp(t, xzOf(t, over), 40)...)
	if _, err := decode(two); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a bomb in the second stream: %v, want ErrTooLarge", err)
	}

	// The same through Open, which spools: ErrTooLarge, and the spool is removed.
	p := filepath.Join(t.TempDir(), "bomb.bin.xz")
	if err := os.WriteFile(p, withDictProp(t, xzOf(t, over), 40), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(p); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Open of an xz over the limit: %v, want ErrTooLarge", err)
	}
	q := filepath.Join(t.TempDir(), "ok.bin.xz")
	if err := os.WriteFile(q, withDictProp(t, xzOf(t, exact), 40), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys, _, err := Open(q)
	if err != nil {
		t.Fatalf("Open of an xz at the limit: %v", err)
	}
	fsys.Close()

	// Zip method 95 hands the entry to the same decoder.
	var method95 zip.Decompressor
	registerZipMethods(func(m uint16, d zip.Decompressor) {
		if m == zipMethodXZ {
			method95 = d
		}
	})
	rc := method95(bytes.NewReader(withDictProp(t, xzOf(t, over), 40)))
	if _, err := drain(rc); !errors.Is(err, ErrTooLarge) {
		t.Errorf("zip method 95 over the limit: %v, want ErrTooLarge", err)
	}
	rc.Close()
	rc = method95(bytes.NewReader(withDictProp(t, xzOf(t, exact), 40)))
	if got, err := drain(rc); err != nil || !bytes.Equal(got, exact) {
		t.Errorf("zip method 95 at the limit: %v", err)
	}
	rc.Close()

	// A stream that is not xz is the decoder's error, not a size refusal.
	if _, err := decode([]byte("not xz at all, not at all")); err == nil || errors.Is(err, ErrTooLarge) {
		t.Errorf("garbage: %v", err)
	}
}

// The planted zip LZMA bomb (method 14): the entry's LZMA header declares 1.5 GiB or
// 4 GiB - 1 over a few bytes. Before (ulikunitz, DictCap as a header limit): refused
// at 1 MiB, but only by trusting the header; a header within the limit allocated what
// it declared. Now the window follows the data.
func TestBombZipLZMADictionary(t *testing.T) {
	want := bytes.Repeat([]byte("planted bomb: a huge dictionary over a few bytes. "), 20)
	for _, dict := range []uint32{3 << 29, 1<<32 - 1} {
		entry := zipLZMAEntry(lzmaAloneOf(t, want, dict))
		var got []byte
		var err error
		n := allocated(func() {
			var r io.Reader
			if r, err = newZipLZMA(bytes.NewReader(entry)); err == nil {
				got, err = drain(r)
			}
		})
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("dict %d: %v", dict, err)
		}
		t.Logf("dict %d: %d bytes allocated for a %d-byte entry", dict, n, len(entry))
		if n >= oneMiB {
			t.Errorf("dict %d: %d bytes allocated", dict, n)
		}
	}

	// The ceiling is exact on the data, whatever the header declares.
	const limit = 1 << 18
	withLimits(t, Limits{MaxDictionary: limit})
	decode := func(data []byte) ([]byte, error) {
		r, err := newZipLZMA(bytes.NewReader(zipLZMAEntry(lzmaAloneOf(t, data, 1<<30))))
		if err != nil {
			return nil, err
		}
		return drain(r)
	}
	exact := randomBytes(limit)
	if got, err := decode(exact); err != nil || !bytes.Equal(got, exact) {
		t.Fatalf("data of exactly the limit: %v", err)
	}
	if _, err := decode(randomBytes(limit + 1)); !errors.Is(err, ErrTooLarge) || !errors.Is(err, cxz.ErrTooLarge) {
		t.Errorf("one byte over: %v, want ErrTooLarge wrapping the decoder's", err)
	}

	// Reserved properties byte: the decoder's refusal at construction, passed on.
	bad := zipLZMAEntry(lzmaAloneOf(t, want, 1<<20))
	bad[4] = 0xFF
	if _, err := newZipLZMA(bytes.NewReader(bad)); err == nil || errors.Is(err, ErrTooLarge) {
		t.Errorf("properties byte 0xFF: %v, want the decoder's error", err)
	}
	// And the framing refusals that were already there.
	for name, in := range map[string][]byte{
		"short preamble":      {0, 0},
		"no property bytes":   {0, 0, 0, 0},
		"too many properties": {0, 0, 65, 0},
		"short properties":    {0, 0, 5, 0, 0x5d},
	} {
		if _, err := newZipLZMA(bytes.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// FuzzXZDecoders feeds arbitrary bytes to the xz and zip-LZMA decoders under a small
// ceiling: they must return, and never read more than the ceiling plus a block.
func FuzzXZDecoders(f *testing.F) {
	data := []byte("seed seed seed seed seed seed")
	f.Add(xzOf(f, data), true)
	f.Add(withDictProp(f, xzOf(f, data), 40), true)
	f.Add(zipLZMAEntry(lzmaAloneOf(f, data, 1<<20)), false)
	f.Add(zipLZMAEntry(lzmaAloneOf(f, data, 1<<32-1)), false)
	f.Fuzz(func(t *testing.T, in []byte, isXZ bool) {
		old := DefaultLimits
		DefaultLimits = Limits{MaxDictionary: 1 << 16}
		defer func() { DefaultLimits = old }()
		var r io.Reader
		var err error
		if isXZ {
			r, _, err = decompressor(FormatXZ, bytes.NewReader(in))
		} else {
			r, err = newZipLZMA(bytes.NewReader(in))
		}
		if err != nil {
			return
		}
		// A bound on what is read: a decoder that ignored the ceiling would return
		// more than a stream this small can legitimately expand to.
		_, _ = io.Copy(io.Discard, io.LimitReader(r, 64<<20))
	})
}
