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

// codecs returns path -> codec for the files of the first package of path.
func codecs(t *testing.T, path string) map[string]int {
	t.Helper()
	a, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	if _, err := a.Verify(context.Background(), nil); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	out := map[string]int{}
	for p, idx := range a.trees[0] {
		out[p] = a.files[idx].codec
	}
	return out
}

// codec returns the codec of path, failing the test when it is missing.
func codec(t *testing.T, codecs map[string]int, path string) int {
	t.Helper()
	c, ok := codecs[path]
	if !ok {
		t.Fatalf("%s missing from the archive: %v", path, codecs)
	}
	return c
}

// partlyRandom returns size bytes of which roughly zeroPercent are zeros, so
// zstd saves a little less than that share.
func partlyRandom(t *testing.T, size, zeroPercent int) []byte {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data[:size-size*zeroPercent/100]); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLargeFileThreshold(t *testing.T) {
	ctx := context.Background()
	text := bytes.Repeat([]byte("compressible "), 20000) // 260 KB
	pkg := func() []Package {
		return []Package{{TitleID: testTID, Kind: 0, Files: map[string]*Source{
			"romfs/big.txt":   SourceData(text),
			"romfs/small.txt": SourceData(text[:50000]),
		}}}
	}
	cases := []struct {
		name      string
		largeRaw  bool
		threshold int64
		big       int
	}{
		{"default threshold", true, 0, 1},
		{"below threshold raw", true, 100 << 10, 0},
		{"rule disabled", false, 100 << 10, 1},
	}
	for _, c := range cases {
		out := filepath.Join(t.TempDir(), "t.nup")
		if _, err := Pack(ctx, pkg(), out, PackOptions{Level: 3, LargeRaw: c.largeRaw, LargeFileThreshold: c.threshold}); err != nil {
			t.Fatalf("%s: Pack: %v", c.name, err)
		}
		got := codecs(t, out)
		if codec(t, got, "romfs/big.txt") != c.big || codec(t, got, "romfs/small.txt") != 1 {
			t.Errorf("%s: codecs = %v, want big=%d small=1", c.name, got, c.big)
		}
	}
}

func TestMinSavings(t *testing.T) {
	ctx := context.Background()
	half := partlyRandom(t, 256<<10, 50)
	little := partlyRandom(t, 256<<10, 6)
	for _, c := range []struct {
		min          float64
		half, little int
	}{
		{0, 1, 1},
		{3, 1, 1},
		{10, 1, 0},
		{60, 0, 0},
	} {
		out := filepath.Join(t.TempDir(), "s.nup")
		pkgs := []Package{{TitleID: testTID, Kind: 0, Files: map[string]*Source{
			"romfs/half":   SourceData(half),
			"romfs/little": SourceData(little),
		}}}
		if _, err := Pack(ctx, pkgs, out, PackOptions{Level: 3, LargeRaw: true, MinSavings: c.min}); err != nil {
			t.Fatalf("MinSavings %v: Pack: %v", c.min, err)
		}
		got := codecs(t, out)
		if codec(t, got, "romfs/half") != c.half || codec(t, got, "romfs/little") != c.little {
			t.Errorf("MinSavings %v: codecs = %v, want half=%d little=%d", c.min, got, c.half, c.little)
		}
	}
}

func TestPolicyValidation(t *testing.T) {
	ctx := context.Background()
	pkgs := []Package{{TitleID: testTID, Kind: 0, Files: map[string]*Source{"romfs/a": SourceData([]byte("x"))}}}
	for _, opts := range []PackOptions{
		{Level: 3, MinSavings: 100},
		{Level: 3, MinSavings: -1},
		{Level: 3, LargeFileThreshold: -1},
	} {
		if _, err := Pack(ctx, pkgs, filepath.Join(t.TempDir(), "v.nup"), opts); err == nil {
			t.Errorf("Pack(%+v): want a validation error", opts)
		}
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
