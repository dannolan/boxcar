package nuptools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/dannolan/boxcar/internal/nup"
	"github.com/dannolan/boxcar/internal/nup/nuptest"
)

const libTID = 0x0100000000010000

func kaiDumpGame(title string) []nuptest.Package {
	text := bytes.Repeat([]byte(title+" says hello to the optimizer\n"), 3000)
	return []nuptest.Package{
		{TitleID: libTID, Kind: 0, Version: 0x10000,
			Files: []nuptest.File{
				{Path: "exefs/main.npdm", Data: buildNPDM(libTID)},
				{Path: "exefs/main", Data: []byte("main-binary")},
				{Path: "romfs/text.txt", Data: text},
				{Path: "romfs/copy.txt", Data: text},
				{Path: "romfs/empty"},
			},
			Meta: nuptest.TLV(5, []byte(title))},
		{TitleID: libTID | 0x1001, Kind: 2, Files: []nuptest.File{{Path: "romfs/dlc.txt", Data: text[:5000]}}},
	}
}

func digestOf(t *testing.T, path string) string {
	t.Helper()
	a, err := nup.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer a.Close()
	if _, err := a.Verify(context.Background(), nil); err != nil {
		t.Fatalf("Verify(%s): %v", path, err)
	}
	return nup.LogicalDigest(a.Contents())
}

func shaOf(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// library builds:
//
//	lib/a.nup              KaiDump-style, compressible
//	lib/b.nup              packed by Boxcar (already optimized)
//	lib/broken.nup         not a NUP
//	lib/notes.txt          ignored
//	lib/sub/c.nup          KaiDump-style, compressible
//	lib/d.nup.001, .002    KaiDump-style, compressible, in two parts
//	lib/sub/split.nup/00   KaiDump-style, in nameless parts (00, 01)
func library(t *testing.T) string {
	t.Helper()
	lib := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(filepath.Join(lib, "sub", "split.nup"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, title := range map[string]string{"a.nup": "Game A", "sub/c.nup": "Game C"} {
		if err := nuptest.WriteKaiDump(filepath.Join(lib, filepath.FromSlash(name)), kaiDumpGame(title), nuptest.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	text := bytes.Repeat([]byte("packed by boxcar\n"), 2000)
	b := []nup.Package{{TitleID: libTID, Kind: 0, Files: map[string]*nup.Source{
		"romfs/b.txt": nup.SourceData(text), "romfs/b2.txt": nup.SourceData(text),
	}}}
	if _, err := nup.Pack(context.Background(), b, filepath.Join(lib, "b.nup"), nup.PackOptions{Level: 3, LargeRaw: true}); err != nil {
		t.Fatal(err)
	}
	for name, title := range map[string]string{"d.nup": "Game D", "sub/split.nup/00": "Game S"} {
		whole := filepath.Join(t.TempDir(), "whole.nup")
		if err := nuptest.WriteKaiDump(whole, kaiDumpGame(title), nuptest.Options{}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(whole)
		if err != nil {
			t.Fatal(err)
		}
		first, second := name+".001", name+".002"
		if strings.HasSuffix(name, "/00") {
			first, second = name, strings.TrimSuffix(name, "00")+"01"
		}
		for part, bytes := range map[string][]byte{first: data[:len(data)/2], second: data[len(data)/2:]} {
			if err := os.WriteFile(filepath.Join(lib, filepath.FromSlash(part)), bytes, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, data := range map[string]string{"broken.nup": "not a nup", "notes.txt": "hi"} {
		if err := os.WriteFile(filepath.Join(lib, filepath.FromSlash(name)), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return lib
}

func byName(r BatchResult) map[string]ArchiveResult {
	out := map[string]ArchiveResult{}
	for _, a := range r.Archives {
		out[filepath.Base(a.Input)] = a
	}
	return out
}

func batchOptions() OptimizeOptions {
	return OptimizeOptions{RepackOptions: nup.RepackOptions{Level: 3, LargeRaw: true}, MinGain: 1}
}

func TestOptimizeBatchDryRun(t *testing.T) {
	lib := library(t)
	before := shaOf(t, filepath.Join(lib, "a.nup"))
	opts := batchOptions()
	opts.DryRun = true
	res, err := Optimize(context.Background(), []string{lib}, opts)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(res)
	if len(got) != 4 || got["a.nup"].Status != "planned" || got["b.nup"].Status != "planned" || got["broken.nup"].Status != "failed" ||
		got["d.nup.001"].Status != "planned" || got["d.nup.001"].SavedBytes <= 0 {
		t.Fatalf("non-recursive dry run: %+v", got)
	}
	if !strings.Contains(got["b.nup"].Reason, "already optimized") || got["a.nup"].SavedBytes <= 0 {
		t.Errorf("verdicts: a=%q b=%q", got["a.nup"].Reason, got["b.nup"].Reason)
	}
	if s := res.Summary; s.Archives != 4 || s.Planned != 3 || s.Failed != 1 || s.SavedBytes <= 0 {
		t.Errorf("summary = %+v", s)
	}

	opts.Recursive = true
	res, err = Optimize(context.Background(), []string{lib}, opts)
	if err != nil {
		t.Fatal(err)
	}
	got = byName(res)
	if len(got) != 6 || got["c.nup"].Status != "planned" || got["split.nup"].Status != "planned" {
		t.Fatalf("recursive dry run: %+v", got)
	}
	if shaOf(t, filepath.Join(lib, "a.nup")) != before {
		t.Errorf("dry run modified an archive")
	}
}

func TestOptimizeBatchInPlace(t *testing.T) {
	lib := library(t)
	paths := map[string]string{"a.nup": filepath.Join(lib, "a.nup"), "c.nup": filepath.Join(lib, "sub", "c.nup")}
	digests := map[string]string{}
	for name, p := range paths {
		digests[name] = digestOf(t, p)
	}
	bBefore := shaOf(t, filepath.Join(lib, "b.nup"))

	var mu sync.Mutex
	var overall []float64
	seen := map[int]bool{}
	finished := map[int]bool{}
	opts := batchOptions()
	opts.Recursive = true
	opts.InPlace = true
	opts.Jobs = 3
	opts.Progress = func(p BatchProgress) {
		mu.Lock()
		defer mu.Unlock()
		overall = append(overall, p.Overall)
		seen[p.Index] = true
		if p.Count != 6 || p.Percent < 0 || p.Percent > 100 {
			t.Errorf("progress = %+v", p)
		}
	}
	opts.Finished = func(index, count int, r ArchiveResult) {
		mu.Lock()
		defer mu.Unlock()
		if finished[index] {
			t.Errorf("archive %d finished twice", index)
		}
		finished[index] = true
	}
	res, err := Optimize(context.Background(), []string{lib, filepath.Join(lib, "a.nup")}, opts)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(res)
	if got["a.nup"].Status != "optimized" || got["c.nup"].Status != "optimized" || got["b.nup"].Status != "skipped" ||
		got["broken.nup"].Status != "failed" || got["split.nup"].Status != "failed" || got["d.nup.001"].Status != "failed" ||
		!strings.Contains(got["split.nup"].Error, "in place") {
		t.Fatalf("statuses: %+v", got)
	}
	s := res.Summary
	if s.Archives != 6 || s.Optimized != 2 || s.Skipped != 1 || s.Failed != 3 || s.SavedBytes <= 0 ||
		s.SavedBytes != got["a.nup"].SavedBytes+got["c.nup"].SavedBytes {
		t.Errorf("summary = %+v", s)
	}
	for name, p := range paths {
		if d := digestOf(t, p); d != digests[name] || got[name].OutputLogicalSHA256 != d {
			t.Errorf("%s: logical digest changed", name)
		}
	}
	if shaOf(t, filepath.Join(lib, "b.nup")) != bBefore {
		t.Errorf("skipped archive was rewritten")
	}
	if len(finished) != 6 || len(seen) < 3 {
		t.Errorf("finished %v, progress seen for %v", finished, seen)
	}
	if !sort.Float64sAreSorted(overall) || overall[len(overall)-1] > 100 {
		t.Errorf("overall progress not monotonic: %v", overall)
	}
	matches, _ := filepath.Glob(filepath.Join(lib, "*.partial"))
	sub, _ := filepath.Glob(filepath.Join(lib, "sub", "*.partial"))
	if len(matches)+len(sub) != 0 {
		t.Errorf("leftover partial files: %v %v", matches, sub)
	}
}

func TestOptimizeBatchOutputs(t *testing.T) {
	lib := library(t)
	out := filepath.Join(t.TempDir(), "optimized")
	opts := batchOptions()
	opts.Recursive = true
	opts.Output = out
	res, err := Optimize(context.Background(), []string{lib}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Optimized != 4 {
		t.Fatalf("summary = %+v", res.Summary)
	}
	// Archives in parts are written whole (they fit one part), named after
	// the whole file.
	for rel, src := range map[string]string{"a.nup": "a.nup", "sub/c.nup": "sub/c.nup", "d.nup": "d.nup.001", "sub/split.nup": "sub/split.nup"} {
		p := filepath.Join(out, filepath.FromSlash(rel))
		if digestOf(t, p) != digestOf(t, filepath.Join(lib, filepath.FromSlash(src))) {
			t.Errorf("%s: content differs", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "b.nup")); err == nil {
		t.Errorf("skipped archive was written")
	}

	single := filepath.Join(t.TempDir(), "single.nup")
	opts.Output = single
	if _, err := Optimize(context.Background(), []string{lib}, opts); err == nil || !strings.Contains(err.Error(), "choose a folder") {
		t.Errorf("several archives into one file: err = %v", err)
	}
	res, err = Optimize(context.Background(), []string{filepath.Join(lib, "a.nup")}, opts)
	if err != nil || res.Archives[0].Status != "optimized" || res.Archives[0].Output != single {
		t.Fatalf("single file: %+v, %v", res, err)
	}
}

// TestOptimizeBatchSkipsOwnOutputFolder: an output folder inside the
// searched folder is not searched, so earlier outputs never become inputs.
func TestOptimizeBatchSkipsOwnOutputFolder(t *testing.T) {
	lib := library(t)
	opts := batchOptions()
	opts.Recursive = true
	opts.Output = filepath.Join(lib, "out")
	if _, err := Optimize(context.Background(), []string{lib}, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(lib, "out", "sub", "c.nup")); err != nil {
		t.Fatalf("first run did not write into out/: %v", err)
	}
	for _, dryRun := range []bool{true, false} {
		opts.DryRun = dryRun
		res, err := Optimize(context.Background(), []string{lib}, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range res.Archives {
			if strings.Contains(a.Input, string(filepath.Separator)+"out"+string(filepath.Separator)) {
				t.Errorf("dry run %v picked up an output: %s", dryRun, a.Input)
			}
		}
		if res.Summary.Archives != 6 {
			t.Errorf("dry run %v: %d archives, want the 6 originals", dryRun, res.Summary.Archives)
		}
	}
}

// TestOptimizeBatchFailedTargetReservesNothing: a missing input must not
// block a valid one that maps to the same output name.
func TestOptimizeBatchFailedTargetReservesNothing(t *testing.T) {
	lib := library(t)
	valid := filepath.Join(t.TempDir(), "valid")
	if err := os.MkdirAll(valid, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(lib, "a.nup"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(valid, "game.nup"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := batchOptions()
	opts.Output = filepath.Join(t.TempDir(), "out")
	res, err := Optimize(context.Background(), []string{filepath.Join(lib, "missing", "game.nup"), filepath.Join(valid, "game.nup")}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Archives[0].Status != "failed" || res.Archives[1].Status != "optimized" {
		t.Fatalf("results = %+v", res.Archives)
	}
}

func TestOptimizeBatchArguments(t *testing.T) {
	lib := library(t)
	empty := t.TempDir()
	cases := map[string]struct {
		inputs []string
		opts   OptimizeOptions
	}{
		"at least one": {nil, OptimizeOptions{DryRun: true}},
		"No .nup":      {[]string{empty}, OptimizeOptions{DryRun: true}},
		"--in-place":   {[]string{lib}, OptimizeOptions{}},
	}
	for want, c := range cases {
		if _, err := Optimize(context.Background(), c.inputs, c.opts); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	res, err := Optimize(context.Background(), []string{filepath.Join(lib, "missing.nup")}, OptimizeOptions{DryRun: true})
	if err != nil || res.Archives[0].Status != "failed" {
		t.Errorf("missing file: %+v, %v", res, err)
	}
}

func TestOptimizeBatchCancelled(t *testing.T) {
	lib := library(t)
	before := shaOf(t, filepath.Join(lib, "a.nup"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := batchOptions()
	opts.InPlace = true
	res, err := Optimize(ctx, []string{lib}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Failed != res.Summary.Archives {
		t.Errorf("summary = %+v", res.Summary)
	}
	if shaOf(t, filepath.Join(lib, "a.nup")) != before {
		t.Errorf("cancelled run modified an archive")
	}
}

// TestOptimizeBatchLatestSnapshot: consolidation is opt-in and verified
// against the merged package set.
func TestOptimizeBatchLatestSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.nup")
	base := nup.Package{TitleID: libTID, Kind: 0, Files: map[string]*nup.Source{
		"exefs/main.npdm": nup.SourceData(buildNPDM(libTID)),
		"exefs/main":      nup.SourceData([]byte("base-main")),
		"romfs/a.bin":     nup.SourceData([]byte("base-a")),
		"romfs/b.bin":     nup.SourceData([]byte("base-b")),
	}}
	update := nup.Package{TitleID: libTID | 0x800, Version: 65536, Kind: 1, Files: map[string]*nup.Source{
		"exefs/main.npdm": nup.SourceData(buildNPDM(libTID)),
		"exefs/main":      nup.SourceData([]byte("update-main")),
		"romfs/b.bin":     nup.SourceData([]byte("update-b")),
	}}
	if _, err := nup.Pack(context.Background(), []nup.Package{base, update}, path, nup.PackOptions{Level: 3, LargeRaw: true}); err != nil {
		t.Fatal(err)
	}
	opts := batchOptions()
	opts.Output = filepath.Join(dir, "kept.nup")
	res, err := Optimize(context.Background(), []string{path}, opts)
	if err != nil || res.Archives[0].Status != "skipped" || res.Archives[0].ContentChanged {
		t.Fatalf("without --latest-snapshot: %+v, %v", res.Archives, err)
	}

	opts.LatestSnapshot = true
	opts.Output = filepath.Join(dir, "latest.nup")
	res, err = Optimize(context.Background(), []string{path}, opts)
	if err != nil || res.Archives[0].Status != "optimized" || !res.Archives[0].ContentChanged {
		t.Fatalf("with --latest-snapshot: %+v, %v", res.Archives, err)
	}
	a, err := nup.Open(context.Background(), opts.Output)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	pkgs := a.Packages()
	if len(pkgs) != 1 || pkgs[0].Version != 65536 {
		t.Fatalf("packages = %+v", pkgs)
	}
	if got := readSource(t, pkgs[0].Files["romfs/b.bin"]); string(got) != "update-b" {
		t.Errorf("romfs/b.bin = %q", got)
	}
	if got := readSource(t, pkgs[0].Files["romfs/a.bin"]); string(got) != "base-a" {
		t.Errorf("romfs/a.bin = %q", got)
	}
}
