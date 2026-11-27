package nup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/nup/nuptest"
)

func fileSHA(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// assertNoPartials fails when dir holds leftover temporary files.
func assertNoPartials(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") {
			t.Errorf("leftover temporary file %s", e.Name())
		}
	}
}

func optimizeT(t *testing.T, in string, opts OptimizeOptions) OptimizeResult {
	t.Helper()
	res, err := Optimize(context.Background(), in, opts)
	if err != nil {
		t.Fatalf("Optimize(%s): %v", filepath.Base(in), err)
	}
	return res
}

// assertSameContent checks an optimized archive against the source's
// contents and extracts it to confirm every byte.
func assertSameContent(t *testing.T, src, out string, expected map[string][]byte) {
	t.Helper()
	a, b := openT(t, src), openT(t, out)
	if err := CompareContents(a.Contents(), b.Contents()); err != nil {
		t.Fatalf("contents differ: %v", err)
	}
	if _, err := b.Verify(context.Background(), nil); err != nil {
		t.Fatalf("Verify(output): %v", err)
	}
	if expected == nil {
		return
	}
	dir := filepath.Join(t.TempDir(), "x")
	if _, err := Extract(context.Background(), out, dir, nil, nil); err != nil {
		t.Fatalf("Extract(output): %v", err)
	}
	for rel, want := range expected {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: err=%v mismatch=%v", rel, err, !bytes.Equal(got, want))
		}
	}
}

func defaultOptions() OptimizeOptions {
	return OptimizeOptions{RepackOptions: RepackOptions{Level: 3, LargeRaw: true}, MinGain: 1}
}

func TestOptimizeKaiDump(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "kaidump.nup")
	expected := writeKaiDump(t, in)
	before := fileSHA(t, in)
	out := filepath.Join(dir, "optimized.nup")

	opts := defaultOptions()
	opts.Output = out
	res := optimizeT(t, in, opts)
	if res.Status != "optimized" || !res.Verified || res.Output != out {
		t.Fatalf("result = %+v", res)
	}
	if res.LogicalSHA256 != res.OutputLogicalSHA256 || res.ContentChanged {
		t.Errorf("logical digest changed: %s -> %s", res.LogicalSHA256, res.OutputLogicalSHA256)
	}
	if res.SizeAfter >= res.SizeBefore || res.SavedBytes != res.SizeBefore-res.SizeAfter {
		t.Errorf("sizes %d -> %d, saved %d", res.SizeBefore, res.SizeAfter, res.SavedBytes)
	}
	if st, _ := os.Stat(out); st.Size() != res.SizeAfter {
		t.Errorf("SizeAfter %d, file %d", res.SizeAfter, st.Size())
	}
	s := res.Stats
	if s.UniqueContents != 5 || s.DuplicateFiles != 1 || s.Compressed < 2 || s.Incompressible < 1 {
		t.Errorf("stats = %+v", *s)
	}
	if s.ReusedFrames+s.KeptRaw+s.Compressed+s.Incompressible+s.Recompressed+s.Decompressed != s.UniqueContents {
		t.Errorf("stats do not add up: %+v", *s)
	}
	if fileSHA(t, in) != before {
		t.Errorf("source archive was modified")
	}
	assertNoPartials(t, dir)
	assertSameContent(t, in, out, expected)

	b := openT(t, out)
	if b.stats["unique_extents"].(int) != 5 {
		t.Errorf("unique_extents = %v, want 5 (copy.txt deduplicated)", b.stats["unique_extents"])
	}
	for _, f := range b.files {
		if f.size == 0 && f.off != 0 {
			t.Errorf("empty file kept offset %d; want 0 (SPEC 3.6)", f.off)
		}
	}
}

// TestOptimizePackedArchive: Pack's output is already optimal, so it is
// skipped; forcing the rewrite reproduces it byte for byte (same layout,
// frames reused, incompressible files still raw).
func TestOptimizePackedArchive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packages, expected := buildSynthetic(t, dir)
	in := filepath.Join(dir, "packed.nup")
	if _, err := Pack(ctx, packages, in, PackOptions{Level: 3, LargeRaw: true}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.nup")
	opts := defaultOptions()
	opts.Output = out
	res := optimizeT(t, in, opts)
	if res.Status != "skipped" || !strings.Contains(res.Reason, "already optimized") {
		t.Fatalf("result = %+v", res)
	}
	if res.SizeAfter != res.SizeBefore || res.SavedBytes != 0 || res.Stats != nil {
		t.Errorf("skipped archives report no change: %+v", res)
	}
	if pathExists(out) {
		t.Fatalf("skipped archive produced an output")
	}
	if res.Plan.ReuseFrames == 0 || res.Plan.DuplicateFiles != 1 || res.Plan.RedundantBytes != 0 {
		t.Errorf("plan = %+v", res.Plan)
	}

	opts.Force = true
	res = optimizeT(t, in, opts)
	if res.Status != "optimized" {
		t.Fatalf("forced result = %+v", res)
	}
	if fileSHA(t, in) != fileSHA(t, out) {
		t.Errorf("forced rewrite of a Pack archive is not byte-identical")
	}
	assertSameContent(t, in, out, expected)
}

// TestOptimizePackParityTrailingEmptyFile: Pack aligns the cursor before an
// empty file too, so an unaligned last extent followed by one moves the
// metadata; the rewrite must do the same to stay byte-identical.
func TestOptimizePackParityTrailingEmptyFile(t *testing.T) {
	dir := t.TempDir()
	odd := make([]byte, 17)
	rand.New(rand.NewSource(3)).Read(odd)
	pkgs := []Package{{TitleID: testTID, Kind: 0, Metadata: []TLV{{Kind: 5, Data: []byte("T")}}, Files: map[string]*Source{
		"romfs/a.bin": SourceData(odd),
		"romfs/z":     SourceData(nil),
	}}}
	in := filepath.Join(dir, "in.nup")
	if _, err := Pack(context.Background(), pkgs, in, PackOptions{Level: 3, LargeRaw: true}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.Force = true
	opts.Output = filepath.Join(dir, "out.nup")
	res := optimizeT(t, in, opts)
	if fileSHA(t, in) != fileSHA(t, opts.Output) {
		t.Errorf("rewrite differs from Pack's layout")
	}
	if res.Plan.EstimatedSize != res.SizeAfter {
		t.Errorf("estimate %d, actual %d", res.Plan.EstimatedSize, res.SizeAfter)
	}
}

// rawBlockFrame wraps data (at most one zstd block) in a valid zstd frame
// that stores it uncompressed, so the frame is larger than the file.
func rawBlockFrame(data []byte) []byte {
	n := len(data)
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x60} // single segment, 2-byte content size
	frame = binary.LittleEndian.AppendUint16(frame, uint16(n-256))
	header := uint32(n)<<3 | 1 // raw block, last
	frame = append(frame, byte(header), byte(header>>8), byte(header>>16))
	return append(frame, data...)
}

// TestOptimizePolicyOnDuplicateExtent: the policy applies to every stored
// extent, including a second copy that deduplication would drop.
func TestOptimizePolicyOnDuplicateExtent(t *testing.T) {
	dir := t.TempDir()
	text := bytes.Repeat([]byte("duplicate frames "), 60) // 1020 bytes
	var good bytes.Buffer
	enc, err := newZstdEncoder(&good, 3, int64(len(text)))
	if err != nil {
		t.Fatal(err)
	}
	enc.Write(text)
	enc.Close()
	noise := make([]byte, 1<<20)
	rand.New(rand.NewSource(4)).Read(noise)
	in := filepath.Join(dir, "in.nup")
	pkgs := []nuptest.Package{{TitleID: testTID, Kind: 0, Files: []nuptest.File{
		{Path: "romfs/a.txt", Data: text, Extent: good.Bytes(), Codec: 1},
		{Path: "romfs/b.txt", Data: text, Extent: rawBlockFrame(text), Codec: 1},
		{Path: "romfs/noise.bin", Data: noise},
	}}}
	if err := nuptest.WriteKaiDump(in, pkgs, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	openT(t, in)
	opts := defaultOptions()
	opts.Output = filepath.Join(dir, "out.nup")
	res := optimizeT(t, in, opts)
	if res.Plan.PolicyFixes != 1 || res.Status != "optimized" || !strings.Contains(res.Reason, "storage policy") {
		t.Fatalf("result = %+v", res)
	}
	if b := openT(t, opts.Output); b.stats["unique_extents"].(int) != 2 {
		t.Errorf("unique_extents = %v, want 2", b.stats["unique_extents"])
	}
}

func TestOptimizeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "kaidump.nup")
	writeKaiDump(t, in)
	first := filepath.Join(dir, "first.nup")
	opts := defaultOptions()
	opts.Output = first
	optimizeT(t, in, opts)

	second := filepath.Join(dir, "second.nup")
	opts.Output = second
	if res := optimizeT(t, first, opts); res.Status != "skipped" {
		t.Errorf("second pass = %+v, want skipped", res)
	}
	opts.Force = true
	optimizeT(t, first, opts)
	if fileSHA(t, first) != fileSHA(t, second) {
		t.Errorf("optimizing an optimized archive changed its bytes")
	}
}

func TestOptimizeInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "game.nup")
	expected := writeKaiDump(t, path)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(dir, "original.nup")
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(original, data, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := defaultOptions()
	opts.InPlace = true
	res := optimizeT(t, path, opts)
	if res.Status != "optimized" || res.Output != path || res.SizeAfter >= res.SizeBefore {
		t.Fatalf("result = %+v", res)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() != res.SizeAfter {
		t.Fatalf("replaced file: %v size %d", err, st.Size())
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", st.Mode().Perm())
	}
	assertNoPartials(t, dir)
	assertSameContent(t, original, path, expected)

	if res := optimizeT(t, path, opts); res.Status != "skipped" {
		t.Errorf("second in-place pass = %+v, want skipped", res)
	}
}

func TestOptimizeRecompress(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packages, expected := buildSynthetic(t, dir)
	in := filepath.Join(dir, "fast.nup")
	if _, err := Pack(ctx, packages, in, PackOptions{Level: 1, LargeRaw: true}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.Level = 9
	opts.Output = filepath.Join(dir, "out.nup")
	if res := optimizeT(t, in, opts); res.Status != "skipped" {
		t.Fatalf("frames are reused without --recompress: %+v", res)
	}
	opts.Recompress = true
	opts.DryRun = true
	if plan := optimizeT(t, in, opts).Plan; plan.Exact || plan.Reencode == 0 {
		t.Errorf("a re-encode estimate is not exact: %+v", plan)
	}
	opts.DryRun = false
	// The re-encode saves 32 bytes here: below a 1% minimum gain the
	// measured result is discarded, with no minimum it is kept.
	if res := optimizeT(t, in, opts); res.Status != "skipped" || pathExists(opts.Output) {
		t.Fatalf("recompression below the minimum gain was kept: %+v", res)
	}
	opts.MinGain = 0
	res := optimizeT(t, in, opts)
	if res.Status != "optimized" || res.Plan.Reencode == 0 {
		t.Fatalf("result = %+v", res)
	}
	again := opts
	again.Output = filepath.Join(dir, "again.nup")
	if res := optimizeT(t, opts.Output, again); res.Status != "skipped" || pathExists(again.Output) {
		t.Errorf("a second --recompress run wrote an output: %+v", res)
	}
	s := res.Stats
	if s.Recompressed+s.ReusedFrames != res.Plan.Reencode || s.Decompressed != 0 {
		t.Errorf("stats = %+v, plan reencode %d", *s, res.Plan.Reencode)
	}
	if res.SizeAfter > res.SizeBefore {
		t.Errorf("recompression grew the archive: %d -> %d", res.SizeBefore, res.SizeAfter)
	}
	assertSameContent(t, in, opts.Output, expected)
}

// TestOptimizeLargeFilePolicy: compressed files above the threshold are
// rewritten raw even though the archive grows, because nx2-flow-style
// loaders would otherwise have to inflate them whole.
func TestOptimizeLargeFilePolicy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	text := bytes.Repeat([]byte("large but compressible\n"), 12000) // 276 KB
	pkgs := []Package{{TitleID: testTID, Kind: 0, Files: map[string]*Source{
		"romfs/big.txt":   SourceData(text),
		"romfs/small.txt": SourceData(text[:4000]),
	}}}
	in := filepath.Join(dir, "all-compressed.nup")
	if _, err := Pack(ctx, pkgs, in, PackOptions{Level: 3, LargeRaw: false}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.LargeFileThreshold = 100 << 10
	opts.InPlace = true
	res := optimizeT(t, in, opts)
	if res.Status != "normalized" || res.Plan.Decode != 1 || res.Plan.PolicyFixes != 1 || res.Stats.Decompressed != 1 {
		t.Fatalf("result = %+v stats %+v", res, res.Stats)
	}
	if res.SizeAfter <= res.SizeBefore {
		t.Errorf("expected growth from storing big.txt raw: %d -> %d", res.SizeBefore, res.SizeAfter)
	}
	if got := codecs(t, in); codec(t, got, "romfs/big.txt") != 0 || codec(t, got, "romfs/small.txt") != 1 {
		t.Errorf("codecs = %v", got)
	}

	// Lifting the rule compresses it again.
	opts.LargeRaw = false
	res = optimizeT(t, in, opts)
	if res.Status != "optimized" || res.Stats.Compressed != 1 {
		t.Fatalf("result = %+v stats %+v", res, res.Stats)
	}
	if got := codecs(t, in); codec(t, got, "romfs/big.txt") != 1 {
		t.Errorf("codecs = %v", got)
	}
}

func TestOptimizeMinSavings(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	little := partlyRandom(t, 256<<10, 6)
	pkgs := []Package{{TitleID: testTID, Kind: 0, Files: map[string]*Source{"romfs/little": SourceData(little)}}}
	in := filepath.Join(dir, "in.nup")
	if _, err := Pack(ctx, pkgs, in, PackOptions{Level: 3, LargeRaw: true}); err != nil {
		t.Fatal(err)
	}
	if codec(t, codecs(t, in), "romfs/little") != 1 {
		t.Fatal("fixture should start compressed")
	}
	opts := defaultOptions()
	opts.MinSavings = 10
	opts.Output = filepath.Join(dir, "out.nup")
	res := optimizeT(t, in, opts)
	if res.Status != "normalized" || res.Plan.PolicyFixes != 1 || res.Stats.Decompressed != 1 {
		t.Fatalf("result = %+v stats %+v", res, res.Stats)
	}
	if codec(t, codecs(t, opts.Output), "romfs/little") != 0 {
		t.Errorf("a frame saving under 10%% should be stored raw")
	}
	assertSameContent(t, in, opts.Output, nil)
}

func TestOptimizeDryRun(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "kaidump.nup")
	writeKaiDump(t, in)
	before := fileSHA(t, in)
	opts := defaultOptions()
	opts.DryRun = true
	opts.InPlace = true
	res := optimizeT(t, in, opts)
	if res.Status != "planned" || res.Output != "" || res.Verified {
		t.Fatalf("result = %+v", res)
	}
	if res.SizeAfter != res.Plan.EstimatedSize || res.SavedBytes != res.Plan.EstimatedSavings {
		t.Errorf("dry run reports the estimate: %+v", res)
	}
	p := res.Plan
	if p.UniqueContents != 5 || p.DuplicateFiles != 1 || p.Compress != 5 || p.RedundantBytes == 0 || !p.Exact {
		t.Errorf("plan = %+v", p)
	}
	if fileSHA(t, in) != before {
		t.Errorf("dry run modified the archive")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dry run created files: %v", entries)
	}

	opts.DryRun = false
	opts.InPlace = false
	opts.Output = filepath.Join(dir, "out.nup")
	actual := optimizeT(t, in, opts)
	if actual.SizeAfter != p.EstimatedSize {
		t.Errorf("exact estimate %d, actual %d", p.EstimatedSize, actual.SizeAfter)
	}
	if actual.SizeAfter > p.WorstCaseSize {
		t.Errorf("output %d exceeds worst case %d", actual.SizeAfter, p.WorstCaseSize)
	}
}

// textStream generates deterministic, moderately compressible text.
type textStream struct {
	rng  *rand.Rand
	left int64
	line []byte
}

var words = strings.Fields("nintendo switch romfs exefs archive extent frame block texture model sound level " +
	"script shader actor stage event message font layout effect physics camera save")

func (s *textStream) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && s.left > 0 {
		if len(s.line) == 0 {
			var b []byte
			for i := 0; i < 8; i++ {
				b = append(b, words[s.rng.Intn(len(words))]...)
				b = append(b, ' ')
			}
			b = append(b, byte('0'+s.rng.Intn(10)), '\n')
			s.line = b
		}
		c := copy(p[n:], s.line)
		if int64(c) > s.left {
			c = int(s.left)
		}
		s.line = s.line[c:]
		n += c
		s.left -= int64(c)
	}
	return n, nil
}

// randomStream generates incompressible bytes, optionally with a run of
// zeros at [zeroAt, zeroAt+zeroLen).
type randomStream struct {
	rng             *rand.Rand
	left            int64
	zeroAt, zeroLen int64
	at              int64
}

func (s *randomStream) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), s.left))
	s.rng.Read(p[:n])
	for i := 0; i < n; i++ {
		if pos := s.at + int64(i); pos >= s.zeroAt && pos < s.zeroAt+s.zeroLen {
			p[i] = 0
		}
	}
	s.at += int64(n)
	s.left -= int64(n)
	return n, nil
}

// TestOptimizeEstimateIgnoresSamplingNoise: files that cannot be compressed
// must not show phantom savings. Each file hides 4 KiB of zeros across a
// 128 KiB zstd block boundary, so no block of the whole file saves enough to
// be compressed, while a sample window starting elsewhere holds the run in
// one block and compresses a little.
func TestOptimizeEstimateIgnoresSamplingNoise(t *testing.T) {
	if testing.Short() {
		t.Skip("large archive")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "incompressible.nup")
	var files []nuptest.File
	for i := 0; i < 200; i++ {
		files = append(files, nuptest.File{Path: fmt.Sprintf("romfs/media/%03d.bin", i), Size: 512 << 10,
			Source: &randomStream{rng: rand.New(rand.NewSource(int64(i))), left: 512 << 10, zeroAt: 254 << 10, zeroLen: 4 << 10}})
	}
	if err := nuptest.WriteKaiDump(in, []nuptest.Package{{TitleID: testTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.DryRun = true
	res := optimizeT(t, in, opts)
	if res.Plan.Exact || res.Plan.EstimatedSavings > res.Plan.ArchiveSize-res.Plan.EstimatedSize || res.Action != "skip" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Reason, "estimated savings 0.0%") || res.Plan.EstimatedSavings > 0 {
		t.Errorf("phantom savings: %d bytes, %q", res.Plan.EstimatedSavings, res.Reason)
	}
	opts.DryRun = false
	opts.Force = true
	opts.Output = filepath.Join(dir, "out.nup")
	if real := optimizeT(t, in, opts); real.Stats.Compressed != 0 {
		t.Errorf("fixture files should not compress: %+v", real.Stats)
	}
}

// TestOptimizeNormalizesDeviations: an archive the reference reader rejects
// is rewritten even when it would not shrink, and the rewrite is clean.
func TestOptimizeNormalizesDeviations(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "deviating.nup")
	noise := make([]byte, 300<<10)
	rand.New(rand.NewSource(5)).Read(noise)
	pkgs := []nuptest.Package{{TitleID: testTID, Kind: 0, Files: []nuptest.File{
		{Path: "romfs/a.bin", Data: noise},
		{Path: "romfs/empty"},
		{Path: "romfs/z.bin", Data: noise[:1000]},
	}}}
	if err := nuptest.WriteKaiDump(in, pkgs, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	a := openT(t, in)
	if d := a.Deviations(); len(d) != 1 || !strings.Contains(d[0], "1 empty file(s) have a nonzero extent offset") {
		t.Fatalf("Deviations = %q", d)
	}
	a.Close() // (Windows cannot replace a file open elsewhere)
	opts := defaultOptions()
	opts.InPlace = true
	opts.DryRun = true
	res := optimizeT(t, in, opts)
	if res.Action != "optimize" || !strings.Contains(res.Reason, "deviates from the NUP spec") || len(res.Plan.Deviations) != 1 {
		t.Fatalf("dry run = %+v", res)
	}
	opts.DryRun = false
	if res = optimizeT(t, in, opts); res.Status != "normalized" || !res.Verified {
		t.Fatalf("result = %+v", res)
	}
	b := openT(t, in)
	if d := b.Deviations(); len(d) != 0 {
		t.Errorf("rewrite still deviates: %q", d)
	}
	b.Close()
	if res = optimizeT(t, in, opts); res.Status != "skipped" {
		t.Errorf("normalized archive should be skipped: %+v", res)
	}
}

// TestDeviationsReportMetadataBeforeBlob places a metadata block in the gap
// between the TOC and the blob region, as some third-party packers do.
func TestDeviationsReportMetadataBeforeBlob(t *testing.T) {
	dir := t.TempDir()
	meta := nuptest.TLV(8, []byte("x"))
	for pad := 1; pad <= 16; pad++ {
		path := filepath.Join(dir, fmt.Sprintf("gap%d.nup", pad))
		pkgs := []nuptest.Package{{TitleID: testTID, Kind: 0, Meta: meta,
			Files: []nuptest.File{{Path: "romfs/a.bin", Data: []byte("data")}},
			Dirs:  []string{"romfs/" + strings.Repeat("p", pad)}}}
		if err := nuptest.WriteKaiDump(path, pkgs, nuptest.Options{}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tocEnd := 128 + int(binary.LittleEndian.Uint64(data[16:24]))
		blob := int(binary.LittleEndian.Uint64(data[56:64]))
		if blob-tocEnd < len(meta) {
			continue
		}
		copy(data[tocEnd:], meta)
		data = mutateTOC(t, data, func(toc []byte) {
			binary.LittleEndian.PutUint64(toc[48+32:], uint64(tocEnd))
		})
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		d := openT(t, path).Deviations()
		if len(d) != 1 || !strings.Contains(d[0], "lies before the blob region") {
			t.Fatalf("Deviations = %q", d)
		}
		opts := defaultOptions()
		opts.Output = filepath.Join(dir, "out.nup")
		optimizeT(t, path, opts)
		if d := openT(t, opts.Output).Deviations(); len(d) != 0 {
			t.Errorf("rewrite still deviates: %q", d)
		}
		return
	}
	t.Fatal("no layout left room for metadata before the blob region")
}

// TestOptimizeLargeArchiveBoundedMemory optimizes a 96 MiB file: the plan
// must fall back to sampling, and the heap must stay far below the file size
// because extents are streamed through fixed buffers.
func TestOptimizeLargeArchiveBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("large archive")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "large.nup")
	const size = 96 << 20
	pkgs := []nuptest.Package{{TitleID: testTID, Kind: 0, Files: []nuptest.File{
		{Path: "exefs/main.npdm", Data: buildNPDM(testTID)},
		{Path: "romfs/huge.txt", Size: size, Source: &textStream{rng: rand.New(rand.NewSource(1)), left: size}},
	}}}
	if err := nuptest.WriteKaiDump(in, pkgs, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var peak uint64
	calls := 0
	opts := defaultOptions()
	opts.Output = filepath.Join(dir, "out.nup")
	opts.Progress = func(string, float64) {
		calls++
		if calls%8 == 0 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapInuse)
		}
	}
	res := optimizeT(t, in, opts)
	if res.Plan.Exact || res.Plan.SampledBytes == 0 || res.Plan.SampledBytes > sampleBudget {
		t.Errorf("plan should be sampled: %+v", res.Plan)
	}
	estimateErr := float64(res.Plan.EstimatedSize-res.SizeAfter) / float64(res.SizeAfter)
	if estimateErr < -0.25 || estimateErr > 0.25 {
		t.Errorf("estimate %d vs actual %d (%.0f%%)", res.Plan.EstimatedSize, res.SizeAfter, 100*estimateErr)
	}
	if res.SizeAfter > res.SizeBefore/2 {
		t.Errorf("text should compress well: %d -> %d", res.SizeBefore, res.SizeAfter)
	}
	if peak == 0 || peak > 48<<20 {
		t.Errorf("peak heap %d MiB while optimizing a %d MiB file", peak>>20, size>>20)
	}
	t.Logf("peak heap %d MiB, %d -> %d bytes, estimate %d", peak>>20, res.SizeBefore, res.SizeAfter, res.Plan.EstimatedSize)
}

// TestOptimizeEstimateStratified: most small text files get no sample window
// of their own; they must be estimated from other .txt samples, not from an
// average dragged down by incompressible video.
func TestOptimizeEstimateStratified(t *testing.T) {
	if testing.Short() {
		t.Skip("large archive")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "mixed.nup")
	var files []nuptest.File
	for i := 0; i < 500; i++ {
		files = append(files, nuptest.File{Path: fmt.Sprintf("romfs/text/%03d.txt", i), Size: 60 << 10,
			Source: &textStream{rng: rand.New(rand.NewSource(int64(i))), left: 60 << 10}})
	}
	for i := 0; i < 2; i++ {
		files = append(files, nuptest.File{Path: fmt.Sprintf("romfs/movie/%d.bk2", i), Size: 40 << 20,
			Source: &randomStream{rng: rand.New(rand.NewSource(int64(100 + i))), left: 40 << 20}})
	}
	if err := nuptest.WriteKaiDump(in, []nuptest.Package{{TitleID: testTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.Output = filepath.Join(dir, "out.nup")
	res := optimizeT(t, in, opts)
	if res.Plan.Exact {
		t.Fatalf("plan should be sampled: %+v", res.Plan)
	}
	estimateErr := float64(res.Plan.EstimatedSize-res.SizeAfter) / float64(res.SizeAfter)
	if estimateErr < -0.1 || estimateErr > 0.1 {
		t.Errorf("estimate %d vs actual %d (%.1f%%)", res.Plan.EstimatedSize, res.SizeAfter, 100*estimateErr)
	}
}

// TestOptimizeProgressIsMonotonic: an incompressible file is read once for
// the compression attempt and again to store it raw; progress must not go
// back.
func TestOptimizeProgressIsMonotonic(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.nup")
	noise := make([]byte, 3<<20)
	rand.New(rand.NewSource(6)).Read(noise)
	pkgs := []nuptest.Package{{TitleID: testTID, Kind: 0, Files: []nuptest.File{{Path: "romfs/noise.bin", Data: noise}}}}
	if err := nuptest.WriteKaiDump(in, pkgs, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	var seen []float64
	opts := defaultOptions()
	opts.Force = true
	opts.Output = filepath.Join(dir, "out.nup")
	opts.Progress = func(label string, pct float64) {
		if label == "Optimizing NUP files" {
			seen = append(seen, pct)
		}
	}
	if res := optimizeT(t, in, opts); res.Stats.Incompressible != 1 {
		t.Fatalf("stats = %+v", res.Stats)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("progress went back from %.1f to %.1f", seen[i-1], seen[i])
		}
	}
}

// TestReplaceSourceRefusesAnotherFile: a different file with the same size
// and modification time at the source path must not be replaced.
func TestReplaceSourceRefusesAnotherFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "game.nup")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// (from the open file, as Open stamps it: on Windows os.Stat loads the
	// file ID lazily, from whatever is at the path when compared)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	stamp := archiveStamp{size: st.Size(), mtime: st.ModTime().UnixNano(), info: st}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("restored"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(dir, "game.nup.partial")
	if err := os.WriteFile(temp, []byte("rewrite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replaceSource(path, temp, stamp); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("replaceSource = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "restored" {
		t.Errorf("the other file was replaced: %q", got)
	}
}

// flipByte flips one byte of path at off.
func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

// extentOf returns the stored extent of the file at path in package 0.
func extentOf(t *testing.T, archive, path string) fileRecord {
	t.Helper()
	a := openT(t, archive)
	idx, ok := a.trees[0][path]
	if !ok {
		t.Fatalf("%s not in archive", path)
	}
	return a.files[idx]
}

func TestOptimizeCorruptRawExtentKeepsSource(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	writeKaiDump(t, in)
	rec := extentOf(t, in, "romfs/a/text.txt")
	flipByte(t, in, rec.off+rec.stored/2)
	before := fileSHA(t, in)

	opts := defaultOptions()
	opts.InPlace = true
	_, err := Optimize(context.Background(), in, opts)
	if err == nil || !strings.Contains(err.Error(), "File SHA-256 mismatch") {
		t.Fatalf("err = %v, want a SHA-256 mismatch", err)
	}
	if fileSHA(t, in) != before {
		t.Errorf("failed optimization modified the source")
	}
	assertNoPartials(t, dir)
}

// TestOptimizeCorruptDuplicateExtent: a damaged second copy is caught even
// though deduplication never writes it.
func TestOptimizeCorruptDuplicateExtent(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	writeKaiDump(t, in)
	rec := extentOf(t, in, "romfs/a/text.txt") // stored after copy.txt
	flipByte(t, in, rec.off+1)
	out := filepath.Join(dir, "out.nup")
	opts := defaultOptions()
	opts.Output = out
	if _, err := Optimize(context.Background(), in, opts); err == nil || !strings.Contains(err.Error(), "text.txt") {
		t.Fatalf("err = %v, want the damaged duplicate reported", err)
	}
	if pathExists(out) {
		t.Errorf("output committed from a damaged source")
	}
	assertNoPartials(t, dir)
}

// TestOptimizeCorruptReusedFrame: a damaged frame passes the structural
// checks and is copied verbatim; the output verification must reject it.
func TestOptimizeCorruptReusedFrame(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	text := bytes.Repeat([]byte("frame payload "), 3000)
	pkgs := []Package{{TitleID: testTID, Kind: 0, Files: map[string]*Source{
		"romfs/a.txt": SourceData(text),
		"romfs/b.txt": SourceData([]byte(strings.Repeat("raw-ish", 3))),
	}}}
	in := filepath.Join(dir, "game.nup")
	if _, err := Pack(ctx, pkgs, in, PackOptions{Level: 3, LargeRaw: true}); err != nil {
		t.Fatal(err)
	}
	rec := extentOf(t, in, "romfs/a.txt")
	head := make([]byte, 18)
	f, _ := os.Open(in)
	f.ReadAt(head, rec.off)
	f.Close()
	params, err := parseZstdFrameHead(head)
	if err != nil {
		t.Fatal(err)
	}
	payload := rec.off + int64(params.headerSize) + 3
	flipByte(t, in, payload+(rec.off+rec.stored-payload)/2)
	check, err := Open(ctx, in)
	if err != nil {
		t.Fatalf("corruption should keep the frame structurally valid: %v", err)
	}
	check.Close()
	before := fileSHA(t, in)
	opts := defaultOptions()
	opts.InPlace = true
	opts.Force = true
	_, err = Optimize(ctx, in, opts)
	detected := false
	for _, s := range []string{"SHA-256 mismatch", "CRC check failed", "zstd", "corrupt"} {
		detected = detected || err != nil && strings.Contains(err.Error(), s)
	}
	if !detected {
		t.Fatalf("damaged frame: err = %v, want a checksum or decoder failure", err)
	}
	if fileSHA(t, in) != before {
		t.Errorf("failed optimization modified the source")
	}
	assertNoPartials(t, dir)
}

func TestOptimizeCancelKeepsSource(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	writeKaiDump(t, in)
	before := fileSHA(t, in)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := defaultOptions()
	opts.InPlace = true
	opts.Progress = func(label string, pct float64) {
		if label == "Optimizing NUP files" && pct > 10 {
			cancel()
		}
	}
	_, err := Optimize(ctx, in, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fileSHA(t, in) != before {
		t.Errorf("cancelled optimization modified the source")
	}
	assertNoPartials(t, dir)
}

func TestOptimizeSourceChangedKeepsSource(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	writeKaiDump(t, in)
	before := fileSHA(t, in)
	opts := defaultOptions()
	opts.InPlace = true
	touched := false
	opts.Progress = func(label string, pct float64) {
		if label == "Optimizing NUP files" && !touched {
			touched = true
			st, _ := os.Stat(in)
			later := st.ModTime().Add(1e9)
			os.Chtimes(in, later, later)
		}
	}
	_, err := Optimize(context.Background(), in, opts)
	if err == nil || !strings.Contains(err.Error(), "NUP changed") {
		t.Fatalf("err = %v, want a changed-source error", err)
	}
	if fileSHA(t, in) != before {
		t.Errorf("source content changed")
	}
	assertNoPartials(t, dir)
}

func TestOptimizeRefusals(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	writeKaiDump(t, in)
	existing := filepath.Join(dir, "existing.nup")
	if err := os.WriteFile(existing, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]OptimizeOptions{
		"Output already exists": {Output: existing},
		"Output is the input":   {Output: in},
		"Output must end in":    {Output: filepath.Join(dir, "out.bin")},
		"Choose an output":      {},
		"compression level":     {Output: filepath.Join(dir, "x.nup"), RepackOptions: RepackOptions{Level: 2}},
		"Minimum gain":          {Output: filepath.Join(dir, "x.nup"), MinGain: 100},
	}
	for want, opts := range cases {
		opts.Force = true
		if _, err := Optimize(context.Background(), in, opts); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	if got, _ := os.ReadFile(existing); string(got) != "keep" {
		t.Errorf("existing output was modified")
	}

	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link.nup")
		if err := os.Symlink(in, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Optimize(context.Background(), link, OptimizeOptions{InPlace: true, Force: true}); err == nil || !strings.Contains(err.Error(), "Linked") {
			t.Errorf("symlinked input: err = %v", err)
		}
	}

	ext := filepath.Join(dir, "ext.nup")
	pkgs, _ := kaiDumpPackages()
	if err := nuptest.WriteKaiDump(ext, pkgs, nuptest.Options{HeaderSize: 0x90, HeaderVersion: 2}); err != nil {
		t.Fatal(err)
	}
	openT(t, ext)
	if _, err := Optimize(context.Background(), ext, OptimizeOptions{InPlace: true, Force: true}); err == nil || !strings.Contains(err.Error(), "header extension") {
		t.Errorf("header extension: err = %v", err)
	}
	assertNoPartials(t, dir)
}

// TestOptimizeTransform: an opt-in transform is verified against the
// intended package set instead of the source.
func TestOptimizeTransform(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	writeKaiDump(t, in)
	opts := defaultOptions()
	opts.Output = filepath.Join(dir, "base-only.nup")
	opts.Transform = func(_ context.Context, pkgs []Package) ([]Package, error) {
		return pkgs[:1], nil
	}
	res := optimizeT(t, in, opts)
	if !res.ContentChanged || res.OutputLogicalSHA256 == res.LogicalSHA256 || res.Status != "optimized" {
		t.Fatalf("result = %+v", res)
	}
	b := openT(t, opts.Output)
	if len(b.Packages()) != 1 {
		t.Errorf("packages = %d, want 1", len(b.Packages()))
	}
	want := openT(t, in).Contents()[:1]
	if err := CompareContents(want, b.Contents()); err != nil {
		t.Errorf("transformed content: %v", err)
	}
}

func TestOptimizeKeepsUnusualRecords(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "kaidump.nup")
	writeKaiDump(t, in)
	data, _ := os.ReadFile(in)
	layout := parseLayout(t, data)
	mutated := mutateTOC(t, data, func(toc []byte) {
		for i := 0; i < layout.nd; i++ {
			if toc[layout.dirRec(i)+4] == 0 { // a root: name it
				copy(toc[layout.dirRec(i):], toc[layout.dirRec(1):layout.dirRec(1)+8])
				break
			}
		}
		for b := 0; b < 4; b++ {
			toc[layout.pkgOff+12+b] = 0xff // leave exefs_dir unset
		}
	})
	odd := filepath.Join(dir, "odd.nup")
	if err := os.WriteFile(odd, mutated, 0o644); err != nil {
		t.Fatal(err)
	}
	c := openT(t, odd).Contents()
	if c[0].Root == "" || c[0].ExeFS {
		t.Fatalf("fixture: root %q exefs %v", c[0].Root, c[0].ExeFS)
	}
	opts := defaultOptions()
	opts.Output = filepath.Join(dir, "out.nup")
	res := optimizeT(t, odd, opts)
	if res.LogicalSHA256 != res.OutputLogicalSHA256 {
		t.Errorf("root name or pointer lost")
	}
	assertSameContent(t, odd, opts.Output, nil)
}

func TestOptimizeRelocatedMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packages, expected := buildSynthetic(t, dir)
	in := filepath.Join(dir, "relocated.nup")
	if _, err := Pack(ctx, packages, in, PackOptions{Level: 3, LargeRaw: true}); err != nil {
		t.Fatal(err)
	}
	if !relocateMetadataToTocGap(t, in) {
		t.Fatal("no metadata relocated")
	}
	opts := defaultOptions()
	opts.Output = filepath.Join(dir, "out.nup")
	if res := optimizeT(t, in, opts); res.Status != "skipped" || res.Plan.EstimatedSavings <= 0 {
		t.Fatalf("only the stale metadata copy is reclaimable: %+v", res)
	}
	opts.Force = true
	res := optimizeT(t, in, opts)
	if res.Status != "optimized" || res.SizeAfter >= res.SizeBefore || res.SizeAfter != res.Plan.EstimatedSize {
		t.Fatalf("result = %+v", res)
	}
	assertSameContent(t, in, opts.Output, expected)
}
