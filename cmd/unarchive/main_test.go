// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"os/exec"
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

	// 2. Recognised, not read yet -- and there is NOTHING in that bucket any
	// more. This row stood in with 7z, then a zip, then gzip, and each became a
	// success the day its format landed; gzip was the last of them.
	//
	// The row cannot be rewritten with another format, because every format the
	// sniffer recognises is now read. So the sentence is not tested here by
	// example: it is held by the count in
	// unarchive.TestEveryDeclaredFormatIsEitherReadOrDeclaredUnread, which
	// fails if a format is ever added to the sniffer alone. A row that names a
	// format is coupled to the LIST; a count is coupled to the taxonomy, which
	// is what this test is about.
	//
	// What is asserted instead is that the format that used to stand here now
	// WORKS, so that the three sentences stay three and this one does not
	// quietly become case 1 or 3.
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
	if err := run([]string{gz}, &out, &errOut); err != nil {
		t.Errorf("a valid gzip gave %v, and gzip is read now", err)
	}

	// 3. Recognised, read, and genuinely broken. Neither of the sentences above
	// fits: the format is known and wired, and the FILE is the problem.
	truncated := filepath.Join(dir, "broken.zip")
	if err := os.WriteFile(truncated, []byte("PK\x03\x04and then nothing useful"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run([]string{truncated}, &out, &errOut)
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

// TestStemTakesAWholeWrapperSuffixOff.
//
// The directory an archive is extracted into is named after the archive, and a
// compressed wrapper carries TWO extensions. Trimming one leaves "fixture.tar",
// which is a directory named after half a suffix -- valid, and visibly wrong to
// whoever opens the folder.
func TestStemTakesAWholeWrapperSuffixOff(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"fixture.tar.gz", "fixture"},
		{"fixture.tgz", "fixture"},
		{"fixture.tar.bz2", "fixture"},
		{"fixture.tar.xz", "fixture"},
		{"fixture.tar.zst", "fixture"},
		{"fixture.tar.lz4", "fixture"},
		{"notes.txt.gz", "notes"},
		{"plain.zip", "plain"},
		{"plain.7z", "plain"},
		// The volume rule still applies, and still comes after the suffix.
		{"movie.part1.rar", "movie"},
		{"movie.part01.rar", "movie"},
	} {
		if got := stem(c.in); got != c.want {
			t.Errorf("stem(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestConvertWritesTheTargetAndRefusesWhatItCannot.
//
// Four sentences the flag has to say, and each one is a different situation for
// whoever typed it: it worked, the target exists, that format cannot be written,
// and -o takes one archive.
func TestConvertWritesTheTargetAndRefusesWhatItCannot(t *testing.T) {
	bin, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("no tar here to build the fixture with")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes.txt"), []byte("a body"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "source.tar")
	cmd := exec.Command(bin, "-cf", archive, "-C", src, ".")
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, out)
	}

	var out, errOut bytes.Buffer
	target := filepath.Join(dir, "converted.zip")
	if err := run([]string{"-o", target, archive}, &out, &errOut); err != nil {
		t.Fatalf("convert: %v\n%s", err, errOut.String())
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the target was not written: %v", err)
	}
	// Judged by unzip, not by us.
	if unzip, err := exec.LookPath("unzip"); err == nil {
		if o, err := exec.Command(unzip, "-t", target).CombinedOutput(); err != nil {
			t.Errorf("unzip rejected what we wrote: %v\n%s", err, o)
		}
	}

	// It is there now, so a second conversion must refuse rather than replace.
	err = run([]string{"-o", target, archive}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "already there") {
		t.Errorf("converting over an existing target gave %v, want a refusal", err)
	}
	// And -f is the way to say you meant it.
	if err := run([]string{"-f", "-o", target, archive}, &out, &errOut); err != nil {
		t.Errorf("-f -o gave %v, want nil", err)
	}

	// A format that is read and not written, and it must say so BEFORE reading
	// the archive: the check is on the target's name.
	err = run([]string{"-o", filepath.Join(dir, "no.rar"), archive}, &out, &errOut)
	if !errors.Is(err, unarchive.ErrCannotWrite) {
		t.Errorf("-o no.rar gave %v, want ErrCannotWrite", err)
	}

	// And it says so BEFORE looking at the source, which matters when the source
	// is 4 GiB: "I cannot write .rar" should not take a minute to arrive.
	//
	// The discriminator is a source that is not an archive AT ALL. Check the
	// target first and the error is about the target; open the source first and
	// the error is about the source. Both are refusals, so asserting merely that
	// it failed proves nothing -- which is what the first version of this
	// assertion did, using an empty stdout, and an ablation that moved the check
	// after the open stayed green because the line it watched for is printed
	// after both.
	notAnArchive := filepath.Join(dir, "prose.txt")
	if err := os.WriteFile(notAnArchive, []byte("nothing archival about this"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"-o", filepath.Join(dir, "no.rar"), notAnArchive}, &out, &errOut)
	if errors.Is(err, unarchive.ErrUnknownFormat) {
		t.Errorf("the source was read before the target was judged: %v", err)
	}
	if !errors.Is(err, unarchive.ErrCannotWrite) {
		t.Errorf("gave %v, want ErrCannotWrite about the target", err)
	}

	// One target means one source.
	err = run([]string{"-o", filepath.Join(dir, "two.zip"), archive, archive}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "one archive") {
		t.Errorf("two sources into one target gave %v, want a refusal", err)
	}
}
