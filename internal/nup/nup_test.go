package nup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const testTID = 0x0100000000010000

// buildNPDM builds a minimal NPDM header carrying tid (mirrors the Python
// test fixture).
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

// writeFile writes content to dir/path (slash-separated), creating parents.
func writeFile(t *testing.T, dir, path string, content []byte) string {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

// mustSourcePath builds a path-backed Source, failing the test on error.
func mustSourcePath(t *testing.T, path string) *Source {
	t.Helper()
	s, err := SourcePath(path)
	if err != nil {
		t.Fatalf("SourcePath(%q): %v", path, err)
	}
	return s
}

// readSource reads a Source's full contents.
func readSource(t *testing.T, s *Source) []byte {
	t.Helper()
	r, err := s.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildSynthetic writes a base+update+dlc package set to dir (a mix of
// path-backed and data-backed sources) and returns the packages plus the
// expected extraction layout (relative path -> content).
func buildSynthetic(t *testing.T, dir string) ([]Package, map[string][]byte) {
	t.Helper()
	const tid = testTID
	zero := bytes.Repeat([]byte{0}, 200000)
	random := make([]byte, 100000)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	baseMain := append([]byte("NSO0"), bytes.Repeat([]byte{1}, 256)...)
	updateMain := append([]byte("NSO0"), bytes.Repeat([]byte{2}, 256)...)

	writeFile(t, dir, "base/exefs/main.npdm", buildNPDM(tid))
	writeFile(t, dir, "base/exefs/main", baseMain)
	writeFile(t, dir, "base/romfs/zero.bin", zero)
	writeFile(t, dir, "base/romfs/random.bin", random)
	writeFile(t, dir, "base/romfs/empty", nil)
	writeFile(t, dir, "base/romfs/日本語/あ.txt", []byte("Hello 日本語"))
	writeFile(t, dir, "base/romfs/copy.bin", zero) // duplicate of zero.bin
	writeFile(t, dir, "update/exefs/main", updateMain)
	writeFile(t, dir, "update/romfs/shared.bin", []byte("shared update"))
	writeFile(t, dir, "dlc/romfs/addon.bin", []byte("addon data"))

	base := Package{
		TitleID: tid, Version: 0, Kind: 0,
		Files: map[string]*Source{
			"exefs/main.npdm":  mustSourcePath(t, filepath.Join(dir, "base/exefs/main.npdm")),
			"exefs/main":       mustSourcePath(t, filepath.Join(dir, "base/exefs/main")),
			"romfs/zero.bin":   mustSourcePath(t, filepath.Join(dir, "base/romfs/zero.bin")),
			"romfs/random.bin": mustSourcePath(t, filepath.Join(dir, "base/romfs/random.bin")),
			"romfs/empty":      mustSourcePath(t, filepath.Join(dir, "base/romfs/empty")),
			"romfs/日本語/あ.txt":  mustSourcePath(t, filepath.Join(dir, "base/romfs/日本語/あ.txt")),
			"romfs/copy.bin":   mustSourcePath(t, filepath.Join(dir, "base/romfs/copy.bin")),
			"romfs/meta.txt":   SourceData([]byte("in-memory metadata")),
		},
		Directories: []string{"exefs", "romfs", "romfs/日本語"},
		Metadata:    []TLV{{Kind: 5, Data: []byte("Test Game")}},
	}
	update := Package{
		TitleID: tid, Version: 1, Kind: 1,
		Files: map[string]*Source{
			"exefs/main":       mustSourcePath(t, filepath.Join(dir, "update/exefs/main")),
			"romfs/shared.bin": mustSourcePath(t, filepath.Join(dir, "update/romfs/shared.bin")),
		},
		Directories: []string{"exefs", "romfs"},
	}
	dlc := Package{
		TitleID: tid | 0x1000, Version: 0, Kind: 2,
		Files:       map[string]*Source{"romfs/addon.bin": mustSourcePath(t, filepath.Join(dir, "dlc/romfs/addon.bin"))},
		Directories: []string{"romfs"},
	}

	expected := map[string][]byte{
		"base/exefs/main.npdm":                   buildNPDM(tid),
		"base/exefs/main":                        baseMain,
		"base/romfs/zero.bin":                    zero,
		"base/romfs/random.bin":                  random,
		"base/romfs/empty":                       nil,
		"base/romfs/日本語/あ.txt":                   []byte("Hello 日本語"),
		"base/romfs/copy.bin":                    zero,
		"base/romfs/meta.txt":                    []byte("in-memory metadata"),
		"updates/1/exefs/main":                   updateMain,
		"updates/1/romfs/shared.bin":             []byte("shared update"),
		"dlc/0100000000011000/0/romfs/addon.bin": []byte("addon data"),
	}
	return []Package{base, update, dlc}, expected
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packages, expected := buildSynthetic(t, dir)
	out := filepath.Join(dir, "game.nup")

	res, err := Pack(ctx, packages, out, PackOptions{Level: 3, LargeRaw: true, Verify: true})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if !res.Verified {
		t.Errorf("Verified = false, want true")
	}
	if res.Packages != 3 {
		t.Errorf("Packages = %d, want 3", res.Packages)
	}
	if res.DuplicateFilesSkipped != 1 {
		t.Errorf("DuplicateFilesSkipped = %d, want 1", res.DuplicateFilesSkipped)
	}

	a, err := Open(ctx, out)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()

	stats, err := a.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if stats["verified"] != true {
		t.Errorf("verified = %v, want true", stats["verified"])
	}
	if stats["packages"] != 3 {
		t.Errorf("packages = %v, want 3", stats["packages"])
	}
	if stats["files"] != 11 {
		t.Errorf("files = %v, want 11", stats["files"])
	}
	if stats["unique_extents"] != 9 {
		t.Errorf("unique_extents = %v, want 9", stats["unique_extents"])
	}
	if stats["uncompressed"].(int64) != res.SourceSize {
		t.Errorf("uncompressed = %v, want %d", stats["uncompressed"], res.SourceSize)
	}
	if stats["raw_files"].(int) < 1 {
		t.Errorf("raw_files = %v, want >= 1 (random.bin is incompressible)", stats["raw_files"])
	}
	if stats["compressed_files"].(int) < 1 {
		t.Errorf("compressed_files = %v, want >= 1", stats["compressed_files"])
	}

	istats, infos, err := a.Inspect(ctx)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if istats["packages"] != 3 {
		t.Errorf("inspect packages = %v, want 3", istats["packages"])
	}
	if len(infos) != 3 {
		t.Fatalf("infos = %d, want 3", len(infos))
	}
	if infos[0].Kind != 0 || infos[0].TitleID != testTID || infos[0].Version != 0 {
		t.Errorf("base info = %+v", infos[0])
	}
	if infos[0].Title != "Test Game" {
		t.Errorf("base Title = %q, want %q", infos[0].Title, "Test Game")
	}
	if infos[0].Files != 8 {
		t.Errorf("base Files = %d, want 8", infos[0].Files)
	}
	if infos[1].Kind != 1 || infos[2].Kind != 2 {
		t.Errorf("kinds = %d,%d, want 1,2", infos[1].Kind, infos[2].Kind)
	}

	extractDir := filepath.Join(dir, "extracted")
	if _, err := Extract(ctx, out, extractDir, nil, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, "nup-manifest.json")); err != nil {
		t.Errorf("nup-manifest.json missing: %v", err)
	}
	for rel, want := range expected {
		got, err := os.ReadFile(filepath.Join(extractDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content mismatch", rel)
		}
	}
}

func TestSafeName(t *testing.T) {
	valid := []string{"a", "file.txt", "日本語", "CON.txt", "con", "COM10", "LPT0", "file..txt"}
	for _, name := range valid {
		if err := SafeName(name, false); err != nil {
			t.Errorf("SafeName(%q, false) = %v, want nil", name, err)
		}
	}
	invalid := []string{"", ".", "..", "a/b", "a\\b", "a\x00b"}
	for _, name := range invalid {
		if err := SafeName(name, false); err == nil {
			t.Errorf("SafeName(%q, false) = nil, want error", name)
		}
	}
	reserved := []string{"CON", "NUL", "COM1", "LPT9", "con", "nul", "com1", "aux", "prn", "COM9", "LPT1"}
	for _, name := range reserved {
		if err := SafeName(name, true); err == nil {
			t.Errorf("SafeName(%q, true) = nil, want reserved-name error", name)
		}
	}
	trailing := []string{"file.", "file ", "file..", "file. "}
	for _, name := range trailing {
		if err := SafeName(name, true); err == nil {
			t.Errorf("SafeName(%q, true) = nil, want trailing-dot/space error", name)
		}
	}
	control := []string{"a\x01b", "a\x1fb"}
	for _, name := range control {
		if err := SafeName(name, true); err == nil {
			t.Errorf("SafeName(%q, true) = nil, want control-char error", name)
		}
	}
	badChars := []string{"a<b", "a>b", "a:b", "a\"b", "a|b", "a?b", "a*b"}
	for _, name := range badChars {
		if err := SafeName(name, true); err == nil {
			t.Errorf("SafeName(%q, true) = nil, want invalid-char error", name)
		}
	}
	winValid := []string{"file.txt", "COM10", "LPT0", "console.txt"}
	for _, name := range winValid {
		if err := SafeName(name, true); err != nil {
			t.Errorf("SafeName(%q, true) = %v, want nil", name, err)
		}
	}
}

func TestPathParts(t *testing.T) {
	parts, err := PathParts("a/b/c")
	if err != nil {
		t.Fatalf("PathParts: %v", err)
	}
	if len(parts) != 3 || parts[0] != "a" || parts[1] != "b" || parts[2] != "c" {
		t.Errorf("parts = %v, want [a b c]", parts)
	}
	if _, err := PathParts("/abs"); err == nil {
		t.Errorf("PathParts(/abs) = nil, want error")
	}
	if _, err := PathParts("a/../b"); err == nil {
		t.Errorf("PathParts(a/../b) = nil, want error")
	}
}

func TestValidatePackages(t *testing.T) {
	base := Package{TitleID: testTID, Version: 0, Kind: 0, Files: map[string]*Source{}}
	update := Package{TitleID: testTID, Version: 1, Kind: 1, Files: map[string]*Source{}}
	standalone := Package{TitleID: testTID, Version: 1, Kind: 1, Flags: 1, Files: map[string]*Source{}}
	dlc := Package{TitleID: testTID | 0x1000, Version: 0, Kind: 2, Files: map[string]*Source{}}

	if err := ValidatePackages(nil); err == nil {
		t.Errorf("nil packages: want error")
	}
	if err := ValidatePackages([]Package{base, base}); err == nil {
		t.Errorf("two bases: want error")
	}
	if err := ValidatePackages([]Package{base, update, update}); err == nil {
		t.Errorf("duplicate identity: want error")
	}
	// update with base present must have flags 0
	badUpdate := update
	badUpdate.Flags = 1
	if err := ValidatePackages([]Package{base, badUpdate}); err == nil {
		t.Errorf("update flags=1 with base: want error")
	}
	// standalone update must have flags 1
	if err := ValidatePackages([]Package{update}); err == nil {
		t.Errorf("standalone update flags=0: want error")
	}
	if err := ValidatePackages([]Package{standalone}); err != nil {
		t.Errorf("standalone update flags=1: %v", err)
	}
	// dlc from a different game
	badDLC := Package{TitleID: testTID + 0x2000, Version: 0, Kind: 2, Files: map[string]*Source{}}
	if err := ValidatePackages([]Package{base, badDLC}); err == nil {
		t.Errorf("dlc different game: want error")
	}
	if err := ValidatePackages([]Package{base, update, dlc}); err != nil {
		t.Errorf("valid set: %v", err)
	}
}

func TestKindNameAndPackagePath(t *testing.T) {
	if KindName(0) != "base" || KindName(1) != "update" || KindName(2) != "addon" ||
		KindName(3) != "addon_update" || KindName(4) != "system" {
		t.Errorf("KindName mismatch")
	}
	if KindName(5) != "" || KindName(-1) != "" {
		t.Errorf("KindName out-of-range should be empty")
	}
	if got := PackagePath(Package{Kind: 0}); got != "base" {
		t.Errorf("PackagePath(base) = %q", got)
	}
	if got := PackagePath(Package{Kind: 1, Version: 7}); got != "updates/7" {
		t.Errorf("PackagePath(update) = %q", got)
	}
	if got := PackagePath(Package{Kind: 2, TitleID: 0xABCD, Version: 3}); got != "dlc/000000000000ABCD/3" {
		t.Errorf("PackagePath(dlc) = %q", got)
	}
}
