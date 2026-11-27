package nup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"

	"github.com/dannolan/boxcar/internal/binfmt"
)

// Content is the logical, storage-independent content of one package: what
// an optimizer must preserve while it changes codecs, deduplication and
// extent placement.
type Content struct {
	TitleID uint64
	Version uint32
	Kind    int
	Flags   int
	// Root is the root directory record's name (normally empty).
	Root string
	// ExeFS and RomFS report whether the package record points at the
	// exefs/ and romfs/ directories.
	ExeFS, RomFS bool
	// Metadata is the TLV block exactly as stored.
	Metadata    []byte
	Directories []string // sorted, every directory including empty ones
	Files       []FileContent
}

// FileContent is one file of a package.
type FileContent struct {
	Path   string
	Size   int64
	SHA256 [32]byte
}

// Contents returns the logical content of every package, in TOC order.
func (a *Archive) Contents() []Content {
	out := make([]Content, len(a.packages))
	for i, p := range a.packages {
		c := Content{
			TitleID: p.TitleID, Version: p.Version, Kind: p.Kind, Flags: p.Flags,
			Root:        p.rootName,
			ExeFS:       a.records[i][1] != none,
			RomFS:       a.records[i][2] != none,
			Metadata:    append([]byte(nil), a.metaRaw[i]...),
			Directories: append([]string(nil), p.Directories...),
		}
		for name, idx := range a.trees[i] {
			f := a.files[idx]
			c.Files = append(c.Files, FileContent{Path: name, Size: f.size, SHA256: f.digest})
		}
		sortFiles(c.Files)
		out[i] = c
	}
	return out
}

// contentsOf returns the content that writing packages would produce. Every
// file must be backed by a parsed archive, which supplies its digest.
func contentsOf(packages []Package) []Content {
	out := make([]Content, len(packages))
	for i, p := range packages {
		dirs := map[string]bool{}
		for _, d := range p.Directories {
			pathParts(d)
			for ; d != "."; d = path.Dir(d) {
				dirs[d] = true
			}
		}
		c := Content{
			TitleID: p.TitleID, Version: p.Version, Kind: p.Kind, Flags: p.Flags,
			Root:     p.rootName,
			Metadata: tlvBytes(p.Metadata),
		}
		for name, src := range p.Files {
			pathParts(name)
			binfmt.Require(src.archive != nil, "Only files of an opened NUP can be repacked: %s", name)
			rec := src.archive.files[src.index]
			c.Files = append(c.Files, FileContent{Path: name, Size: rec.size, SHA256: rec.digest})
			for d := path.Dir(name); d != "."; d = path.Dir(d) {
				dirs[d] = true
			}
		}
		for d := range dirs {
			c.Directories = append(c.Directories, d)
		}
		sort.Strings(c.Directories)
		c.ExeFS = dirs["exefs"] && p.unsetDirs&1 == 0
		c.RomFS = dirs["romfs"] && p.unsetDirs&2 == 0
		sortFiles(c.Files)
		out[i] = c
	}
	return out
}

func sortFiles(files []FileContent) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}

// LogicalDigest is a SHA-256 over a canonical encoding of contents. Two
// archives that pass verification and share this digest hold the same
// packages, metadata, directories and file bytes, however they store them.
func LogicalDigest(contents []Content) string {
	h := sha256.New()
	var b []byte
	str := func(s []byte) {
		b = binfmt.AppendU32(b, uint32(len(s)))
		b = append(b, s...)
	}
	flag := func(v bool) {
		if v {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
	}
	b = append(b, "NUP logical content v1\x00"...)
	b = binfmt.AppendU32(b, uint32(len(contents)))
	for _, c := range contents {
		b = binfmt.AppendU64(b, c.TitleID)
		b = binfmt.AppendU32(b, c.Version)
		b = append(b, byte(c.Kind), byte(c.Flags))
		flag(c.ExeFS)
		flag(c.RomFS)
		str([]byte(c.Root))
		str(c.Metadata)
		b = binfmt.AppendU32(b, uint32(len(c.Directories)))
		for _, d := range c.Directories {
			str([]byte(d))
		}
		b = binfmt.AppendU64(b, uint64(len(c.Files)))
		for _, f := range c.Files {
			str([]byte(f.Path))
			b = binfmt.AppendU64(b, uint64(f.Size))
			b = append(b, f.SHA256[:]...)
			if len(b) >= Block {
				h.Write(b)
				b = b[:0]
			}
		}
	}
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// CompareContents reports the first difference between want and got.
func CompareContents(want, got []Content) error {
	if len(want) != len(got) {
		return fmt.Errorf("Logical content mismatch: %d packages instead of %d.", len(got), len(want))
	}
	for i := range want {
		w, g := &want[i], &got[i]
		label := fmt.Sprintf("package %d (%s %016X v%d)", i, KindName(w.Kind), w.TitleID, w.Version)
		mismatch := func(what string) error {
			return fmt.Errorf("Logical content mismatch: %s: %s differs.", label, what)
		}
		switch {
		case w.TitleID != g.TitleID || w.Version != g.Version || w.Kind != g.Kind:
			return mismatch("identity")
		case w.Flags != g.Flags:
			return mismatch("flags")
		case w.Root != g.Root:
			return mismatch("root directory name")
		case w.ExeFS != g.ExeFS || w.RomFS != g.RomFS:
			return mismatch("exefs/romfs record pointer")
		case !bytes.Equal(w.Metadata, g.Metadata):
			return mismatch("metadata")
		}
		if len(w.Directories) != len(g.Directories) {
			return mismatch("directory list")
		}
		for j := range w.Directories {
			if w.Directories[j] != g.Directories[j] {
				return mismatch("directory " + w.Directories[j])
			}
		}
		if len(w.Files) != len(g.Files) {
			return mismatch("file list")
		}
		for j := range w.Files {
			wf, gf := w.Files[j], g.Files[j]
			switch {
			case wf.Path != gf.Path:
				return mismatch("file " + wf.Path)
			case wf.Size != gf.Size:
				return mismatch("size of " + wf.Path)
			case wf.SHA256 != gf.SHA256:
				return mismatch("SHA-256 of " + wf.Path)
			}
		}
	}
	return nil
}
