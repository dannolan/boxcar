package dlc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/metadata"
	"github.com/dannolan/boxcar/internal/nca"
	"github.com/dannolan/boxcar/internal/pfs0"
	"github.com/dannolan/boxcar/internal/region"
	"github.com/dannolan/boxcar/internal/romfs"
	"github.com/dannolan/boxcar/internal/stream"
)

const (
	testTID     = uint64(0x0100000000001001)
	testTIDHex  = "0100000000001001"
	testBaseHex = "0100000000000000"
)

// makeFixture builds a synthetic DLC dump: a 16-hex title-ID folder containing
// a romfs/ tree. The bytes are fixed so the cross-check is deterministic.
func makeFixture(t *testing.T) string {
	t.Helper()
	tidDir := filepath.Join(t.TempDir(), testTIDHex)
	if err := os.MkdirAll(filepath.Join(tidDir, "romfs", "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(tidDir, "romfs", rel), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("data1.bin", "hello")
	write("subdir/data2.bin", "world")
	return tidDir
}

func TestParseID(t *testing.T) {
	if v, err := ParseID(" 0100000000001001 "); err != nil || v != testTID {
		t.Errorf("ParseID(%q) = %#x, %v; want %#x", "0100000000001001", v, err, testTID)
	}
	if v, err := ParseID("ABCDEFabcdef1234"); err != nil || v != 0xabcdefabcdef1234 {
		t.Errorf("mixed-case ParseID = %#x, %v", v, err)
	}
	for _, bad := range []string{"", "zzzz", "010000000000100", "01000000000010011", "010000000000100g"} {
		if _, err := ParseID(bad); err == nil || !strings.Contains(err.Error(), "exactly 16 hexadecimal") {
			t.Errorf("ParseID(%q) err = %v, want hex-length error", bad, err)
		}
	}
	// Out of the add-on-content range on the low 13 bits.
	for _, bad := range []string{"0000000000000000", "0100000000001800", "0100000000000fff", "0100000000000001"} {
		if _, err := ParseID(bad); err == nil || !strings.Contains(err.Error(), "add-on-content range") {
			t.Errorf("ParseID(%q) err = %v, want range error", bad, err)
		}
	}
}

func TestParseVersion(t *testing.T) {
	cases := map[string]uint32{
		"0":          0,
		"123":        123,
		"4294967295": 0xffffffff,
	}
	for in, want := range cases {
		v, err := ParseVersion(in)
		if err != nil || v != want {
			t.Errorf("ParseVersion(%q) = %d, %v; want %d", in, v, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "1.5", "4294967296", " 12a "} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) expected error", bad)
		}
	}
}

func TestFindDiscovery(t *testing.T) {
	ctx := context.Background()
	// A version folder nested under the title-ID folder.
	base := t.TempDir()
	tidDir := filepath.Join(base, testTIDHex)
	os.MkdirAll(filepath.Join(tidDir, "1", "romfs"), 0o755)
	os.WriteFile(filepath.Join(tidDir, "1", "romfs", "a.bin"), []byte("a"), 0o644)

	found, err := Find(ctx, tidDir, "")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("len(found) = %d, want 1", len(found))
	}
	s := found[0]
	if s.TitleID != testTID {
		t.Errorf("TitleID = %#x, want %#x", s.TitleID, testTID)
	}
	if s.Version != 1 {
		t.Errorf("Version = %d, want 1", s.Version)
	}
	if s.Label != "1" {
		t.Errorf("Label = %q, want %q", s.Label, "1")
	}
}

func TestFindRomfsParentClimb(t *testing.T) {
	ctx := context.Background()
	// Selecting the romfs folder directly climbs to its parent.
	tidDir := makeFixture(t)
	found, err := Find(ctx, filepath.Join(tidDir, "romfs"), "")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(found) != 1 || found[0].TitleID != testTID {
		t.Fatalf("found = %+v, want 1 snapshot with tid %#x", found, testTID)
	}
	if found[0].Label != "Selected folder" {
		t.Errorf("Label = %q, want Selected folder", found[0].Label)
	}
}

func TestFindOverrideConflict(t *testing.T) {
	ctx := context.Background()
	tidDir := makeFixture(t)
	_, err := Find(ctx, tidDir, "0100000000001002")
	if err == nil || !strings.Contains(err.Error(), "differs from the dump folder title ID") {
		t.Fatalf("Find override conflict err = %v, want conflict error", err)
	}
}

func TestFindMissingTID(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mydlc")
	os.MkdirAll(filepath.Join(dir, "romfs"), 0o755)
	os.WriteFile(filepath.Join(dir, "romfs", "a.bin"), []byte("a"), 0o644)
	_, err := Find(ctx, dir, "")
	if err == nil || !strings.Contains(err.Error(), "No DLC title ID in the folder names") {
		t.Fatalf("Find missing tid err = %v, want missing-tid error", err)
	}
}

func TestBuildAndVerify(t *testing.T) {
	ctx := context.Background()
	root := makeFixture(t)
	output := filepath.Join(t.TempDir(), "out.nsp")
	snap := Snapshot{Root: root, TitleID: testTID, Version: 0, Label: "Selected folder"}

	rep, err := Build(ctx, snap, output, Options{VerifyOutput: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if rep.OutputFormat != "NSP" {
		t.Errorf("OutputFormat = %q", rep.OutputFormat)
	}
	if rep.TitleID != "0100000000001001" {
		t.Errorf("TitleID = %q", rep.TitleID)
	}
	if rep.BaseTitleID != testBaseHex {
		t.Errorf("BaseTitleID = %q, want %q", rep.BaseTitleID, testBaseHex)
	}
	if rep.DLCIndex != 1 {
		t.Errorf("DLCIndex = %d, want 1", rep.DLCIndex)
	}
	if rep.DLCVersion != 0 {
		t.Errorf("DLCVersion = %d", rep.DLCVersion)
	}
	if !rep.Verified {
		t.Errorf("Verified = false, want true")
	}
	if rep.BaseValidated {
		t.Errorf("BaseValidated = true, want false")
	}
	if rep.SourceFileCount != 2 {
		t.Errorf("SourceFileCount = %d, want 2", rep.SourceFileCount)
	}

	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if rep.Size != int64(len(raw)) {
		t.Errorf("Size = %d, want %d", rep.Size, len(raw))
	}
	wantSHA := sha256.Sum256(raw)
	if rep.SHA256 != hex.EncodeToString(wantSHA[:]) {
		t.Errorf("SHA256 mismatch")
	}

	// Parse the output: exactly two NCAs (Data=4, Meta=1).
	f, err := os.Open(output)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	r, err := region.New(f)
	if err != nil {
		t.Fatalf("region: %v", err)
	}
	files, err := pfs0.Partition(ctx, r)
	if err != nil {
		t.Fatalf("partition: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	kinds := map[byte]bool{}
	for name, data := range files {
		if !strings.HasSuffix(name, ".nca") {
			t.Errorf("entry %q lacks .nca suffix", name)
		}
		_, kind, _, err := nca.Sections(ctx, data, true)
		if err != nil {
			t.Fatalf("Sections(%s): %v", name, err)
		}
		kinds[kind] = true
	}
	if !kinds[1] || !kinds[4] {
		t.Errorf("content types = %v, want {1,4}", kinds)
	}

	// Verify the output independently (recomputes source digests).
	sources := inventory(ctx, filepath.Join(root, "romfs"), nil)
	n, err := Verify(ctx, output, snap, sources, nil, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if n != 2 {
		t.Errorf("Verify returned %d, want 2", n)
	}
}

func TestBuildEmptyRomfs(t *testing.T) {
	ctx := context.Background()
	tidDir := filepath.Join(t.TempDir(), testTIDHex)
	if err := os.MkdirAll(filepath.Join(tidDir, "romfs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	snap := Snapshot{Root: tidDir, TitleID: testTID, Version: 0, Label: "Selected folder"}
	_, err := Build(ctx, snap, filepath.Join(t.TempDir(), "out.nsp"), Options{})
	if err == nil || !strings.Contains(err.Error(), "RomFS folder is empty") {
		t.Fatalf("Build empty romfs err = %v, want empty error", err)
	}
}

func TestBuildOutputExists(t *testing.T) {
	ctx := context.Background()
	root := makeFixture(t)
	output := filepath.Join(t.TempDir(), "out.nsp")
	if err := os.WriteFile(output, []byte("x"), 0o644); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	snap := Snapshot{Root: root, TitleID: testTID, Version: 0, Label: "Selected folder"}
	_, err := Build(ctx, snap, output, Options{})
	if err == nil || !strings.Contains(err.Error(), "Output already exists") {
		t.Fatalf("Build output-exists err = %v, want exists error", err)
	}
}

func TestBuildTitleIDRange(t *testing.T) {
	ctx := context.Background()
	root := makeFixture(t)
	snap := Snapshot{Root: root, TitleID: 0x0100000000001800, Version: 0, Label: "x"}
	_, err := Build(ctx, snap, filepath.Join(t.TempDir(), "out.nsp"), Options{})
	if err == nil || !strings.Contains(err.Error(), "add-on-content range") {
		t.Fatalf("Build range err = %v, want range error", err)
	}
}

func TestBaseID(t *testing.T) {
	if got := BaseID(testTID); got != 0x0100000000000000 {
		t.Errorf("BaseID(%#x) = %#x, want %#x", testTID, got, 0x0100000000000000)
	}
}

func TestBaseGameInfo(t *testing.T) {
	ctx := context.Background()
	const baseTid = uint64(0x0100000000000000)
	base := buildBaseGameNSP(t, baseTid)

	info, err := BaseGameInfo(ctx, base)
	if err != nil {
		t.Fatalf("BaseGameInfo: %v", err)
	}
	if info.TitleID != baseTid {
		t.Errorf("TitleID = %#x, want %#x", info.TitleID, baseTid)
	}
	if info.DLCBaseID != baseTid+0x1000 {
		t.Errorf("DLCBaseID = %#x, want %#x", info.DLCBaseID, baseTid+0x1000)
	}
	if info.Title != "My Base Game" {
		t.Errorf("Title = %q, want %q", info.Title, "My Base Game")
	}

	// The matching DLC snapshot validates against this base game.
	validateBase(Snapshot{Root: "/unused", TitleID: testTID, Version: 0, Label: ""}, info)
}

func buildBaseGameNSP(t *testing.T, baseTid uint64) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	buildNCA := func(subdir string, files map[string][]byte, contentType byte) *stream.Stream {
		t.Helper()
		root := filepath.Join(dir, subdir)
		for rel, data := range files {
			p := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		var mf []romfs.ManifestEntry
		rs, err := romfs.Build(ctx, root, &mf, nil, nil)
		if err != nil {
			t.Fatalf("romfs.Build: %v", err)
		}
		fsh, body, err := nca.RomfsSection(ctx, rs, nil)
		if err != nil {
			t.Fatalf("RomfsSection: %v", err)
		}
		s, err := nca.NCA([]nca.Section{{FSH: fsh, Body: body}}, baseTid, contentType)
		if err != nil {
			t.Fatalf("NCA: %v", err)
		}
		return s
	}

	entries, _, err := metadata.ControlData(baseTid, "1.0", "My Base Game", nil, nil)
	if err != nil {
		t.Fatalf("ControlData: %v", err)
	}
	program := buildNCA("program", map[string][]byte{"main": []byte("dummy")}, 0)
	control := buildNCA("control", map[string][]byte{"control.nacp": entries[0].Data}, 2)

	mat := func(s *stream.Stream) []byte {
		b, err := s.ReadHead(s.Size(), &stream.WriteOpts{Ctx: ctx})
		if err != nil {
			t.Fatalf("ReadHead: %v", err)
		}
		return b
	}
	image, err := pfs0.Package([]pfs0.Entry{
		{Name: "program.nca", Part: stream.Bytes(mat(program))},
		{Name: "control.nca", Part: stream.Bytes(mat(control))},
	})
	if err != nil {
		t.Fatalf("pfs0.Package: %v", err)
	}
	out := filepath.Join(dir, "base.nsp")
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := image.WriteTo(f, &stream.WriteOpts{Ctx: ctx}); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return out
}

// --- gold cross-check against the Python reference ---

const crosscheckPy = `
import os, sys
sys.path.insert(0, os.environ.get('NSPBOX_PYREF', '/Users/dan/Downloads/nsp-box-src/src'))
import dlc
from pathlib import Path

mode = sys.argv[1]
root = sys.argv[2]
tid = int(sys.argv[3], 16)
version = int(sys.argv[4])
snap = dlc.DlcSnapshot(Path(root), tid, version, 'Selected folder')
if mode == 'build':
    output = sys.argv[5]
    dlc.build(snap, output, verify_output=False)
    print('BUILT', Path(output).resolve())
elif mode == 'verify':
    path = sys.argv[5]
    sources = dlc.inventory(snap)
    n = dlc.verify(path, snap, sources)
    print('VERIFIED', n)
`

func findPython(t *testing.T) string {
	t.Helper()
	venv := envOr("NSPBOX_PYTHON", "/Users/dan/Downloads/nsp-box-go/.pyref-venv/bin/python")
	if fi, err := os.Stat(venv); err == nil && !fi.IsDir() {
		return venv
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	t.Skip("python3 unavailable; skipping gold cross-check")
	return ""
}

func runPython(t *testing.T, python, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command(python, append([]string{script}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestCrossCheckPython(t *testing.T) {
	ctx := context.Background()
	python := findPython(t)
	if _, err := os.Stat(filepath.Join(envOr("NSPBOX_PYREF", "/Users/dan/Downloads/nsp-box-src/src"), "dlc.py")); err != nil {
		t.Skip("python reference source not found; skipping gold cross-check")
	}

	root := makeFixture(t)
	work := t.TempDir()
	goOut := filepath.Join(work, "go.nsp")
	pyOut := filepath.Join(work, "py.nsp")

	snap := Snapshot{Root: root, TitleID: testTID, Version: 0, Label: "Selected folder"}
	if _, err := Build(ctx, snap, goOut, Options{VerifyOutput: true}); err != nil {
		t.Fatalf("Go Build: %v", err)
	}

	script := filepath.Join(work, "crosscheck.py")
	if err := os.WriteFile(script, []byte(crosscheckPy), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	runPython(t, python, script, "build", root, testTIDHex, "0", pyOut)

	goBytes, err := os.ReadFile(goOut)
	if err != nil {
		t.Fatalf("read go output: %v", err)
	}
	pyBytes, err := os.ReadFile(pyOut)
	if err != nil {
		t.Fatalf("read py output: %v", err)
	}
	if !bytes.Equal(goBytes, pyBytes) {
		t.Errorf("NSP mismatch: go %d bytes, python %d bytes", len(goBytes), len(pyBytes))
		if len(goBytes) == len(pyBytes) {
			for i := range goBytes {
				if goBytes[i] != pyBytes[i] {
					t.Errorf("first differing byte at %#x: go %#x, py %#x", i, goBytes[i], pyBytes[i])
					break
				}
			}
		}
	}

	// Python dlc.verify must accept the Go-produced NSP.
	runPython(t, python, script, "verify", root, testTIDHex, "0", goOut)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
