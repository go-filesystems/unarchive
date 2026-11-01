// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Limits.MaxEntries reaches the cpio parser: an archive of more records than the
// limit is refused with ErrTooLarge before the list of them is built.
func TestCpioEntryLimitReachesTheParser(t *testing.T) {
	var buf bytes.Buffer
	b := newCpioBuilder(&buf)
	for i := 0; i < 20; i++ {
		if err := b.AddFile(fmt.Sprintf("f%02d", i), 0o644, 3, bytes.NewReader([]byte("abc"))); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "a.cpio")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	withLimits(t, Limits{MaxEntries: 1000})
	fsys, _, err := Open(p)
	if err != nil {
		t.Fatalf("under the limit: %v", err)
	}
	fsys.Close()

	withLimits(t, Limits{MaxEntries: 5})
	if _, _, err := Open(p); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the limit: %v, want ErrTooLarge", err)
	}
}

// Limits.MaxDictionary reaches lzip: a member coding 512 MiB over eleven bytes is
// refused with ErrTooLarge when the limit is 1 MiB, before anything is allocated.
func TestLzipDictionaryLimitReachesTheReader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bomb.txt.lz")
	if err := os.WriteFile(p, append([]byte("LZIP\x01\x1d"), 0, 0, 0, 0, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	withLimits(t, Limits{MaxDictionary: 1 << 20})
	cost := allocated(func() {
		_, _, err := Open(p)
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("512 MiB member under a 1 MiB limit: %v, want ErrTooLarge", err)
		}
	})
	if cost > 1<<20 {
		t.Errorf("the refusal allocated %d bytes", cost)
	}
}
