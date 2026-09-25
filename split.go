// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// numberedPart matches a name ending in a dot and digits: the way `split -n`,
// 7-Zip's -v, and every download manager spell "part N of a bigger file".
var numberedPart = regexp.MustCompile(`^(.*)\.([0-9]{2,})$`)

// ErrMissingPart is returned when a numbered set has a hole in it.
//
// A hole is not the same as the end. Concatenating parts 1, 2 and 4 produces a
// file of a plausible SIZE whose middle is wrong, and every reader then reports
// corruption -- of the archive, which is intact. Refusing here names the part
// that is absent.
var ErrMissingPart = errors.New("unarchive: a part of this split is missing")

// splitParts returns every file of the numbered set path begins, in order, when
// path is its FIRST part and there is more than one.
//
// ⛔ The name decides here, and it has to. Whether more bytes follow is not
// written anywhere in the bytes that are present: part one of a split zip is
// byte-for-byte the beginning of a whole zip. The name is the only statement
// that the rest exists -- which is a different thing from letting the name decide
// what the FORMAT is, and that still comes from the content.
//
// Only a first part starts a set. Handed part three, this returns nothing, so
// `unarchive film.zip.003` reads that file alone and fails on it rather than
// silently doing something clever with a set the caller did not name.
func splitParts(path string) ([]string, error) {
	m := numberedPart.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return nil, nil
	}
	stem, digits := m[1], m[2]
	n, err := strconv.Atoi(digits)
	if err != nil || n != 1 {
		return nil, nil
	}
	dir := filepath.Dir(path)
	parts := []string{path}
	for i := 2; ; i++ {
		// The width is the first part's: .001 is followed by .002, and .01 by
		// .02. A set that changes width mid-way is not one this produces or
		// reads, and guessing both spellings would accept a set that is two.
		name := filepath.Join(dir, fmt.Sprintf("%s.%0*d", stem, len(digits), i))
		if _, err := os.Stat(name); err != nil {
			break
		}
		parts = append(parts, name)
	}
	if len(parts) < 2 {
		return nil, nil
	}
	return parts, nil
}

// joined reads a numbered set as one file.
//
// The parts are opened all at once and their offsets precomputed, so a ReadAt
// lands on the right one by a search rather than by walking: an archive's
// central directory is at the END, which means the first thing every reader does
// is seek past every part.
type joined struct {
	files  []*os.File
	starts []int64 // starts[i] is where files[i] begins in the whole
	size   int64
	names  []string
}

// openJoined opens every part and presents them as one io.ReaderAt.
func openJoined(parts []string) (*joined, error) {
	j := &joined{names: parts}
	for _, p := range parts {
		f, err := os.Open(p)
		if err != nil {
			j.Close()
			return nil, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			j.Close()
			return nil, err
		}
		j.files = append(j.files, f)
		j.starts = append(j.starts, j.size)
		j.size += st.Size()
	}
	return j, nil
}

func (j *joined) Size() int64 { return j.size }

// Close closes every part, and reports the FIRST failure while still closing the
// rest: stopping early would leak the remaining handles.
func (j *joined) Close() error {
	var first error
	for _, f := range j.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	j.files = nil
	return first
}

// ReadAt reads across the part boundaries.
//
// A read that spans two parts is the normal case rather than an edge: the
// boundary falls wherever the split fell, which has nothing to do with where an
// archive's structures are.
//
// ⛔ The loop walks the parts by INDEX, and that is the point. An earlier version
// re-searched for the part on every pass and relied on the read advancing; an
// ablation that broke partAt turned it into an infinite loop, and the test suite
// reported a TIMEOUT rather than a failure -- a hang where a wrong answer was
// wanted. Advancing i unconditionally makes termination a property of the shape
// instead of a property of the data.
func (j *joined) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("unarchive: negative offset")
	}
	if off >= j.size {
		return 0, io.EOF
	}
	read := 0
	for i := j.partAt(off); read < len(p) && i < len(j.files); i++ {
		at := off + int64(read)
		avail := j.partEnd(i) - at
		if avail <= 0 {
			continue
		}
		want := min(int64(len(p)-read), avail)
		n, err := j.files[i].ReadAt(p[read:read+int(want)], at-j.starts[i])
		read += n
		if err != nil && !errors.Is(err, io.EOF) {
			return read, err
		}
		if int64(n) < want {
			// The part is shorter than it was when it was opened, so something
			// rewrote it underneath us. Saying which one beats a silent short
			// read that every reader above reports as a corrupt archive.
			return read, fmt.Errorf("%s: %w", j.names[i], ErrMissingPart)
		}
	}
	if read < len(p) {
		return read, io.EOF
	}
	return read, nil
}

// partEnd is where part i ends in the whole.
func (j *joined) partEnd(i int) int64 {
	if i+1 < len(j.starts) {
		return j.starts[i+1]
	}
	return j.size
}

// partAt says which part holds a whole-file offset, by binary search.
//
// Since ReadAt walks forward from here and skips parts that end before the
// offset, this is an OPTIMISATION: too low is merely slow. Too high skips data,
// which is why the test pins it against a linear search rather than trusting the
// suite to notice -- an ablation that made it always return 0 passes, and that is
// correct behaviour rather than a gap.
func (j *joined) partAt(off int64) int {
	lo, hi := 0, len(j.starts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if j.starts[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// WithoutPartNumber strips a trailing part number, so film.zip.001 gives
// film.zip.
//
// It is exported for the same reason InnerName is: the command needs the answer
// for a different question -- what to call the directory it extracts into -- and
// computing it from filepath.Ext there leaves "film.zip", a directory named after
// the archive's own extension.
func WithoutPartNumber(name string) string {
	if m := numberedPart.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}
