package romfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/region"
	"github.com/dannolan/boxcar/internal/stream"
)

// writeTree creates a fixed fixture under root and returns the expected
// romfs-internal path -> content map. Paths use "/" separators.
func writeTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		"a.txt":            []byte("hello world"),
		"héllo wörld.txt":  []byte("unicode content ✓"),
		"sub/b.bin":        bytes.Repeat([]byte{0xAB}, 300),
		"sub/nested/c.dat": []byte("nested data"),
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func writeToFile(t *testing.T, s *stream.Stream) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "romfs.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteTo(f, nil); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func openRegion(t *testing.T, path string) *region.Region {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	r, err := region.New(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNameHash(t *testing.T) {
	cases := []struct {
		parent uint32
		name   string
		want   uint32
	}{
		{0, "", 0x075bcd15},
		{0, "a", 0xa83ade09},
		{0, "ab", 0x4d41d692},
		{0, "hello", 0xd62a0aef},
		{0x18, "world", 0xd1bff6e4},
		{0x20, "hello", 0xd62a1aef},
		{12345, "abc", 0xf2180ed7},
		{0, "cafe.txt", 0xa85d3932},
	}
	for _, c := range cases {
		if got := nameHash(c.parent, []byte(c.name)); got != c.want {
			t.Errorf("nameHash(%#x, %q) = %#x, want %#x", c.parent, c.name, got, c.want)
		}
	}
}

func TestBucketCount(t *testing.T) {
	want := []int{3, 3, 3, 3, 5, 5, 7, 7, 9, 9, 11, 11, 13, 13, 15, 15, 17, 17, 19, 19, 23, 23, 23, 23, 29, 29}
	for n, w := range want {
		if got := bucketCount(n); got != w {
			t.Errorf("bucketCount(%d) = %d, want %d", n, got, w)
		}
	}
}

func TestBuildRoundTrip(t *testing.T) {
	root := t.TempDir()
	expected := writeTree(t, root)

	var manifest []ManifestEntry
	s, err := Build(context.Background(), root, &manifest, nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if s == nil {
		t.Fatal("nil stream")
	}

	p := writeToFile(t, s)
	r := openRegion(t, p)

	if r.Size() != s.Size() {
		t.Fatalf("size mismatch: stream %d region %d", s.Size(), r.Size())
	}

	// Verify header fields and structural invariants.
	hdr := make([]byte, 0x50)
	if err := r.ReadAt(0, hdr); err != nil {
		t.Fatal(err)
	}
	getQ := func(i int) uint64 { return binfmt.U64At(hdr, i*8) }
	if getQ(0) != 0x50 {
		t.Fatalf("magic = %#x, want 0x50", getQ(0))
	}
	// 3 dirs (root, sub, nested) -> bucketCount(3)=3 -> 12 bytes; 4 files -> 5 -> 20 bytes.
	if getQ(2) != uint64(4*bucketCount(3)) {
		t.Fatalf("dir hash size = %d", getQ(2))
	}
	if getQ(6) != uint64(4*bucketCount(4)) {
		t.Fatalf("file hash size = %d", getQ(6))
	}
	if getQ(1) != 0x50 {
		t.Fatalf("dir hash offset = %d, want 0x50", getQ(1))
	}
	if getQ(3) != getQ(1)+getQ(2) {
		t.Fatalf("dir table offset = %d", getQ(3))
	}
	if getQ(5) != getQ(3)+getQ(4) {
		t.Fatalf("file hash offset = %d", getQ(5))
	}
	if getQ(7) != getQ(5)+getQ(6) {
		t.Fatalf("file table offset = %d", getQ(7))
	}
	datap := int64(getQ(9))
	if datap != binfmt.Align(int64(getQ(7))+int64(getQ(8)), 0x10) {
		t.Fatalf("data offset = %d", datap)
	}

	// Read back through the bounded walker.
	files, err := Files(context.Background(), r)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != len(expected) {
		t.Fatalf("got %d files, want %d", len(files), len(expected))
	}
	for rel, want := range expected {
		got, ok := files[rel]
		if !ok {
			t.Fatalf("missing %q", rel)
		}
		b, err := got.Slice()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, want) {
			t.Fatalf("%q content mismatch", rel)
		}
	}

	// Manifest: same files, sizes, and offsets pointing at the right bytes.
	if len(manifest) != len(expected) {
		t.Fatalf("manifest %d entries, want %d", len(manifest), len(expected))
	}
	for _, m := range manifest {
		rel, err := filepath.Rel(root, m.Path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		want, ok := expected[rel]
		if !ok {
			t.Fatalf("unexpected manifest path %q", rel)
		}
		if m.Size != int64(len(want)) {
			t.Fatalf("size mismatch for %q: %d != %d", rel, m.Size, len(want))
		}
		sub, err := r.Sub(datap+m.RomfsOffset, m.Size)
		if err != nil {
			t.Fatal(err)
		}
		b, err := sub.Slice()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, want) {
			t.Fatalf("payload mismatch at RomfsOffset for %q", rel)
		}
	}
}

func TestBuildEmpty(t *testing.T) {
	root := t.TempDir()
	var manifest []ManifestEntry
	s, err := Build(context.Background(), root, &manifest, nil, nil)
	if err != nil {
		t.Fatalf("Build empty: %v", err)
	}
	if len(manifest) != 0 {
		t.Fatalf("manifest has %d entries", len(manifest))
	}
	r := openRegion(t, writeToFile(t, s))
	files, err := Files(context.Background(), r)
	if err != nil {
		t.Fatalf("Files empty: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

func TestBuildNotify(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 1024; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%04d", i)), []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var calls []int
	var manifest []ManifestEntry
	if _, err := Build(context.Background(), root, &manifest, func(n int) { calls = append(calls, n) }, nil); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != 1024 {
		t.Fatalf("notify calls = %v", calls)
	}
	if len(manifest) != 1024 {
		t.Fatalf("manifest len = %d", len(manifest))
	}
}

func TestBuildRejectsSymlinkFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}
	var manifest []ManifestEntry
	_, err := Build(context.Background(), root, &manifest, nil, nil)
	if err == nil {
		t.Fatal("expected error for symlink file")
	}
	if !strings.Contains(err.Error(), "Linked folders/files are not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildRejectsSymlinkDir(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(root, "linkdir")); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}
	var manifest []ManifestEntry
	_, err := Build(context.Background(), root, &manifest, nil, nil)
	if err == nil {
		t.Fatal("expected error for symlink dir")
	}
	if !strings.Contains(err.Error(), "Linked folders/files are not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildSkipsSpecialFiles(t *testing.T) {
	// Named pipes (FIFO) are special files; Python romfs skips them silently.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "regular.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "pipe")
	if err := mkFifo(fifo); err != nil {
		t.Skip("fifo unavailable: " + err.Error())
	}
	var manifest []ManifestEntry
	s, err := Build(context.Background(), root, &manifest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 1 {
		t.Fatalf("manifest len = %d, want 1", len(manifest))
	}
	r := openRegion(t, writeToFile(t, s))
	files, err := Files(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
}

func TestFilesRejectsBadMagic(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root)
	var manifest []ManifestEntry
	s, err := Build(context.Background(), root, &manifest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := writeToFile(t, s)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	binfmt.PutU64(data, 0, 0x51) // corrupt header magic
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Files(context.Background(), openRegion(t, p))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Invalid RomFS header") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFilesRejectsBadTableBounds(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root)
	var manifest []ManifestEntry
	s, err := Build(context.Background(), root, &manifest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := writeToFile(t, s)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	binfmt.PutU64(data, 24, 0xffffffffffffffff) // dir table offset beyond file
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Files(context.Background(), openRegion(t, p))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Invalid RomFS table bounds") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFilesRejectsCycle(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root) // includes sub/
	var manifest []ManifestEntry
	s, err := Build(context.Background(), root, &manifest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := writeToFile(t, s)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	dtpos := int(binfmt.U64At(data, 24)) // vals[3]
	binfmt.PutU32(data, dtpos+8, 0)      // root dir's child -> itself (cycle)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Files(context.Background(), openRegion(t, p))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Cyclic or invalid RomFS directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}
