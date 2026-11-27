package nup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// NUP archives kept in parts follow nx2-flow's convention (docs/layout.md,
// "A NUP file in parts"; loader/runtime/nupparts.h is its one definition,
// shared by the loader and the menu). A FAT32 card takes no file of 4 GiB or
// more, so a game's file is cut into pieces with whatever tool is at hand.
// The parts are the whole file's bytes in order, of any sizes, and their
// names say which they are (the whole file being "Game.nup"):
//
//	Game.nup.001  Game.nup.002   a number as the last ending (0 or 1 first, up to four digits)
//	Game.nup.part1               "part" and a number
//	Game.nup.aa  Game.nup.ab     letters, after ".nup" only
//	Game.nu0  Game.nu1           the ending's last letter made a number
//	Game.part1.nup  Game.001.nup the number before the ending
//	<folder>/00  <folder>/01     nameless parts: the folder is the game
//
// One game's parts share the name and the way, follow one another from 0 or
// 1 with none missing, and are at least two (a folder of nameless parts may
// hold one). A name that looks like a part without such company is a file
// of its own. This file ports those rules so Boxcar reads exactly the games
// nx2-flow reads, and writes parts the way nx2-flow's own file receiver
// does: "<name>.001", "<name>.002", ... all but the last of one size.

// MaxParts is the most parts nx2-flow's loader opens (NUP_PARTS_MAX).
const MaxParts = 64

// FAT32PartSize is the part size nx2-flow's receiver writes for a card that
// cannot take a file whole (kPartSize, 4 GiB - 64 KiB): FAT32 files end
// just below 4 GiB.
const FAT32PartSize = 0xFFFF0000

// MinPartSize is the smallest part size Boxcar writes.
const MinPartSize = 4096

const partIndexLimit = 1024 // nupparts.h LIMIT

type partKind int

const (
	partNone partKind = iota
	partNumber
	partWord
	partLetters
	partNux
	partMid
	partMidWord
	partBare
)

type partName struct {
	kind  partKind
	index int
	stem  string // what one game's parts have in common
}

func partNumberOf(s string) int { // 1-4 digits and nothing else, or -1
	if len(s) < 1 || len(s) > 4 {
		return -1
	}
	v := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return -1
		}
		v = v*10 + int(s[i]-'0')
	}
	return v
}

func partWordOf(s string) int { // "part" and a number, or -1
	if len(s) > 4 && strings.EqualFold(s[:4], "part") {
		return partNumberOf(s[4:])
	}
	return -1
}

func endsNup(s string) bool { return len(s) > 4 && strings.EqualFold(s[len(s)-4:], ".nup") }

// parsePartName reads a file name as a part's (nup_part_name).
func parsePartName(name string) (partName, bool) {
	if name == "" || name[0] == '.' {
		return partName{}, false
	}
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		if v := partNumberOf(name); v >= 0 {
			return partName{kind: partBare, index: v}, true
		}
		return partName{}, false
	}
	base, end := name[:dot], name[dot+1:]
	if v := partNumberOf(end); v >= 0 {
		return partName{partNumber, v, base}, true
	}
	if v := partWordOf(end); v >= 0 {
		return partName{partWord, v, base}, true
	}
	if len(end) >= 2 && len(end) <= 3 && endsNup(base) {
		v := 0
		for i := 0; i < len(end); i++ {
			if end[i] < 'a' || end[i] > 'z' {
				return partName{}, false
			}
			v = v*26 + int(end[i]-'a')
		}
		return partName{partLetters, v, base}, true
	}
	if len(end) == 3 && strings.EqualFold(end[:2], "nu") && end[2] >= '0' && end[2] <= '9' {
		return partName{partNux, int(end[2] - '0'), base}, true
	}
	if strings.EqualFold(end, "nup") && dot > 0 {
		mid := strings.LastIndexByte(base, '.')
		if mid <= 0 {
			return partName{}, false
		}
		m := base[mid+1:]
		if v := partNumberOf(m); v >= 0 {
			return partName{partMid, v, base[:mid]}, true
		}
		if v := partWordOf(m); v >= 0 {
			return partName{partMidWord, v, base[:mid]}, true
		}
	}
	return partName{}, false
}

// wholeName is the name of the file a part belongs to ("Game.nup").
func (p partName) wholeName(dir string) string {
	switch p.kind {
	case partBare:
		name := filepath.Base(dir)
		if !endsNup(name) {
			name += ".nup"
		}
		return name
	case partNux, partMid, partMidWord:
		return p.stem + ".nup"
	}
	if !endsNup(p.stem) {
		return p.stem + ".nup"
	}
	return p.stem
}

type partGroup struct {
	names  map[int]string
	lowest int
	others int // files that are a game's whole file or look like another game's parts
	dup    bool
}

// group collects the entries of dir that are parts of the same game as
// like (nup__group); with like.kind == partNone the first named part met
// decides.
func group(dir string, like *partName) (partGroup, error) {
	g := partGroup{names: map[int]string{}, lowest: partIndexLimit}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return g, err
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) >= 256 {
			continue
		}
		p, ok := parsePartName(name)
		if !ok {
			if endsNup(name) {
				g.others++
			}
			continue
		}
		if like.kind == partNone && p.kind != partBare {
			*like = p
		}
		if p.kind != like.kind || p.index >= partIndexLimit || !strings.EqualFold(p.stem, like.stem) {
			if p.kind != partBare {
				g.others++
			}
			continue
		}
		if _, seen := g.names[p.index]; seen {
			g.dup = true // "Game.nup.1" and "Game.nup.001": not a game's parts
			continue
		}
		g.names[p.index] = name
		if p.index < g.lowest {
			g.lowest = p.index
		}
	}
	return g, nil
}

func (g partGroup) run() int {
	n := 0
	for {
		if _, ok := g.names[g.lowest+n]; !ok {
			return n
		}
		n++
	}
}

// ErrLaterPart reports a path that is a part of a game other than its first.
var ErrLaterPart = errors.New("this file is a later part of a NUP in parts; open its first part")

// FindParts returns the files a path stands for, in order: the path itself
// for a whole file, every part for a game's first part, or the parts in a
// folder that holds one game's parts (nup_parts_find). name is the whole
// file's name ("Game.nup"). A later part returns ErrLaterPart.
func FindParts(path string) (parts []string, name string, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	var dir string
	var like partName
	var g partGroup
	if st.IsDir() {
		dir = path
		like = partName{kind: partBare}
		if g, err = group(dir, &like); err != nil {
			return nil, "", err
		}
		if len(g.names) == 0 || g.dup {
			like = partName{}
			if g, err = group(dir, &like); err != nil {
				return nil, "", err
			}
			if len(g.names) < 2 || g.others > 0 || g.dup {
				return nil, "", fmt.Errorf("%s is not a NUP in parts (nameless parts 00, 01, ... or one game's named parts)", path)
			}
		}
		if g.lowest > 1 || g.run() != len(g.names) {
			return nil, "", fmt.Errorf("%s: the parts must be numbered from 0 or 1 with none missing", path)
		}
	} else {
		dir = filepath.Dir(path)
		base := filepath.Base(path)
		p, ok := parsePartName(base)
		if ok && p.kind == partBare {
			return FindParts(dir)
		}
		if !ok {
			return []string{path}, base, nil
		}
		like = p
		if g, err = group(dir, &like); err != nil {
			return nil, "", err
		}
		run := g.run()
		if g.dup || len(g.names) < 2 || g.lowest > 1 || run < 2 || p.index >= g.lowest+run {
			return []string{path}, base, nil // looks like a part, but is a file of its own
		}
		if p.index != g.lowest {
			return nil, "", ErrLaterPart
		}
	}
	n := g.run()
	if n > MaxParts {
		return nil, "", fmt.Errorf("%s: %d parts; nx2-flow opens at most %d", path, n, MaxParts)
	}
	for k := g.lowest; k < g.lowest+n; k++ {
		p := filepath.Join(dir, g.names[k])
		if pst, err := os.Stat(p); err != nil {
			return nil, "", err
		} else if !pst.Mode().IsRegular() {
			return nil, "", fmt.Errorf("%s is not a file", p)
		}
		parts = append(parts, p)
	}
	return parts, like.wholeName(dir), nil
}

// partStamp is one opened part's identity, size and modification time.
type partStamp struct {
	path  string
	size  int64
	mtime int64
	info  os.FileInfo
}

// partsReader reads parts as the one file they are.
type partsReader struct {
	files []*os.File
	ends  []int64 // cumulative end offset of each part
}

func openPartsReader(paths []string) (*partsReader, []partStamp, error) {
	r := &partsReader{}
	var stamps []partStamp
	var end int64
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			r.Close()
			return nil, nil, err
		}
		r.files = append(r.files, f)
		st, err := f.Stat()
		if err != nil {
			r.Close()
			return nil, nil, err
		}
		if !st.Mode().IsRegular() {
			r.Close()
			return nil, nil, fmt.Errorf("%s is not a file", p)
		}
		end += st.Size()
		r.ends = append(r.ends, end)
		stamps = append(stamps, partStamp{path: p, size: st.Size(), mtime: st.ModTime().UnixNano(), info: st})
	}
	return r, stamps, nil
}

func (r *partsReader) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	k := sort.Search(len(r.ends), func(i int) bool { return r.ends[i] > off })
	n := 0
	for len(b) > 0 && k < len(r.files) {
		start := int64(0)
		if k > 0 {
			start = r.ends[k-1]
		}
		want := b
		if left := r.ends[k] - off; int64(len(want)) > left {
			want = want[:left]
		}
		m, err := r.files[k].ReadAt(want, off-start)
		n += m
		off += int64(m)
		b = b[m:]
		if m < len(want) {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF // a part shrank
			}
			return n, err
		}
		k++
	}
	if len(b) > 0 {
		return n, io.EOF
	}
	return n, nil
}

func (r *partsReader) Close() error {
	var first error
	for _, f := range r.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	r.files = nil
	return first
}

// partWriter writes one file as numbered parts of a fixed size, each under a
// temporary name until publishParts gives them their final names. It
// implements what the optimizer's writer needs from an *os.File.
type partWriter struct {
	final string // the whole file's final path ("Game.nup")
	tag   string // random part of the temporary names
	size  int64
	mode  os.FileMode
	files []*os.File
	pos   int64

	mu   sync.Mutex
	high int64 // bytes currently in the parts, for disk reservations
}

func newPartWriter(final, tag string, size int64, mode os.FileMode) (*partWriter, error) {
	if size < MinPartSize {
		return nil, fmt.Errorf("part size %d is below the %d-byte minimum", size, MinPartSize)
	}
	w := &partWriter{final: final, tag: tag, size: size, mode: mode}
	if _, err := w.part(0); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *partWriter) tempPath(k int) string {
	return fmt.Sprintf("%s.%03d.%s.partial", w.final, k+1, w.tag)
}

func (w *partWriter) part(k int) (*os.File, error) {
	if k >= MaxParts {
		return nil, fmt.Errorf("the output needs more than %d parts of %d bytes; choose a larger part size", MaxParts, w.size)
	}
	for len(w.files) <= k {
		f, err := os.OpenFile(w.tempPath(len(w.files)), os.O_RDWR|os.O_CREATE|os.O_EXCL, w.mode)
		if err != nil {
			return nil, err
		}
		w.files = append(w.files, f)
	}
	return w.files[k], nil
}

func (w *partWriter) WriteAt(b []byte, off int64) (int, error) {
	n := 0
	for len(b) > 0 {
		k := int(off / w.size)
		f, err := w.part(k)
		if err != nil {
			return n, err
		}
		in := off % w.size
		chunk := b
		if left := w.size - in; int64(len(chunk)) > left {
			chunk = chunk[:left]
		}
		m, err := f.WriteAt(chunk, in)
		n += m
		off += int64(m)
		b = b[m:]
		w.mu.Lock()
		if off > w.high {
			w.high = off
		}
		w.mu.Unlock()
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (w *partWriter) Write(b []byte) (int, error) {
	n, err := w.WriteAt(b, w.pos)
	w.pos += int64(n)
	return n, err
}

func (w *partWriter) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += w.pos
	default:
		return w.pos, errors.New("partWriter: unsupported seek")
	}
	if off < 0 {
		return w.pos, errors.New("partWriter: negative position")
	}
	w.pos = off
	return off, nil
}

// Truncate drops everything from n on: later parts are removed.
func (w *partWriter) Truncate(n int64) error {
	keep := int((n + w.size - 1) / w.size)
	if keep < 1 {
		keep = 1
	}
	for len(w.files) > keep {
		k := len(w.files) - 1
		w.files[k].Close()
		if err := os.Remove(w.tempPath(k)); err != nil {
			return err
		}
		w.files = w.files[:k]
	}
	last := len(w.files) - 1
	if err := w.files[last].Truncate(n - int64(last)*w.size); err != nil {
		return err
	}
	w.mu.Lock()
	w.high = n
	w.mu.Unlock()
	return nil
}

func (w *partWriter) Sync() error {
	for _, f := range w.files {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func (w *partWriter) Close() error {
	var first error
	for _, f := range w.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (w *partWriter) written() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.high
}

// temps lists the temporary part paths in order.
func (w *partWriter) temps() []string {
	out := make([]string, len(w.files))
	for k := range w.files {
		out[k] = w.tempPath(k)
	}
	return out
}

// finals lists the names the parts get: the whole file's own name when it
// fits one part, else "<name>.001", "<name>.002", ...
func (w *partWriter) finals() []string {
	if len(w.files) == 1 {
		return []string{w.final}
	}
	out := make([]string, len(w.files))
	for k := range w.files {
		out[k] = fmt.Sprintf("%s.%03d", w.final, k+1)
	}
	return out
}

func (w *partWriter) removeTemps() {
	for _, p := range w.temps() {
		_ = os.Remove(p)
	}
}

// partsCollide reports a file that would join or shadow the parts of final
// (the whole file itself, or any "<name>.<number>").
func partsCollide(final string) error {
	if pathExists(final) {
		return errors.New("Output already exists; choose a new filename.")
	}
	entries, err := os.ReadDir(filepath.Dir(final))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	base := filepath.Base(final)
	for _, e := range entries {
		if p, ok := parsePartName(e.Name()); ok && p.kind == partNumber && strings.EqualFold(p.stem, base) {
			return fmt.Errorf("%s already exists beside the output and would be read as one of its parts; choose a new filename", e.Name())
		}
	}
	return nil
}

// publishParts gives the verified temporary parts their final names, each
// with an exclusive publication, the last part first: nx2-flow takes parts
// for a game only from "<name>.001" (or .000) on, so the game appears only
// when its first part does, complete. On failure every part published so
// far is removed again.
func publishParts(temps, finals []string) error {
	var done []string
	for k := len(temps) - 1; k >= 0; k-- {
		if err := commitNew(temps[k], finals[k]); err != nil {
			for _, p := range done {
				_ = os.Remove(p)
			}
			return err
		}
		done = append(done, finals[k])
	}
	return nil
}
