package unarchive

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// arNegativeBSDName is a 68-byte ar archive whose only member is named
// "#1/-5": a BSD long name with a NEGATIVE length.
func arNegativeBSDName() []byte {
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	// name[16] mtime[12] uid[6] gid[6] mode[8] size[10] magic[2]
	fmt.Fprintf(&b, "%-16s%-12s%-6s%-6s%-8s%-10s`\n", "#1/-5", "0", "0", "0", "644", "0")
	return b.Bytes()
}

// TestAuditArNegativeBSDNameLength: openAr must refuse the archive with an
// error. On HEAD it panics in make([]byte, n) with n = -5.
func TestAuditArNegativeBSDNameLength(t *testing.T) {
	data := arNegativeBSDName()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("openAr PANICKED on a %d-byte archive: %v", len(data), r)
		}
	}()
	_, err := openAr(bytes.NewReader(data), int64(len(data)), nil)
	if err == nil {
		t.Fatal("openAr accepted a negative BSD long-name length")
	}
}

// TestAuditArNegativeBSDNameLengthViaOpen is the same through the public entry
// point, to show that the content sniffing routes the file there.
func TestAuditArNegativeBSDNameLengthViaOpen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "evil.a")
	if err := os.WriteFile(p, arNegativeBSDName(), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Open PANICKED: %v", r)
		}
	}()
	fsys, _, err := Open(p)
	if err == nil {
		fsys.Close()
		t.Fatal("Open accepted a negative BSD long-name length")
	}
}
