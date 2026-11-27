package nup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
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

// TestOptimizeMeasureReplacesUncertainSkip: with Measure, a skip verdict
// based on a sampled estimate becomes a real rewrite whose measured size
// decides; nothing is published when it does not reach the minimum gain.
func TestOptimizeMeasureReplacesUncertainSkip(t *testing.T) {
	if testing.Short() {
		t.Skip("writes an archive larger than the sampling budget")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "big.nup")
	chunk := compressibleText(1 << 20)
	var files []nuptest.File
	for i := 0; i < 72; i++ { // 72 MiB of candidates: above sampleBudget, so the estimate is sampled
		data := append([]byte(fmt.Sprintf("%04d", i)), chunk[4:]...)
		files = append(files, nuptest.File{Path: fmt.Sprintf("romfs/t%02d.txt", i), Data: data})
	}
	if err := nuptest.WriteKaiDump(in, []nuptest.Package{{TitleID: testTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.MinGain = 99.9
	opts.Output = filepath.Join(dir, "out.nup")
	res := optimizeT(t, in, opts)
	if res.Status != "skipped" || res.Plan.Basis != "estimated" {
		t.Fatalf("estimate-based run = %+v", res)
	}
	opts.Measure = true
	opts.DryRun = true
	if res = optimizeT(t, in, opts); res.Action != "measure" || !strings.Contains(res.Reason, "full temporary rewrite") {
		t.Fatalf("dry run with Measure = %+v", res)
	}
	opts.DryRun = false
	res = optimizeT(t, in, opts)
	// (measured by counting: no temporary file is written)
	if res.Status != "skipped" || !strings.Contains(res.Reason, "counted, not written") || pathExists(opts.Output) {
		t.Fatalf("measured run below the gain = %+v", res)
	}
	assertNoPartials(t, dir)
	opts.MinGain = 10
	res = optimizeT(t, in, opts)
	if res.Status != "optimized" || !res.Verified || res.SavedBytes <= 0 {
		t.Fatalf("measured run = %+v", res)
	}
}

// TestOptimizeParallelDeterministic: the output is byte-identical whatever
// the number of workers, and identical to the streaming (writer-only) path.
func TestOptimizeParallelDeterministic(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.nup")
	r := rand.New(rand.NewSource(3))
	var files []nuptest.File
	for i := 0; i < 120; i++ {
		var data []byte
		switch i % 4 {
		case 0: // compressible
			data = compressibleText(int(r.Int63n(300<<10)) + 1)
		case 1: // incompressible
			data = make([]byte, r.Int63n(200<<10)+1)
			r.Read(data)
		case 2: // barely compressible: around the 5% minimum
			data = make([]byte, 128<<10)
			r.Read(data)
			copy(data, bytes.Repeat([]byte{0}, int(r.Int63n(16<<10))))
		default: // an existing frame, re-encoded with Recompress
			text := compressibleText(int(r.Int63n(100<<10)) + 1)
			files = append(files, nuptest.File{Path: fmt.Sprintf("romfs/f%03d.bin", i), Data: text, Extent: frameAt(t, text, zstd.SpeedFastest), Codec: 1})
			continue
		}
		files = append(files, nuptest.File{Path: fmt.Sprintf("romfs/f%03d.bin", i), Data: data})
	}
	if err := nuptest.WriteKaiDump(in, []nuptest.Package{{TitleID: testTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	defer func(w int, c int64) { testWorkers, parChunk = w, c }(testWorkers, parChunk)
	var want [32]byte
	for _, c := range []struct {
		workers int
		chunk   int64
	}{{1, 32 << 20}, {3, 32 << 20}, {8, 32 << 20}, {4, 0}} {
		testWorkers, parChunk = c.workers, c.chunk
		opts := defaultOptions()
		opts.MinSavings = 5
		opts.Recompress = true
		opts.Output = filepath.Join(dir, fmt.Sprintf("out-%d-%d.nup", c.workers, c.chunk))
		optimizeT(t, in, opts)
		got := fileSHA(t, opts.Output)
		if want == ([32]byte{}) {
			want = got
		} else if got != want {
			t.Errorf("workers=%d chunk=%d: output differs", c.workers, c.chunk)
		}
	}
}

// TestOptimizeParallelCancelReleasesBudget: cancelling mid-write stops the
// workers, removes the partial file and returns every claimed byte and CPU
// token to the shared budgets.
func TestOptimizeParallelCancelReleasesBudget(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.nup")
	var files []nuptest.File
	for i := 0; i < 64; i++ {
		files = append(files, nuptest.File{Path: fmt.Sprintf("romfs/t%02d.txt", i), Data: compressibleText(512 << 10)})
	}
	if err := nuptest.WriteKaiDump(in, []nuptest.Package{{TitleID: testTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := defaultOptions()
	opts.Output = filepath.Join(dir, "out.nup")
	opts.Progress = func(label string, pct float64) {
		if label == "Optimizing NUP files" && pct > 20 {
			cancel()
		}
	}
	if _, err := Optimize(ctx, in, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancellation", err)
	}
	assertNoPartials(t, dir)
	if pathExists(opts.Output) {
		t.Error("output published after cancellation")
	}
	for _, b := range []*budget{memBudget, cpuTokens} {
		b.mu.Lock()
		used := b.used
		b.mu.Unlock()
		if used != 0 {
			t.Errorf("budget still holds %d after cancellation", used)
		}
	}
}

// TestEstimateAppliesPolicyPerFile: files the estimate has no samples of
// take the policy-applied outcome of their sampled peers. Here half the
// files save about 7% (a frame) and half nothing (kept raw): the average
// ratio of their samples (about 0.965) is above the 5% minimum-savings
// cut-off, but the expected saving is still about 3.5% of their bytes,
// which the estimate must report rather than treating every unsampled file
// as raw.
func TestEstimateAppliesPolicyPerFile(t *testing.T) {
	if testing.Short() {
		t.Skip("writes an archive larger than the sampling budget")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "mixed.nup")
	r := rand.New(rand.NewSource(11))
	var files []nuptest.File
	for i := 0; i < 640; i++ { // 80 MiB in 128 KiB files: more files than sample windows
		data := make([]byte, 128<<10)
		r.Read(data)
		if i%2 == 0 {
			clear(data[:9<<10])
		}
		files = append(files, nuptest.File{Path: fmt.Sprintf("romfs/f%03d.bin", i), Data: data})
	}
	if err := nuptest.WriteKaiDump(in, []nuptest.Package{{TitleID: testTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.MinSavings = 5
	opts.MinGain = 0
	opts.Output = filepath.Join(dir, "out.nup")
	opts.DryRun = true
	plan := optimizeT(t, in, opts)
	if plan.Plan.Exact {
		t.Fatal("expected a sampled estimate")
	}
	opts.DryRun = false
	res := optimizeT(t, in, opts)
	est := 100 * float64(plan.SavedBytes) / float64(plan.SizeBefore)
	got := 100 * float64(res.SavedBytes) / float64(res.SizeBefore)
	if got < 2 || est < got-1 || est > got+1 {
		t.Errorf("estimated %.2f%% savings, measured %.2f%%", est, got)
	}
}
