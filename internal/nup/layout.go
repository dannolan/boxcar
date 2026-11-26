package nup

import (
	"context"
	"sort"

	"github.com/dannolan/boxcar/internal/binfmt"
)

type fileNode struct {
	nameOff, nameLen int
	source           *Source
	off              int64
	stored           int64
	digest           [32]byte
	codec            int
}

type dirNode struct {
	nameOff, nameLen int
	first, count     int
}

type childEntry struct {
	isDir bool
	index int
}

type metaExtent struct{ off, size int64 }

// layout is the node/name plan of a package set: the tree half of
// nup_format.pack, shared by Pack and Repack so both emit the same TOC for
// the same logical content.
type layout struct {
	dirs     []dirNode
	files    []fileNode
	children []childEntry
	roots    []int
	pool     []byte
	names    map[string]int
	pooloff  int
	tocsize  int64
	bloboff  int64
}

// planLayout builds the directory/file node arrays, child slots and interned
// name pool (mirrors pack's nested dicts).
func planLayout(ctx context.Context, packages []Package) *layout {
	l := &layout{pool: []byte{}, names: map[string]int{}}
	intern := func(name string) (int, int) {
		data := []byte(name)
		key := string(data)
		if off, ok := l.names[key]; ok {
			return off, len(data)
		}
		off := len(l.pool)
		l.names[key] = off
		l.pool = append(l.pool, data...)
		return off, len(data)
	}

	for pi := range packages {
		p := &packages[pi]
		tree := map[string]any{}
		dirList := append([]string(nil), p.Directories...)
		sort.Strings(dirList)
		for _, path := range dirList {
			branch := tree
			for _, name := range pathParts(path) {
				next, ok := branch[name].(map[string]any)
				if !ok {
					next = map[string]any{}
					branch[name] = next
				}
				branch = next
			}
		}
		for path, source := range p.Files {
			parts := pathParts(path)
			branch := tree
			for _, name := range parts[:len(parts)-1] {
				if _, isFile := branch[name].(*Source); isFile {
					binfmt.Fail("File/directory collision.")
				}
				next, ok := branch[name].(map[string]any)
				if !ok {
					next = map[string]any{}
					branch[name] = next
				}
				branch = next
			}
			last := parts[len(parts)-1]
			binfmt.Require(branch[last] == nil, "Duplicate file/directory path.")
			branch[last] = source
		}

		var visit func(branch map[string]any, name string, depth int) int
		visit = func(branch map[string]any, name string, depth int) int {
			binfmt.Check(ctx)
			binfmt.Require(depth < 256, "Directory nesting exceeds 255 levels.")
			index := len(l.dirs)
			l.dirs = append(l.dirs, dirNode{})
			start := len(l.children)
			namesSorted := sortedKeys(branch)
			l.children = append(l.children, make([]childEntry, len(namesSorted))...)
			for j, n := range namesSorted {
				value := branch[n]
				if sub, ok := value.(map[string]any); ok {
					l.children[start+j] = childEntry{isDir: true, index: visit(sub, n, depth+1)}
				} else {
					src := value.(*Source)
					l.children[start+j] = childEntry{isDir: false, index: len(l.files)}
					no, nl := intern(n)
					l.files = append(l.files, fileNode{nameOff: no, nameLen: nl, source: src})
				}
			}
			no, nl := intern(name)
			l.dirs[index] = dirNode{nameOff: no, nameLen: nl, first: start, count: len(namesSorted)}
			return index
		}
		l.roots = append(l.roots, visit(tree, p.rootName, 0))
	}

	binfmt.Require(len(l.dirs)+len(l.files) < int(none), "Too many NUP nodes.")

	l.pooloff = 48 + len(packages)*64 + len(l.dirs)*24 + len(l.files)*80 + len(l.children)*4
	l.tocsize = binfmt.Align(int64(l.pooloff+len(l.pool)), 16)
	binfmt.Require(l.tocsize <= 256*1024*1024, "NUP file table exceeds 256 MiB.")
	l.bloboff = binfmt.Align(128+l.tocsize, 16)
	return l
}

// toc assembles the table of contents once every file node has its extent.
func (l *layout) toc(packages []Package, meta []metaExtent) []byte {
	toc := make([]byte, l.tocsize)
	copy(toc[0:4], "TOC\x01")
	binfmt.PutU32(toc, 4, uint32(len(packages)))
	binfmt.PutU64(toc, 8, uint64(len(l.dirs)))
	binfmt.PutU64(toc, 16, uint64(len(l.files)))
	binfmt.PutU64(toc, 24, uint64(l.pooloff))
	binfmt.PutU64(toc, 32, uint64(len(l.pool)))
	at := 48
	for pi := range packages {
		p := &packages[pi]
		root := l.roots[pi]
		exefs := none
		romfs := none
		for _, target := range [2]string{"exefs", "romfs"} {
			d := l.dirs[root]
			for _, ch := range l.children[d.first : d.first+d.count] {
				if !ch.isDir {
					continue
				}
				cd := l.dirs[ch.index]
				wantOff, wantLen := internedName(l.names, target)
				if cd.nameOff == wantOff && cd.nameLen == wantLen {
					if target == "exefs" {
						exefs = uint32(ch.index)
					} else {
						romfs = uint32(ch.index)
					}
				}
			}
		}
		if p.unsetDirs&1 != 0 {
			exefs = none
		}
		if p.unsetDirs&2 != 0 {
			romfs = none
		}
		binfmt.PutU64(toc, at+0, p.TitleID)
		binfmt.PutU32(toc, at+8, uint32(root))
		binfmt.PutU32(toc, at+12, exefs)
		binfmt.PutU32(toc, at+16, romfs)
		binfmt.PutU32(toc, at+20, p.Version)
		toc[at+24] = byte(p.Kind)
		toc[at+25] = byte(p.Flags)
		binfmt.PutU64(toc, at+32, uint64(meta[pi].off))
		binfmt.PutU32(toc, at+40, uint32(meta[pi].size))
		at += 64
	}
	for _, d := range l.dirs {
		binfmt.PutU32(toc, at+0, uint32(d.nameOff))
		binfmt.PutU32(toc, at+4, uint32(d.nameLen))
		binfmt.PutU32(toc, at+8, uint32(d.first))
		binfmt.PutU32(toc, at+12, uint32(d.count))
		at += 24
	}
	for _, f := range l.files {
		binfmt.PutU32(toc, at+0, uint32(f.nameOff))
		binfmt.PutU32(toc, at+4, uint32(f.nameLen))
		binfmt.PutU64(toc, at+8, uint64(f.source.size))
		binfmt.PutU64(toc, at+16, uint64(f.off))
		binfmt.PutU64(toc, at+24, uint64(f.stored))
		copy(toc[at+32:at+64], f.digest[:])
		toc[at+64] = byte(f.codec)
		at += 80
	}
	for _, ch := range l.children {
		node := uint32(ch.index)
		if !ch.isDir {
			node += uint32(len(l.dirs))
		}
		binfmt.PutU32(toc, at, node)
		at += 4
	}
	copy(toc[l.pooloff:l.pooloff+len(l.pool)], l.pool)
	return toc
}

// superblock builds the 128-byte header: identity UUID, TOC hash and the
// blob/file extents.
func superblock(packages []Package, toc []byte, bloboff, end int64) []byte {
	identityBuf := make([]byte, 0, len(packages)*13)
	for _, p := range packages {
		identityBuf = binfmt.AppendU64(identityBuf, p.TitleID)
		identityBuf = binfmt.AppendU32(identityBuf, p.Version)
		identityBuf = append(identityBuf, byte(p.Kind))
	}
	identityHash := binfmt.SHA256Sum(identityBuf)
	header := make([]byte, 128)
	copy(header[0:4], "NUP\x01")
	binfmt.PutU16(header, 4, 128)
	binfmt.PutU16(header, 6, 1)
	binfmt.PutU64(header, 8, 128)
	binfmt.PutU64(header, 16, uint64(len(toc)))
	copy(header[24:56], binfmt.SHA256Sum(toc))
	binfmt.PutU64(header, 56, uint64(bloboff))
	binfmt.PutU64(header, 64, uint64(end))
	binfmt.PutU32(header, 72, uint32(len(packages)))
	copy(header[80:96], identityHash[:16])
	return header
}

// internedName returns the interned offset (or -1) and length of name.
func internedName(names map[string]int, name string) (int, int) {
	off, ok := names[name]
	if !ok {
		off = -1
	}
	return off, len(name)
}
