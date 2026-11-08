// Package romfs builds and reads Nintendo RomFS filesystem images. The writer
// ports container_format.romfs (plus bucket_count and name_hash); the reader
// ports the bounded walker nup_tools.romfs_files (depth limit + cycle
// detection) rather than the unbounded verify_format.romfs_files.
//
// Layout (little-endian):
//
//	header       0x50 bytes: 10 uint64 Q values
//	    [0]=0x50  [1]=dirHashOff  [2]=dirHashSize
//	    [3]=dirTableOff  [4]=dirTableSize
//	    [5]=fileHashOff  [6]=fileHashSize
//	    [7]=fileTableOff [8]=fileTableSize  [9]=dataOff (0x10-aligned)
//	dir hash table   bucketCount(numDirs) uint32 buckets (EMPTY=0xffffffff)
//	dir table        per dir: 0x18-byte entry + name, padded to 4
//	file hash table  bucketCount(numFiles) uint32 buckets
//	file table       per file: 0x20-byte entry + name, padded to 4
//	padding          to 0x10
//	data             file payloads, each 0x10-aligned via gap padding
package romfs

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/region"
	"github.com/dannolan/boxcar/internal/stream"
)

// empty is the RomFS "no entry" sentinel (Python EMPTY).
const empty = uint32(0xffffffff)

// ManifestEntry records one file, in file order, as emitted by Build. Path is
// the on-disk source path; RomfsOffset is the byte offset of the payload
// relative to the data section (header field [9]).
type ManifestEntry struct {
	Path        string
	Size        int64
	RomfsOffset int64
}

// dirNode is one directory-table row.
type dirNode struct {
	name    []byte // UTF-8 bytes; empty for the root
	parent  *dirNode
	dirs    []*dirNode
	files   []*fileNode
	off     uint32 // byte offset into the directory table
	sibling uint32 // next sibling in the directory table (sorted order)
}

// fileNode is one file-table row.
type fileNode struct {
	path    string // full on-disk source path
	name    []byte // UTF-8 bytes
	parent  *dirNode
	size    int64
	off     uint32 // byte offset into the file table
	dataoff uint64 // payload offset relative to the data section
	sibling uint32 // next sibling in the file table (sorted order)
}

// nameHash ports container_format.name_hash: a 32-bit rotate-right-5 XOR chain
// seeded with parent ^ 123456789.
func nameHash(parent uint32, name []byte) uint32 {
	h := parent ^ 123456789
	for _, c := range name {
		h = ((h >> 5) | (h << 27)) ^ uint32(c)
	}
	return h
}

// bucketCount ports container_format.bucket_count.
func bucketCount(n int) int {
	if n < 3 {
		return 3
	}
	if n < 19 {
		return n | 1
	}
	for {
		if n%2 != 0 && n%3 != 0 && n%5 != 0 && n%7 != 0 && n%11 != 0 && n%13 != 0 && n%17 != 0 {
			return n
		}
		n++
	}
}

// Build assembles a RomFS image from the file tree rooted at root. It appends
// one ManifestEntry per file (in file order) to *manifest, calls notify every
// 1024 files scanned, and returns an immutable stream whose WriteTo emits the
// full image. Python's romfs does not report table-building progress, so the
// progress argument is accepted but unused.
func Build(ctx context.Context, root string, manifest *[]ManifestEntry, notify func(count int), progress binfmt.Progress) (s *stream.Stream, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	_ = progress

	var dirs []*dirNode
	var files []*fileNode
	var visit func(path string, parent *dirNode) *dirNode
	visit = func(path string, parent *dirNode) *dirNode {
		binfmt.Check(ctx)
		var name []byte
		if parent != nil {
			name = []byte(filepath.Base(path))
		}
		d := &dirNode{name: name, parent: parent, sibling: empty}
		dirs = append(dirs, d)

		entries, rerr := os.ReadDir(path)
		if rerr != nil {
			binfmt.Fail("%v", rerr)
		}
		// Sorted by raw UTF-8 bytes, exactly like Python
		// sorted(scanned, key=lambda e: e.name.encode()).
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

		for _, e := range entries {
			childPath := filepath.Join(path, e.Name())
			info, serr := os.Lstat(childPath)
			if serr != nil {
				binfmt.Fail("%v", serr)
			}
			// Refuse symlinks exactly like Python's linked() check. The
			// Windows FILE_ATTRIBUTE_REPARSE_POINT check is not performed here
			// (deviation: no x/sys/windows dependency in this package).
			if info.Mode()&os.ModeSymlink != 0 {
				binfmt.Fail("Linked folders/files are not supported: %s", childPath)
			}
			if info.IsDir() {
				d.dirs = append(d.dirs, visit(childPath, d))
			} else if info.Mode().IsRegular() {
				f := &fileNode{path: childPath, name: []byte(e.Name()), parent: d, size: info.Size(), sibling: empty}
				files = append(files, f)
				d.files = append(d.files, f)
				if notify != nil && len(files)%1024 == 0 {
					notify(len(files))
				}
			}
			// Special files (sockets, fifos, devices) are silently skipped,
			// matching Python romfs which has no else branch.
		}
		return d
	}
	visit(root, nil)

	// Assign table offsets.
	var doff int64
	for _, d := range dirs {
		d.off = uint32(doff)
		doff += 0x18 + binfmt.Align(int64(len(d.name)), 4)
	}
	var foff, dataoff int64
	for i, f := range files {
		if i%1024 == 0 {
			binfmt.Check(ctx)
		}
		f.off = uint32(foff)
		foff += 0x20 + binfmt.Align(int64(len(f.name)), 4)
		f.dataoff = uint64(binfmt.Align(dataoff, 0x10))
		dataoff = int64(f.dataoff) + f.size
	}

	// Link siblings in linear time (Python notes repeated scans here used to
	// be quadratic).
	for _, d := range dirs {
		for i, child := range d.dirs {
			if i+1 < len(d.dirs) {
				child.sibling = d.dirs[i+1].off
			}
		}
		for i, child := range d.files {
			if i+1 < len(d.files) {
				child.sibling = d.files[i+1].off
			}
		}
	}

	dh := make([]uint32, bucketCount(len(dirs)))
	for i := range dh {
		dh[i] = empty
	}
	fh := make([]uint32, bucketCount(len(files)))
	for i := range fh {
		fh[i] = empty
	}

	// Directory table.
	dt := make([]byte, doff)
	for _, d := range dirs {
		var par uint32
		if d.parent != nil {
			par = d.parent.off
		}
		child, first := empty, empty
		if len(d.dirs) > 0 {
			child = d.dirs[0].off
		}
		if len(d.files) > 0 {
			first = d.files[0].off
		}
		bucket := nameHash(par, d.name) % uint32(len(dh))
		o := int(d.off)
		binfmt.PutU32(dt, o, par)
		binfmt.PutU32(dt, o+4, d.sibling)
		binfmt.PutU32(dt, o+8, child)
		binfmt.PutU32(dt, o+12, first)
		binfmt.PutU32(dt, o+16, dh[bucket])
		binfmt.PutU32(dt, o+20, uint32(len(d.name)))
		dh[bucket] = d.off
		copy(dt[o+0x18:], d.name)
	}

	// File table + payload parts.
	ft := make([]byte, foff)
	payload := make([]stream.Part, 0, len(files)*2)
	var pos int64
	for _, f := range files {
		if len(*manifest)%1024 == 0 {
			binfmt.Check(ctx)
		}
		bucket := nameHash(f.parent.off, f.name) % uint32(len(fh))
		o := int(f.off)
		binfmt.PutU32(ft, o, f.parent.off)
		binfmt.PutU32(ft, o+4, f.sibling)
		binfmt.PutU64(ft, o+8, f.dataoff)
		binfmt.PutU64(ft, o+16, uint64(f.size))
		binfmt.PutU32(ft, o+24, fh[bucket])
		binfmt.PutU32(ft, o+28, uint32(len(f.name)))
		fh[bucket] = f.off
		copy(ft[o+0x20:], f.name)

		gap := int64(f.dataoff) - pos
		if gap > 0 {
			payload = append(payload, stream.Padding(gap))
		}
		payload = append(payload, stream.File(f.path, f.size))
		pos = int64(f.dataoff) + f.size

		*manifest = append(*manifest, ManifestEntry{Path: f.path, Size: f.size, RomfsOffset: int64(f.dataoff)})
	}

	dhb := make([]byte, 4*len(dh))
	for i, v := range dh {
		binfmt.PutU32(dhb, i*4, v)
	}
	fhb := make([]byte, 4*len(fh))
	for i, v := range fh {
		binfmt.PutU32(fhb, i*4, v)
	}

	dhpos := int64(0x50)
	dtpos := dhpos + int64(len(dhb))
	fhpos := dtpos + int64(len(dt))
	ftpos := fhpos + int64(len(fhb))
	datap := binfmt.Align(ftpos+int64(len(ft)), 0x10)

	header := make([]byte, 0x50)
	binfmt.PutU64(header, 0, 0x50)
	binfmt.PutU64(header, 8, uint64(dhpos))
	binfmt.PutU64(header, 16, uint64(len(dhb)))
	binfmt.PutU64(header, 24, uint64(dtpos))
	binfmt.PutU64(header, 32, uint64(len(dt)))
	binfmt.PutU64(header, 40, uint64(fhpos))
	binfmt.PutU64(header, 48, uint64(len(fhb)))
	binfmt.PutU64(header, 56, uint64(ftpos))
	binfmt.PutU64(header, 64, uint64(len(ft)))
	binfmt.PutU64(header, 72, uint64(datap))

	parts := []stream.Part{
		stream.Bytes(header),
		stream.Bytes(dhb),
		stream.Bytes(dt),
		stream.Bytes(fhb),
		stream.Bytes(ft),
		stream.Padding(datap - (ftpos + int64(len(ft)))),
	}
	parts = append(parts, payload...)
	return stream.New(parts)
}

// Files reads a RomFS image into a map of internal path -> file sub-region,
// the exact port of nup_tools.romfs_files: header sanity, table bounds vs the
// region size, per-entry bounds, cycle detection via seen sets, a depth<256
// recursion limit, and duplicate-path rejection. safe_name is skipped (the
// nup layer performs that); names are UTF-8 validated instead.
func Files(ctx context.Context, r *region.Region) (result map[string]*region.Region, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}

	binfmt.Require(r.Size() >= 0x50, "Invalid RomFS header.")
	var vals [10]uint64
	for i := range vals {
		v, rerr := r.U64(int64(i) * 8)
		binfmt.Require(rerr == nil, "Invalid RomFS header.")
		vals[i] = v
	}
	binfmt.Require(vals[0] == 0x50, "Invalid RomFS header.")

	sz := uint64(r.Size())
	dt := vals[3]
	ds := vals[4]
	ft := vals[7]
	fs := vals[8]
	data := vals[9]
	binfmt.Require(0x50 <= dt && dt <= sz && ds <= sz-dt && ft <= sz && fs <= sz-ft && data <= sz,
		"Invalid RomFS table bounds.")

	result = make(map[string]*region.Region)
	seenDirs := map[uint32]bool{}
	seenFiles := map[uint32]bool{}

	read := func(off uint64, n int) []byte {
		b := make([]byte, n)
		if rerr := r.ReadAt(int64(off), b); rerr != nil {
			binfmt.Fail("%v", rerr)
		}
		return b
	}

	var visit func(off uint32, prefix string, depth int)
	visit = func(off uint32, prefix string, depth int) {
		binfmt.Require(depth < 256, "RomFS directory nesting is too deep.")
		for off != empty {
			binfmt.Check(ctx)
			binfmt.Require(!seenDirs[off] && uint64(off)+24 <= ds, "Cyclic or invalid RomFS directory.")
			seenDirs[off] = true

			entry := read(dt+uint64(off), 24)
			sibling := binfmt.U32At(entry, 4)
			child := binfmt.U32At(entry, 8)
			first := binfmt.U32At(entry, 12)
			n := binfmt.U32At(entry, 20)

			binfmt.Require(uint64(n) <= ds-uint64(off)-24, "RomFS directory name out of bounds.")
			nameBytes := read(dt+uint64(off)+24, int(n))
			binfmt.Require(utf8.Valid(nameBytes), "RomFS directory name is not valid UTF-8.")
			name := string(nameBytes)

			var current string
			if prefix != "" && name != "" {
				current = prefix + "/" + name
			} else if prefix != "" {
				current = prefix
			} else {
				current = name
			}

			for entryOff := first; entryOff != empty; {
				binfmt.Check(ctx)
				binfmt.Require(!seenFiles[entryOff] && uint64(entryOff)+32 <= fs, "Cyclic or invalid RomFS file.")
				seenFiles[entryOff] = true

				fentry := read(ft+uint64(entryOff), 32)
				next := binfmt.U32At(fentry, 4)
				fo := binfmt.U64At(fentry, 8)
				fsize := binfmt.U64At(fentry, 16)
				fn := binfmt.U32At(fentry, 28)

				binfmt.Require(uint64(fn) <= fs-uint64(entryOff)-32 &&
					fo <= sz-data && fsize <= sz-data-fo, "RomFS file out of bounds.")

				fnameBytes := read(ft+uint64(entryOff)+32, int(fn))
				binfmt.Require(utf8.Valid(fnameBytes), "RomFS file name is not valid UTF-8.")
				fname := string(fnameBytes)

				var path string
				if current != "" {
					path = current + "/" + fname
				} else {
					path = fname
				}
				_, dup := result[path]
				binfmt.Require(!dup, "Duplicate RomFS file.")

				sub, serr := r.Sub(int64(data)+int64(fo), int64(fsize))
				binfmt.Require(serr == nil, "%v", serr)
				result[path] = sub

				entryOff = next
			}

			if child != empty {
				visit(child, current, depth+1)
			}
			off = sibling
		}
	}
	visit(0, "", 0)
	return result, nil
}
