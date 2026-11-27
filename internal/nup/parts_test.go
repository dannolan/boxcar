package nup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/nup/nuptest"
)

func touch(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, n)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func baseNames(paths []string) string {
	var out []string
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return strings.Join(out, " ")
}

// TestFindParts follows nx2-flow's nupparts.h: every naming way, numbering
// from 0 or 1 with none missing, at least two parts, later parts, and names
// that only look like parts.
func TestFindParts(t *testing.T) {
	cases := []struct {
		files []string
		open  string
		want  string // parts, "" for an error
		name  string
	}{
		{[]string{"G.nup.001", "G.nup.002", "G.nup.003"}, "G.nup.001", "G.nup.001 G.nup.002 G.nup.003", "G.nup"},
		{[]string{"G.nup.00", "G.nup.01"}, "G.nup.00", "G.nup.00 G.nup.01", "G.nup"},
		{[]string{"G.nup.part1", "G.nup.part2"}, "G.nup.part1", "G.nup.part1 G.nup.part2", "G.nup"},
		{[]string{"G.nup.aa", "G.nup.ab", "G.nup.ac"}, "G.nup.aa", "G.nup.aa G.nup.ab G.nup.ac", "G.nup"},
		{[]string{"G.nu0", "G.nu1"}, "G.nu0", "G.nu0 G.nu1", "G.nup"},
		{[]string{"G.part1.nup", "G.part2.nup"}, "G.part1.nup", "G.part1.nup G.part2.nup", "G.nup"},
		{[]string{"G.001.nup", "G.002.nup"}, "G.001.nup", "G.001.nup G.002.nup", "G.nup"},
		{[]string{"g/00", "g/01", "g/notes.txt"}, "g", "00 01", "g.nup"},
		{[]string{"g/00", "g/01"}, "g/00", "00 01", "g.nup"},
		{[]string{"G.nup/00"}, "G.nup", "00", "G.nup"}, // a folder of nameless parts may hold one
		{[]string{"g/G.nup.001", "g/G.nup.002"}, "g", "G.nup.001 G.nup.002", "G.nup"},
		// alone: a whole file of its own
		{[]string{"G.nup.001"}, "G.nup.001", "G.nup.001", "G.nup.001"},
		{[]string{"G.nup.001", "G.nup.003"}, "G.nup.001", "G.nup.001", "G.nup.001"},
		{[]string{"G.nup.002", "G.nup.003"}, "G.nup.002", "G.nup.002", "G.nup.002"},    // must start at 0 or 1
		{[]string{"G.nup.1", "G.nup.001", "G.nup.2"}, "G.nup.1", "G.nup.1", "G.nup.1"}, // two files for one number
		{[]string{"Plain.nup"}, "Plain.nup", "Plain.nup", "Plain.nup"},
		// errors
		{[]string{"G.nup.001", "G.nup.002"}, "G.nup.002", "", ""}, // a later part
		{[]string{"g/G.nup.001", "g/G.nup.002", "g/Other.nup"}, "g", "", ""},
		{[]string{"g/02", "g/03"}, "g", "", ""},
		{[]string{"g/01", "g/03"}, "g", "", ""},
	}
	for i, c := range cases {
		dir := t.TempDir()
		touch(t, dir, c.files...)
		parts, name, err := FindParts(filepath.Join(dir, c.open))
		if c.want == "" {
			if err == nil {
				t.Errorf("%d %v: want an error, got %s", i, c.files, baseNames(parts))
			}
			continue
		}
		if err != nil {
			t.Errorf("%d %v: %v", i, c.files, err)
			continue
		}
		if got := baseNames(parts); got != c.want || name != c.name {
			t.Errorf("%d %v: got %q (%s), want %q (%s)", i, c.files, got, name, c.want, c.name)
		}
	}
	dir := t.TempDir()
	touch(t, dir, "G.nup.001", "G.nup.002")
	if _, _, err := FindParts(filepath.Join(dir, "G.nup.002")); !errors.Is(err, ErrLaterPart) {
		t.Errorf("later part: %v", err)
	}
}

// cut writes data as parts named by name(k), of the given sizes (the last
// takes the rest).
func cut(t *testing.T, data []byte, sizes []int, name func(k int) string) {
	t.Helper()
	for k := range sizes {
		start := 0
		for _, s := range sizes[:k] {
			start += s
		}
		end := start + sizes[k]
		if k == len(sizes)-1 {
			end = len(data)
		}
		p := name(k)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data[start:end], 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// splitFixture writes a KaiDump-style archive of about 300 KB that does
// not compress, so its optimized form still needs many small parts.
func splitFixture(t *testing.T, dir string) (string, map[string][]byte) {
	t.Helper()
	in := filepath.Join(dir, "whole.nup")
	expected := map[string][]byte{}
	var files []nuptest.File
	for i, size := range []int{150000, 90000, 60000, 0, 5000} {
		data := make([]byte, size)
		if _, err := io.ReadFull(nuptest.Content(int64(i), int64(size), 1), data); err != nil {
			t.Fatal(err)
		}
		p := fmt.Sprintf("romfs/f%d.bin", i)
		files = append(files, nuptest.File{Path: p, Data: data})
		expected["base/"+p] = data
	}
	files = append(files, nuptest.File{Path: "romfs/text.txt", Data: compressibleText(40000)})
	expected["base/romfs/text.txt"] = compressibleText(40000)
	if err := nuptest.WriteKaiDump(in, []nuptest.Package{{TitleID: testTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	return in, expected
}

// TestOpenArchiveInParts reads an archive cut by several tools at odd
// sizes (parts that split the header, the table and extents) exactly like
// the whole file.
func TestOpenArchiveInParts(t *testing.T) {
	dir := t.TempDir()
	in, expected := splitFixture(t, dir)
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	whole := openT(t, in)
	defer whole.Close()
	sizes := []int{100, 1000, 4097, 70000}
	layouts := map[string]func(k int) string{
		"G.nup.001":   func(k int) string { return filepath.Join(dir, "a", "G.nup."+pad3(k+1)) },
		"G.nup.aa":    func(k int) string { return filepath.Join(dir, "b", "G.nup.a"+string(rune('a'+k))) },
		"G.nu0":       func(k int) string { return filepath.Join(dir, "c", "G.nu"+string(rune('0'+k))) },
		"G.part1.nup": func(k int) string { return filepath.Join(dir, "d", "G.part"+string(rune('1'+k))+".nup") },
		"":            func(k int) string { return filepath.Join(dir, "e", "Game.nup", "0"+string(rune('0'+k))) },
	}
	for first, name := range layouts {
		cut(t, data, sizes, name)
		open := filepath.Dir(name(0))
		if first != "" {
			open = name(0)
		}
		a, err := Open(ctxT(), open)
		if err != nil {
			t.Fatalf("%s: %v", first, err)
		}
		if len(a.Parts()) != len(sizes) {
			t.Errorf("%s: %d parts", first, len(a.Parts()))
		}
		if err := CompareContents(whole.Contents(), a.Contents()); err != nil {
			t.Errorf("%s: %v", first, err)
		}
		if _, err := a.Verify(ctxT(), nil); err != nil {
			t.Errorf("%s: Verify: %v", first, err)
		}
		a.Close()
		x := filepath.Join(t.TempDir(), "x")
		if _, err := Extract(ctxT(), open, x, nil, nil); err != nil {
			t.Fatalf("%s: Extract: %v", first, err)
		}
		for rel, want := range expected {
			got, err := os.ReadFile(filepath.Join(x, filepath.FromSlash(rel)))
			if err != nil || string(got) != string(want) {
				t.Errorf("%s: %s differs (%v)", first, rel, err)
			}
		}
	}
}

func pad3(n int) string {
	s := "00" + string(rune('0'+n%10))
	if n >= 10 {
		s = "0" + string(rune('0'+n/10)) + string(rune('0'+n%10))
	}
	return s
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestOptimizeInParts: an archive in parts is optimized to an archive in
// parts named like nx2-flow's receiver names them, all but the last of the
// part size, readable from its first part; output that fits one part keeps
// the plain name; nothing is left behind and in-place is refused.
func TestOptimizeInParts(t *testing.T) {
	dir := t.TempDir()
	in, expected := splitFixture(t, dir)
	data, _ := os.ReadFile(in)
	src := filepath.Join(dir, "src")
	cut(t, data, []int{120000, 120000}, func(k int) string { return filepath.Join(src, "G.nup."+pad3(k+1)) })
	first := filepath.Join(src, "G.nup.001")

	opts := OptimizeOptions{RepackOptions: RepackOptions{Level: 3}, SplitSize: 65536}
	opts.Output = filepath.Join(dir, "out", "G.nup")
	res := optimizeT(t, first, opts)
	if res.Status != "optimized" || len(res.Parts) < 2 || res.Output != res.Parts[0] {
		t.Fatalf("result = %+v", res)
	}
	files := listDir(t, filepath.Join(dir, "out"))
	if len(files) != len(res.Parts) {
		t.Errorf("output folder holds %v, want only the parts %v", files, res.Parts)
	}
	var total int64
	for k, p := range res.Parts {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(p) != "G.nup."+pad3(k+1) {
			t.Errorf("part %d is named %s", k, filepath.Base(p))
		}
		if k < len(res.Parts)-1 && st.Size() != 65536 || k == len(res.Parts)-1 && (st.Size() == 0 || st.Size() > 65536) {
			t.Errorf("part %d has %d bytes", k, st.Size())
		}
		total += st.Size()
	}
	if total != res.SizeAfter {
		t.Errorf("parts hold %d bytes, result says %d", total, res.SizeAfter)
	}
	assertSameContent(t, first, res.Output, expected)

	// The output name, or a part-like neighbour, already exists.
	if _, err := Optimize(ctxT(), first, opts); err == nil {
		t.Error("optimizing onto existing parts succeeded")
	}
	touch(t, filepath.Join(dir, "out2"), "G.nup.004")
	opts.Output = filepath.Join(dir, "out2", "G.nup")
	if _, err := Optimize(ctxT(), first, opts); err == nil || !strings.Contains(err.Error(), "G.nup.004") {
		t.Errorf("a stray part beside the output: %v", err)
	}
	if got := listDir(t, filepath.Join(dir, "out2")); len(got) != 1 {
		t.Errorf("out2 holds %v", got)
	}

	// Fits one part: the plain name.
	opts.Output = filepath.Join(dir, "out3", "G.nup")
	opts.SplitSize = 1 << 30
	res = optimizeT(t, first, opts)
	if res.Output != opts.Output || res.Parts != nil {
		t.Errorf("one part: %+v", res)
	}
	if got := listDir(t, filepath.Join(dir, "out3")); len(got) != 1 || got[0] != "G.nup" {
		t.Errorf("out3 holds %v", got)
	}

	// Too many parts: refused, nothing left.
	opts.Output = filepath.Join(dir, "out4", "G.nup")
	opts.SplitSize = MinPartSize
	if res.SizeAfter/MinPartSize < MaxParts {
		t.Skip("fixture too small for the part limit")
	}
	if _, err := Optimize(ctxT(), first, opts); err == nil || !strings.Contains(err.Error(), "parts") {
		t.Errorf("too many parts: %v", err)
	}
	if got := listDir(t, filepath.Join(dir, "out4")); len(got) != 0 {
		t.Errorf("out4 holds %v", got)
	}
}

func TestOptimizeInPartsRefusesInPlace(t *testing.T) {
	dir := t.TempDir()
	in, _ := splitFixture(t, dir)
	data, _ := os.ReadFile(in)
	cut(t, data, []int{50000, 50000}, func(k int) string { return filepath.Join(dir, "G.nup."+pad3(k+1)) })
	opts := OptimizeOptions{RepackOptions: RepackOptions{Level: 3}, InPlace: true}
	if _, err := Optimize(ctxT(), filepath.Join(dir, "G.nup.001"), opts); err == nil || !strings.Contains(err.Error(), "in place") {
		t.Errorf("in-place on parts: %v", err)
	}
}

// TestPartsWriterTruncate covers the raw-fallback rewind across a part
// boundary: later parts are removed, the kept part is cut.
func TestPartsWriterTruncate(t *testing.T) {
	dir := t.TempDir()
	w, err := newPartWriter(filepath.Join(dir, "x.nup"), "t", MinPartSize, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3*MinPartSize+10)
	if _, err := w.Write(buf); err != nil {
		t.Fatal(err)
	}
	if len(w.temps()) != 4 || w.written() != int64(len(buf)) {
		t.Fatalf("%d parts, %d bytes", len(w.temps()), w.written())
	}
	if err := w.Truncate(MinPartSize + 5); err != nil {
		t.Fatal(err)
	}
	if len(w.temps()) != 2 {
		t.Fatalf("%d parts after truncate", len(w.temps()))
	}
	if st, _ := os.Stat(w.temps()[1]); st.Size() != 5 {
		t.Errorf("second part has %d bytes", st.Size())
	}
	w.Close()
	w.removeTemps()
	if got := listDir(t, dir); len(got) != 0 {
		t.Errorf("left %v", got)
	}
}

func ctxT() context.Context { return context.Background() }
