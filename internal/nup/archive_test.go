package nup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tocLayout records the byte offsets of the TOC sub-arrays so corruption
// tests can mutate precise fields.
type tocLayout struct {
	pc, nd, nf int
	po, ps     int
	pkgOff     int
	dirOff     int
	fileOff    int
	childOff   int
	slots      int
}

func (l tocLayout) fileRec(i int) int { return l.fileOff + i*80 }
func (l tocLayout) dirRec(i int) int  { return l.dirOff + i*24 }
func (l tocLayout) child(i int) int   { return l.childOff + i*4 }

// parseLayout reads the TOC header of a packed archive and computes offsets.
func parseLayout(t *testing.T, data []byte) tocLayout {
	t.Helper()
	tocsize := int(binary.LittleEndian.Uint64(data[16:24]))
	toc := data[128 : 128+tocsize]
	pc := int(binary.LittleEndian.Uint32(toc[4:8]))
	nd := int(binary.LittleEndian.Uint64(toc[8:16]))
	nf := int(binary.LittleEndian.Uint64(toc[16:24]))
	po := int(binary.LittleEndian.Uint64(toc[24:32]))
	ps := int(binary.LittleEndian.Uint64(toc[32:40]))
	pkgOff := 48
	dirOff := pkgOff + pc*64
	fileOff := dirOff + nd*24
	childOff := fileOff + nf*80
	slots := (po - childOff) / 4
	return tocLayout{pc, nd, nf, po, ps, pkgOff, dirOff, fileOff, childOff, slots}
}

// mutateTOC applies edit to the TOC and recomputes the header SHA-256.
func mutateTOC(t *testing.T, data []byte, edit func(toc []byte)) []byte {
	t.Helper()
	out := append([]byte(nil), data...)
	tocsize := int(binary.LittleEndian.Uint64(out[16:24]))
	toc := out[128 : 128+tocsize]
	edit(toc)
	sum := sha256.Sum256(toc)
	copy(out[24:56], sum[:])
	return out
}

// packTwoRawFiles packs a single base package with two incompressible (raw)
// files under romfs, returning the archive bytes.
func packTwoRawFiles(t *testing.T, dir string) []byte {
	t.Helper()
	a := make([]byte, 200<<10)
	b := make([]byte, 200<<10)
	if _, err := rand.Read(a); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	pkg := Package{
		TitleID: testTID, Version: 0, Kind: 0,
		Files:       map[string]*Source{"romfs/a": SourceData(a), "romfs/b": SourceData(b)},
		Directories: []string{"romfs"},
	}
	out := filepath.Join(dir, "raw.nup")
	if _, err := Pack(context.Background(), []Package{pkg}, out, PackOptions{Level: 3, LargeRaw: true, Verify: false}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// expectOpenError writes data to a temp file and asserts Open fails with an
// error containing wantSubstr.
func expectOpenError(t *testing.T, data []byte, wantSubstr string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corrupt.nup")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), path)
	if err == nil {
		t.Errorf("Open: want error containing %q, got nil", wantSubstr)
		return
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("Open: error %q does not contain %q", err.Error(), wantSubstr)
	}
}

func TestCorruptBadMagic(t *testing.T) {
	data := packTwoRawFiles(t, t.TempDir())
	data[0] ^= 0xff
	expectOpenError(t, data, "Not a supported NUP v1 archive.")
}

func TestCorruptBadTOCSHA(t *testing.T) {
	data := packTwoRawFiles(t, t.TempDir())
	data[128] ^= 0xff // flip a TOC byte without recomputing the hash
	expectOpenError(t, data, "NUP table SHA-256 mismatch.")
}

func TestCorruptIdentityMismatch(t *testing.T) {
	data := packTwoRawFiles(t, t.TempDir())
	data[80] ^= 0xff // identity UUID in the superblock
	expectOpenError(t, data, "NUP identity UUID mismatch.")
}

func TestCorruptExtentOverlap(t *testing.T) {
	data := packTwoRawFiles(t, t.TempDir())
	layout := parseLayout(t, data)
	// Move file[1]'s extent to overlap file[0]'s (raw files, no frame walk).
	off0 := binary.LittleEndian.Uint64(data[128+layout.fileRec(0)+16 : 128+layout.fileRec(0)+24])
	mutated := mutateTOC(t, data, func(toc []byte) {
		binary.LittleEndian.PutUint64(toc[layout.fileRec(1)+16:], off0+16)
	})
	expectOpenError(t, mutated, "Overlapping NUP extents/metadata.")
}

func TestCorruptCyclicDirTree(t *testing.T) {
	data := packTwoRawFiles(t, t.TempDir())
	layout := parseLayout(t, data)
	// Point dir[1]'s first child back at dir[1] itself (a cycle).
	mutated := mutateTOC(t, data, func(toc []byte) {
		binary.LittleEndian.PutUint32(toc[layout.child(1):], 1)
	})
	expectOpenError(t, mutated, "Cyclic, shared or overly deep directory tree.")
}

func TestCorruptDuplicateChildNames(t *testing.T) {
	data := packTwoRawFiles(t, t.TempDir())
	layout := parseLayout(t, data)
	// Rename file[1] to file[0]'s name -> duplicate sibling names.
	mutated := mutateTOC(t, data, func(toc []byte) {
		no := binary.LittleEndian.Uint32(toc[layout.fileRec(0):])
		nl := binary.LittleEndian.Uint32(toc[layout.fileRec(0)+4:])
		binary.LittleEndian.PutUint32(toc[layout.fileRec(1):], no)
		binary.LittleEndian.PutUint32(toc[layout.fileRec(1)+4:], nl)
	})
	expectOpenError(t, mutated, "Child names are duplicate or unsorted.")
}

func TestCorruptUnsortedChildNames(t *testing.T) {
	data := packTwoRawFiles(t, t.TempDir())
	layout := parseLayout(t, data)
	// Swap the two file names so "b" sorts before "a".
	mutated := mutateTOC(t, data, func(toc []byte) {
		no0 := binary.LittleEndian.Uint32(toc[layout.fileRec(0):])
		nl0 := binary.LittleEndian.Uint32(toc[layout.fileRec(0)+4:])
		no1 := binary.LittleEndian.Uint32(toc[layout.fileRec(1):])
		nl1 := binary.LittleEndian.Uint32(toc[layout.fileRec(1)+4:])
		binary.LittleEndian.PutUint32(toc[layout.fileRec(0):], no1)
		binary.LittleEndian.PutUint32(toc[layout.fileRec(0)+4:], nl1)
		binary.LittleEndian.PutUint32(toc[layout.fileRec(1):], no0)
		binary.LittleEndian.PutUint32(toc[layout.fileRec(1)+4:], nl0)
	})
	expectOpenError(t, mutated, "Child names are duplicate or unsorted.")
}

func TestCorruptCodecOverRawBytes(t *testing.T) {
	data := packTwoRawFiles(t, t.TempDir())
	layout := parseLayout(t, data)
	// Mark a raw extent as compressed: the frame walk must fail.
	mutated := mutateTOC(t, data, func(toc []byte) {
		toc[layout.fileRec(0)+64] = 1
	})
	expectOpenError(t, mutated, "Zstandard")
}

func TestCorruptUnsupportedCodec(t *testing.T) {
	data := packTwoRawFiles(t, t.TempDir())
	layout := parseLayout(t, data)
	mutated := mutateTOC(t, data, func(toc []byte) {
		toc[layout.fileRec(0)+64] = 2
	})
	expectOpenError(t, mutated, "Unknown codec or file flags.")
}
