package nup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/klauspost/compress/zstd"
)

type fileRecord struct {
	name   string
	size   int64
	off    int64
	stored int64
	digest [32]byte
	codec  int
	window int64 // the zstd frame's decoder window (codec 1)
}

type dirRecord struct {
	name  string
	first int
	count int
}

type archiveStamp struct {
	size  int64
	mtime int64
	info  os.FileInfo // identity of the file that was opened (nil for an archive in parts)
	parts []partStamp // every file read, in order (one for a whole file)
}

type extentKey struct{ off, end int64 }
type extentProps struct {
	size   int64
	digest [32]byte
	codec  int
}

type verifyKey struct {
	size   int64
	off    int64
	stored int64
	digest [32]byte
	codec  int
}

// Archive is an opened, parsed NUP v1 archive.
type Archive struct {
	path     string
	f        archiveFile
	stamp    archiveStamp
	packages []Package
	files    []fileRecord
	dirs     []dirRecord
	trees    []map[string]int
	stats    Stats

	headerSize, headerVersion int
	records                   [][3]uint32 // root, exefs, romfs per package
	metaRaw                   [][]byte    // metadata blocks as stored

	// Tolerated deviations (ARCHITECTURE.md § Deviations 7 and 8).
	metaBeforeBlob, emptyOffsets int
}

type archiveFile interface {
	io.ReaderAt
	io.Closer
}

// Open parses a NUP archive (port of nup_format.Archive.__init__). path is
// a whole file, the first part of an archive in parts, or a folder holding
// one archive's parts (see FindParts).
func Open(ctx context.Context, path string) (a *Archive, err error) {
	parts, _, err := FindParts(path)
	if err != nil {
		return nil, err
	}
	return openParts(ctx, path, parts)
}

// openParts parses the archive made of parts, read in order as one file.
func openParts(ctx context.Context, path string, parts []string) (a *Archive, err error) {
	defer binfmt.Recover(&err)
	var f archiveFile
	var stamps []partStamp
	if len(parts) == 1 {
		file, err := os.Open(parts[0])
		if err != nil {
			return nil, err
		}
		st, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, err
		}
		f, stamps = file, []partStamp{{path: parts[0], size: st.Size(), mtime: st.ModTime().UnixNano(), info: st}}
	} else {
		r, st, err := openPartsReader(parts)
		if err != nil {
			return nil, err
		}
		f, stamps = r, st
	}
	success := false
	defer func() {
		if !success {
			f.Close()
		}
	}()
	a = &Archive{path: path, f: f}
	a.stamp = archiveStamp{parts: stamps}
	for _, p := range stamps {
		a.stamp.size += p.size
	}
	if len(stamps) == 1 {
		a.stamp.mtime, a.stamp.info = stamps[0].mtime, stamps[0].info
	}
	a.parse(ctx)
	success = true
	return a, nil
}

// Parts lists the files the archive was read from, in order.
func (a *Archive) Parts() []string {
	out := make([]string, len(a.stamp.parts))
	for i, p := range a.stamp.parts {
		out[i] = p.path
	}
	return out
}

// Close releases the archive file handle.
func (a *Archive) Close() error {
	if a.f != nil {
		err := a.f.Close()
		a.f = nil
		return err
	}
	return nil
}

// Packages returns the parsed package set.
func (a *Archive) Packages() []Package { return a.packages }

// Deviations describes what this archive does that Boxcar accepts but the
// NUP spec or the reference reader does not; empty for compliant archives.
// Rewriting the archive (Pack, Optimize) removes them.
func (a *Archive) Deviations() []string {
	var out []string
	if a.emptyOffsets > 0 {
		out = append(out, fmt.Sprintf("%d empty file(s) have a nonzero extent offset (written by KaiDump); "+
			"the NUP spec requires 0 and strict readers reject the archive", a.emptyOffsets))
	}
	if a.metaBeforeBlob > 0 {
		out = append(out, fmt.Sprintf("metadata of %d package(s) lies before the blob region, "+
			"which the NSP-Box reader rejects", a.metaBeforeBlob))
	}
	return out
}

func (a *Archive) readAt(off, size int64) []byte {
	binfmt.Require(off >= 0 && off <= a.stamp.size && size >= 0 && size <= a.stamp.size-off, "Archive offset is out of bounds.")
	buf := make([]byte, size)
	n, err := a.f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		binfmt.Fail("%v", err)
	}
	binfmt.Require(int64(n) == size, "Truncated archive.")
	return buf
}

func (a *Archive) unchanged() error {
	for _, p := range a.stamp.parts {
		st, err := os.Stat(p.path)
		if err != nil {
			return err
		}
		if !os.SameFile(st, p.info) || st.Size() != p.size || st.ModTime().UnixNano() != p.mtime {
			return errors.New("NUP changed while reading.")
		}
	}
	return nil
}

// satMul/satAdd clamp to MaxUint64 so hostile 64-bit record counts cannot
// overflow the table-array arithmetic (Python uses arbitrary precision).
func satMul(x, y uint64) uint64 {
	if x != 0 && y > ^uint64(0)/x {
		return ^uint64(0)
	}
	return x * y
}

func satAdd(x, y uint64) uint64 {
	if ^uint64(0)-x < y {
		return ^uint64(0)
	}
	return x + y
}

func (a *Archive) parse(ctx context.Context) {
	binfmt.Check(ctx)
	header := a.readAt(0, 128)
	hs := int(binfmt.U16At(header, 4))
	hv := int(binfmt.U16At(header, 6))
	to := int64(binfmt.U64At(header, 8))
	ts := int64(binfmt.U64At(header, 16))
	th := header[24:56]
	bo := int64(binfmt.U64At(header, 56))
	fs := int64(binfmt.U64At(header, 64))
	pc := int(binfmt.U32At(header, 72))
	flags := binfmt.U32At(header, 76)
	identity := header[80:96]
	res := header[96:128]

	binfmt.Require(bytes.Equal(header[0:4], []byte("NUP\x01")) && hs >= 128 && hv >= 1 && to == int64(hs),
		"Not a supported NUP v1 archive.")
	binfmt.Require(flags == 0 && allZero(res) && fs == a.stamp.size && bo%16 == 0 && fs >= bo && bo >= to+ts && pc > 0 && pc <= 4096,
		"Invalid NUP superblock.")
	binfmt.Require(int64(48) <= ts && ts <= 256*1024*1024, "Invalid or excessive NUP table size.")
	a.headerSize, a.headerVersion = hs, hv

	toc := a.readAt(to, ts)
	binfmt.Require(bytes.Equal(binfmt.SHA256Sum(toc), th), "NUP table SHA-256 mismatch.")

	np := int(binfmt.U32At(toc, 4))
	nd := binfmt.U64At(toc, 8)
	nf := binfmt.U64At(toc, 16)
	po := binfmt.U64At(toc, 24)
	ps := binfmt.U64At(toc, 32)
	tr := binfmt.U64At(toc, 40)
	binfmt.Require(bytes.Equal(toc[0:4], []byte("TOC\x01")) && np == pc && tr == 0, "Invalid NUP table header.")

	arrays := satAdd(48, satAdd(satMul(uint64(pc), 64), satAdd(satMul(nd, 24), satMul(nf, 80))))
	binfmt.Require(arrays <= po && po <= uint64(ts) && ps <= uint64(ts)-po && (po-arrays)%4 == 0,
		"NUP table arrays exceed their region.")

	pool := toc[int(po) : int(po)+int(ps)]
	poolSize := int(ps)
	slots := int((po - arrays) / 4)
	ndInt := int(nd)
	nfInt := int(nf)

	name := func(off, size int, root bool) string {
		binfmt.Require(off <= poolSize && size <= poolSize-off, "Invalid NUP name range.")
		b := pool[off : off+size]
		binfmt.Require(validUTF8(b), "Invalid UTF-8 in NUP name.")
		s := string(b)
		if s != "" || !root {
			safeName(s, false)
		}
		return s
	}

	regions := [][2]int64{}
	records := [][3]uint32{}
	at := 48
	for i := 0; i < pc; i++ {
		tid := binfmt.U64At(toc, at+0)
		root := binfmt.U32At(toc, at+8)
		ex := binfmt.U32At(toc, at+12)
		rom := binfmt.U32At(toc, at+16)
		ver := binfmt.U32At(toc, at+20)
		kind := toc[at+24]
		pflags := toc[at+25]
		r := binfmt.U16At(toc, at+26)
		r2 := binfmt.U32At(toc, at+28)
		mo := binfmt.U64At(toc, at+32)
		ms := uint64(binfmt.U32At(toc, at+40))
		r3 := toc[at+44 : at+64]
		at += 64

		binfmt.Require(int64(root) < int64(nd) &&
			(ex == none || int64(ex) < int64(nd)) &&
			(rom == none || int64(rom) < int64(nd)) &&
			r == 0 && r2 == 0 && allZero(r3), "Invalid NUP package record.")
		binfmt.Require((mo == 0) == (ms == 0), "Invalid metadata extent.")
		binfmt.Require(ms <= 32*1024*1024, "Package metadata exceeds 32 MiB.")
		var metadata []TLV
		var raw []byte
		if ms != 0 {
			// Metadata may live anywhere in the file as long as it stays
			// in-bounds and does not overlap other extents (checked below).
			// The reference implementation additionally requires blobOff <=
			// metadataOff, which rejects third-party packers that place
			// metadata in the TOC-to-blob gap (observed in the wild); we
			// accept them. Documented deviation — see ARCHITECTURE.md.
			binfmt.Require(mo <= uint64(fs) && ms <= uint64(fs)-mo, "Invalid package metadata bounds.")
			if mo < uint64(bo) {
				a.metaBeforeBlob++
			}
			regions = append(regions, [2]int64{int64(mo), int64(mo) + int64(ms)})
			var err error
			raw = a.readAt(int64(mo), int64(ms))
			metadata, err = parseTlvs(raw)
			if err != nil {
				binfmt.Fail("%v", err)
			}
		}
		a.packages = append(a.packages, Package{
			TitleID: tid, Version: ver, Kind: int(kind), Flags: int(pflags),
			Files: map[string]*Source{}, Metadata: metadata,
		})
		records = append(records, [3]uint32{root, ex, rom})
		a.metaRaw = append(a.metaRaw, raw)
	}
	a.records = records

	validatePackages(a.packages)

	var identityBuf []byte
	for _, p := range a.packages {
		identityBuf = binfmt.AppendU64(identityBuf, p.TitleID)
		identityBuf = binfmt.AppendU32(identityBuf, p.Version)
		identityBuf = append(identityBuf, byte(p.Kind))
	}
	binfmt.Require(bytes.Equal(binfmt.SHA256Sum(identityBuf)[:16], identity), "NUP identity UUID mismatch.")

	roots := map[uint32]bool{}
	for _, rec := range records {
		roots[rec[0]] = true
	}
	binfmt.Require(len(roots) == pc, "Packages share a root directory.")

	at = 48 + pc*64
	for i := 0; i < ndInt; i++ {
		no := int(binfmt.U32At(toc, at+0))
		nl := int(binfmt.U32At(toc, at+4))
		first := int(binfmt.U32At(toc, at+8))
		count := int(binfmt.U32At(toc, at+12))
		r := binfmt.U64At(toc, at+16)
		at += 24
		binfmt.Require(r == 0 && first <= slots && count <= slots-first, "Invalid directory child range.")
		a.dirs = append(a.dirs, dirRecord{name: name(no, nl, roots[uint32(i)]), first: first, count: count})
	}

	extentIDs := map[extentKey]extentProps{}
	for i := 0; i < nfInt; i++ {
		no := int(binfmt.U32At(toc, at+0))
		nl := int(binfmt.U32At(toc, at+4))
		size := binfmt.U64At(toc, at+8)
		off := binfmt.U64At(toc, at+16)
		stored := binfmt.U64At(toc, at+24)
		digestBytes := toc[at+32 : at+64]
		codec := toc[at+64]
		cf := toc[at+65]
		r := binfmt.U16At(toc, at+66)
		r3 := toc[at+68 : at+80]
		at += 80

		binfmt.Require((codec == 0 || codec == 1) && cf == 0 && r == 0 && allZero(r3), "Unknown codec or file flags.")
		var window int64
		if size == 0 {
			// The reference also requires off == 0. KaiDump records the blob
			// cursor as an empty file's offset; nothing is read from it, so
			// any offset is accepted. Documented deviation — see ARCHITECTURE.md.
			binfmt.Require(stored == 0 && codec == 0, "Invalid empty-file extent.")
			if off != 0 {
				a.emptyOffsets++
			}
		} else {
			binfmt.Require(uint64(bo) <= off && off%16 == 0 && stored > 0 && off <= uint64(fs) && stored <= uint64(fs)-off,
				"File extent out of bounds.")
			binfmt.Require(codec != 0 || stored == size, "Raw file size mismatch.")
			key := extentKey{off: int64(off), end: int64(off) + int64(stored)}
			props := extentProps{size: int64(size), digest: to32(digestBytes), codec: int(codec)}
			if prev, exists := extentIDs[key]; exists {
				binfmt.Require(prev == props, "Conflicting shared extent.")
			} else {
				regions = append(regions, [2]int64{int64(off), int64(off) + int64(stored)})
				extentIDs[key] = props
			}
			if codec == 1 {
				offI := int64(off)
				storedI := int64(stored)
				sizeI := int64(size)
				head := a.readAt(offI, min64(storedI, 18))
				params, err := parseZstdFrameHead(head)
				if err != nil {
					binfmt.Fail("%v", err)
				}
				binfmt.Require(params.contentSize == sizeI && params.dictID == 0, "Zstandard frame size/dictionary is unsupported.")
				binfmt.Require(params.windowSize <= MaxWindow, "Zstandard frame requires a decoder window larger than 1 GiB.")
				window = params.windowSize
				cursor := offI + int64(params.headerSize)
				for {
					binfmt.Check(ctx)
					binfmt.Require(cursor+3 <= offI+storedI, "Truncated Zstandard block header.")
					blk := a.readAt(cursor, 3)
					block := int64(uint32(blk[0]) | uint32(blk[1])<<8 | uint32(blk[2])<<16)
					cursor += 3
					last := block & 1
					kind := (block >> 1) & 3
					blocksize := block >> 3
					binfmt.Require(kind != 3 && blocksize <= 128*1024, "Invalid Zstandard block.")
					if kind == 1 {
						cursor++
					} else {
						cursor += blocksize
					}
					binfmt.Require(cursor <= offI+storedI, "Truncated Zstandard block payload.")
					if last == 1 {
						break
					}
				}
				if params.hasChecksum {
					cursor += 4
				}
				binfmt.Require(cursor == offI+storedI, "NUP extent must contain exactly one Zstandard frame.")
			}
		}
		a.files = append(a.files, fileRecord{name: name(no, nl, false), size: int64(size), off: int64(off), stored: int64(stored), digest: to32(digestBytes), codec: int(codec), window: window})
	}

	sort.Slice(regions, func(i, j int) bool {
		if regions[i][0] != regions[j][0] {
			return regions[i][0] < regions[j][0]
		}
		return regions[i][1] < regions[j][1]
	})
	for i := 1; i < len(regions); i++ {
		binfmt.Require(regions[i-1][1] <= regions[i][0], "Overlapping NUP extents/metadata.")
	}

	children := make([]uint32, slots)
	for i := 0; i < slots; i++ {
		children[i] = binfmt.U32At(toc, int(arrays)+i*4)
	}

	used := map[int]bool{}
	usedSlots := map[int]bool{}
	for pIdx := range a.packages {
		root := int(records[pIdx][0])
		ex := records[pIdx][1]
		rom := records[pIdx][2]
		paths := map[string]int{}
		directories := map[string]bool{}
		directoryPaths := map[int]string{}

		var walk func(node int, prefix string, depth int)
		walk = func(node int, prefix string, depth int) {
			binfmt.Check(ctx)
			binfmt.Require(!used[node] && depth < 256, "Cyclic, shared or overly deep directory tree.")
			used[node] = true
			directoryPaths[node] = prefix
			if prefix != "" {
				directories[prefix] = true
			}
			d := a.dirs[node]
			var previous []byte
			for slot := d.first; slot < d.first+d.count; slot++ {
				binfmt.Require(!usedSlots[slot], "Overlapping child slot ranges.")
				usedSlots[slot] = true
				child := int(children[slot])
				binfmt.Require(child < ndInt+nfInt, "Invalid child node ID.")
				var label string
				if child < ndInt {
					label = a.dirs[child].name
				} else {
					label = a.files[child-ndInt].name
				}
				raw := []byte(label)
				safeName(label, false)
				binfmt.Require(previous == nil || bytes.Compare(previous, raw) < 0, "Child names are duplicate or unsorted.")
				previous = raw
				path := label
				if prefix != "" {
					path = prefix + "/" + label
				}
				if child < ndInt {
					walk(child, path, depth+1)
				} else {
					binfmt.Require(!used[child], "Shared file node.")
					used[child] = true
					paths[path] = child - ndInt
				}
			}
		}
		walk(root, "", 0)

		if ex != none {
			binfmt.Require(directoryPaths[int(ex)] == "exefs", "Package content directory does not match its tree.")
		}
		if rom != none {
			binfmt.Require(directoryPaths[int(rom)] == "romfs", "Package content directory does not match its tree.")
		}
		p := &a.packages[pIdx]
		for d := range directories {
			p.Directories = append(p.Directories, d)
		}
		sort.Strings(p.Directories)
		p.rootName = a.dirs[root].name
		if ex == none && directories["exefs"] {
			p.unsetDirs |= 1
		}
		if rom == none && directories["romfs"] {
			p.unsetDirs |= 2
		}
		for path, index := range paths {
			idx := index
			p.Files[path] = &Source{
				size: a.files[idx].size,
				opener: func() (io.ReadCloser, error) {
					return a.openFile(idx)
				},
				validator: func() error { return a.unchanged() },
				archive:   a,
				index:     idx,
			}
		}
		a.trees = append(a.trees, paths)
	}
	binfmt.Require(len(used) == ndInt+nfInt && len(usedSlots) == slots, "Unreferenced NUP nodes or child slots.")

	var uncompressed int64
	compressed, raw := 0, 0
	for _, f := range a.files {
		uncompressed += f.size
		if f.codec == 1 {
			compressed++
		} else {
			raw++
		}
	}
	var storedSum int64
	for k := range extentIDs {
		storedSum += k.end - k.off
	}
	a.stats = Stats{
		"packages":         pc,
		"files":            nfInt,
		"uncompressed":     uncompressed,
		"stored":           storedSum,
		"archive_size":     fs,
		"compressed_files": compressed,
		"raw_files":        raw,
		"unique_extents":   len(extentIDs),
	}
}

type sliceReadCloser struct {
	*io.SectionReader
	f *os.File
}

func (s *sliceReadCloser) Close() error { return s.f.Close() }

type wrapReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (w *wrapReadCloser) Close() error {
	var first error
	for _, c := range w.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// openFile returns a bounded, re-openable reader over one file's extent
// (port of nup_format.Archive.open_file).
func (a *Archive) openFile(index int) (io.ReadCloser, error) {
	rec := a.files[index]
	f, err := os.Open(a.path)
	if err != nil {
		return nil, err
	}
	sr := &sliceReadCloser{SectionReader: io.NewSectionReader(f, rec.off, rec.stored), f: f}
	if rec.codec == 1 {
		dec, err := zstd.NewReader(sr,
			zstd.WithDecoderMaxWindow(MaxWindow),
			zstd.WithDecoderConcurrency(1),
		)
		if err != nil {
			sr.Close()
			return nil, err
		}
		rc := dec.IOReadCloser()
		return &wrapReadCloser{Reader: rc, closers: []io.Closer{rc, sr}}, nil
	}
	return sr, nil
}

// streamer is the read buffer and zstd decoder streamFile reuses from one
// file to the next; archives hold tens of thousands of small files.
type streamer struct {
	buf []byte
	dec *zstd.Decoder
}

func (s *streamer) Close() {
	if s.dec != nil {
		s.dec.Close()
	}
}

// streamFile decompresses file index, hashes it, and streams it to w (nil
// discards), invoking perChunk after each output chunk. It reads through the
// archive's own handle with positioned reads (safe from several goroutines);
// a file replaced meanwhile is caught by unchanged().
func (a *Archive) streamFile(ctx context.Context, s *streamer, index int, w io.Writer, perChunk func(n int)) error {
	rec := a.files[index]
	var src io.Reader = io.NewSectionReader(a.f, rec.off, rec.stored)
	if rec.codec == 1 {
		if s.dec == nil {
			dec, err := zstd.NewReader(nil,
				zstd.WithDecoderMaxWindow(MaxWindow),
				zstd.WithDecoderConcurrency(1),
			)
			if err != nil {
				return err
			}
			s.dec = dec
		}
		if err := s.dec.Reset(src); err != nil {
			// A failed Reset leaves the decoder in an unknown state; drop it
			// so the next file gets a fresh one.
			s.dec.Close()
			s.dec = nil
			return err
		}
		src = s.dec
	}
	if s.buf == nil {
		s.buf = make([]byte, Block)
	}
	buf := s.buf
	h := sha256.New()
	var count int64
	for {
		binfmt.Check(ctx)
		n, rerr := src.Read(buf)
		if n > 0 {
			count += int64(n)
			binfmt.Require(count <= rec.size, "Decompressed file exceeds its declared size.")
			h.Write(buf[:n])
			if w != nil {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if perChunk != nil {
				perChunk(n)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	binfmt.Require(count == rec.size && bytes.Equal(h.Sum(nil), rec.digest[:]), "File SHA-256 mismatch: %s", rec.name)
	return nil
}

// FileChunks streams the hash-verifying decompressed contents of one file to w.
func (a *Archive) FileChunks(ctx context.Context, index int, w io.Writer) (err error) {
	defer binfmt.Recover(&err)
	binfmt.Require(index >= 0 && index < len(a.files), "Invalid file index.")
	s := &streamer{}
	defer s.Close()
	return a.streamFile(ctx, s, index, w, nil)
}

// Verify re-hashes every unique extent and returns the stats (port of
// nup_format.Archive.verify).
func (a *Archive) Verify(ctx context.Context, progress binfmt.Progress) (stats Stats, err error) {
	defer binfmt.Recover(&err)
	total := a.stats["uncompressed"].(int64)
	// Each distinct extent is checked once; files sharing it count as done.
	var todo []int
	verified := map[verifyKey]bool{}
	var done int64
	for i, f := range a.files {
		key := verifyKey{size: f.size, off: f.off, stored: f.stored, digest: f.digest, codec: f.codec}
		if verified[key] {
			done += f.size
			continue
		}
		verified[key] = true
		todo = append(todo, i)
	}
	// Extents are hashed by up to one worker per CPU, sharing the process-
	// wide CPU tokens with the optimizer. Of the failures found before the
	// others stop, the one earliest in file order is reported.
	var mu sync.Mutex
	report := func(n int64) {
		mu.Lock()
		defer mu.Unlock()
		done += n
		if progress != nil {
			progress("Verifying NUP files", 100*float64(done)/float64(max64(total, 1)))
		}
	}
	vctx, cancel := context.WithCancel(ctx)
	defer cancel()
	next := make(chan int)
	failAt, failErr := len(a.files), error(nil)
	var wg sync.WaitGroup
	// Each worker keeps a decoder (for the largest window it may meet) and
	// a read buffer, reserved from the working-memory budget.
	var window int64
	for _, i := range todo {
		if a.files[i].codec == 1 {
			window = max64(window, a.files[i].window)
		}
	}
	perWorker := int64(Block)
	if window > 0 {
		perWorker += decoderCost(window)
	}
	workers, _, reserved, rerr := memBudget.reserveFlexible(ctx, 0, perWorker,
		min(runtime.GOMAXPROCS(0), max(len(todo), 1)), 0, 0)
	if rerr != nil {
		binfmt.Check(ctx)
		return nil, rerr
	}
	defer memBudget.release(reserved)
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &streamer{}
			defer s.Close()
			for i := range next {
				err := cpuTokens.acquire(vctx, 1)
				if err == nil {
					err = func() (err error) {
						defer binfmt.Recover(&err)
						return a.streamFile(vctx, s, i, nil, func(n int) { report(int64(n)) })
					}()
					cpuTokens.release(1)
				}
				if err != nil {
					mu.Lock()
					if i < failAt && !(errors.Is(err, context.Canceled) && ctx.Err() == nil) {
						failAt, failErr = i, err
					}
					mu.Unlock()
					cancel()
				}
			}
		}()
	}
feed:
	for _, i := range todo {
		select {
		case next <- i:
		case <-vctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	binfmt.Check(ctx)
	if failErr != nil {
		return nil, failErr
	}
	report(0)
	if err := a.unchanged(); err != nil {
		return nil, err
	}
	out := Stats{}
	for k, v := range a.stats {
		out[k] = v
	}
	out["verified"] = true
	return out, nil
}

// Inspect returns the archive stats and per-package info (port of
// nup_format.Archive.inspect).
func (a *Archive) Inspect(ctx context.Context) (stats Stats, infos []PackageInfo, err error) {
	defer binfmt.Recover(&err)
	binfmt.Check(ctx)
	stats = Stats{}
	for k, v := range a.stats {
		stats[k] = v
	}
	for i, p := range a.packages {
		types := make([]string, len(p.Metadata))
		for j, t := range p.Metadata {
			types[j] = fmt.Sprintf("%04X", t.Kind)
		}
		title := ""
		for _, t := range p.Metadata {
			if t.Kind == 5 {
				title = strings.ToValidUTF8(string(t.Data), "\uFFFD")
				break
			}
		}
		infos = append(infos, PackageInfo{
			Index:         i,
			Kind:          p.Kind,
			TitleID:       p.TitleID,
			Version:       p.Version,
			Flags:         uint32(p.Flags),
			Files:         len(p.Files),
			MetadataTypes: types,
			Title:         title,
		})
	}
	return stats, infos, nil
}
