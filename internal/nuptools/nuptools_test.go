package nuptools

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/dannolan/boxcar/internal/nsp"
	"github.com/dannolan/boxcar/internal/nup"
	"github.com/dannolan/boxcar/internal/snapshot"
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

// writeContent writes a complete exefs+romfs content tree at dir.
func writeContent(t *testing.T, dir string, tid uint64, romfs map[string][]byte) {
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
	romfsDir := filepath.Join(dir, "romfs")
	if err := os.MkdirAll(romfsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range romfs {
		p := filepath.Join(romfsDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverBundle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const baseTID = 0x0100000000010000
	writeContent(t, filepath.Join(root, "base"), baseTID, map[string][]byte{"file.bin": []byte("base")})
	writeContent(t, filepath.Join(root, "updates", "1"), baseTID, map[string][]byte{"file.bin": []byte("upd1")})
	writeContent(t, filepath.Join(root, "updates", "2"), baseTID, map[string][]byte{"file.bin": []byte("upd2")})
	// DLC: data-only (romfs, no exefs).
	dlcDir := filepath.Join(root, "dlc", "0100000000011000")
	if err := os.MkdirAll(filepath.Join(dlcDir, "romfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dlcDir, "romfs", "dlc.bin"), []byte("dlc"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgs, err := Discover(ctx, root, nil, nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(pkgs) != 4 {
		t.Fatalf("Discover returned %d packages, want 4", len(pkgs))
	}
	byKind := map[int][]nup.Package{}
	for _, p := range pkgs {
		byKind[p.Kind] = append(byKind[p.Kind], p)
	}
	if len(byKind[0]) != 1 || byKind[0][0].TitleID != baseTID {
		t.Errorf("base = %+v, want tid %016X", byKind[0], baseTID)
	}
	if len(byKind[1]) != 2 {
		t.Fatalf("updates = %d, want 2", len(byKind[1]))
	}
	versions := map[uint32]bool{}
	for _, p := range byKind[1] {
		versions[p.Version] = true
		if p.Flags != 0 {
			t.Errorf("update flags = %d, want 0 (base present)", p.Flags)
		}
	}
	if !versions[1] || !versions[2] {
		t.Errorf("update versions = %v, want {1,2}", versions)
	}
	if len(byKind[2]) != 1 || byKind[2][0].TitleID != 0x0100000000011000 {
		t.Errorf("dlc = %+v, want tid 0100000000011000", byKind[2])
	}
}

func TestPackFolderLatestSnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const baseTID = 0x0100000000010000
	writeContent(t, filepath.Join(root, "base"), baseTID, map[string][]byte{"file.bin": []byte("base")})
	writeContent(t, filepath.Join(root, "updates", "1"), baseTID, map[string][]byte{"file.bin": []byte("upd1")})

	out := filepath.Join(t.TempDir(), "out.nup")
	res, err := PackFolder(ctx, root, out, true, nil, nil, "", nil, nup.PackOptions{Level: 1, Verify: false})
	if err != nil {
		t.Fatalf("PackFolder(latest): %v", err)
	}
	if res.Packages != 1 {
		t.Errorf("latest-snapshot packages = %d, want 1 (merged)", res.Packages)
	}

	out2 := filepath.Join(t.TempDir(), "out2.nup")
	res2, err := PackFolder(ctx, root, out2, false, nil, nil, "", nil, nup.PackOptions{Level: 1, Verify: false})
	if err != nil {
		t.Fatalf("PackFolder(all): %v", err)
	}
	if res2.Packages != 2 {
		t.Errorf("no-latest-snapshot packages = %d, want 2 (base+update)", res2.Packages)
	}
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const tid = 0x0100000000010000
	writeContent(t, root, tid, map[string][]byte{
		"file.bin":       []byte("hello romfs"),
		"sub/nested.bin": []byte("nested payload"),
	})

	snap := snapshot.Snapshot{Root: root, TitleID: tid, Version: 123, Label: "Selected folder", Title: "Test Game"}
	outDir := t.TempDir()
	origNSP := filepath.Join(outDir, "orig.nsp")
	if _, err := nsp.Build(ctx, snap, nsp.Options{Output: origNSP, Title: "Test Game", DisplayVersion: "1.0.0", Verify: false}); err != nil {
		t.Fatalf("nsp.Build: %v", err)
	}

	// NSP -> NUP.
	nupPath := filepath.Join(outDir, "game.nup")
	if _, err := NspToNup(ctx, origNSP, nupPath, nil, nup.PackOptions{Level: 1, Verify: false}); err != nil {
		t.Fatalf("NspToNup: %v", err)
	}

	// Extract the NUP to a folder and confirm the layout.
	extractDir := filepath.Join(outDir, "extracted")
	if _, err := nup.Extract(ctx, nupPath, extractDir, nil, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, "base", "exefs", "main.npdm")); err != nil {
		t.Errorf("extracted exefs/main.npdm missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, "base", "romfs", "file.bin")); err != nil {
		t.Errorf("extracted romfs/file.bin missing: %v", err)
	}

	// NUP -> NSP.
	backNSP := filepath.Join(outDir, "back.nsp")
	if _, err := NupToNsp(ctx, nupPath, backNSP, -1, false, nil, nil); err != nil {
		t.Fatalf("NupToNsp: %v", err)
	}

	orig, err := os.ReadFile(origNSP)
	if err != nil {
		t.Fatal(err)
	}
	back, err := os.ReadFile(backNSP)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(orig, back) {
		t.Errorf("round-trip NSP differs from original")
		for i := 0; i < len(orig) && i < len(back); i++ {
			if orig[i] != back[i] {
				t.Errorf("first differing byte at %#x: orig=%#x back=%#x", i, orig[i], back[i])
				break
			}
		}
		if len(orig) != len(back) {
			t.Errorf("sizes differ: orig=%d back=%d", len(orig), len(back))
		}
	}
}

func TestNspPackagesDeep(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const tid = 0x0100000000010000
	writeContent(t, root, tid, map[string][]byte{"file.bin": []byte("payload")})
	snap := snapshot.Snapshot{Root: root, TitleID: tid, Version: 5, Label: "Selected folder", Title: "T"}
	out := filepath.Join(t.TempDir(), "g.nsp")
	if _, err := nsp.Build(ctx, snap, nsp.Options{Output: out, Title: "T", DisplayVersion: "1", Verify: false}); err != nil {
		t.Fatalf("nsp.Build: %v", err)
	}
	pkgs, validate, err := NspPackages(ctx, out, true, nil)
	if err != nil {
		t.Fatalf("NspPackages: %v", err)
	}
	defer validate()
	if len(pkgs) != 1 {
		t.Fatalf("NspPackages returned %d packages, want 1", len(pkgs))
	}
	p := pkgs[0]
	if p.Kind != 0 || p.TitleID != tid || p.Version != 5 {
		t.Errorf("package = kind %d tid %016X v %d, want kind 0 tid %016X v 5", p.Kind, p.TitleID, p.Version, tid)
	}
	if p.Files["exefs/main.npdm"] == nil || p.Files["exefs/main"] == nil || p.Files["romfs/file.bin"] == nil {
		t.Errorf("missing expected files: %v", keys(p.Files))
	}
}

func TestMergeGameSemantics(t *testing.T) {
	base := nup.Package{
		TitleID: 0x0100000000010000, Version: 0, Kind: 0,
		Files: map[string]*nup.Source{
			"exefs/main.npdm": nup.SourceData(buildNPDM(0x0100000000010000)),
			"exefs/main":      nup.SourceData([]byte("base-main")),
			"romfs/a.bin":     nup.SourceData([]byte("base-a")),
			"romfs/b.bin":     nup.SourceData([]byte("base-b")),
		},
		Directories: []string{"exefs", "romfs"},
	}
	update := nup.Package{
		TitleID: 0x0100000000010000, Version: 2, Kind: 1,
		Files: map[string]*nup.Source{
			"exefs/main.npdm": nup.SourceData(buildNPDM(0x0100000000010000)),
			"exefs/main":      nup.SourceData([]byte("update-main")),
			"romfs/b.bin":     nup.SourceData([]byte("update-b")),
			"romfs/c.bin":     nup.SourceData([]byte("update-c")),
		},
		Directories: []string{"exefs", "romfs"},
	}
	dlc := nup.Package{
		TitleID: 0x0100000000011000, Version: 0, Kind: 2,
		Files:       map[string]*nup.Source{"romfs/dlc.bin": nup.SourceData([]byte("dlc"))},
		Directories: []string{"romfs"},
	}

	merged, err := MergeGame([]nup.Package{base, update, dlc}, -1)
	if err != nil {
		t.Fatalf("MergeGame: %v", err)
	}
	if merged.Version != 2 {
		t.Errorf("merged version = %d, want 2", merged.Version)
	}
	// update exefs wins.
	if got := readSource(t, merged.Files["exefs/main"]); string(got) != "update-main" {
		t.Errorf("exefs/main = %q, want update-main", got)
	}
	// romfs shadowing: b from update, a from base, c from update.
	if got := readSource(t, merged.Files["romfs/a.bin"]); string(got) != "base-a" {
		t.Errorf("romfs/a.bin = %q, want base-a", got)
	}
	if got := readSource(t, merged.Files["romfs/b.bin"]); string(got) != "update-b" {
		t.Errorf("romfs/b.bin = %q, want update-b", got)
	}
	if got := readSource(t, merged.Files["romfs/c.bin"]); string(got) != "update-c" {
		t.Errorf("romfs/c.bin = %q, want update-c", got)
	}

	// DLC untouched by compact.
	compacted, err := CompactPackages([]nup.Package{base, update, dlc})
	if err != nil {
		t.Fatalf("CompactPackages: %v", err)
	}
	if len(compacted) != 2 {
		t.Fatalf("CompactPackages returned %d packages, want 2 (merged + dlc)", len(compacted))
	}
	if compacted[1].Kind != 2 || compacted[1].TitleID != 0x0100000000011000 {
		t.Errorf("dlc package = %+v, want kind 2 tid 0100000000011000", compacted[1])
	}
}

func TestPresentationTitleOverride(t *testing.T) {
	pkgs := []nup.Package{{
		TitleID: 0x0100000000010000, Version: 0, Kind: 0,
		Files:    map[string]*nup.Source{},
		Metadata: []nup.TLV{{Kind: 5, Data: []byte("Old Title")}},
	}}
	out, err := Presentation(pkgs, "New Title", nil)
	if err != nil {
		t.Fatalf("Presentation: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Presentation returned %d packages, want 1", len(out))
	}
	var title []byte
	for _, t := range out[0].Metadata {
		if t.Kind == 5 {
			title = t.Data
		}
	}
	if string(title) != "New Title" {
		t.Errorf("title = %q, want New Title", title)
	}
}

func readSource(t *testing.T, s *nup.Source) []byte {
	t.Helper()
	r, err := s.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func keys(m map[string]*nup.Source) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
