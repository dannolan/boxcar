package pfs0

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"testing"

	"github.com/dannolan/boxcar/internal/region"
	"github.com/dannolan/boxcar/internal/stream"
)

// writeTemp streams s into a fresh temp file and returns it open (caller must
// close). The file is synced so region pread sees the bytes.
func writeTemp(t *testing.T, s *stream.Stream) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "pfs0-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteTo(f, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	return f
}

func openRegion(t *testing.T, f *os.File) *region.Region {
	t.Helper()
	r, err := region.New(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFileSystemGoldenPFS0(t *testing.T) {
	entries := []Entry{
		{Name: "a", Part: stream.Bytes([]byte("ABC"))},
		{Name: "bc", Part: stream.Bytes([]byte("DE"))},
	}
	s, err := FileSystem(entries, false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Size() != 112 {
		t.Fatalf("size = %d, want 112", s.Size())
	}
	got, err := s.ReadHead(s.Size(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Golden vector produced by the Python reference
	// (container_format.filesystem) for the same two entries; see hexdump in
	// the Python source run.
	want, _ := hex.DecodeString(
		"504653300200000010000000000000000000000000000000" +
			"030000000000000000000000000000001000000000000000" +
			"020000000000000002000000000000006100626300000000" +
			"000000000000000041424300000000000000000000000000" +
			"44450000000000000000000000000000")
	if !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch:\n got %x\nwant %x", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	entries := []Entry{
		{Name: "a.txt", Part: stream.Bytes([]byte("hello world"))},
		{Name: "empty", Part: stream.Bytes(nil)},
		{Name: "bin", Part: stream.Bytes([]byte{0, 1, 2, 3, 0xff, 0x7f})},
	}
	s, err := FileSystem(entries, false)
	if err != nil {
		t.Fatal(err)
	}
	f := writeTemp(t, s)
	defer f.Close()

	parts, err := Partition(context.Background(), openRegion(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != len(entries) {
		t.Fatalf("got %d entries, want %d", len(parts), len(entries))
	}
	for _, e := range entries {
		view, ok := parts[e.Name]
		if !ok {
			t.Fatalf("missing entry %q", e.Name)
		}
		want := stream.Bytes(e.Part.Data)
		if view.Size() != want.Size {
			t.Fatalf("%q: size = %d, want %d", e.Name, view.Size(), want.Size)
		}
		got, err := view.Slice()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want.Data) {
			t.Fatalf("%q: bytes = %x, want %x", e.Name, got, want.Data)
		}
	}
}

func TestHFS0RoundTrip(t *testing.T) {
	// Second body exceeds 0x200 so hashedSize == min(size, 0x200).
	big := bytes.Repeat([]byte("DE"), 300)
	entries := []Entry{
		{Name: "small", Part: stream.Bytes([]byte("ABC"))},
		{Name: "big", Part: stream.Bytes(big)},
	}
	s, err := FileSystem(entries, true)
	if err != nil {
		t.Fatal(err)
	}
	f := writeTemp(t, s)
	defer f.Close()

	parts, err := Partition(context.Background(), openRegion(t, f))
	if err != nil {
		t.Fatalf("valid HFS0 should parse: %v", err)
	}
	if parts["big"].Size() != int64(len(big)) {
		t.Fatalf("big size = %d, want %d", parts["big"].Size(), len(big))
	}
	got, err := parts["big"].Slice()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("big bytes mismatch")
	}
}

func TestHFS0CorruptHashFails(t *testing.T) {
	entries := []Entry{
		{Name: "small", Part: stream.Bytes([]byte("ABC"))},
		{Name: "big", Part: stream.Bytes(bytes.Repeat([]byte("DE"), 300))},
	}
	s, err := FileSystem(entries, true)
	if err != nil {
		t.Fatal(err)
	}
	f := writeTemp(t, s)
	name := f.Name()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Flip one bit of the first entry's stored hash (entry 0 hash at 0x10+32).
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	b[0x30] ^= 0xff
	if err := os.WriteFile(name, b, 0o644); err != nil {
		t.Fatal(err)
	}

	f2, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if _, err := Partition(context.Background(), openRegion(t, f2)); err == nil {
		t.Fatal("corrupted HFS0 hash must fail")
	}
}

func TestPaddingAndAlignment(t *testing.T) {
	// PFS0: entry bodies pad to 0x10.
	s, err := FileSystem([]Entry{
		{Name: "x", Part: stream.Bytes([]byte{1})},        // 1 -> 16
		{Name: "y", Part: stream.Bytes(make([]byte, 17))}, // 17 -> 32
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	// header 16 + entries 48 + table (align(64+4,16)-64=16) + 16 + 32 = 128.
	if s.Size() != 128 {
		t.Fatalf("PFS0 size = %d, want 128", s.Size())
	}

	// HFS0: bodies pad to 0x200.
	hs, err := FileSystem([]Entry{
		{Name: "x", Part: stream.Bytes([]byte{1})},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	// header 16 + entry 64 + table (align(80+2,512)-80=432) + body 512 = 1024.
	if hs.Size() != 1024 {
		t.Fatalf("HFS0 size = %d, want 1024", hs.Size())
	}
}

func TestPackageRejectsSizeLimitIsNoop(t *testing.T) {
	s, err := Package([]Entry{{Name: "a", Part: stream.Bytes([]byte("x"))}})
	if err != nil {
		t.Fatal(err)
	}
	// Package is a plain PFS0 (hfs=false) with the same layout as FileSystem.
	direct, err := FileSystem([]Entry{{Name: "a", Part: stream.Bytes([]byte("x"))}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Size() != direct.Size() {
		t.Fatalf("Package size = %d, want %d", s.Size(), direct.Size())
	}
}

func TestPartitionRejectsBadMagic(t *testing.T) {
	// 16 bytes: "XXXX" + count=0 + tablesize=0 + 0.
	b := append([]byte("XXXX"), make([]byte, 12)...)
	f := writeTemp(t, mustStream(b))
	defer f.Close()
	if _, err := Partition(context.Background(), openRegion(t, f)); err == nil {
		t.Fatal("bad magic must fail")
	}
}

func mustStream(b []byte) *stream.Stream {
	s, err := stream.New(b)
	if err != nil {
		panic(err)
	}
	return s
}
