package nup

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDedup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := bytes.Repeat([]byte("a"), 1<<20)
	different := append(append([]byte{}, first[:65536]...), bytes.Repeat([]byte("b"), (1<<20)-65536)...)
	pkg := Package{
		TitleID: testTID, Version: 0, Kind: 0,
		Files: map[string]*Source{
			"romfs/a": SourceData(first),
			"romfs/b": SourceData(first),
			"romfs/c": SourceData(different),
		},
		Directories: []string{"romfs"},
	}
	out := filepath.Join(dir, "dedup.nup")
	res, err := Pack(ctx, []Package{pkg}, out, PackOptions{Level: 3, LargeRaw: true, Verify: true})
	if err != nil {
		t.Fatalf("Pack: %v", err)
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
	if stats["unique_extents"].(int) != 2 {
		t.Errorf("unique_extents = %v, want 2 (a/b shared, c distinct)", stats["unique_extents"])
	}
	// c must not be shared with a/b.
	if got := readSource(t, a.Packages()[0].Files["romfs/c"]); !bytes.Equal(got, different) {
		t.Errorf("romfs/c content mismatch")
	}
	if got := readSource(t, a.Packages()[0].Files["romfs/a"]); !bytes.Equal(got, first) {
		t.Errorf("romfs/a content mismatch")
	}
}

func TestRawFallback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	data := make([]byte, 2<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	pkg := Package{
		TitleID: testTID, Version: 0, Kind: 0,
		Files:       map[string]*Source{"romfs/random.bin": SourceData(data)},
		Directories: []string{"romfs"},
	}
	out := filepath.Join(dir, "raw.nup")
	if _, err := Pack(ctx, []Package{pkg}, out, PackOptions{Level: 3, LargeRaw: true, Verify: true}); err != nil {
		t.Fatalf("Pack: %v", err)
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
	if stats["raw_files"].(int) != 1 {
		t.Errorf("raw_files = %v, want 1", stats["raw_files"])
	}
	if stats["compressed_files"].(int) != 0 {
		t.Errorf("compressed_files = %v, want 0", stats["compressed_files"])
	}
	if stats["stored"].(int64) != int64(len(data)) {
		t.Errorf("stored = %v, want %d (raw fallback stores full size)", stats["stored"], len(data))
	}
}

func TestLevelValidation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pkg := Package{TitleID: testTID, Version: 0, Kind: 0, Files: map[string]*Source{"romfs/a": SourceData([]byte("x"))}}
	out := filepath.Join(dir, "bad.nup")
	_, err := Pack(ctx, []Package{pkg}, out, PackOptions{Level: 2})
	if err == nil || !strings.Contains(err.Error(), "Choose compression level 1, 3 or 9.") {
		t.Errorf("err = %v, want level error", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("output should not exist after failed pack")
	}
}

func TestSourceMutation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := writeFile(t, dir, "romfs/data.bin", []byte("original"))
	src, err := SourcePath(path)
	if err != nil {
		t.Fatal(err)
	}
	pkg := Package{
		TitleID: testTID, Version: 0, Kind: 0,
		Files:       map[string]*Source{"romfs/data.bin": src},
		Directories: []string{"romfs"},
	}
	out := filepath.Join(dir, "mut.nup")
	// Mutate the source (different size guarantees a stamp mismatch) before pack.
	if err := os.WriteFile(path, []byte("mutated content"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Pack(ctx, []Package{pkg}, out, PackOptions{Level: 3})
	if err == nil || !strings.Contains(err.Error(), "Source changed during packaging: data.bin") {
		t.Errorf("err = %v, want source-changed error", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("output should not exist after failed pack")
	}
}

func TestSourceValidateDirect(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.bin", []byte("data"))
	src, err := SourcePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Validate(); err != nil {
		t.Fatalf("Validate before mutation: %v", err)
	}
	if err := os.WriteFile(path, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := src.Validate(); err == nil {
		t.Errorf("Validate after mutation: want error")
	}
}

func TestProgressCallbacks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packages, _ := buildSynthetic(t, dir)
	out := filepath.Join(dir, "prog.nup")
	var labels []string
	var percents []float64
	prog := func(label string, done float64) {
		labels = append(labels, label)
		percents = append(percents, done)
	}
	if _, err := Pack(ctx, packages, out, PackOptions{Level: 3, LargeRaw: true, Verify: true, Progress: prog}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	sawCompress, sawVerify := false, false
	for _, l := range labels {
		switch l {
		case "Compressing / storing files":
			sawCompress = true
		case "Verifying NUP files":
			sawVerify = true
		}
	}
	if !sawCompress {
		t.Errorf("did not observe 'Compressing / storing files'")
	}
	if !sawVerify {
		t.Errorf("did not observe 'Verifying NUP files'")
	}
	for _, p := range percents {
		if p < 0 || p > 100 {
			t.Errorf("percent %v out of range [0,100]", p)
		}
	}
}

func TestPackRefusesOverwrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pkg := Package{TitleID: testTID, Version: 0, Kind: 0, Files: map[string]*Source{"romfs/a": SourceData([]byte("x"))}}
	out := filepath.Join(dir, "game.nup")
	if _, err := Pack(ctx, []Package{pkg}, out, PackOptions{Level: 3}); err != nil {
		t.Fatalf("first Pack: %v", err)
	}
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(ctx, []Package{pkg}, out, PackOptions{Level: 3}); err == nil {
		t.Errorf("second Pack over existing output: want error")
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("existing output was modified")
	}
}
