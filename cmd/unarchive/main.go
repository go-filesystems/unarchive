// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Command unarchive extracts an archive without being told what it is.
//
// The format is read from the file's own bytes, a multi-volume set is followed
// even when its files carry text a download inserted into their names, and every
// entry is checked against the size its header declared -- because an extractor
// that reports success is not the same as one that got everything.
//
//	unarchive film.part1.rar           # into ./film/
//	unarchive -C /tmp/out film.rar     # into /tmp/out
//	unarchive -n film.rar              # say what would happen, write nothing
//	unarchive -o film.7z film.rar      # convert, without extracting anything
//
// It is pure Go and needs nothing installed: the same binary runs everywhere Go
// compiles.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/rar"
	"github.com/go-filesystems/unarchive"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "unarchive: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("unarchive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dest := fs.String("C", "", "extract into this directory (default: one named after the archive)")
	force := fs.Bool("f", false, "replace files that are already there")
	dry := fs.Bool("n", false, "say what would be extracted, write nothing")
	quiet := fs.Bool("q", false, "say nothing but errors")
	out := fs.String("o", "", "convert into this archive instead of extracting "+
		"(.tar, .tar.gz, .tgz, .tar.xz, .tar.zst, .tar.lz4, .zip, .7z)")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: unarchive [-C dir] [-f] [-n] [-q] archive...\n"+
			"       unarchive -o target archive\n\n"+
			"Extracts an archive, deciding what it is from its bytes rather than its\n"+
			"name, following a multi-volume set, and checking every entry against the\n"+
			"size its header declares.\n\n"+
			"With -o it converts instead: nothing is extracted, and the target's\n"+
			"format comes from its NAME, because an output file has no bytes yet.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("name an archive")
	}
	if *out != "" {
		// One target, so one source: two archives converted into the same file
		// would silently leave only the second, and a flag that quietly discards
		// an argument is worse than one that refuses it.
		if fs.NArg() != 1 {
			return fmt.Errorf("-o takes one archive, not %d", fs.NArg())
		}
		return convert(fs.Arg(0), *out, *force, *quiet, stdout)
	}
	for _, arg := range fs.Args() {
		if err := one(arg, *dest, *force, *dry, *quiet, stdout); err != nil {
			return err
		}
	}
	return nil
}

// convert rewrites one archive as another format, without extracting it.
//
// Nothing is written until the seal, and the seal is a rename, so an
// interruption leaves both the source and whatever the target held before
// exactly as they were.
func convert(arg, target string, force, quiet bool, stdout io.Writer) error {
	// Asked FIRST, before any work: "I cannot write a .Z" should arrive
	// immediately, not after reading a 4 GiB archive.
	t, err := unarchive.TargetFor(target)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil && !force {
		return fmt.Errorf("%s is already there (use -f to replace it)", target)
	}
	o, format, err := unarchive.Writable(arg)
	if err != nil {
		return err
	}
	defer o.Close()
	if !quiet {
		fmt.Fprintf(stdout, "%s: %s -> %s: %s\n",
			filepath.Base(arg), format, filepath.Base(target), t)
		// A format this package writes with a caveat says so HERE, where somebody
		// is choosing the output. .rar is the case: it goes out stored, and
		// finding that out from the file sizes afterwards is finding it out too
		// late.
		if note := t.Archive.Note(); note != "" {
			fmt.Fprintf(stdout, "  note: %s is %s\n", t.Archive, note)
		}
	}
	if err := unarchive.SealTo(o, target); err != nil {
		return err
	}
	if !quiet {
		if st, err := os.Stat(target); err == nil {
			fmt.Fprintf(stdout, "  wrote %s (%s)\n", filepath.Base(target), humanSize(st.Size()))
		}
	}
	return nil
}

func one(arg, dest string, force, dry, quiet bool, stdout io.Writer) error {
	fsys, format, err := unarchive.Open(arg)
	if err != nil {
		return err
	}
	defer fsys.Close()

	if !quiet {
		fmt.Fprintf(stdout, "%s: %s\n", filepath.Base(arg), format)
		// What the volume set turned out to be, when there is one. A hole shows
		// up here rather than halfway through a 3 GB file.
		if r, ok := fsys.(*rar.FS); ok {
			if v := r.VolumeSet(); v != nil && v.Count() > 1 {
				fmt.Fprintf(stdout, "  %d volumes: %v\n", v.Count(), v.Numbers())
			}
		}
	}
	if dest == "" {
		dest = filepath.Join(filepath.Dir(arg), stem(filepath.Base(arg)))
	}
	if dry {
		return list(fsys, stdout)
	}
	opt := unarchive.Options{Overwrite: force}
	if !quiet {
		opt.Progress = func(name string, n int64) {
			fmt.Fprintf(stdout, "  %s (%s)\n", name, humanSize(n))
		}
	}
	res, err := unarchive.Extract(fsys, dest, opt)
	if err != nil {
		return err
	}
	if !quiet {
		fmt.Fprintf(stdout, "  %d file(s), %s into %s\n", res.Files, humanSize(res.Bytes), dest)
	}
	return nil
}

// list prints what the archive holds, so a person can look before landing
// several gigabytes somewhere.
func list(fsys filesystem.Filesystem, stdout io.Writer) error {
	entries, err := unarchive.List(fsys)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Dir {
			fmt.Fprintf(stdout, "  %s/\n", e.Path)
			continue
		}
		fmt.Fprintf(stdout, "  %s (%s)\n", e.Path, humanSize(e.Size))
	}
	fmt.Fprintf(stdout, "  %d entr(y/ies), nothing written\n", len(entries))
	return nil
}

// stem is the archive's name without its extension or its volume marker, which
// is what a directory made for it should be called.
func stem(name string) string {
	// A numbered part comes off first of all, because it sits OUTSIDE everything
	// else: film.zip.001 is a part of film.zip, so the directory is film -- not
	// film.zip, which is what trimming one extension leaves.
	name = unarchive.WithoutPartNumber(name)
	// Then a compressed wrapper's suffix, as a whole: trimming one extension from
	// fixture.tar.gz leaves fixture.tar, and the directory is then named after
	// half a suffix.
	name = unarchive.InnerName(name)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if i := strings.LastIndex(strings.ToLower(name), ".part"); i > 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

// humanSize is a size a person can read.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, exp := float64(n), 0
	for v >= unit && exp < 4 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGT"[exp-1])
}
