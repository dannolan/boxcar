package nsp

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/pfs0"
	"github.com/dannolan/boxcar/internal/region"
	"github.com/dannolan/boxcar/internal/snapshot"
	"github.com/dannolan/boxcar/internal/testutil"
)

// buildNPDM builds a minimal META/ACI0 main.npdm carrying the given title ID.
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

// writeSnapshot writes a complete exefs+romfs snapshot at dir.
func writeSnapshot(t *testing.T, dir string, tid uint64, romfsFiles map[string][]byte) {
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
	for name, data := range romfsFiles {
		p := filepath.Join(romfs, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSnapshot(t, root, 0x0100000000010000, map[string][]byte{
		"file.bin":       []byte("hello romfs"),
		"sub/nested.bin": []byte("nested payload"),
	})

	snap := snapshot.Snapshot{Root: root, TitleID: 0x0100000000010000, Version: 123, Label: "Selected folder", Title: "Test Game"}

	outDir := t.TempDir()
	output := filepath.Join(outDir, "game.nsp")
	report, err := Build(ctx, snap, Options{
		Output:         output,
		Title:          "Test Game",
		DisplayVersion: "1.0.0",
		Verify:         true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := os.Stat(output); err != nil {
		t.Fatalf("output not created: %v", err)
	}
	if report.Size <= 0 {
		t.Errorf("report.Size = %d, want > 0", report.Size)
	}
	if !report.Verified {
		t.Error("report.Verified = false, want true")
	}
	if report.SourceFileCount != 4 {
		t.Errorf("report.SourceFileCount = %d, want 4", report.SourceFileCount)
	}
	if len(report.SHA256) != 64 {
		t.Errorf("report.SHA256 = %q, want 64 hex chars", report.SHA256)
	}
	if report.TitleID != "0100000000010000" {
		t.Errorf("report.TitleID = %q, want 0100000000010000", report.TitleID)
	}
	if len(report.Files) != 4 {
		t.Errorf("report.Files has %d entries, want 4", len(report.Files))
	}
	for _, f := range report.Files {
		if f["sha256"] == "" || f["size"].(int64) <= 0 || f["path"] == "" {
			t.Errorf("bad checked file entry: %v", f)
		}
	}

	// Re-open and confirm the PFS0 parses into three NCAs.
	f, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := region.New(f)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := pfs0.Partition(ctx, r)
	if err != nil {
		t.Fatalf("pfs0.Partition: %v", err)
	}
	if len(parts) != 3 {
		t.Errorf("PFS0 has %d entries, want 3", len(parts))
	}

	// Independent readback verification.
	inv, err := snapshot.Inventory(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := VerifyOutput(ctx, output, snap, inv, nil, nil)
	if err != nil {
		t.Fatalf("VerifyOutput: %v", err)
	}
	if len(checked) != 4 {
		t.Errorf("VerifyOutput returned %d files, want 4", len(checked))
	}
}

func TestBuildRefusesExistingOutput(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, 0x0100000000010000, map[string][]byte{"file.bin": []byte("x")})
	snap := snapshot.Snapshot{Root: root, TitleID: 0x0100000000010000, Version: 1}

	outDir := t.TempDir()
	output := filepath.Join(outDir, "game.nsp")
	if _, err := Build(context.Background(), snap, Options{Output: output, Title: "T", DisplayVersion: "1", Verify: false}); err != nil {
		t.Fatal(err)
	}
	_, err := Build(context.Background(), snap, Options{Output: output, Title: "T", DisplayVersion: "1", Verify: false})
	if err == nil {
		t.Fatal("second Build must refuse existing output")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want 'already exists'", err.Error())
	}
}

func TestBuildValidations(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, 0x0100000000010000, map[string][]byte{"file.bin": []byte("x")})
	snap := snapshot.Snapshot{Root: root, TitleID: 0x0100000000010000, Version: 1}
	outDir := t.TempDir()

	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"empty title", Options{Output: filepath.Join(outDir, "a.nsp"), Title: "  ", DisplayVersion: "1"}, "Enter a game title."},
		{"display version too long", Options{Output: filepath.Join(outDir, "a.nsp"), Title: "T", DisplayVersion: "this is way too long!"}, "Display version must fit within 15 UTF-8 bytes."},
		{"wrong suffix", Options{Output: filepath.Join(outDir, "a.xci"), Title: "T", DisplayVersion: "1"}, "NSP Box supports NSP output only."},
		{"output inside exefs", Options{Output: filepath.Join(root, "exefs", "out.nsp"), Title: "T", DisplayVersion: "1"}, "Save the NSP outside the source exefs and romfs folders."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Build(context.Background(), snap, c.opts)
			if err == nil {
				t.Fatalf("expected error %q, got nil", c.want)
			}
			if err.Error() != c.want {
				t.Errorf("error = %q, want %q", err.Error(), c.want)
			}
		})
	}
}

func TestSourceMutationDetected(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, 0x0100000000010000, map[string][]byte{"file.bin": []byte("x")})
	inv, err := snapshot.Inventory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "romfs", "file.bin"), []byte("CHANGED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.ValidateSources(inv); err == nil {
		t.Fatal("ValidateSources must detect the mutation")
	}
}

const pythonScript = `
import os, sys
sys.path.insert(0, os.environ["NSPBOX_PYREF"])
from pathlib import Path
import engine

mode = sys.argv[1]
root = sys.argv[2]
output = sys.argv[3]
title = sys.argv[4]
display_version = sys.argv[5]
snap_version = int(sys.argv[6])

tid = engine.read_title_id(Path(root) / 'exefs' / 'main.npdm')
snap = engine.Snapshot(root=Path(root), title_id=tid, version=snap_version, label='Selected folder', title=title, nacp=None)

if mode == 'build':
    engine.build(snap, output, title, display_version, verify=False)
    print('OK')
elif mode == 'verify':
    inventory = engine.source_inventory(Path(root))
    checked = engine.verify_image(Path(output), snap, inventory, lambda *a: None, None, None)
    print('OK', len(checked))
`

func runPython(t *testing.T, args ...string) string {
	t.Helper()
	python, pyref := testutil.SkipUnlessPyRef(t)
	script := filepath.Join(t.TempDir(), "script.py")
	if err := os.WriteFile(script, []byte(pythonScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "NSPBOX_PYREF="+pyref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGoldenCrossCheck builds the same fixture with the Python reference
// engine and compares the NSP byte-for-byte, then runs the Python verifier on
// the Go-produced NSP. It skips gracefully when the venv Python is absent.
func TestGoldenCrossCheck(t *testing.T) {
	python, _ := testutil.SkipUnlessPyRef(t)

	_ = python // runPython resolves the same environment

	root := t.TempDir()
	writeSnapshot(t, root, 0x0100000000010000, map[string][]byte{
		"file.bin":       []byte("hello romfs"),
		"sub/nested.bin": []byte("nested payload"),
		"big.bin":        bytes.Repeat([]byte("0123456789abcdef"), 300000), // ~4.8 MiB
	})
	snap := snapshot.Snapshot{Root: root, TitleID: 0x0100000000010000, Version: 123, Label: "Selected folder", Title: "Golden Game"}

	outDir := t.TempDir()
	goNSP := filepath.Join(outDir, "go.nsp")
	if _, err := Build(context.Background(), snap, Options{Output: goNSP, Title: "Golden Game", DisplayVersion: "1.2.3", Verify: false}); err != nil {
		t.Fatalf("Go Build: %v", err)
	}

	pyNSP := filepath.Join(outDir, "py.nsp")
	runPython(t, "build", root, pyNSP, "Golden Game", "1.2.3", "123")

	goBytes, err := os.ReadFile(goNSP)
	if err != nil {
		t.Fatal(err)
	}
	pyBytes, err := os.ReadFile(pyNSP)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(goBytes, pyBytes) {
		t.Errorf("Go NSP differs from Python NSP (byte-identical check failed)")
		for i := 0; i < len(goBytes) && i < len(pyBytes); i++ {
			if goBytes[i] != pyBytes[i] {
				t.Errorf("first differing byte at %#x: go=%#x py=%#x", i, goBytes[i], pyBytes[i])
				break
			}
		}
		if len(goBytes) != len(pyBytes) {
			t.Errorf("sizes differ: go=%d py=%d", len(goBytes), len(pyBytes))
		}
		return
	}

	out := runPython(t, "verify", root, goNSP, "Golden Game", "1.2.3", "123")
	if !strings.HasPrefix(out, "OK") {
		t.Errorf("Python verify_image rejected the Go NSP: %s", out)
	}
}
