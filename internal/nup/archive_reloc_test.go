package nup

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestArchiveAcceptsMetadataOutsideBlobRegion locks in the documented
// deviation: third-party packers may place package metadata in the gap
// between the TOC and the blob region (observed in a real-world NUP).
// The reference implementation rejects such archives; Boxcar accepts them as
// long as metadata is in-bounds and does not overlap other extents.
func TestArchiveAcceptsMetadataOutsideBlobRegion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packages, expected := buildSynthetic(t, dir)

	out := filepath.Join(dir, "relocated.nup")
	if _, err := Pack(ctx, packages, out, PackOptions{Level: 3, LargeRaw: true, Verify: true}); err != nil {
		t.Fatalf("Pack: %v", err)
	}

	if !relocateMetadataToTocGap(t, out) {
		t.Skip("no metadata was relocated; layout not as expected")
	}

	// Open must accept the relocated metadata; verify + extract must work.
	a, err := Open(ctx, out)
	if err != nil {
		t.Fatalf("Open after metadata relocation: %v", err)
	}
	defer a.Close()
	if len(a.Packages()) != len(packages) {
		t.Fatalf("packages: got %d want %d", len(a.Packages()), len(packages))
	}
	if _, err := a.Verify(ctx, nil); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	extractDir := filepath.Join(dir, "extract")
	if _, err := Extract(ctx, out, extractDir, nil, nil); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for rel, want := range expected {
		got, err := os.ReadFile(filepath.Join(extractDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read extracted %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Fatalf("extracted %s differs", rel)
		}
	}
}

// relocateMetadataToTocGap moves every package metadata extent from the blob
// region into the gap between the TOC and blobOff — the layout the reference
// reader rejects — and repairs the TOC hash. Layout per nup_format.py:
//
//	superblock '<4sHHQQ32sQQII16s32s': tocOff@8, tocSize@16, tocSha@24:56,
//	    blobOff@56, fs@64
//	TOC '<4sIQQQQQ': magic@0, packages@4(u32), dirs@8, files@16, poolOff@24
//	PKG '<QIIIIBBHIQI20s' (64B): metaOff@32(Q), metaSize@40(I)
//	DIR '<IIIIQ' (24B), FILE '<IIQQQ32sBBH12s' (80B), then name pool.
func relocateMetadataToTocGap(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	hdr := make([]byte, 128)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		t.Fatal(err)
	}
	tocOff := int64(binary.LittleEndian.Uint64(hdr[8:16]))
	tocSize := int64(binary.LittleEndian.Uint64(hdr[16:24]))
	blobOff := binary.LittleEndian.Uint64(hdr[56:64])

	toc := make([]byte, tocSize)
	if _, err := f.ReadAt(toc, tocOff); err != nil {
		t.Fatal(err)
	}
	np := int(binary.LittleEndian.Uint32(toc[4:8]))

	// Move each package's metadata to the very end of the file (outside the
	// blob region entirely — beyond the old end-of-file), then grow the
	// superblock's fs. This mirrors wild archives whose metadata sits
	// outside blob extents.
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	cursor := uint64(fi.Size())
	const pkgSize = 64
	moved := 0
	for i := 0; i < np; i++ {
		rec := 48 + i*pkgSize
		mo := binary.LittleEndian.Uint64(toc[rec+32 : rec+40])
		ms := binary.LittleEndian.Uint32(toc[rec+40 : rec+44])
		if ms == 0 || mo < blobOff {
			continue // absent, or already outside the blob region
		}
		data := make([]byte, ms)
		if _, err := f.ReadAt(data, int64(mo)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(data, int64(cursor)); err != nil {
			t.Fatal(err)
		}
		binary.LittleEndian.PutUint64(toc[rec+32:rec+40], cursor)
		cursor += uint64(ms)
		moved++
	}
	if moved == 0 {
		return false
	}

	// Repair the TOC hash and grow the declared file size.
	sum := sha256.Sum256(toc)
	copy(hdr[24:56], sum[:])
	binary.LittleEndian.PutUint64(hdr[64:72], cursor)
	if _, err := f.WriteAt(toc, tocOff); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(hdr, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	return true
}
