package nup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/diskfree"
	"github.com/klauspost/compress/zstd"
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

type fpKey struct {
	size  int64
	first [32]byte
}

type seenKey struct {
	digest [32]byte
	size   int64
}

type extentInfo struct {
	off   int64
	size  int64
	codec int
}

type dedupResult struct {
	stream io.ReadCloser // open when the caller must write the extent
	first  []byte        // already-read prefix (min 64 KiB), or empty
	digest []byte        // full digest when pre-hashed (dup-check/reopen)
	extent *extentInfo   // non-nil when an exact duplicate was found
}

// Pack streams the given packages into output (port of nup_format.pack).
func Pack(ctx context.Context, packages []Package, output string, opts PackOptions) (res Result, err error) {
	defer binfmt.Recover(&err)

	level := opts.Level
	if level == 0 {
		level = 3
	}
	validatePackages(packages)
	binfmt.Require(level == 1 || level == 3 || level == 9, "Choose compression level 1, 3 or 9.")

	abs, err := filepath.Abs(output)
	if err != nil {
		return res, err
	}
	output = abs
	binfmt.Require(strings.ToLower(filepath.Ext(output)) == ".nup", "Output must end in .nup.")
	binfmt.Require(!pathExists(output), "Output already exists; choose a new filename.")
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return res, err
	}

	// Tree construction + name interning (mirrors pack's nested dicts).
	var dirs []dirNode
	var files []fileNode
	var children []childEntry
	var roots []int
	pool := []byte{}
	names := map[string]int{}
	intern := func(name string) (int, int) {
		data := []byte(name)
		key := string(data)
		if off, ok := names[key]; ok {
			return off, len(data)
		}
		off := len(pool)
		names[key] = off
		pool = append(pool, data...)
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
			index := len(dirs)
			dirs = append(dirs, dirNode{})
			start := len(children)
			namesSorted := sortedKeys(branch)
			children = append(children, make([]childEntry, len(namesSorted))...)
			for j, n := range namesSorted {
				value := branch[n]
				if sub, ok := value.(map[string]any); ok {
					children[start+j] = childEntry{isDir: true, index: visit(sub, n, depth+1)}
				} else {
					src := value.(*Source)
					children[start+j] = childEntry{isDir: false, index: len(files)}
					no, nl := intern(n)
					files = append(files, fileNode{nameOff: no, nameLen: nl, source: src})
				}
			}
			no, nl := intern(name)
			dirs[index] = dirNode{nameOff: no, nameLen: nl, first: start, count: len(namesSorted)}
			return index
		}
		roots = append(roots, visit(tree, "", 0))
	}

	binfmt.Require(len(dirs)+len(files) < int(none), "Too many NUP nodes.")

	pooloff := 48 + len(packages)*64 + len(dirs)*24 + len(files)*80 + len(children)*4
	tocsize := binfmt.Align(int64(pooloff+len(pool)), 16)
	binfmt.Require(tocsize <= 256*1024*1024, "NUP file table exceeds 256 MiB.")
	bloboff := binfmt.Align(128+tocsize, 16)

	var total int64
	for _, f := range files {
		total += f.source.size
	}
	binfmt.Require(uint64(total)+uint64(bloboff) <= 0xffffffffffffffff, "NUP payload exceeds its 64-bit size limit.")
	free, err := diskfree.Available(filepath.Dir(output))
	if err != nil {
		return res, err
	}
	binfmt.Require(free >= uint64(total)+uint64(bloboff)+32*1024*1024, "Not enough free space for the worst-case NUP size.")

	temp := output + "." + randomHex() + ".partial"
	defer func() { _ = os.Remove(temp) }()

	out, err := os.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return res, err
	}
	defer out.Close()

	if err := writeZeros(out, bloboff); err != nil {
		return res, err
	}

	seen := map[seenKey]extentInfo{}
	prefixes := map[fpKey]bool{}
	duplicateFiles := 0
	var done int64
	onRead := func(count int64) {
		if opts.Progress != nil {
			opts.Progress("Compressing / storing files", 3+76*float64(done+count)/float64(max64(total, 1)))
		}
	}

	for i := range files {
		binfmt.Check(ctx)
		f := &files[i]
		source := f.source
		if err := source.Validate(); err != nil {
			return res, err
		}
		dr := dedupStream(ctx, source, prefixes, seen, onRead)
		if dr.extent != nil {
			f.off = dr.extent.off
			f.stored = dr.extent.size
			f.digest = to32(dr.digest)
			f.codec = dr.extent.codec
			duplicateFiles++
		} else {
			cur, _ := out.Seek(0, io.SeekCurrent)
			start := binfmt.Align(cur, 16)
			var stored int64
			var digest []byte
			var codec int
			var extentErr error
			// Close the dedup stream even when writeExtent fails (Python's
			// `with dedup_stream(...)` contextmanager).
			func() {
				defer dr.stream.Close()
				if err := writeZeros(out, start-cur); err != nil {
					extentErr = err
					return
				}
				stored, digest, codec = writeExtent(ctx, out, source, dr.stream, dr.first, level, opts.LargeRaw, onRead, dr.digest)
			}()
			if extentErr != nil {
				return res, extentErr
			}
			if source.size == 0 {
				f.off, f.stored, f.digest, f.codec = 0, 0, to32(digest), 0
			} else {
				f.off = start
				f.stored = stored
				f.digest = to32(digest)
				f.codec = codec
				seen[seenKey{digest: to32(digest), size: source.size}] = extentInfo{off: start, size: stored, codec: codec}
			}
		}
		done += source.size
	}

	// Metadata extents.
	type metaExtent struct{ off, size int64 }
	meta := make([]metaExtent, len(packages))
	for i := range packages {
		p := &packages[i]
		data := tlvBytes(p.Metadata)
		binfmt.Require(len(data) <= 32*1024*1024, "Package metadata exceeds 32 MiB.")
		if len(data) != 0 {
			cur, _ := out.Seek(0, io.SeekCurrent)
			meta[i] = metaExtent{off: cur, size: int64(len(data))}
			if _, err := out.Write(data); err != nil {
				return res, err
			}
		}
	}

	end, _ := out.Seek(0, io.SeekCurrent)

	// TOC assembly.
	toc := make([]byte, tocsize)
	copy(toc[0:4], "TOC\x01")
	binfmt.PutU32(toc, 4, uint32(len(packages)))
	binfmt.PutU64(toc, 8, uint64(len(dirs)))
	binfmt.PutU64(toc, 16, uint64(len(files)))
	binfmt.PutU64(toc, 24, uint64(pooloff))
	binfmt.PutU64(toc, 32, uint64(len(pool)))
	at := 48
	for pi := range packages {
		p := &packages[pi]
		root := roots[pi]
		exefs := none
		romfs := none
		for _, target := range [2]string{"exefs", "romfs"} {
			d := dirs[root]
			for _, ch := range children[d.first : d.first+d.count] {
				if !ch.isDir {
					continue
				}
				cd := dirs[ch.index]
				wantOff, wantLen := internedName(names, target)
				if cd.nameOff == wantOff && cd.nameLen == wantLen {
					if target == "exefs" {
						exefs = uint32(ch.index)
					} else {
						romfs = uint32(ch.index)
					}
				}
			}
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
	for _, d := range dirs {
		binfmt.PutU32(toc, at+0, uint32(d.nameOff))
		binfmt.PutU32(toc, at+4, uint32(d.nameLen))
		binfmt.PutU32(toc, at+8, uint32(d.first))
		binfmt.PutU32(toc, at+12, uint32(d.count))
		at += 24
	}
	for _, f := range files {
		binfmt.PutU32(toc, at+0, uint32(f.nameOff))
		binfmt.PutU32(toc, at+4, uint32(f.nameLen))
		binfmt.PutU64(toc, at+8, uint64(f.source.size))
		binfmt.PutU64(toc, at+16, uint64(f.off))
		binfmt.PutU64(toc, at+24, uint64(f.stored))
		copy(toc[at+32:at+64], f.digest[:])
		toc[at+64] = byte(f.codec)
		at += 80
	}
	for _, ch := range children {
		node := uint32(ch.index)
		if !ch.isDir {
			node += uint32(len(dirs))
		}
		binfmt.PutU32(toc, at, node)
		at += 4
	}
	copy(toc[pooloff:pooloff+len(pool)], pool)

	// Identity + superblock.
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
	binfmt.PutU64(header, 16, uint64(tocsize))
	copy(header[24:56], binfmt.SHA256Sum(toc))
	binfmt.PutU64(header, 56, uint64(bloboff))
	binfmt.PutU64(header, 64, uint64(end))
	binfmt.PutU32(header, 72, uint32(len(packages)))
	copy(header[80:96], identityHash[:16])

	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return res, err
	}
	if _, err := out.Write(header); err != nil {
		return res, err
	}
	if _, err := out.Write(toc); err != nil {
		return res, err
	}
	if err := out.Sync(); err != nil {
		return res, err
	}

	if opts.Verify {
		a, err := Open(ctx, temp)
		if err != nil {
			return res, err
		}
		_, verr := a.Verify(ctx, func(label string, v float64) {
			if opts.Progress != nil {
				opts.Progress(label, 80+v*0.19)
			}
		})
		a.Close()
		if verr != nil {
			return res, verr
		}
	}

	for _, p := range packages {
		for _, src := range p.Files {
			if err := src.Validate(); err != nil {
				return res, err
			}
		}
	}
	if opts.SourceGuard != nil {
		if err := opts.SourceGuard(); err != nil {
			return res, err
		}
	}
	binfmt.Check(ctx)
	if err := commitFile(temp, output); err != nil {
		return res, err
	}
	if opts.Progress != nil {
		opts.Progress("NUP ready", 100)
	}
	return Result{
		Output:                output,
		Size:                  end,
		SourceSize:            total,
		SavedBytes:            total - end,
		Verified:              opts.Verify,
		Packages:              len(packages),
		DuplicateFilesSkipped: duplicateFiles,
	}, nil
}

// internedName returns the interned offset (or -1) and length of name.
func internedName(names map[string]int, name string) (int, int) {
	off, ok := names[name]
	if !ok {
		off = -1
	}
	return off, len(name)
}

func writeZeros(out *os.File, n int64) error {
	if n <= 0 {
		return nil
	}
	buf := make([]byte, 1<<20)
	for n > 0 {
		c := int64(len(buf))
		if n < c {
			c = n
		}
		if _, err := out.Write(buf[:c]); err != nil {
			return err
		}
		n -= c
	}
	return nil
}

func randomHex() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal enough to be unrecoverable in practice.
		return "0000000000"
	}
	return hex.EncodeToString(b)
}

func to32(b []byte) [32]byte {
	var out [32]byte
	copy(out[:], b)
	return out
}

// dedupStream samples the source once; only plausible duplicates get a full
// pre-hash (port of nup_format.dedup_stream).
func dedupStream(ctx context.Context, source *Source, prefixes map[fpKey]bool, seen map[seenKey]extentInfo, onRead func(int64)) *dedupResult {
	stream, err := source.Open()
	if err != nil {
		binfmt.Fail("%v", err)
	}
	first := make([]byte, min64(source.size, 64*1024))
	if _, err := io.ReadFull(stream, first); err != nil {
		stream.Close()
		binfmt.Fail("Short source read.")
	}
	fingerprint := fpKey{size: source.size, first: to32(binfmt.SHA256Sum(first))}
	if !prefixes[fingerprint] || source.size == 0 {
		prefixes[fingerprint] = true
		return &dedupResult{stream: stream, first: first}
	}

	h := sha256.New()
	h.Write(first)
	count := int64(len(first))
	onRead(count)
	buf := make([]byte, Block)
	for {
		binfmt.Check(ctx)
		n, rerr := stream.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			count += int64(n)
			binfmt.Require(count <= source.size, "Source grew during duplicate checking.")
			onRead(count)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			stream.Close()
			binfmt.Fail("%v", rerr)
		}
	}
	binfmt.Require(count == source.size, "Source shrank during duplicate checking.")
	if err := source.Validate(); err != nil {
		stream.Close()
		binfmt.Fail("%v", err)
	}
	digest := h.Sum(nil)
	if ext, ok := seen[seenKey{digest: to32(digest), size: source.size}]; ok {
		stream.Close()
		return &dedupResult{digest: digest, extent: &ext}
	}
	stream.Close()

	stream2, err := source.Open()
	if err != nil {
		binfmt.Fail("%v", err)
	}
	return &dedupResult{stream: stream2, digest: digest}
}

// writeExtent writes one file extent, compressing with zstd (or storing raw),
// with an early raw fallback (port of nup_format.write_extent).
func writeExtent(ctx context.Context, out *os.File, source *Source, stream io.ReadCloser, first []byte, level int, largeRaw bool, onRead func(int64), knownDigest []byte) (stored int64, digest []byte, codec int) {
	start, _ := out.Seek(0, io.SeekCurrent)
	codecBool := chooseCodec(source.size, largeRaw)
	h := sha256.New()
	var count int64
	failed := false
	var enc *zstd.Encoder
	if codecBool {
		if e, err := newZstdEncoder(out, level, source.size); err != nil {
			codecBool = false
		} else {
			enc = e
		}
	}

	buf := make([]byte, Block)
	var b []byte
	if len(first) > 0 {
		b = first
	} else {
		n, err := stream.Read(buf)
		if err != nil && err != io.EOF {
			binfmt.Fail("%v", err)
		}
		b = buf[:n]
	}
	for len(b) > 0 {
		binfmt.Check(ctx)
		count += int64(len(b))
		binfmt.Require(count <= source.size, "Source grew during packaging.")
		h.Write(b)
		if !failed {
			var werr error
			if enc != nil {
				_, werr = enc.Write(b)
			} else {
				_, werr = out.Write(b)
			}
			if werr != nil {
				failed = true
				enc = nil
			}
		}
		onRead(count)
		n, err := stream.Read(buf)
		if err != nil && err != io.EOF {
			binfmt.Fail("%v", err)
		}
		b = buf[:n]
		if enc != nil {
			cur, _ := out.Seek(0, io.SeekCurrent)
			if cur-start >= source.size {
				failed = true
				enc = nil
			}
		}
	}
	binfmt.Require(count == source.size, "Source shrank during packaging.")
	if enc != nil {
		if err := enc.Close(); err != nil {
			failed = true
		}
	}
	if err := source.Validate(); err != nil {
		binfmt.Fail("%v", err)
	}
	cur, _ := out.Seek(0, io.SeekCurrent)
	stored = cur - start
	digest = h.Sum(nil)
	if knownDigest != nil {
		binfmt.Require(bytes.Equal(knownDigest, digest), "Source changed after duplicate checking.")
	}

	if failed || (enc != nil && stored >= source.size) {
		if _, err := out.Seek(start, io.SeekStart); err != nil {
			binfmt.Fail("%v", err)
		}
		if err := out.Truncate(start); err != nil {
			binfmt.Fail("%v", err)
		}
		rh := sha256.New()
		var n int64
		original, err := source.Open()
		if err != nil {
			binfmt.Fail("%v", err)
		}
		defer original.Close()
		for {
			binfmt.Check(ctx)
			nread, rerr := original.Read(buf)
			if nread > 0 {
				if _, werr := out.Write(buf[:nread]); werr != nil {
					binfmt.Fail("%v", werr)
				}
				rh.Write(buf[:nread])
				n += int64(nread)
				onRead(n)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				binfmt.Fail("%v", rerr)
			}
		}
		if err := source.Validate(); err != nil {
			binfmt.Fail("%v", err)
		}
		binfmt.Require(n == source.size && bytes.Equal(rh.Sum(nil), digest), "Source changed during raw fallback.")
		stored = source.size
		codecBool = false
	}

	if codecBool {
		codec = 1
	}
	return stored, digest, codec
}

// newZstdEncoder builds a single-threaded streaming encoder that writes a
// known content size (so the frame verifies content_size == size) and forces
// single-segment frames for small inputs (EncodeAll path).
//
// Level mapping: 1→SpeedFastest, 3→SpeedDefault, 9→SpeedBetterCompression.
// Ratios differ from the C zstd backend but the emitted frames are standard
// and pass the same frame-validation rules.
func newZstdEncoder(out io.Writer, level int, size int64) (*zstd.Encoder, error) {
	var lvl zstd.EncoderLevel
	switch level {
	case 1:
		lvl = zstd.SpeedFastest
	case 3:
		lvl = zstd.SpeedDefault
	case 9:
		lvl = zstd.SpeedBetterCompression
	default:
		return nil, fmt.Errorf("unsupported compression level %d", level)
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(lvl),
		zstd.WithEncoderConcurrency(1),
		zstd.WithSingleSegment(true),
	)
	if err != nil {
		return nil, err
	}
	enc.ResetContentSize(out, size)
	return enc, nil
}
