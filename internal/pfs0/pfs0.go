// Package pfs0 ports the Python reference PFS0/HFS0 container writer and
// reader: container_format.filesystem / container_format.package and
// verify_format.partition. PFS0 and HFS0 wrap a flat list of named file
// entries; HFS0 additionally stores a SHA-256 over the first 0x200 bytes of
// each entry body.
package pfs0

import (
	"bytes"
	"context"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/region"
	"github.com/dannolan/boxcar/internal/stream"
)

// Entry is one named file inside a PFS0/HFS0 container. Part is a file-backed
// or literal stream.Part; its bytes are the entry body.
type Entry struct {
	Name string
	Part stream.Part
}

// FileSystem builds a PFS0 (hfs=false) or HFS0 (hfs=true) container stream,
// the exact port of container_format.filesystem.
func FileSystem(entries []Entry, hfs bool) (s *stream.Stream, err error) {
	defer binfmt.Recover(&err)

	blobs := make([]*stream.Stream, len(entries))
	namesLen := 0
	for i, e := range entries {
		b, serr := stream.New(e.Part)
		binfmt.Require(serr == nil, "entry stream: %v", serr)
		blobs[i] = b
		namesLen += len(e.Name) + 1
	}

	entrySize := 0x18
	alignTo := int64(0x10)
	magic := []byte("PFS0")
	if hfs {
		entrySize = 0x40
		alignTo = 0x200
		magic = []byte("HFS0")
	}

	prefix := int64(0x10) + int64(entrySize)*int64(len(entries))
	tableSize := binfmt.Align(prefix+int64(namesLen), alignTo) - prefix

	table := make([]byte, 0, tableSize)
	for _, e := range entries {
		table = append(table, e.Name...)
		table = append(table, 0)
	}
	table = append(table, make([]byte, tableSize-int64(len(table)))...)

	hdr := make([]byte, prefix)
	copy(hdr[0:4], magic)
	binfmt.PutU32(hdr, 4, uint32(len(entries)))
	binfmt.PutU32(hdr, 8, uint32(tableSize))
	binfmt.PutU32(hdr, 12, 0)

	parts := make([]any, 0, len(entries)+2)
	parts = append(parts, hdr, table)

	dataOff := int64(0)
	nameOff := int64(0)
	for i, e := range entries {
		blob := blobs[i]
		at := 0x10 + i*entrySize
		binfmt.PutU64(hdr, at, uint64(dataOff))
		binfmt.PutU64(hdr, at+8, uint64(blob.Size()))
		binfmt.PutU32(hdr, at+16, uint32(nameOff))
		if hfs {
			hashedSize := blob.Size()
			if hashedSize > 0x200 {
				hashedSize = 0x200
			}
			binfmt.PutU32(hdr, at+20, uint32(hashedSize))
			binfmt.PutU64(hdr, at+24, 0)
			head, herr := blob.ReadHead(hashedSize, nil)
			binfmt.Require(herr == nil, "read head: %v", herr)
			copy(hdr[at+32:at+64], binfmt.SHA256Sum(head))
		} else {
			binfmt.PutU32(hdr, at+20, 0)
		}
		nameOff += int64(len(e.Name)) + 1
		padded := blob.Pad(alignTo)
		parts = append(parts, padded)
		dataOff += padded.Size()
	}

	return stream.New(parts...)
}

// Package builds an NSP container, the exact port of container_format.package
// with kind='nsp' (the only kind the Python reference emits). It is a plain
// PFS0 with the 64-bit size-limit guard.
func Package(entries []Entry) (s *stream.Stream, err error) {
	defer binfmt.Recover(&err)
	result, ferr := FileSystem(entries, false)
	binfmt.Require(ferr == nil, "filesystem: %v", ferr)
	if uint64(result.Size()) > 0xffffffffffffffff {
		binfmt.Fail("NSP size exceeds the PFS0 64-bit size limit.")
	}
	return result, nil
}

// Partition reads a PFS0 or HFS0 region into a map of name -> sub-region,
// validating sizes and (for HFS0) entry hashes. The exact port of
// verify_format.partition with offset=0.
func Partition(ctx context.Context, r *region.Region) (result map[string]*region.Region, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}

	var magicBuf [4]byte
	rerr := r.ReadAt(0, magicBuf[:])
	binfmt.Require(rerr == nil, "read container magic: %v", rerr)
	isPFS := bytes.Equal(magicBuf[:], []byte("PFS0"))
	isHFS := bytes.Equal(magicBuf[:], []byte("HFS0"))
	binfmt.Require(isPFS || isHFS, "invalid container magic %q", magicBuf[:])

	n := int64(requireU32(r, 4, "entry count"))
	stsize := int64(requireU32(r, 8, "string table size"))

	es := int64(0x18)
	if isHFS {
		es = 0x40
	}
	strings := int64(0x10) + n*es
	data := strings + stsize

	result = make(map[string]*region.Region, n)
	for i := int64(0); i < n; i++ {
		binfmt.Check(ctx)
		at := int64(0x10) + i*es
		rel := requireU64(r, at, "entry data offset")
		size := requireU64(r, at+8, "entry size")
		nameOff := int64(requireU32(r, at+16, "entry name offset"))

		name := readName(r, strings, stsize, nameOff)

		view, serr := r.Sub(data+int64(rel), int64(size))
		binfmt.Require(serr == nil && view.Size() == int64(size), "entry %q is outside the container", name)

		if isHFS {
			hashSize := int64(requireU32(r, at+20, "hashed size"))
			hashOff := int64(requireU64(r, at+24, "hash offset"))
			sub, serr2 := view.Sub(hashOff, hashSize)
			binfmt.Require(serr2 == nil, "HFS hash range for %q", name)
			actual, derr := sub.Digest(ctx)
			binfmt.Require(derr == nil, "digest %q: %v", name, derr)
			expected := make([]byte, 32)
			rerr2 := r.ReadAt(at+32, expected)
			binfmt.Require(rerr2 == nil, "read hash for %q: %v", name, rerr2)
			binfmt.Require(bytes.Equal(actual, expected), "HFS hash %q", name)
		}

		result[name] = view
	}
	return result, nil
}

// requireU32 reads a little-endian uint32, failing like Python's struct u() on
// a truncated container.
func requireU32(r *region.Region, off int64, what string) uint32 {
	v, err := r.U32(off)
	binfmt.Require(err == nil, "read %s: %v", what, err)
	return v
}

// requireU64 reads a little-endian uint64.
func requireU64(r *region.Region, off int64, what string) uint64 {
	v, err := r.U64(off)
	binfmt.Require(err == nil, "read %s: %v", what, err)
	return v
}

// readName extracts a NUL-terminated UTF-8 name from the string table, the
// port of `bytes(buf[strings+nameoff:strings+stsize]).split(b'\0')[0].decode()`.
func readName(r *region.Region, strings, stsize, nameOff int64) string {
	if nameOff < 0 || nameOff > stsize {
		return ""
	}
	buf := make([]byte, stsize-nameOff)
	binfmt.Require(r.ReadAt(strings+nameOff, buf) == nil, "read entry name")
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf)
}
