package nup

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/dannolan/boxcar/internal/nup/nuptest"
	"github.com/klauspost/compress/zstd"
)

// frameAt encodes data as one zstd frame with a known content size.
func frameAt(t *testing.T, data []byte, lvl zstd.EncoderLevel) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(lvl), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	enc.ResetContentSize(&b, int64(len(data)))
	enc.Write(data)
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// storedOf returns the stored size and codec of path in package 0.
func storedOf(t *testing.T, a *Archive, path string) (int64, int) {
	t.Helper()
	for _, rec := range a.files {
		if rec.name == path || filepath.Base(path) == rec.name {
			return rec.stored, rec.codec
		}
	}
	t.Fatalf("%s not found", path)
	return 0, 0
}

func compressibleText(n int) []byte {
	r := rand.New(rand.NewSource(7))
	var b bytes.Buffer
	words := []string{"alpha", "beta", "gamma", "delta", "mushroom", "kingdom", "level", "save", "flag"}
	for b.Len() < n {
		fmt.Fprintf(&b, "%s=%d;", words[r.Intn(len(words))], r.Intn(1000))
	}
	return b.Bytes()[:n]
}

// TestOptimizePicksBestDuplicateExtent: when one content is stored several
// times, the output is built from the smallest compliant extent, even when
// it is not the first: a raw first copy is not compressed again, and a
// strong frame is not replaced by a weaker re-encode.
func TestOptimizePicksBestDuplicateExtent(t *testing.T) {
	dir := t.TempDir()
	text := compressibleText(300 << 10)
	strong := frameAt(t, text, zstd.SpeedBestCompression)
	in := filepath.Join(dir, "in.nup")
	pkgs := []nuptest.Package{{TitleID: testTID, Kind: 0, Files: []nuptest.File{
		{Path: "romfs/a.txt", Data: text},                                                        // raw, first
		{Path: "romfs/b.txt", Data: text, Extent: strong, Codec: 1},                              // smallest frame
		{Path: "romfs/c.txt", Data: text, Extent: frameAt(t, text, zstd.SpeedFastest), Codec: 1}, // weaker frame
	}}}
	if err := nuptest.WriteKaiDump(in, pkgs, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, recompress := range []bool{false, true} {
		opts := defaultOptions()
		opts.Recompress = recompress
		opts.Output = filepath.Join(dir, fmt.Sprintf("out-%v.nup", recompress))
		res := optimizeT(t, in, opts)
		if res.Plan.BetterExtents != 1 || res.Plan.DuplicateFiles != 2 {
			t.Fatalf("recompress=%v plan = %+v", recompress, res.Plan)
		}
		if !recompress && (res.Stats.ReusedFrames != 1 || res.Stats.Compressed != 0) {
			t.Errorf("stats = %+v, want the strong frame reused and nothing compressed", res.Stats)
		}
		b := openT(t, opts.Output)
		stored, codec := storedOf(t, b, "a.txt")
		if codec != 1 || stored > int64(len(strong)) {
			t.Errorf("recompress=%v: stored %d (codec %d), want at most the existing best frame's %d", recompress, stored, codec, len(strong))
		}
		assertSameContent(t, in, opts.Output, nil)
	}
}

// TestOptimizeLargeRawPrefersRawDuplicate: for a file the policy keeps raw,
// a raw copy is used instead of decoding a frame of the same content.
func TestOptimizeLargeRawPrefersRawDuplicate(t *testing.T) {
	dir := t.TempDir()
	text := compressibleText(200 << 10)
	in := filepath.Join(dir, "in.nup")
	pkgs := []nuptest.Package{{TitleID: testTID, Kind: 0, Files: []nuptest.File{
		{Path: "romfs/a.bin", Data: text, Extent: frameAt(t, text, zstd.SpeedDefault), Codec: 1},
		{Path: "romfs/b.bin", Data: text},
	}}}
	if err := nuptest.WriteKaiDump(in, pkgs, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.LargeFileThreshold = 64 << 10
	opts.Output = filepath.Join(dir, "out.nup")
	res := optimizeT(t, in, opts)
	if res.Stats.KeptRaw != 1 || res.Stats.Decompressed != 0 || res.Plan.BetterExtents != 1 {
		t.Fatalf("stats = %+v plan = %+v", res.Stats, res.Plan)
	}
	assertSameContent(t, in, opts.Output, nil)
}

// TestOptimizeChecksDroppedFirstExtent: the first copy is still verified
// when a later, better extent supplies the bytes; a damaged first copy fails
// the run.
func TestOptimizeChecksDroppedFirstExtent(t *testing.T) {
	dir := t.TempDir()
	text := compressibleText(100 << 10)
	in := filepath.Join(dir, "in.nup")
	pkgs := []nuptest.Package{{TitleID: testTID, Kind: 0, Files: []nuptest.File{
		{Path: "romfs/a.txt", Data: text},
		{Path: "romfs/b.txt", Data: text, Extent: frameAt(t, text, zstd.SpeedDefault), Codec: 1},
	}}}
	if err := nuptest.WriteKaiDump(in, pkgs, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(raw, text[:4096])
	if at < 0 {
		t.Fatal("raw copy not found")
	}
	raw[at+5000] ^= 0xff
	if err := os.WriteFile(in, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.Output = filepath.Join(dir, "out.nup")
	if _, err := Optimize(t.Context(), in, opts); err == nil {
		t.Fatal("a damaged dropped extent was not detected")
	}
	if pathExists(opts.Output) {
		t.Error("output written despite the damaged source")
	}
}
