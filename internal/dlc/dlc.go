// Package dlc ports the Python reference dlc.py: building and verifying
// data-only extracted DLC repacks. No game executable, rights or keys are
// created. The writer path is a 1:1 port of dlc.build (romfs -> data NCA ->
// AddOnContent CNMT -> meta NCA -> PFS0 NSP); the reader path ports
// dlc.verify and dlc.base_game_info with the exact Python error strings.
package dlc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/diskfree"
	"github.com/dannolan/boxcar/internal/metadata"
	"github.com/dannolan/boxcar/internal/nca"
	"github.com/dannolan/boxcar/internal/pfs0"
	"github.com/dannolan/boxcar/internal/region"
	"github.com/dannolan/boxcar/internal/romfs"
	"github.com/dannolan/boxcar/internal/snapshot"
	"github.com/dannolan/boxcar/internal/stream"
)

const baseMask = 0xffffffffffffe000

// Snapshot is a located, already-identified DLC dump (dlc.DlcSnapshot).
type Snapshot struct {
	Root    string
	TitleID uint64
	Version uint32
	Label   string
}

// Stamp is a captured source-file stat (size + mtime nanoseconds). It is an
// alias of internal/snapshot.Stamp so callers share one type.
type Stamp = snapshot.Stamp

// BaseInfo is the result of inspecting a base-game NSP's headers/control only
// (dlc.base_game_info).
type BaseInfo struct {
	TitleID   uint64
	DLCBaseID uint64
	Title     string
}

// Report mirrors the dlc.build report dict keys exactly.
type Report struct {
	Output          string  `json:"output"`
	OutputFormat    string  `json:"output_format"`
	TitleID         string  `json:"title_id"`
	BaseTitleID     string  `json:"base_title_id"`
	DLCIndex        uint64  `json:"dlc_index"`
	DLCVersion      uint32  `json:"dlc_version"`
	Size            int64   `json:"size"`
	SHA256          string  `json:"sha256"`
	SourceFileCount int     `json:"source_file_count"`
	Verified        bool    `json:"verified"`
	BaseValidated   bool    `json:"base_validated"`
	Metadata        string  `json:"metadata"`
	Seconds         float64 `json:"seconds"`
}

// Options configure Build.
type Options struct {
	VerifyOutput bool
	BaseGame     string
	Progress     binfmt.Progress
}

// BaseID returns the base title ID for an add-on-content title ID
// (tid & 0xffffffffffffe000), the Python BASE_MASK.
func BaseID(tid uint64) uint64 { return tid & baseMask }

var hex16 = regexp.MustCompile(`^[0-9a-fA-F]{16}$`)

// ParseID parses a 16-hex-digit DLC title ID and enforces the add-on-content
// range (0x1000..0x17ff on the low 13 bits).
func ParseID(v string) (uint64, error) {
	text := strings.TrimSpace(v)
	if !hex16.MatchString(text) {
		return 0, errors.New("DLC title ID must contain exactly 16 hexadecimal digits.")
	}
	tid, err := strconv.ParseUint(text, 16, 64)
	if err != nil {
		return 0, errors.New("DLC title ID must contain exactly 16 hexadecimal digits.")
	}
	if rerr := titleIDRangeErr(tid); rerr != nil {
		return 0, rerr
	}
	return tid, nil
}

func titleIDRangeErr(tid uint64) error {
	low := tid & 0x1fff
	if tid == 0 || low < 0x1000 || low > 0x17ff {
		return errors.New("This title ID is not in the add-on-content range (base + 0x1000–0x17FF).")
	}
	return nil
}

// ParseVersion parses a decimal DLC version in [0, 0xffffffff].
func ParseVersion(v string) (uint32, error) {
	text := strings.TrimSpace(v)
	if !isASCII(text) || !isDecimal(text) {
		return 0, errors.New("DLC version must be an integer from 0 to 4294967295, as stored in the dump.")
	}
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil || n > 0xffffffff {
		return 0, errors.New("DLC version must be an integer from 0 to 4294967295, as stored in the dump.")
	}
	return uint32(n), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// Find locates DLC snapshots under folder (dlc.find_snapshots). titleIDOverride
// supplies the DLC title ID when no 16-hex folder name is present.
func Find(ctx context.Context, folder string, titleIDOverride string) (found []Snapshot, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}

	folderAbs, aerr := filepath.Abs(folder)
	binfmt.Require(aerr == nil, "%v", aerr)
	fi, lerr := os.Lstat(folderAbs)
	if lerr != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		binfmt.Fail("Choose an existing, unlinked extracted DLC folder.")
	}
	if strings.EqualFold(filepath.Base(folderAbs), "romfs") {
		folderAbs = filepath.Dir(folderAbs)
	}

	var override uint64
	if titleIDOverride != "" {
		var perr error
		override, perr = ParseID(titleIDOverride)
		binfmt.Require(perr == nil, "%v", perr)
	}

	var scan func(path string, depth int)
	scan = func(path string, depth int) {
		binfmt.Check(ctx)
		if isLinked(path) {
			binfmt.Fail("Linked folders are not supported: %s", path)
		}
		romfsDir := filepath.Join(path, "romfs")
		if isDir(romfsDir) {
			if isLinked(romfsDir) {
				binfmt.Fail("Linked RomFS folders are not supported.")
			}
			if isFile(filepath.Join(path, "exefs", "main")) {
				binfmt.Fail("This contains a game executable. Use the Game tab for full game snapshots.")
			}
			// Closest 16-hex folder name among path and up to four ancestors.
			var detected uint64
			detectedSet := false
			candidates := []string{path}
			for cur, i := path, 0; i < 4; i++ {
				parent := filepath.Dir(cur)
				if parent == cur {
					break
				}
				candidates = append(candidates, parent)
				cur = parent
			}
			for _, c := range candidates {
				if hex16.MatchString(filepath.Base(c)) {
					v, perr := ParseID(filepath.Base(c))
					binfmt.Require(perr == nil, "%v", perr)
					detected = v
					detectedSet = true
					break
				}
			}
			if override != 0 && detectedSet && override != detected && path == folderAbs {
				binfmt.Fail("Entered DLC title ID differs from the dump folder title ID.")
			}
			var tid uint64
			if detectedSet {
				tid = detected
			} else {
				tid = override
			}
			if tid == 0 {
				binfmt.Fail("No DLC title ID in the folder names. Enter its original DLC title ID and scan again.")
			}
			var version uint32
			base := filepath.Base(path)
			if isDecimal(base) && utf8.RuneCountInString(base) < 11 {
				v, perr := ParseVersion(base)
				binfmt.Require(perr == nil, "%v", perr)
				version = v
			}
			var label string
			if path == folderAbs {
				label = "Selected folder"
			} else {
				label, _ = filepath.Rel(folderAbs, path)
			}
			found = append(found, Snapshot{Root: resolve(path), TitleID: tid, Version: version, Label: label})
			return
		}
		if depth >= 4 {
			return
		}
		entries, derr := os.ReadDir(path)
		binfmt.Require(derr == nil, "%v", derr)
		for _, child := range entries {
			if isDir(filepath.Join(path, child.Name())) {
				scan(filepath.Join(path, child.Name()), depth+1)
			}
		}
	}

	scan(folderAbs, 0)
	if len(found) == 0 {
		binfmt.Fail("No extracted DLC RomFS found. Select dlc, a title/version folder, or its romfs folder.")
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].TitleID != found[j].TitleID {
			return found[i].TitleID < found[j].TitleID
		}
		if found[i].Version != found[j].Version {
			return found[i].Version > found[j].Version
		}
		return found[i].Label < found[j].Label
	})
	return found, nil
}

// BaseGameInfo inspects a plaintext base-game NSP's headers/control only; it
// never hashes or copies the base game's payload (dlc.base_game_info).
func BaseGameInfo(ctx context.Context, path string) (info BaseInfo, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}

	f, ferr := os.Open(path)
	binfmt.Require(ferr == nil, "%v", ferr)
	defer f.Close()
	r, rerr := region.New(f)
	binfmt.Require(rerr == nil, "%v", rerr)

	files := containerFiles(ctx, r)
	programs := map[uint64]struct{}{}
	var controlTID uint64
	var control []byte
	haveControl := false

	for _, data := range files {
		binfmt.Check(ctx)
		var magic [4]byte
		if merr := data.ReadAt(0x200, magic[:]); merr != nil || string(magic[:]) != "NCA3" {
			continue
		}
		tid, terr := data.U64(0x210)
		binfmt.Require(terr == nil, "%v", terr)
		ct, cerr := data.Byte(0x205)
		binfmt.Require(cerr == nil, "%v", cerr)
		if ct == 0 {
			programs[tid] = struct{}{}
		} else if ct == 2 && data.Size() <= 8*1024*1024 {
			_, _, secs, serr := nca.Sections(ctx, data, false)
			binfmt.Require(serr == nil, "%v", serr)
			var controlRomfs *region.Region
			for _, s := range secs {
				if s.Kind == "romfs" {
					controlRomfs = s.Data
					break
				}
			}
			if controlRomfs != nil {
				rf, rferr := romfs.Files(ctx, controlRomfs)
				binfmt.Require(rferr == nil, "%v", rferr)
				if nacp, ok := rf["control.nacp"]; ok && nacp.Size() == 0x4000 {
					b, berr := nacp.Slice()
					binfmt.Require(berr == nil, "%v", berr)
					controlTID = tid
					control = b
					haveControl = true
				}
			}
		}
	}

	binfmt.Require(len(programs) == 1, "Expected one plaintext base-game Program NCA; encrypted/multi-program inputs are not supported.")
	var tid uint64
	for t := range programs {
		tid = t
	}
	if tid&0x1fff != 0 {
		binfmt.Fail("Choose a complete base-game snapshot, not an update/DLC package.")
	}
	info = BaseInfo{TitleID: tid, DLCBaseID: tid + 0x1000, Title: stem(path)}
	if haveControl && controlTID == tid {
		info.DLCBaseID = binfmt.U64At(control, 0x3070)
		if control[0x3215] == 0 {
			title := control[:0x200]
			if i := bytes.IndexByte(title, 0); i >= 0 {
				title = title[:i]
			}
			info.Title = strings.ToValidUTF8(string(title), "\uFFFD")
			if info.Title == "" {
				info.Title = stem(path)
			}
		}
	}
	return info, nil
}

func validateBase(snap Snapshot, info BaseInfo) {
	base := BaseID(snap.TitleID)
	if info.TitleID != base || info.DLCBaseID != base+0x1000 {
		binfmt.Fail("DLC title ID does not match this base game or its control metadata DLC base ID.")
	}
}

// Verify checks a produced DLC NSP against its snapshot and sources
// (dlc.verify). captured, when non-nil, supplies pre-computed per-source
// SHA-256 digests keyed by absolute path. It returns the source file count.
func Verify(ctx context.Context, path string, snap Snapshot, sources map[string]Stamp, progress binfmt.Progress, captured map[string]*[]byte) (count int, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}

	f, ferr := os.Open(path)
	binfmt.Require(ferr == nil, "%v", ferr)
	defer f.Close()
	r, rerr := region.New(f)
	binfmt.Require(rerr == nil, "%v", rerr)

	files := containerFiles(ctx, r)
	binfmt.Require(len(files) == 2, "Expected exactly Data and Meta NCAs")

	parsed := map[byte][]nca.NamedSection{}
	hashes := map[string][]byte{}
	for name, data := range files {
		binfmt.Check(ctx)
		d, derr := data.Byte(0x204)
		binfmt.Require(derr == nil && d == 0, "NCA distribution type")
		h, herr := data.Digest(ctx)
		binfmt.Require(herr == nil, "%v", herr)
		hashes[name] = h
		binfmt.Require(len(name) >= 32 && hex.EncodeToString(h[:16]) == name[:32], "NCA content ID")
		tid, kind, secs, serr := nca.Sections(ctx, data, true)
		binfmt.Require(serr == nil, "%v", serr)
		binfmt.Require(tid == snap.TitleID, "DLC title ID")
		_, dup := parsed[kind]
		binfmt.Require(!dup, "Duplicate content type")
		parsed[kind] = secs
	}
	binfmt.Require(len(parsed) == 2 && parsed[1] != nil && parsed[4] != nil, "DLC must contain Meta and Data, no game executable")

	metaSecs := parsed[1]
	binfmt.Require(len(metaSecs) > 0, "Meta NCA has no sections")
	cfiles, perr := pfs0.Partition(ctx, metaSecs[0].Data)
	binfmt.Require(perr == nil, "%v", perr)
	wantName := fmt.Sprintf("AddOnContent_%016x.cnmt", snap.TitleID)
	var cname string
	var c *region.Region
	for n, reg := range cfiles {
		cname, c = n, reg
	}
	binfmt.Require(len(cfiles) == 1 && cname == wantName, "CNMT filename")

	cb, cerr := c.Slice()
	binfmt.Require(cerr == nil, "%v", cerr)
	binfmt.Require(int64(len(cb)) >= 0x20, "CNMT size")
	tid2 := binfmt.U64At(cb, 0)
	version := binfmt.U32At(cb, 8)
	kind := cb[12]
	extra := binfmt.U16At(cb, 14)
	recCount := binfmt.U16At(cb, 16)
	metas := binfmt.U16At(cb, 18)
	binfmt.Require(tid2 == snap.TitleID && version == snap.Version && kind == 0x82 && extra == 0x18 && recCount == 1 && metas == 0, "CNMT identity/type")
	binfmt.Require(binfmt.U64At(cb, 0x20) == BaseID(snap.TitleID), "CNMT application mapping")
	binfmt.Require(int64(len(cb)) == 0x20+int64(extra)+0x38+32, "CNMT size")
	binfmt.Require(bytes.Equal(binfmt.SHA256Sum(cb[:len(cb)-32]), cb[len(cb)-32:]), "CNMT digest")

	at := 0x20 + int(extra)
	name := hex.EncodeToString(cb[at+32:at+48]) + ".nca"
	dataRegion, ok := files[name]
	binfmt.Require(ok && bytes.Equal(hashes[name], cb[at:at+32]), "CNMT Data hash")
	binfmt.Require(leU48(cb[at+48:at+54]) == uint64(dataRegion.Size()), "CNMT Data size")
	binfmt.Require(cb[at+54] == 2 && cb[at+55] == 0, "CNMT Data record type/index")

	var dataRomfs *region.Region
	for _, s := range parsed[4] {
		if s.Kind == "romfs" {
			dataRomfs = s.Data
			break
		}
	}
	binfmt.Require(dataRomfs != nil, "DLC RomFS missing")
	packed, perr2 := romfs.Files(ctx, dataRomfs)
	binfmt.Require(perr2 == nil, "%v", perr2)

	romfsRoot := filepath.Join(snap.Root, "romfs")
	original := make(map[string]string, len(sources))
	for p := range sources {
		rel, relerr := filepath.Rel(romfsRoot, p)
		binfmt.Require(relerr == nil, "%v", relerr)
		original[filepath.ToSlash(rel)] = p
	}
	binfmt.Require(len(original) == len(packed), "DLC RomFS file list")
	for k := range original {
		_, ok := packed[k]
		binfmt.Require(ok, "DLC RomFS file list")
	}

	var total int64
	for _, st := range sources {
		total += st.Size
	}
	var done int64
	for name, p := range original {
		binfmt.Check(ctx)
		var expected []byte
		if captured != nil {
			cp := captured[p]
			if cp == nil || *cp == nil {
				binfmt.Fail("DLC file hash: %s", name)
			}
			expected = *cp
		} else {
			fd, fderr := region.FileDigest(ctx, p)
			binfmt.Require(fderr == nil, "%v", fderr)
			expected = fd
		}
		actual, derr := packed[name].Digest(ctx)
		binfmt.Require(derr == nil, "%v", derr)
		binfmt.Require(bytes.Equal(actual, expected), "DLC file hash: %s", name)
		done += sources[p].Size
		denom := total
		if denom < 1 {
			denom = 1
		}
		if progress != nil {
			progress("Comparing DLC source files", 82+17*float64(done)/float64(denom))
		}
	}
	return len(sources), nil
}

// Build produces a DLC NSP from a snapshot (dlc.build).
func Build(ctx context.Context, snap Snapshot, output string, opts Options) (rep Report, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	notify := opts.Progress

	snap.Root = resolve(snap.Root)
	if rerr := titleIDRangeErr(snap.TitleID); rerr != nil {
		binfmt.Fail("%s", rerr.Error())
	}
	output = resolve(output)
	kind := strings.ToLower(strings.TrimPrefix(filepath.Ext(output), "."))
	binfmt.Require(kind == "nsp" && strings.ToLower(filepath.Ext(output)) == ".nsp", "NSP Box supports .nsp output only.")
	binfmt.Require(!isUnder(snap.Root, output), "Save the DLC package outside its source snapshot folder.")
	if _, serr := os.Stat(output); serr == nil {
		binfmt.Fail("Output already exists. Choose another filename; nothing is overwritten.")
	}

	var info *BaseInfo
	if opts.BaseGame != "" {
		bi, berr := BaseGameInfo(ctx, opts.BaseGame)
		binfmt.Require(berr == nil, "%v", berr)
		info = &bi
		validateBase(snap, *info)
	}

	if notify != nil {
		notify("Checking DLC source files", 1)
	}
	sources := inventory(ctx, filepath.Join(snap.Root, "romfs"), notify)

	binfmt.Require(os.MkdirAll(filepath.Dir(output), 0o755) == nil, "create output directory")
	free, derr := diskfree.Available(filepath.Dir(output))
	binfmt.Require(derr == nil, "%v", derr)
	var totalSize int64
	for _, st := range sources {
		totalSize += st.Size
	}
	if float64(free) < float64(totalSize)*1.02+32*1024*1024 {
		binfmt.Fail("Not enough free disk space for the DLC package.")
	}

	romfsDir := filepath.Join(snap.Root, "romfs")
	var manifest []romfs.ManifestEntry
	romfsStream, rerr := romfs.Build(ctx, romfsDir, &manifest, func(n int) {
		if notify != nil {
			notify(fmt.Sprintf("Building DLC RomFS · %s files", comma(n)), 2)
		}
	}, nil)
	binfmt.Require(rerr == nil, "%v", rerr)

	buildProgress := func(label string, done float64) {
		if notify != nil {
			notify("Building DLC integrity tree", 2+22*done/100)
		}
	}
	dataFSH, dataBody, derr2 := nca.RomfsSection(ctx, romfsStream, buildProgress)
	binfmt.Require(derr2 == nil, "%v", derr2)
	data, derr3 := nca.NCA([]nca.Section{{FSH: dataFSH, Body: dataBody}}, snap.TitleID, 4)
	binfmt.Require(derr3 == nil, "%v", derr3)

	hashProgress := func(label string, done float64) {
		if notify != nil {
			notify("Hashing DLC content", 24+20*done/100)
		}
	}
	digest, derr4 := data.SHA256(&stream.WriteOpts{Ctx: ctx, Block: 4 << 20, Progress: hashProgress})
	binfmt.Require(derr4 == nil, "%v", derr4)

	name := fmt.Sprintf("%x", digest[:16]) + ".nca"
	cnmt := metadata.AddOnCNMT(snap.TitleID, snap.Version, digest, data.Size())
	fs, ferr := pfs0.FileSystem([]pfs0.Entry{{Name: fmt.Sprintf("AddOnContent_%016x.cnmt", snap.TitleID), Part: stream.Bytes(cnmt)}}, false)
	binfmt.Require(ferr == nil, "%v", ferr)
	metaFSH, metaBody, merr := nca.PFSSection(ctx, fs)
	binfmt.Require(merr == nil, "%v", merr)
	meta, merr2 := nca.NCA([]nca.Section{{FSH: metaFSH, Body: metaBody}}, snap.TitleID, 1)
	binfmt.Require(merr2 == nil, "%v", merr2)

	metaDigest, merr3 := meta.SHA256(&stream.WriteOpts{Ctx: ctx, Block: 4 << 20, Progress: hashProgress})
	binfmt.Require(merr3 == nil, "%v", merr3)
	metaName := fmt.Sprintf("%x", metaDigest[:16]) + ".cnmt.nca"
	metaBytes, merr4 := meta.ReadHead(meta.Size(), &stream.WriteOpts{Ctx: ctx})
	binfmt.Require(merr4 == nil, "%v", merr4)
	binfmt.Require(int64(len(metaBytes)) == meta.Size(), "meta NCA materialization")

	// pfs0 entries are single file-backed or literal parts, so the composite
	// data NCA stream is materialized to a temp file first. The bytes are
	// byte-identical to Python's direct streaming.
	dataTempDir, terr := os.MkdirTemp("", "nspbox-dlc-*")
	binfmt.Require(terr == nil, "%v", terr)
	defer os.RemoveAll(dataTempDir)
	dataTemp := filepath.Join(dataTempDir, "data.nca")

	captured := map[string]*[]byte(nil)
	if opts.VerifyOutput {
		captured = make(map[string]*[]byte, len(sources))
		for p := range sources {
			var buf []byte
			captured[p] = &buf
		}
	}
	dtf, terr2 := os.Create(dataTemp)
	binfmt.Require(terr2 == nil, "%v", terr2)
	wopts := &stream.WriteOpts{Ctx: ctx, Block: 4 << 20}
	if opts.VerifyOutput {
		wopts.Hashes = captured
	}
	cw := &countingWriter{w: dtf, total: data.Size(), notify: notify, label: "Writing DLC " + strings.ToUpper(kind)}
	werr := data.WriteTo(cw, wopts)
	cwerr := dtf.Close()
	binfmt.Require(werr == nil, "%v", werr)
	binfmt.Require(cwerr == nil, "%v", cwerr)

	image, ierr := pfs0.Package([]pfs0.Entry{
		{Name: name, Part: stream.File(dataTemp, data.Size())},
		{Name: metaName, Part: stream.Bytes(metaBytes)},
	})
	binfmt.Require(ierr == nil, "%v", ierr)

	temp := filepath.Join(filepath.Dir(output), filepath.Base(output)+"."+randomHex(10)+".partial")
	defer func() { _ = os.Remove(temp) }()
	f, oferr := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	binfmt.Require(oferr == nil, "%v", oferr)
	defer f.Close()

	h := sha256.New()
	pw := &countingWriter{w: f, h: h, total: image.Size(), notify: notify, label: "Writing DLC " + strings.ToUpper(kind)}
	iwerr := image.WriteTo(pw, &stream.WriteOpts{Ctx: ctx, Block: 4 << 20})
	binfmt.Require(iwerr == nil, "%v", iwerr)
	if serr := f.Sync(); serr != nil {
		binfmt.Fail("fsync: %v", serr)
	}
	if cerr := f.Close(); cerr != nil {
		binfmt.Fail("close: %v", cerr)
	}

	if opts.VerifyOutput {
		if notify != nil {
			notify("Verifying DLC container integrity", 75)
		}
		_, verr := Verify(ctx, temp, snap, sources, notify, captured)
		binfmt.Require(verr == nil, "%v", verr)
	}

	for p, old := range sources {
		binfmt.Check(ctx)
		st, sterr := os.Stat(p)
		binfmt.Require(sterr == nil, "%v", sterr)
		if st.Size() != old.Size || st.ModTime().UnixNano() != old.MtimeNS {
			binfmt.Fail("A DLC source file changed during packaging. Retry with the dump idle.")
		}
	}
	binfmt.Check(ctx)
	if cerr := commitFile(temp, output); cerr != nil {
		binfmt.Fail("commit output: %v", cerr)
	}

	rep = Report{
		Output:          output,
		OutputFormat:    strings.ToUpper(kind),
		TitleID:         fmt.Sprintf("%016X", snap.TitleID),
		BaseTitleID:     fmt.Sprintf("%016X", BaseID(snap.TitleID)),
		DLCIndex:        snap.TitleID & 0x7ff,
		DLCVersion:      snap.Version,
		Size:            image.Size(),
		SHA256:          hex.EncodeToString(h.Sum(nil)),
		SourceFileCount: len(sources),
		Verified:        opts.VerifyOutput,
		BaseValidated:   info != nil,
		Metadata:        "Generated minimal AddOnContent CNMT; original requirements unavailable.",
		Seconds:         time.Since(started).Seconds(),
	}
	if notify != nil {
		notify("DLC package ready", 100)
	}
	return rep, nil
}

// --- internal helpers ---

func containerFiles(ctx context.Context, r *region.Region) map[string]*region.Region {
	var magic [4]byte
	if merr := r.ReadAt(0, magic[:]); merr != nil || string(magic[:]) != "PFS0" {
		binfmt.Fail("Choose a plaintext NSP produced by NSP Box.")
	}
	files, err := pfs0.Partition(ctx, r)
	binfmt.Require(err == nil, "%v", err)
	return files
}

func inventory(ctx context.Context, root string, notify binfmt.Progress) map[string]Stamp {
	if isLinked(root) {
		binfmt.Fail("Linked source folders are not supported.")
	}
	result := map[string]Stamp{}
	var visit func(folder string)
	visit = func(folder string) {
		entries, derr := os.ReadDir(folder)
		binfmt.Require(derr == nil, "%v", derr)
		for _, e := range entries {
			binfmt.Check(ctx)
			p := filepath.Join(folder, e.Name())
			info, lerr := os.Lstat(p)
			binfmt.Require(lerr == nil, "%v", lerr)
			if info.Mode()&os.ModeSymlink != 0 {
				binfmt.Fail("Linked source files/folders are not supported: %s", p)
			}
			if info.IsDir() {
				visit(p)
			} else if info.Mode().IsRegular() {
				result[p] = Stamp{Size: info.Size(), MtimeNS: info.ModTime().UnixNano()}
				if len(result)%256 == 0 && notify != nil {
					notify(fmt.Sprintf("Checking DLC source files · %s found", comma(len(result))), 1)
				}
			}
		}
	}
	visit(root)
	if len(result) == 0 {
		binfmt.Fail("The DLC RomFS folder is empty.")
	}
	return result
}

// countingWriter streams to w, optionally hashing, and reports progress
// across a 45..74 span like Python's write loop.
type countingWriter struct {
	w      io.Writer
	h      hash.Hash
	total  int64
	done   int64
	notify binfmt.Progress
	label  string
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	if n > 0 {
		if c.h != nil {
			c.h.Write(b[:n])
		}
		c.done += int64(n)
		if c.notify != nil && c.total > 0 {
			c.notify(c.label, 45+29*float64(c.done)/float64(c.total))
		}
	}
	return n, err
}

func leU48(b []byte) uint64 {
	var v uint64
	for i := 0; i < 6; i++ {
		v |= uint64(b[i]) << (8 * i)
	}
	return v
}

func comma(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(c)
	}
	return out.String()
}

func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n, time.Now().UnixNano())
	}
	return hex.EncodeToString(b)[:n]
}

func stem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func isLinked(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func isUnder(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// resolve makes p absolute and resolves symlinks for the deepest existing
// component, mirroring pathlib.Path.resolve(strict=False).
func resolve(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	existing := p
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		tail = append(tail, filepath.Base(existing))
		existing = parent
	}
	if resolved, err := filepath.EvalSymlinks(existing); err == nil {
		existing = resolved
	}
	for i := len(tail) - 1; i >= 0; i-- {
		existing = filepath.Join(existing, tail[i])
	}
	return existing
}
