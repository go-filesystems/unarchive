// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"fmt"
	"os"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/rar"
)

// Open reads the archive at path, deciding what it is from its bytes, and
// returns it as a filesystem together with the format it turned out to be.
//
// A multi-volume set is followed from here: the rar driver resolves the rest of
// the set by volume NUMBER within path's directory, so files carrying an id a
// download inserted are still reached and nothing is renamed.
func Open(path string) (filesystem.Filesystem, Format, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, FormatUnknown, err
	}
	format, err := Sniff(f)
	closeErr := f.Close()
	if err != nil {
		return nil, format, fmt.Errorf("%s: %w", path, err)
	}
	if closeErr != nil {
		return nil, format, closeErr
	}
	switch format {
	case FormatRAR:
		fsys, err := rar.Open(path)
		if err != nil {
			return nil, format, fmt.Errorf("%s: %w", path, err)
		}
		return fsys, format, nil
	default:
		// Recognised and not read yet, which is a different sentence from "I do
		// not know what this is" and sends a person somewhere else.
		return nil, format, fmt.Errorf("%s: %s: %w", path, format, ErrNotImplemented)
	}
}
