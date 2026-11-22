package unarchive

import (
	"bytes"
	"fmt"
	"testing"
)

// arMember builds one raw member with an arbitrary name and size field.
func arMember(name, size string, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%-16s%-12s%-6s%-6s%-8s%-10s`\n", name, "0", "0", "0", "644", size)
	b.WriteString(body)
	return b.Bytes()
}

// TestArHostileLengthFields: every length field of the ar parser (member size,
// BSD long-name length, SysV string-table index) with out-of-range values must
// give an error, never a panic and never an accepted member.
func TestArHostileLengthFields(t *testing.T) {
	cases := map[string][]byte{
		"bsd negative":        arMember("#1/-5", "0", ""),
		"bsd negative, body":  arMember("#1/-1", "8", "abcdefgh"),
		"bsd > size":          arMember("#1/9", "4", "abcd"),
		"bsd > remaining":     arMember("#1/99", "99", "abcd"),
		"bsd huge":            arMember("#1/99999999999999999999", "4", "abcd"),
		"bsd not a number":    arMember("#1/xx", "4", "abcd"),
		"size beyond end":     arMember("a/", "9999999999", "abcd"),
		"size negative":       arMember("a/", "-4", "abcd"),
		"size not a number":   arMember("a/", "zz", "abcd"),
		"sysv index negative": arMember("/-3", "4", "abcd"),
		"sysv index no table": arMember("/5", "4", "abcd"),
		"sysv index huge":     arMember("/99999999999999999999", "4", "abcd"),
		"string table > end":  arMember("//", "50", "abcd"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			data := append([]byte(arMagic), body...)
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("openAr panicked: %v", r)
				}
			}()
			if _, err := openAr(bytes.NewReader(data), int64(len(data)), nil); err == nil {
				t.Fatal("openAr accepted a hostile length field")
			}
		})
	}
}

// FuzzOpenAr indexes arbitrary bytes as ar and reads every member found. The
// corpus under testdata/fuzz/FuzzOpenAr is committed.
func FuzzOpenAr(f *testing.F) {
	f.Add([]byte(arMagic))
	f.Add(append([]byte(arMagic), arMember("a/", "3", "abc\n")...))
	f.Add(append([]byte(arMagic), arMember("#1/4", "7", "namexyz\n")...))
	f.Add(append([]byte(arMagic), arMember("#1/-5", "0", "")...))
	f.Add(append(append([]byte(arMagic), arMember("//", "8", "long/\nab")...), arMember("/0", "1", "x\n")...))
	f.Fuzz(func(t *testing.T, data []byte) {
		fsys, err := openAr(bytes.NewReader(data), int64(len(data)), nil)
		if err != nil {
			return
		}
		defer fsys.Close()
		ents, err := fsys.ListDir("")
		if err != nil {
			return
		}
		for _, e := range ents {
			_, _ = fsys.ReadFile(e.Name())
		}
	})
}

// TestArLengthBoundaries pins the edges of the guards: a zero-length BSD name
// is accepted (its member has no name, so it is not listed), and a SysV index
// equal to the table length is refused.
func TestArLengthBoundaries(t *testing.T) {
	data := append([]byte(arMagic), arMember("#1/0", "2", "x\n")...)
	if _, err := openAr(bytes.NewReader(data), int64(len(data)), nil); err != nil {
		t.Errorf("#1/0: %v", err)
	}
	data = append([]byte(arMagic), arMember("//", "4", "ab/\n")...)
	data = append(data, arMember("/4", "2", "x\n")...)
	if _, err := openAr(bytes.NewReader(data), int64(len(data)), nil); err == nil {
		t.Error("index == len(table) accepted")
	}
}
