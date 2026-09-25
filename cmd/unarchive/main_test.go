// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-filesystems/unarchive"
)

func TestNamingNoArchiveSaysSo(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run(nil, &out, &errOut)
	if err == nil {
		t.Fatal("no arguments was accepted")
	}
	if !strings.Contains(errOut.String(), "usage:") {
		t.Errorf("the usage was not shown; stderr held %q", errOut.String())
	}
}

// TestSomethingThatIsNotAnArchiveIsNamedAsSuch: "I do not know what this is"
// has to read differently from "I know and cannot open it yet", because they
// send a person to different places.
func TestSomethingThatIsNotAnArchiveIsNamedAsSuch(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(plain, []byte("nothing archival about this"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := run([]string{plain}, &out, &errOut)
	if !errors.Is(err, unarchive.ErrUnknownFormat) {
		t.Errorf("a plain file gave %v, want ErrUnknownFormat", err)
	}

	// A recognised format that is not read yet says the other thing. A zip's
	// magic is enough to be recognised by.
	zip := filepath.Join(dir, "something.zip")
	if err := os.WriteFile(zip, []byte("PK\x03\x04and then some"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = run([]string{zip}, &out, &errOut)
	if !errors.Is(err, unarchive.ErrNotImplemented) {
		t.Errorf("a zip gave %v, want ErrNotImplemented", err)
	}
	if !strings.Contains(err.Error(), "zip") {
		t.Errorf("the error does not name the format: %v", err)
	}
}

// TestStemIsWhatADirectoryShouldBeCalled: the default destination is made from
// the archive's name, so a volume marker must not end up in it -- "film.part1"
// is not the name of anything.
func TestStemIsWhatADirectoryShouldBeCalled(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"film.rar", "film"},
		{"film.part1.rar", "film"},
		{"film.part12.rar", "film"},
		{"[MOVIE] Ange Elle 1080p.part1 [49736866].rar", "[MOVIE] Ange Elle 1080p"},
		{"notes.zip", "notes"},
		{"no-extension", "no-extension"},
	} {
		if got := stem(c.in); got != c.want {
			t.Errorf("stem(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanSizeReadsLikeASize(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
	} {
		if got := humanSize(c.in); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
