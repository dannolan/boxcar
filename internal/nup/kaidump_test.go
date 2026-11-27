package nup

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/dannolan/boxcar/internal/nup/nuptest"
)

// kaiDumpPackages is a KaiDump-shaped game: one base package (KaiDump packs
// the update into it) plus one DLC, with an empty file between two others so
// its recorded offset is the unaligned blob cursor.
func kaiDumpPackages() ([]nuptest.Package, map[string][]byte) {
	const tid = testTID
	npdm := buildNPDM(tid)
	main := append([]byte("NSO0"), bytes.Repeat([]byte{7}, 300)...)
	text := bytes.Repeat([]byte("all work and no play makes a dull boy\n"), 4000)
	odd := []byte("odd-sized")
	meta := append(nuptest.TLV(1, npdm), nuptest.TLV(5, []byte("Kai Game"))...)
	meta = append(meta, nuptest.TLV(8, []byte("KaiDump"))...)
	meta = append(meta, nuptest.TLV(0, []byte("a55uka numbers TLVs from 0"))...)
	meta = append(meta, nuptest.TLV(0x8001, []byte{0xde, 0xad, 0xbe, 0xef})...)
	base := nuptest.Package{
		TitleID: tid, Version: 0x20000, Kind: 0,
		Files: []nuptest.File{
			{Path: "exefs/main.npdm", Data: npdm},
			{Path: "exefs/main", Data: main},
			{Path: "romfs/z/odd.bin", Data: odd},
			{Path: "romfs/z/empty.txt"},
			{Path: "romfs/a/text.txt", Data: text},
			{Path: "romfs/a/copy.txt", Data: text},
		},
		Dirs: []string{"romfs/unused"},
		Meta: meta,
	}
	dlc := nuptest.Package{
		TitleID: tid | 0x1001, Kind: 2,
		Files: []nuptest.File{{Path: "romfs/addon.bin", Data: []byte("addon")}},
		Meta:  nuptest.TLV(8, []byte("KaiDump")),
	}
	expected := map[string][]byte{
		"base/exefs/main.npdm":                   npdm,
		"base/exefs/main":                        main,
		"base/romfs/z/odd.bin":                   odd,
		"base/romfs/z/empty.txt":                 nil,
		"base/romfs/a/text.txt":                  text,
		"base/romfs/a/copy.txt":                  text,
		"dlc/0100000000011001/0/romfs/addon.bin": []byte("addon"),
	}
	return []nuptest.Package{base, dlc}, expected
}

func writeKaiDump(t *testing.T, path string) map[string][]byte {
	t.Helper()
	packages, expected := kaiDumpPackages()
	if err := nuptest.WriteKaiDump(path, packages, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	return expected
}

// TestArchiveAcceptsKaiDumpEmptyFileOffsets locks in the documented
// deviation: KaiDump records the blob cursor as an empty file's extent
// offset. The reference rejects such archives; Boxcar ignores the offset of
// an empty file because nothing is ever read from it.
func TestArchiveAcceptsKaiDumpEmptyFileOffsets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "kaidump.nup")
	expected := writeKaiDump(t, path)

	a, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	var emptyOff int64 = -1
	for _, f := range a.files {
		if f.size == 0 {
			emptyOff = f.off
		}
	}
	if emptyOff <= 0 || emptyOff%16 == 0 {
		t.Fatalf("fixture should give the empty file an unaligned, nonzero offset; got %d", emptyOff)
	}
	stats, err := a.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if stats["compressed_files"].(int) != 0 || stats["unique_extents"].(int) != 6 {
		t.Errorf("stats = %v, want 0 compressed files and 6 extents (no dedup)", stats)
	}
	out := filepath.Join(dir, "extracted")
	if _, err := Extract(ctx, path, out, nil, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for rel, want := range expected {
		got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: err=%v, content mismatch=%v", rel, err, !bytes.Equal(got, want))
		}
	}
	if st, err := os.Stat(filepath.Join(out, "base", "romfs", "unused")); err != nil || !st.IsDir() {
		t.Errorf("empty directory not extracted: %v", err)
	}
}

func TestEmptyFileStillRejectsStoredBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kaidump.nup")
	writeKaiDump(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	layout := parseLayout(t, data)
	toc := data[128 : 128+int(binary.LittleEndian.Uint64(data[16:24]))]
	empty := -1
	for i := 0; i < layout.nf; i++ {
		if binary.LittleEndian.Uint64(toc[layout.fileRec(i)+8:]) == 0 {
			empty = i
		}
	}
	if empty < 0 {
		t.Fatal("fixture has no empty file")
	}
	mutated := mutateTOC(t, data, func(toc []byte) {
		binary.LittleEndian.PutUint64(toc[layout.fileRec(empty)+24:], 16)
	})
	expectOpenError(t, mutated, "Invalid empty-file extent.")
	mutated = mutateTOC(t, data, func(toc []byte) {
		toc[layout.fileRec(empty)+64] = 1
	})
	expectOpenError(t, mutated, "Invalid empty-file extent.")
}
