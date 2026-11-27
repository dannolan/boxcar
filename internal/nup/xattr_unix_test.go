//go:build linux || darwin

package nup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestOptimizeInPlaceKeepsExtendedAttributes: an in-place rewrite carries the
// original's extended attributes (the store for POSIX ACLs and macOS
// metadata) over to the replacement.
func TestOptimizeInPlaceKeepsExtendedAttributes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "game.nup")
	writeKaiDump(t, path)
	if err := unix.Setxattr(path, "user.boxcar.test", []byte("kept"), 0); err != nil {
		t.Skipf("no user extended attributes here: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.InPlace = true
	if res := optimizeT(t, path, opts); res.Status != "optimized" {
		t.Fatalf("result = %+v", res)
	}
	buf := make([]byte, 16)
	n, err := unix.Getxattr(path, "user.boxcar.test", buf)
	if err != nil || !bytes.Equal(buf[:n], []byte("kept")) {
		t.Fatalf("attribute after replacement: %q, %v", buf[:n], err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", st.Mode().Perm())
	}
}
