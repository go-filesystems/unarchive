// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-filesystems/cpio"
	filesystem "github.com/go-filesystems/interface"
)

// openCpio indexes a cpio archive.
//
// The parsing lives in go-filesystems/cpio now -- newc, crc, odc and the old
// binary variant in both byte orders -- and what is left here is the half that is
// this package's: turning records into a filesystem through newIndexFS.
//
// ⛔ The parser moved out because go-filesystems/rpm had a second copy of it, with
// a notice at the top of its file saying so and naming the cost: a defect fixed in
// one copy is still present in the other. One such defect was already on record --
// a next offset returned as zero, which looped for ever and reported a ten-minute
// timeout rather than a failure.
//
// What did NOT move is the filesystem shape, on purpose. rpm has its whole payload
// decompressed in memory and hands out sections of that; this package indexes into
// a file it keeps open. Sharing the filesystem would have meant one of them taking
// the other's shape.
func openCpio(ra io.ReaderAt, size int64, closer io.Closer) (filesystem.Filesystem, error) {
	recs, err := cpio.RecordsLimit(ra, size, DefaultLimits.resolved().MaxEntries)
	if err != nil {
		// The package's own sentinels are what callers here match on, so the
		// parser's are translated rather than leaked. A caller asking "is this a
		// format you know" must not have to know cpio's spelling of no.
		switch {
		case errors.Is(err, cpio.ErrNotCpio):
			return nil, fmt.Errorf("%w", ErrUnknownFormat)
		case errors.Is(err, cpio.ErrTooLarge):
			return nil, fmt.Errorf("%s: %w", err, ErrTooLarge)
		case errors.Is(err, cpio.ErrTruncated):
			return nil, fmt.Errorf("%s: %w", err, ErrIncomplete)
		}
		return nil, err
	}

	index, err := newIndexFS(ra, toRecords(recs))
	if err != nil {
		return nil, err
	}
	return &closerFS{Filesystem: FromFS(index), closer: nopCloser(closer)}, nil
}

// toRecords carries cpio's records into this package's own, which the index and the
// ar reader share.
//
// ⛔ The NAME is passed through, NOT cleaned here. go-filesystems/cpio returns it as
// recorded -- "./real.txt" from GNU cpio, "real.txt" from others, backslashes from
// an archive made on Windows -- and newIndexFS runs indexClean over every name it is
// given. Cleaning here as well would be a second place stating the same rule, which
// is how two spellings of one name get into a package; an ablation that removed it
// changed nothing, which is how this was noticed.
func toRecords(recs []cpio.Record) []record {
	out := make([]record, 0, len(recs))
	donor := hardLinkDonors(recs)
	for i, r := range recs {
		rec := record{
			name:   r.Name,
			mode:   r.Mode,
			size:   r.Size,
			offset: r.Offset,
			link:   r.Link,
			mtime:  r.MTime,
		}
		if j := donor[i]; j >= 0 {
			rec.size, rec.offset = recs[j].Size, recs[j].Offset
		}
		out = append(out, rec)
	}
	return out
}

// hardLinkDonors finds, for each empty regular record that is a link of a
// multiply-linked inode, the record that holds its data.
//
// ⛔ In the newc and crc formats every name of an inode with several links is
// written with a filesize of 0 EXCEPT THE LAST, which carries the data (cpio(5),
// "New ASCII Format"; GNU cpio and rpm both write it so). Read naively, every
// link but the last extracts as an empty file while Extract reports success.
//
// The parser keeps the inode but not the link count, so a link is recognised the
// way the writers lay it out: a RUN of adjacent regular records with one inode,
// all empty but the last. Requiring adjacency and a non-empty last record keeps
// two unrelated archives concatenated by accident, or one lone empty file, from
// borrowing bytes that are not theirs.
//
// Computed in ONE backward pass (hardLinkDonors), not by scanning ahead from each
// record, which is quadratic for a long run of empty files sharing one inode.
func hardLinkDonors(recs []cpio.Record) []int {
	donor := make([]int, len(recs)) // index of the record holding the data, or -1
	for i := len(recs) - 1; i >= 0; i-- {
		donor[i] = -1
		r := recs[i]
		if r.Size != 0 || !r.Mode.IsRegular() || i+1 == len(recs) {
			continue
		}
		n := recs[i+1]
		if n.Inode != r.Inode || !n.Mode.IsRegular() {
			continue
		}
		if n.Size != 0 {
			donor[i] = i + 1
		} else {
			donor[i] = donor[i+1]
		}
	}
	return donor
}
