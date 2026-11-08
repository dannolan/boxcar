package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func buildNPDM(tid uint64) []byte {
	b := make([]byte, 0x100)
	copy(b[:4], "META")
	off := uint32(0x80)
	binary.LittleEndian.PutUint32(b[0x70:], off)
	binary.LittleEndian.PutUint32(b[0x74:], 0x20)
	copy(b[off:off+4], "ACI0")
	binary.LittleEndian.PutUint64(b[off+0x10:], tid)
	return b
}

func writeSnapshot(t *testing.T, dir string, tid uint64) {
	t.Helper()
	exefs := filepath.Join(dir, "exefs")
	if err := os.MkdirAll(exefs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exefs, "main"), []byte("main-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exefs, "main.npdm"), buildNPDM(tid), 0o644); err != nil {
		t.Fatal(err)
	}
	romfs := filepath.Join(dir, "romfs")
	if err := os.MkdirAll(romfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(romfs, "file.bin"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVersionFlag(t *testing.T) {
	if code := run([]string{"-version"}); code != 0 {
		t.Errorf("run(-version) = %d, want 0", code)
	}
}

func TestBuildCommand(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, 0x0100000000010000)
	out := filepath.Join(t.TempDir(), "game.nsp")
	code := run([]string{"build", "--package", root, "--output", out, "--title", "Test Game", "--skip-verification", "--no-package-folder"})
	if code != 0 {
		t.Fatalf("run(build) = %d, want 0", code)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("output not created: %v", err)
	}
}

func TestBuildMissingArgs(t *testing.T) {
	if code := run([]string{"build"}); code != 1 {
		t.Errorf("run(build) with no args = %d, want 1", code)
	}
}

func TestNupUnknownOperation(t *testing.T) {
	if code := run([]string{"nup", "--nup-operation", "bogus", "--input", "x"}); code != 1 {
		t.Errorf("run(nup bogus) = %d, want 1", code)
	}
}

func TestNupInspectMissingOutput(t *testing.T) {
	// inspect does not require --output; a missing input should error.
	if code := run([]string{"nup", "--nup-operation", "inspect", "--input", "/nonexistent.nup"}); code != 1 {
		t.Errorf("run(nup inspect missing) = %d, want 1", code)
	}
}

func TestPatchUnknownAction(t *testing.T) {
	if code := run([]string{"patch", "bogus", "x"}); code != 1 {
		t.Errorf("run(patch bogus) = %d, want 1", code)
	}
}

func TestKeysUnknownAction(t *testing.T) {
	if code := run([]string{"keys", "bogus", "x"}); code != 1 {
		t.Errorf("run(keys bogus) = %d, want 1", code)
	}
}
