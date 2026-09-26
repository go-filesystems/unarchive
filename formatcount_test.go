// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package unarchive

import (
	"embed"
	"regexp"
	"sort"
	"testing"
)

// ⛔ format.go's own source, so a test can count the Format constants instead of
// being told how many there are.
//
// Three tests in this package check that they account for every format, and all
// three used to do it by comparing their own list against a number written by
// hand. That compares two things the test author controls: six formats were added
// and the constant still said 16, so every one of those tests kept passing while
// covering none of the new ones. The guard has to read the declarations.
//
//go:embed format.go
var formatSource embed.FS

var formatDecl = regexp.MustCompile(`(?m)^\s+(Format[A-Za-z0-9]+)\s+Format\s+=`)

// declaredFormats is every Format constant, read out of the source.
func declaredFormats(t *testing.T) []string {
	t.Helper()
	b, err := formatSource.ReadFile("format.go")
	if err != nil {
		t.Fatalf("read format.go: %v", err)
	}
	var names []string
	for _, m := range formatDecl.FindAllStringSubmatch(string(b), -1) {
		names = append(names, m[1])
	}
	if len(names) < 10 {
		t.Fatalf("found %d Format constants in format.go, which cannot be right: "+
			"the pattern stopped matching and every count below would be vacuous",
			len(names))
	}
	sort.Strings(names)
	return names
}

// TestTheFormatCountIsReadNotDeclared. Every test that claims to account for all
// the formats is checked against the source, and the number those tests carry is
// checked here in one place.
func TestTheFormatCountIsReadNotDeclared(t *testing.T) {
	names := declaredFormats(t)
	t.Logf("%d Format constants: %v", len(names), names)
	if len(names) != formatsInThisPackage {
		t.Errorf("format.go declares %d formats and the tests are written for %d: "+
			"a format was added and nobody decided which lists it belongs in.\n"+
			"  declared: %v", len(names), formatsInThisPackage, names)
	}
}

// formatsInThisPackage is the one place the number lives, and
// TestTheFormatCountIsReadNotDeclared is what keeps it honest.
const formatsInThisPackage = 24
