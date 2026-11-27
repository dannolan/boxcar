//go:build !windows

package nup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

func TestRenameNoReplace(t *testing.T) {
	dir := t.TempDir()
	temp := filepath.Join(dir, "t")
	out := filepath.Join(dir, "out")
	for _, f := range []string{temp, out} {
		if err := os.WriteFile(f, []byte(filepath.Base(f)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := renameNoReplace(temp, out)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("no exclusive rename on this system")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing output: err = %v", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "out" {
		t.Errorf("existing output was replaced")
	}
	os.Remove(out)
	if err := renameNoReplace(temp, out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "t" || pathExists(temp) {
		t.Errorf("rename did not move the file")
	}
}

func TestLockSourceExcludesSecondOptimizer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "game.nup")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockSource(path); err == nil || !strings.Contains(err.Error(), "Another process") {
		t.Fatalf("second lock: err = %v", err)
	}
	if _, err := Optimize(t.Context(), path, OptimizeOptions{InPlace: true, Force: true}); err == nil ||
		!strings.Contains(err.Error(), "Another process") {
		t.Errorf("optimizing a locked archive: err = %v", err)
	}
	unlock()
	again, err := lockSource(path)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	again()
}

// TestOptimizeInPlaceKeepsPrivateArchivePrivate: the partial copy of a 0600
// archive is never readable by anyone else, even while it is written.
func TestOptimizeInPlaceKeepsPrivateArchivePrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "game.nup")
	writeKaiDump(t, path)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	checked := false
	opts := defaultOptions()
	opts.InPlace = true
	opts.Progress = func(label string, _ float64) {
		if label != "Optimizing NUP files" {
			return
		}
		partials, _ := filepath.Glob(filepath.Join(dir, "*.partial"))
		for _, p := range partials {
			if st, err := os.Stat(p); err == nil {
				checked = true
				if st.Mode().Perm()&^0o600 != 0 {
					t.Errorf("partial file mode %v is wider than the source's 0600", st.Mode().Perm())
				}
			}
		}
	}
	optimizeT(t, path, opts)
	if !checked {
		t.Fatal("never saw the partial file")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("replaced archive mode = %v, want 0600", st.Mode().Perm())
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
