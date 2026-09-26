// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"errors"
	"fmt"
	"path"
	"sort"
)

// ErrNestedUnderNonDirectory is returned when an archive lists an entry beneath a
// path that is not a directory -- a file under a symbolic link, most often.
var ErrNestedUnderNonDirectory = errors.New("unarchive: an entry is nested under something that is not a directory")

// refuseNestingUnderANonDirectory rejects an archive that lists entries beneath a
// path it also declares to be a file or a symbolic link.
//
// ⛔ This is refused at OPEN because it cannot be reported later. Extract walks
// the tree, and it only descends into entries whose type says directory -- so a
// file listed under a symbolic link is never reached, never written, and never
// mentioned: it disappears with a successful exit and a count that does not
// include it. Measured on a tar holding `link -> /tmp/x` followed by `link/pwned`:
// Files was 0, err was nil, and the entry was simply gone.
//
// Nothing was written outside the destination -- extractSymlink and refuseLinkAt
// see to that, and the same measurement confirmed the outside file was untouched.
// The problem this closes is the silence, not an escape.
//
// go-filesystems/xar refuses the same shape in its own reader, so an archive that
// is malformed this way is now refused wherever it comes from.
//
// isDir answers for a path the archive declared; kids maps a directory to the
// paths listed under it.
func refuseNestingUnderANonDirectory(kids map[string][]string, isDir func(string) bool) error {
	// Sorted, so the same archive always names the same entry. An unordered map
	// would make the message depend on the run.
	parents := make([]string, 0, len(kids))
	for parent := range kids {
		parents = append(parents, parent)
	}
	sort.Strings(parents)
	for _, parent := range parents {
		if parent == "." || parent == "" || parent == "/" {
			continue
		}
		if isDir(parent) {
			continue
		}
		children := append([]string(nil), kids[parent]...)
		sort.Strings(children)
		if len(children) == 0 {
			continue
		}
		return fmt.Errorf("%q lists %q beneath it and is not a directory: %w",
			parent, path.Base(children[0]), ErrNestedUnderNonDirectory)
	}
	return nil
}
