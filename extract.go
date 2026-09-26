// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// ErrEscapes is returned for an entry whose path would land outside the
// destination.
//
// An archive is somebody else's list of names, and "../../.ssh/authorized_keys"
// is a name. Every extractor that trusted it has had the same bug, so this one
// does not trust it: the resolved path is checked against the destination, and
// an entry that leaves it is refused rather than sanitised, because silently
// rewriting a path puts the file somewhere the person did not ask for either.
var ErrEscapes = errors.New("unarchive: entry would be written outside the destination")

// ErrExists is returned when a file is already there and Overwrite is not set.
var ErrExists = errors.New("unarchive: file exists")

// ErrIncomplete is returned when an entry yielded fewer bytes than its header
// declared.
//
// It is the point of this package. An extractor that writes what it got and
// reports success leaves a file of exactly the right SIZE when the source was
// zero-padded, and every check afterwards agrees with it.
var ErrIncomplete = errors.New("unarchive: entry is shorter than its header declares")

// Options tune an extraction.
type Options struct {
	// Overwrite lets an existing file be replaced. Off by default: an
	// extraction that overwrites by default destroys data on a mistyped
	// destination, and the mistake is not recoverable.
	Overwrite bool

	// Perm is the mode for created files and directories, before umask.
	// Zero means 0o644 for files and 0o755 for directories.
	Perm os.FileMode

	// Progress, when set, is told each entry as it completes.
	Progress func(name string, bytes int64)
}

// Result is what an extraction did.
type Result struct {
	Dirs  int
	Files int
	// Links counts symbolic links written. They were counted as Files until
	// today, and written as empty regular ones -- see walk.
	Links int
	Bytes int64
}

// Extract writes everything in fsys under dest.
//
// Each file is compared against the size its header declared, and a shortfall
// is an error rather than a smaller number -- see ErrIncomplete.
func Extract(fsys filesystem.Filesystem, dest string, opt Options) (*Result, error) {
	opener, ok := fsys.(filesystem.Opener)
	if !ok {
		return nil, errors.New("unarchive: this filesystem hands out no file handles")
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absDest, dirPerm(opt)); err != nil {
		return nil, err
	}
	res := &Result{}
	if err := walk(fsys, opener, "", absDest, opt, res); err != nil {
		return res, err
	}
	return res, nil
}

func dirPerm(opt Options) os.FileMode {
	if opt.Perm != 0 {
		return opt.Perm | 0o111
	}
	return 0o755
}

func filePerm(opt Options) os.FileMode {
	if opt.Perm != 0 {
		return opt.Perm
	}
	return 0o644
}

func walk(fsys filesystem.Filesystem, opener filesystem.Opener, dir, absDest string, opt Options, res *Result) error {
	entries, err := fsys.ListDir(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", displayDir(dir), err)
	}
	for _, e := range entries {
		// Refused HERE, on the component, before anything joins it. A name of
		// ".." is not a name, it is a traversal -- and path.Join normalises it
		// away, so a check made after joining is never asked the dangerous
		// question. That is how "a/../../escaped.txt" passed: the archive listed
		// a directory called "..", join turned "a/.." into ".", and the result
		// sat inside the destination while the walk climbed out of it.
		if name := e.Name(); name == "" || name == "." || name == ".." ||
			strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("%q in %q: %w", name, displayDir(dir), ErrEscapes)
		}
		inner := path.Join(dir, e.Name())
		target, err := resolve(absDest, inner)
		if err != nil {
			return err
		}
		if e.FileType() == fileTypeDir {
			// ⛔ A symbolic link already sitting here is refused rather than
			// followed. MkdirAll walks THROUGH a link to a directory, so an
			// archive that lists a link called "sub" and then a directory called
			// "sub" would have this writing wherever the link points -- which is
			// the whole of the symlink extraction attack, and resolve() cannot
			// see it because the path it checked was perfectly well behaved.
			if err := refuseLinkAt(target, inner); err != nil {
				return err
			}
			if err := os.MkdirAll(target, dirPerm(opt)); err != nil {
				return err
			}
			res.Dirs++
			if err := walk(fsys, opener, inner, absDest, opt, res); err != nil {
				return err
			}
			continue
		}
		// Asked BEFORE extractFile, because a link is not a file with no bytes in
		// it -- which is what every one of them came out as until today: an empty
		// regular file, silently, counted in Files, while ReadLink had the target
		// all along.
		if link, ok := isSymlink(fsys, inner, e); ok {
			if err := extractSymlink(link, target, opt); err != nil {
				return fmt.Errorf("%s: %w", inner, err)
			}
			res.Links++
			if opt.Progress != nil {
				opt.Progress(inner, 0)
			}
			continue
		}
		n, err := extractFile(fsys, opener, inner, target, opt)
		if err != nil {
			return fmt.Errorf("%s: %w", inner, err)
		}
		res.Files++
		res.Bytes += n
		if opt.Progress != nil {
			opt.Progress(inner, n)
		}
	}
	return nil
}

// fileTypeDir is the directory marker this org's DirEntry carries.
const fileTypeDir = 2

// fileTypeSymlink is DT_LNK, which is the value go-filesystems' drivers agree on
// for a symbolic link.
//
// ⛔ Not every driver sets it. This package's own tar reader reported 0 for
// everything that was not a directory, so the type byte alone cannot answer "is
// this a link" -- ReadLink is what answers that, and the type byte is used to
// tell a driver that does NOT know from one that says no. See isSymlink.
const fileTypeSymlink = 10

func displayDir(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// resolve turns an entry's own name into a path under dest, and refuses one
// that would leave.
func resolve(absDest, inner string) (string, error) {
	if path.IsAbs(inner) || strings.HasPrefix(inner, "../") || inner == ".." {
		return "", fmt.Errorf("%q: %w", inner, ErrEscapes)
	}
	target := filepath.Join(absDest, filepath.FromSlash(inner))
	rel, err := filepath.Rel(absDest, target)
	if err != nil {
		return "", err
	}
	// Checked after joining as well as before, because a name can be made of
	// pieces that are each harmless: the answer that matters is where the path
	// ENDED UP, not what it looked like.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q: %w", inner, ErrEscapes)
	}
	return target, nil
}

// isSymlink says whether this entry is a symbolic link, and what it points at.
//
// ⛔ There is no shared sentinel for "not a link" across the org's drivers -- this
// package's tar reader returns a bare errors.New, xar and rpm each return their
// own -- so the error cannot be matched. What can be said is this:
//
//   - a driver that DECLARES fileTypeSymlink is believed, and a ReadLink that
//     then fails is a real failure rather than a "no". Reporting the entry as a
//     file there would put an empty one where a link belongs, which is the defect
//     this function exists to end;
//   - a driver that declares nothing (tar reports 0 for everything that is not a
//     directory) is asked, and a target that comes back is taken at its word.
//
// An empty target is not a link either way: a link to nowhere is not something an
// archive can record, and treating it as one would make a truncated header read as
// a link to the current directory.
func isSymlink(fsys filesystem.Filesystem, inner string, e filesystem.DirEntry) (string, bool) {
	link, err := fsys.ReadLink(inner)
	switch {
	case err == nil && link != "":
		return link, true
	case e.FileType() == fileTypeSymlink:
		// Declared a link and would not say where to. Nothing good can be written
		// here, so the caller is told to stop rather than given a file.
		return "", true
	}
	return "", false
}

// extractSymlink writes one link, with the target the archive recorded.
//
// ⛔ The target is NOT rewritten and NOT refused, including when it is absolute or
// climbs above dest. That is a decision, and here is the whole of it.
//
// Creating a link writes nothing outside dest: a target is a string in an inode.
// What would write outside is a later entry going THROUGH the link, and that is
// closed separately and unconditionally -- refuseLinkAt for a directory, and
// extractFile for a file. Those two are the guarantee; refusing the target would
// add nothing to them.
//
// What refusing WOULD do is fail the other duty. An RPM or a .deb commonly holds
// absolute links (/etc/alternatives/... among them), and an archive whose links
// are refused is an archive this package cannot extract at all. Rewriting the
// target instead -- stripping a leading separator, as bsdtar does -- produces a
// tree that means something different from what was packed, which is worse than
// either.
//
// So the link is written as recorded, and a caller walking the result afterwards
// is in the same position as one walking what tar wrote. That is said in the
// README rather than left to be found out.
func extractSymlink(link, target string, opt Options) error {
	if link == "" {
		return fmt.Errorf("declared a symbolic link and no target: %w", ErrIncomplete)
	}
	if err := os.MkdirAll(filepath.Dir(target), dirPerm(opt)); err != nil {
		return err
	}
	// Lstat rather than Stat, so a link that is already there is seen as a link
	// rather than as whatever it points at. The rule is extractFile's: replaced
	// only when asked.
	if _, err := os.Lstat(target); err == nil {
		if !opt.Overwrite {
			return fmt.Errorf("%s: %w", target, ErrExists)
		}
		// Remove, never truncate: truncating would write through it.
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	return os.Symlink(filepath.FromSlash(link), target)
}

// refuseLinkAt stops a write from going through a symbolic link that is already
// there. See the call in walk for what that prevents.
func refuseLinkAt(target, inner string) error {
	fi, err := os.Lstat(target)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	return fmt.Errorf("%q: a symbolic link is already there, and writing through "+
		"it would leave %s: %w", inner, filepath.Dir(target), ErrEscapes)
}

// extractFile writes one entry and checks it against its declared size.
func extractFile(fsys filesystem.Filesystem, opener filesystem.Opener, inner, target string, opt Options) (int64, error) {
	st, err := fsys.Stat(inner)
	if err != nil {
		return 0, err
	}
	declared := int64(st.Size())

	h, err := opener.OpenFile(inner)
	if err != nil {
		return 0, err
	}
	defer h.Close()

	if err := os.MkdirAll(filepath.Dir(target), dirPerm(opt)); err != nil {
		return 0, err
	}
	// ⛔ O_TRUNC follows a symbolic link, so overwriting one writes to whatever it
	// points at. With Overwrite the link is removed and a real file put in its
	// place; without it, ErrExists is the answer anyway.
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 && opt.Overwrite {
		if err := os.Remove(target); err != nil {
			return 0, err
		}
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !opt.Overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	out, err := os.OpenFile(target, flags, filePerm(opt))
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return 0, fmt.Errorf("%s: %w", target, ErrExists)
		}
		return 0, err
	}

	written, copyErr := copyEntry(out, h)
	closeErr := out.Close()
	if copyErr != nil {
		return written, copyErr
	}
	if closeErr != nil {
		return written, closeErr
	}
	// The check this package exists for. A file the right length with a
	// zero-filled tail passes every size comparison there is; a file SHORTER
	// than its header said is the same fault caught one step earlier.
	if written != declared {
		return written, fmt.Errorf("%w: %d of %d bytes", ErrIncomplete, written, declared)
	}
	return written, nil
}

// copyEntry streams the whole entry through its ReaderAt. Forwards only, which
// is the direction an archive is cheap in.
func copyEntry(w io.Writer, h filesystem.File) (int64, error) {
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	var off int64
	for {
		n, err := h.ReadAt(buf, off)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return off, werr
			}
			off += int64(n)
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF):
			return off, nil
		default:
			// A driver that names a short entry -- rar.ErrShort -- arrives here,
			// and is passed on rather than turned into a smaller count.
			return off, err
		}
	}
}

// Entry is one thing an archive holds, as [List] reports it.
type Entry struct {
	Path string
	Size int64
	Dir  bool
}

// List names everything in fsys without writing anything.
//
// It walks the same way Extract does -- and refuses the same names, so a person
// who lists before extracting is told about an entry that would leave the
// destination BEFORE several gigabytes land somewhere.
func List(fsys filesystem.Filesystem) ([]Entry, error) {
	var out []Entry
	err := listDir(fsys, "", &out)
	return out, err
}

func listDir(fsys filesystem.Filesystem, dir string, out *[]Entry) error {
	entries, err := fsys.ListDir(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", displayDir(dir), err)
	}
	for _, e := range entries {
		if name := e.Name(); name == "" || name == "." || name == ".." ||
			strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("%q in %q: %w", name, displayDir(dir), ErrEscapes)
		}
		inner := path.Join(dir, e.Name())
		if e.FileType() == fileTypeDir {
			*out = append(*out, Entry{Path: inner, Dir: true})
			if err := listDir(fsys, inner, out); err != nil {
				return err
			}
			continue
		}
		var size int64
		if st, err := fsys.Stat(inner); err == nil {
			size = int64(st.Size())
		}
		*out = append(*out, Entry{Path: inner, Size: size})
	}
	return nil
}
