// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"compress/gzip"
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

// TestThreeKindsOfNoAreThreeDifferentSentences.
//
// "I do not know what this is", "I know exactly what this is and do not read it
// yet", and "I read this format and this file is broken" send a person to three
// different places. Collapsing them into one error is how somebody spends an
// evening on the wrong question.
func TestThreeKindsOfNoAreThreeDifferentSentences(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer

	// 1. Nothing recognises the bytes.
	plain := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(plain, []byte("nothing archival about this"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{plain}, &out, &errOut); !errors.Is(err, unarchive.ErrUnknownFormat) {
		t.Errorf("a plain file gave %v, want ErrUnknownFormat", err)
	}

	// 2. Recognised, not read yet -- and the fixture is VALID, because that is
	// what this sentence is about. A truncated file would land in case 3 no
	// matter which formats are wired.
	//
	// gzip is the format standing in for it today. When gzip is wired this row
	// moves to whatever is still unread, and the test is coupled to the
	// TAXONOMY rather than to the list -- which is the mistake it made twice: it
	// used 7z, then a zip, and each became case 3 the day that format landed.
	gz := filepath.Join(dir, "something.gz")
	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write([]byte("a real stream, properly framed")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gz, gzBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run([]string{gz}, &out, &errOut)
	if !errors.Is(err, unarchive.ErrNotImplemented) {
		t.Errorf("a valid gzip gave %v, want ErrNotImplemented", err)
	}
	if err != nil && !strings.Contains(err.Error(), "gzip") {
		t.Errorf("the error does not name the format: %v", err)
	}

	// 3. Recognised, read, and genuinely broken. Neither of the sentences above
	// fits: the format is known and wired, and the FILE is the problem.
	truncated := filepath.Join(dir, "broken.zip")
	if err := os.WriteFile(truncated, []byte("PK\x03\x04and then nothing useful"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = run([]string{truncated}, &out, &errOut)
	if err == nil {
		t.Fatal("a truncated zip was accepted")
	}
	if errors.Is(err, unarchive.ErrUnknownFormat) || errors.Is(err, unarchive.ErrNotImplemented) {
		t.Errorf("a broken zip was reported as unknown or unimplemented: %v", err)
	}
	if !strings.Contains(err.Error(), "zip") {
		t.Errorf("the error does not say it was reading a zip: %v", err)
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
