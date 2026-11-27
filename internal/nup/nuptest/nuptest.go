// Package nuptest writes NUP archives the way third-party packers lay them
// out, so tests can exercise the reader and the optimizer on archives that
// were not produced by Boxcar's own writer. It deliberately does not import
// internal/nup.
package nuptest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// File is one file of a package, at a slash-separated path below the
// package root. Content comes from Data, or from Source (exactly Size bytes)
// when Source is set, so large inputs never have to sit in memory. Extent,
// when set, is stored instead of the content, with codec Codec; Data still
// supplies the file's size and SHA-256.
type File struct {
	Path   string
	Data   []byte
	Size   int64
	Source io.Reader
	Extent []byte
	Codec  uint8
}

// Package is one NUP package record and its tree.
type Package struct {
	TitleID uint64
	Version uint32
	Kind    uint8
	Flags   uint8
	// Files are walked the way KaiDump scans a dump: depth first, with each
	// directory's entries in the order they first appear here. File records
	// and extents follow that walk; only each directory's child list is
	// sorted.
	Files []File
	// Dirs lists directories that must exist even when empty.
	Dirs []string
	// Meta is the raw metadata TLV block (see TLV); nil writes none.
	Meta []byte
}

// Options adjust the superblock.
type Options struct {
	// HeaderSize and HeaderVersion replace the v1 values 0x80 and 1.
	HeaderSize    uint16
	HeaderVersion uint16
}

// TLV serializes one metadata record.
func TLV(kind uint16, data []byte) []byte {
	out := binary.LittleEndian.AppendUint16(nil, kind)
	out = binary.LittleEndian.AppendUint16(out, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(data)))
	return append(out, data...)
}

type node struct {
	name     string
	dir      bool
	children []*node
	file     *File
	index    int
	offset   int64
	digest   [32]byte
}

func (n *node) child(name string, dir bool) (*node, error) {
	for _, c := range n.children {
		if c.name == name {
			if c.dir != dir {
				return nil, fmt.Errorf("nuptest: %q is both a file and a directory", name)
			}
			return c, nil
		}
	}
	c := &node{name: name, dir: dir}
	n.children = append(n.children, c)
	return c, nil
}

func (n *node) path(parts []string, last bool) (*node, error) {
	cur := n
	for i, part := range parts {
		var err error
		cur, err = cur.child(part, !(last && i == len(parts)-1))
		if err != nil {
			return nil, err
		}
	}
	return cur, nil
}

// WriteKaiDump writes packages the way KaiDump 1.0.x (source/nup.c) does:
// every file is stored raw with no deduplication, names are not interned,
// the TOC size is left unaligned, metadata is appended after the blob region
// and empty files carry the current blob cursor as their extent offset.
func WriteKaiDump(path string, packages []Package, opts Options) error {
	hs := opts.HeaderSize
	if hs == 0 {
		hs = 0x80
	}
	if hs < 0x80 {
		return errors.New("nuptest: the superblock is at least 0x80 bytes")
	}
	hv := opts.HeaderVersion
	if hv == 0 {
		hv = 1
	}

	var dirs, files []*node
	roots := make([]*node, len(packages))
	var scan func(n *node)
	scan = func(n *node) {
		n.index = len(dirs)
		dirs = append(dirs, n)
		for _, c := range n.children {
			if c.dir {
				scan(c)
			} else {
				c.index = len(files)
				files = append(files, c)
			}
		}
	}
	for i, p := range packages {
		root := &node{dir: true}
		for fi := range p.Files {
			f := &p.Files[fi]
			leaf, err := root.path(strings.Split(f.Path, "/"), true)
			if err != nil {
				return err
			}
			if leaf.file != nil {
				return fmt.Errorf("nuptest: duplicate file %q", f.Path)
			}
			leaf.file = f
		}
		for _, d := range p.Dirs {
			if _, err := root.path(strings.Split(d, "/"), false); err != nil {
				return err
			}
		}
		roots[i] = root
		scan(root)
	}

	children := 0
	pool := 0
	for _, d := range dirs {
		children += len(d.children)
		pool += len(d.name)
	}
	for _, f := range files {
		pool += len(f.name)
	}
	tocSize := 48 + 64*len(packages) + 24*len(dirs) + 80*len(files) + 4*children + pool
	blobOff := align(int64(hs)+int64(tocSize), 16)

	out, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := out.Write(make([]byte, blobOff)); err != nil {
		return err
	}

	position := blobOff
	for _, f := range files {
		size := int64(len(f.file.Data))
		if f.file.Source != nil {
			size = f.file.Size
		}
		if size > 0 {
			aligned := align(position, 16)
			if _, err := out.Write(make([]byte, aligned-position)); err != nil {
				return err
			}
			position = aligned
		}
		f.offset = position
		if f.file.Extent != nil {
			f.digest = sha256.Sum256(f.file.Data)
			if _, err := out.Write(f.file.Extent); err != nil {
				return err
			}
			position += int64(len(f.file.Extent))
			continue
		}
		h := sha256.New()
		var src io.Reader = bytes.NewReader(f.file.Data)
		if f.file.Source != nil {
			src = f.file.Source
		}
		n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(src, size))
		if err != nil {
			return err
		}
		if n != size {
			return errors.New("nuptest: short file source")
		}
		copy(f.digest[:], h.Sum(nil))
		position += size
	}

	metaOffsets := make([]int64, len(packages))
	for i, p := range packages {
		if len(p.Meta) == 0 {
			continue
		}
		metaOffsets[i] = position
		if _, err := out.Write(p.Meta); err != nil {
			return err
		}
		position += int64(len(p.Meta))
	}

	toc := make([]byte, tocSize)
	le := binary.LittleEndian
	copy(toc[0:4], "TOC\x01")
	le.PutUint32(toc[4:], uint32(len(packages)))
	le.PutUint64(toc[8:], uint64(len(dirs)))
	le.PutUint64(toc[16:], uint64(len(files)))
	childAt := 48 + 64*len(packages) + 24*len(dirs) + 80*len(files)
	poolAt := childAt + 4*children
	le.PutUint64(toc[24:], uint64(poolAt))
	le.PutUint64(toc[32:], uint64(pool))

	at := 48
	for i, p := range packages {
		exefs, romfs := uint32(0xffffffff), uint32(0xffffffff)
		for _, c := range roots[i].children {
			if c.dir && c.name == "exefs" {
				exefs = uint32(c.index)
			}
			if c.dir && c.name == "romfs" {
				romfs = uint32(c.index)
			}
		}
		le.PutUint64(toc[at:], p.TitleID)
		le.PutUint32(toc[at+8:], uint32(roots[i].index))
		le.PutUint32(toc[at+12:], exefs)
		le.PutUint32(toc[at+16:], romfs)
		le.PutUint32(toc[at+20:], p.Version)
		toc[at+24] = p.Kind
		toc[at+25] = p.Flags
		le.PutUint64(toc[at+32:], uint64(metaOffsets[i]))
		le.PutUint32(toc[at+40:], uint32(len(p.Meta)))
		at += 64
	}

	stringAt := 0
	slot := 0
	names := toc[poolAt:]
	for _, d := range dirs {
		sorted := append([]*node(nil), d.children...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
		le.PutUint32(toc[at:], uint32(stringAt))
		le.PutUint32(toc[at+4:], uint32(len(d.name)))
		le.PutUint32(toc[at+8:], uint32(slot))
		le.PutUint32(toc[at+12:], uint32(len(sorted)))
		copy(names[stringAt:], d.name)
		stringAt += len(d.name)
		for _, c := range sorted {
			id := uint32(c.index)
			if !c.dir {
				id += uint32(len(dirs))
			}
			le.PutUint32(toc[childAt+4*slot:], id)
			slot++
		}
		at += 24
	}
	for _, f := range files {
		size := int64(len(f.file.Data))
		if f.file.Source != nil {
			size = f.file.Size
		}
		stored := size
		if f.file.Extent != nil {
			stored = int64(len(f.file.Extent))
			toc[at+64] = f.file.Codec
		}
		le.PutUint32(toc[at:], uint32(stringAt))
		le.PutUint32(toc[at+4:], uint32(len(f.name)))
		le.PutUint64(toc[at+8:], uint64(size))
		le.PutUint64(toc[at+16:], uint64(f.offset))
		le.PutUint64(toc[at+24:], uint64(stored))
		copy(toc[at+32:at+64], f.digest[:])
		copy(names[stringAt:], f.name)
		stringAt += len(f.name)
		at += 80
	}

	identity := sha256.New()
	for _, p := range packages {
		var rec [13]byte
		le.PutUint64(rec[0:], p.TitleID)
		le.PutUint32(rec[8:], p.Version)
		rec[12] = p.Kind
		identity.Write(rec[:])
	}
	header := make([]byte, hs)
	copy(header[0:4], "NUP\x01")
	le.PutUint16(header[4:], hs)
	le.PutUint16(header[6:], hv)
	le.PutUint64(header[8:], uint64(hs))
	le.PutUint64(header[16:], uint64(tocSize))
	tocHash := sha256.Sum256(toc)
	copy(header[24:56], tocHash[:])
	le.PutUint64(header[56:], uint64(blobOff))
	le.PutUint64(header[64:], uint64(position))
	le.PutUint32(header[72:], uint32(len(packages)))
	copy(header[80:96], identity.Sum(nil)[:16])

	if _, err := out.WriteAt(header, 0); err != nil {
		return err
	}
	if _, err := out.WriteAt(toc, int64(hs)); err != nil {
		return err
	}
	return out.Close()
}

func align(n, a int64) int64 { return (n + a - 1) / a * a }
