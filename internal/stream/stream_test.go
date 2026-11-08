package stream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dannolan/boxcar/internal/binfmt"
)

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWriteToLayout(t *testing.T) {
	dir := t.TempDir()
	// 7000-byte file spans several 4096 blocks; the literal tail shares a
	// block with the file tail, exactly like Python Blob.chunks.
	fileData := bytes.Repeat([]byte("abcdefg"), 1000)
	p := writeFile(t, dir, "f.bin", fileData)
	s, err := New(File(p, int64(len(fileData))), Bytes([]byte("tail")))
	if err != nil {
		t.Fatal(err)
	}
	if s.Size() != int64(len(fileData)+4) {
		t.Fatalf("Size = %d, want %d", s.Size(), len(fileData)+4)
	}
	var out bytes.Buffer
	if err := s.WriteTo(&out, &WriteOpts{Block: 4096}); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, fileData...), []byte("tail")...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("WriteTo output mismatch")
	}
}

func TestHashCaptureMatchesFile(t *testing.T) {
	dir := t.TempDir()
	small := []byte("tiny")
	big := bytes.Repeat([]byte{0x11}, 10000) // spans multiple blocks
	p1 := writeFile(t, dir, "small.bin", small)
	p2 := writeFile(t, dir, "big.bin", big)
	s, err := New(Bytes([]byte("head")), File(p1, int64(len(small))), File(p2, int64(len(big))))
	if err != nil {
		t.Fatal(err)
	}
	gotSmall, gotBig := new([]byte), new([]byte)
	hashes := map[string]*[]byte{p1: gotSmall, p2: gotBig}
	var out bytes.Buffer
	if err := s.WriteTo(&out, &WriteOpts{Block: 4096, Hashes: hashes}); err != nil {
		t.Fatal(err)
	}
	wantSmall := sha256.Sum256(small)
	wantBig := sha256.Sum256(big)
	if !bytes.Equal(*gotSmall, wantSmall[:]) {
		t.Fatalf("small hash = %x, want %x", *gotSmall, wantSmall)
	}
	if !bytes.Equal(*gotBig, wantBig[:]) {
		t.Fatalf("big hash = %x, want %x", *gotBig, wantBig)
	}
}

func TestHashCaptureEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "empty.bin", nil)
	s, err := New(File(p, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := new([]byte)
	hashes := map[string]*[]byte{p: got}
	if err := s.WriteTo(&bytes.Buffer{}, &WriteOpts{Hashes: hashes}); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(nil)
	if !bytes.Equal(*got, want[:]) {
		t.Fatalf("empty hash = %x, want %x", *got, want)
	}
}

func TestMultiPassReopensFiles(t *testing.T) {
	// Python Blob re-opens file parts on every pass; ReadHead then WriteTo
	// must both see the content.
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0x5A}, 9000)
	p := writeFile(t, dir, "f.bin", data)
	s, err := New(File(p, int64(len(data))))
	if err != nil {
		t.Fatal(err)
	}
	head, err := s.ReadHead(32, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(head, data[:32]) {
		t.Fatalf("ReadHead = %x", head)
	}
	var out bytes.Buffer
	if err := s.WriteTo(&out, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("second pass mismatch")
	}
}

func TestSHA256(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("xyzzy"), 2000)
	p := writeFile(t, dir, "f.bin", data)
	s, err := New(File(p, int64(len(data))), Padding(16))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := s.SHA256(&WriteOpts{Block: 1024})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	h.Write(data)
	h.Write(make([]byte, 16))
	if !bytes.Equal(sum, h.Sum(nil)) {
		t.Fatal("SHA256 mismatch")
	}
	// Empty stream must not produce a NaN progress ratio.
	empty, _ := New(Bytes(nil))
	var last float64
	empty.SHA256(&WriteOpts{Progress: func(_ string, done float64) { last = done }})
	if last != last { // NaN check
		t.Fatal("NaN progress on empty stream")
	}
}

func TestPadAndSize(t *testing.T) {
	s, err := New(Bytes([]byte("abc")))
	if err != nil {
		t.Fatal(err)
	}
	if s.Size() != 3 {
		t.Fatalf("Size = %d", s.Size())
	}
	p := s.Pad(16)
	if p.Size() != 16 {
		t.Fatalf("padded size = %d, want 16", p.Size())
	}
	head, err := p.ReadHead(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(head, append([]byte("abc"), make([]byte, 13)...)) {
		t.Fatal("pad bytes not zero-filled tail")
	}
}

func TestPaddingNegativePanics(t *testing.T) {
	var err error
	func() {
		defer binfmt.Recover(&err)
		Padding(-1)
	}()
	if err == nil || !stringsContains(err.Error(), "Negative padding") {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteToCancellation(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0x33}, 1<<20)
	p := writeFile(t, dir, "f.bin", data)
	s, err := New(File(p, int64(len(data))), File(p, int64(len(data))))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.WriteTo(&discard{}, &WriteOpts{Ctx: ctx, Block: 1 << 12})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestShortReadSurfaces(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "f.bin", []byte("short"))
	s, err := New(File(p, 100)) // size lies about the real file
	if err != nil {
		t.Fatal(err)
	}
	err = s.WriteTo(&discard{}, nil)
	if err == nil || !stringsContains(err.Error(), "short read") {
		t.Fatalf("err = %v, want short read", err)
	}
}

func TestNewUnsupportedPart(t *testing.T) {
	if _, err := New(struct{}{}); err == nil {
		t.Fatal("expected error for unsupported part")
	}
	if _, err := New(-1); err == nil {
		t.Fatal("expected error for unsupported int part (use Padding)")
	}
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }

func stringsContains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
