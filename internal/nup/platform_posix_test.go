//go:build !windows

package nup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestClaimAndRenameNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	temp := filepath.Join(dir, "out.nup.partial")
	out := filepath.Join(dir, "out.nup")
	if err := os.WriteFile(temp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := claimAndRename(temp, out); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing output: err = %v", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "existing" {
		t.Errorf("existing output was replaced")
	}
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if err := claimAndRename(temp, out); err != nil {
		t.Fatalf("claimAndRename: %v", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "new" || pathExists(temp) {
		t.Errorf("output %q, temp left: %v", got, pathExists(temp))
	}
}

// TestOptimizeOnFilesystemWithoutHardLinks runs against a directory on such a
// filesystem (an exFAT or FAT32 SD card) named by BOXCAR_NOLINK_DIR.
func TestOptimizeOnFilesystemWithoutHardLinks(t *testing.T) {
	root := os.Getenv("BOXCAR_NOLINK_DIR")
	if root == "" {
		t.Skip("set BOXCAR_NOLINK_DIR to a folder on exFAT/FAT32")
	}
	dir, err := os.MkdirTemp(root, "boxcar-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) }) // after the archives' own cleanups close them
	in := filepath.Join(dir, "game.nup")
	expected := writeKaiDump(t, in)
	if err := os.Link(in, filepath.Join(dir, "link.nup")); err == nil {
		t.Skip("BOXCAR_NOLINK_DIR supports hard links")
	}
	opts := defaultOptions()
	opts.Output = filepath.Join(dir, "out.nup")
	if res := optimizeT(t, in, opts); res.Status != "optimized" {
		t.Fatalf("result = %+v", res)
	}
	assertSameContent(t, in, opts.Output, expected)
	if _, err := Optimize(t.Context(), in, opts); err == nil {
		t.Errorf("existing output was not refused")
	}
	opts.Output = ""
	opts.InPlace = true
	if res := optimizeT(t, in, opts); res.Status != "optimized" {
		t.Fatalf("in place: %+v", res)
	}
	assertNoPartials(t, dir)
}
