package nup

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/nup/nuptest"
)

func openT(t *testing.T, path string) *Archive {
	t.Helper()
	a, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%s): %v", filepath.Base(path), err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// packAsBoxcar packs the KaiDump fixture's packages with Boxcar's own writer
// (compressed, deduplicated, metadata re-serialized from parsed TLVs).
func packAsBoxcar(t *testing.T, path string, pkgs []nuptest.Package) {
	t.Helper()
	var packages []Package
	for _, p := range pkgs {
		files := map[string]*Source{}
		for _, f := range p.Files {
			files[f.Path] = SourceData(f.Data)
		}
		meta, err := parseTlvs(p.Meta)
		if err != nil {
			t.Fatal(err)
		}
		packages = append(packages, Package{
			TitleID: p.TitleID, Version: p.Version, Kind: int(p.Kind), Flags: int(p.Flags),
			Files: files, Directories: p.Dirs, Metadata: meta,
		})
	}
	if _, err := Pack(context.Background(), packages, path, PackOptions{Level: 3, LargeRaw: true, Verify: true}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
}

func TestLogicalDigestIgnoresStorage(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "kaidump.nup")
	writeKaiDump(t, raw)
	packed := filepath.Join(dir, "boxcar.nup")
	pkgs, _ := kaiDumpPackages()
	packAsBoxcar(t, packed, pkgs)

	a, b := openT(t, raw), openT(t, packed)
	if a.stats["compressed_files"].(int) != 0 || b.stats["compressed_files"].(int) == 0 {
		t.Fatalf("fixtures should differ in storage: %v vs %v", a.stats, b.stats)
	}
	if err := CompareContents(a.Contents(), b.Contents()); err != nil {
		t.Fatalf("CompareContents: %v", err)
	}
	da, db := LogicalDigest(a.Contents()), LogicalDigest(b.Contents())
	if da != db || len(da) != 64 {
		t.Fatalf("logical digests differ: %s vs %s", da, db)
	}
	if err := CompareContents(a.Contents(), contentsOf(a.Packages())); err != nil {
		t.Errorf("parsed packages disagree with parsed contents: %v", err)
	}
}

func TestCompareContentsReportsDifferences(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kaidump.nup")
	writeKaiDump(t, path)
	base := openT(t, path).Contents()
	clone := func() []Content {
		out := make([]Content, len(base))
		for i, c := range base {
			c.Metadata = append([]byte(nil), c.Metadata...)
			c.Directories = append([]string(nil), c.Directories...)
			c.Files = append([]FileContent(nil), c.Files...)
			out[i] = c
		}
		return out
	}
	cases := map[string]func(c []Content){
		"metadata":                    func(c []Content) { c[0].Metadata[len(c[0].Metadata)-1] ^= 1 },
		"SHA-256 of romfs/a/copy.txt": func(c []Content) { c[0].Files[2].SHA256[0] ^= 1 },
		"size of romfs/a/copy.txt":    func(c []Content) { c[0].Files[2].Size++ },
		"file list":                   func(c []Content) { c[1].Files = nil },
		"directory list":              func(c []Content) { c[0].Directories = c[0].Directories[1:] },
		"flags":                       func(c []Content) { c[1].Flags = 1 },
		"exefs/romfs record pointer":  func(c []Content) { c[0].ExeFS = false },
		"root directory name":         func(c []Content) { c[0].Root = "x" },
		"identity":                    func(c []Content) { c[1].Version++ },
	}
	for what, edit := range cases {
		changed := clone()
		edit(changed)
		err := CompareContents(base, changed)
		if err == nil || !strings.Contains(err.Error(), what) {
			t.Errorf("%s: CompareContents = %v", what, err)
		}
		if LogicalDigest(base) == LogicalDigest(changed) {
			t.Errorf("%s: logical digest unchanged", what)
		}
	}
	if err := CompareContents(base, base[:1]); err == nil {
		t.Errorf("package count difference not reported")
	}
	if err := CompareContents(base, clone()); err != nil {
		t.Errorf("identical contents: %v", err)
	}
}

// TestContentsKeepUnsetDirectoryPointers covers a record that leaves
// exefs_dir unset although the tree has exefs/: the parsed packages carry
// that fact so a rewrite can reproduce it.
func TestContentsKeepUnsetDirectoryPointers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kaidump.nup")
	writeKaiDump(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	layout := parseLayout(t, data)
	mutated := mutateTOC(t, data, func(toc []byte) {
		binary.LittleEndian.PutUint32(toc[layout.pkgOff+12:], 0xffffffff)
	})
	unset := filepath.Join(dir, "unset.nup")
	if err := os.WriteFile(unset, mutated, 0o644); err != nil {
		t.Fatal(err)
	}
	a := openT(t, unset)
	c := a.Contents()
	if c[0].ExeFS || !c[0].RomFS {
		t.Fatalf("pointers = exefs %v romfs %v, want false/true", c[0].ExeFS, c[0].RomFS)
	}
	if err := CompareContents(c, contentsOf(a.Packages())); err != nil {
		t.Errorf("contentsOf lost the unset pointer: %v", err)
	}
}
