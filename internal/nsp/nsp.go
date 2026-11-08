// Package nsp assembles complete title snapshots into unsigned plaintext
// emulator NSP files and independently verifies them, porting engine.build
// and engine.verify_image from the Python reference implementation.
package nsp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dannolan/boxcar/internal/artwork"
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

const appVersion = "4.0.0"

// Companion is the optional Switch 2 main.npdm sidecar description.
type Companion struct {
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Purpose string `json:"purpose"`
}

// Report is the JSON build report, mirroring engine.build's report dict keys.
type Report struct {
	App                      string             `json:"app"`
	Version                  string             `json:"version"`
	Title                    string             `json:"title"`
	TitleID                  string             `json:"title_id"`
	Snapshot                 string             `json:"snapshot"`
	SnapshotVersion          uint32             `json:"snapshot_version"`
	DisplayVersion           string             `json:"display_version"`
	Size                     int64              `json:"size"`
	SHA256                   string             `json:"sha256"`
	OriginalControlPreserved bool               `json:"original_control_preserved"`
	SourceFileCount          int                `json:"source_file_count"`
	Verified                 bool               `json:"verified"`
	Files                    []map[string]any   `json:"files"`
	TimingsSeconds           map[string]float64 `json:"timings_seconds"`
	VerificationMethod       string             `json:"verification_method"`
	OutputFormat             string             `json:"output_format"`
	Output                   string             `json:"output"`
	PackageFolder            bool               `json:"package_folder"`
	Switch2Companion         *Companion         `json:"switch2_companion"`
	Format                   string             `json:"format"`
}

// Options configures Build.
type Options struct {
	Output         string
	Title          string
	DisplayVersion string
	Artwork        *artwork.Selection
	Verify         bool
	PackageFolder  bool
	Progress       binfmt.Progress
}

type phaseState struct {
	lo, hi float64
	label  string
}

type nspFile struct {
	name string
	body *stream.Stream
}

type parsedNCA struct {
	titleID     uint64
	contentType byte
	secs        []nca.NamedSection
}

// Build assembles snap into an NSP written to opts.Output, the exact port of
// engine.build. On success it returns the full report; the report is also
// written to <output>.nsp.report.json when opts.PackageFolder is set.
func Build(ctx context.Context, snap snapshot.Snapshot, opts Options) (rep Report, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}

	started := time.Now()
	timings := map[string]float64{}
	tick := started

	output := resolvePath(opts.Output)
	root := resolvePath(snap.Root)
	if opts.PackageFolder {
		name := filepath.Base(output)
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		output = resolvePath(filepath.Join(filepath.Dir(output), stem, name))
	}

	if strings.TrimSpace(opts.Title) == "" {
		binfmt.Fail("Enter a game title.")
	}
	if len([]byte(opts.DisplayVersion)) > 15 {
		binfmt.Fail("Display version must fit within 15 UTF-8 bytes.")
	}
	kind := strings.ToLower(strings.TrimPrefix(filepath.Ext(output), "."))
	if kind != "nsp" {
		binfmt.Fail("NSP Box supports NSP output only.")
	}
	if !strings.EqualFold(filepath.Ext(output), "."+kind) {
		binfmt.Fail("Output filename must end in .%s for %s output.", kind, strings.ToUpper(kind))
	}
	if isWithin(filepath.Join(root, "exefs"), output) || isWithin(filepath.Join(root, "romfs"), output) {
		binfmt.Fail("Save the NSP outside the source exefs and romfs folders.")
	}
	if pathExists(output) {
		binfmt.Fail("An output with that name already exists. Choose another filename.")
	}

	npdmPath := filepath.Join(root, "exefs", "main.npdm")
	npdm, rerr := os.ReadFile(npdmPath)
	if rerr != nil {
		return Report{}, rerr
	}
	tid2, rerr := metadata.ReadTitleID(npdm)
	if rerr != nil {
		return Report{}, rerr
	}
	if tid2 != snap.TitleID {
		binfmt.Fail("Source title ID changed. Scan the folder again.")
	}

	companion := ""
	var companionData []byte
	if opts.PackageFolder && snap.TitleID>>56 == 4 {
		companion = filepath.Join(filepath.Dir(output), "main.npdm")
		companionData = npdm
		if pathExists(companion) {
			existing, cerr := os.ReadFile(companion)
			if cerr != nil {
				return Report{}, cerr
			}
			if !bytes.Equal(existing, companionData) {
				binfmt.Fail("A different main.npdm already exists in the output folder. Choose a separate folder for this game/snapshot; existing metadata will not be overwritten.")
			}
		}
	}

	notify := func(label string, done float64) {
		if opts.Progress != nil {
			opts.Progress(label, done)
		}
	}
	phase := &phaseState{label: "Preparing"}
	onProgress := func(_ string, pct float64) {
		notify(phase.label, phase.lo+(phase.hi-phase.lo)*pct/100.0)
	}

	notify("Checking source files", 1)
	inv, ierr := snapshot.Inventory(ctx, root, func(label string, found int) {
		notify(label, float64(found))
	})
	if ierr != nil {
		return Report{}, ierr
	}
	timings["source_scan"] = time.Since(tick).Seconds()
	tick = time.Now()

	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return Report{}, err
	}
	var totalSize int64
	for _, st := range inv {
		totalSize += st.Size
	}
	estimate := int64(float64(totalSize)*1.02) + 32*1024*1024
	free, ferr := diskfree.Available(filepath.Dir(output))
	if ferr != nil {
		return Report{}, ferr
	}
	if free < uint64(estimate) {
		binfmt.Fail("Not enough free disk space for this NSP.")
	}

	art, aerr := artwork.Bytes(opts.Artwork)
	if aerr != nil {
		return Report{}, aerr
	}

	notify("Building RomFS file tables", 2)
	var sourceManifest []romfs.ManifestEntry
	rblob, brerr := romfs.Build(ctx, filepath.Join(root, "romfs"), &sourceManifest,
		func(count int) { notify(fmt.Sprintf("Building RomFS file tables · %s files", comma(count)), 2) }, nil)
	if brerr != nil {
		return Report{}, brerr
	}

	exblob, xerr := buildExefs(root)
	if xerr != nil {
		return Report{}, xerr
	}

	phase.lo, phase.hi, phase.label = 2, 22, "Building RomFS integrity tree"
	notify(phase.label, 2)
	pfsFSH, exPadded, perr := nca.PFSSection(ctx, exblob)
	if perr != nil {
		return Report{}, perr
	}
	romFSH, rPadded, rserr := nca.RomfsSection(ctx, rblob, onProgress)
	if rserr != nil {
		return Report{}, rserr
	}
	program, nerr := nca.NCA([]nca.Section{{FSH: pfsFSH, Body: exPadded}, {FSH: romFSH, Body: rPadded}}, snap.TitleID, 0)
	if nerr != nil {
		return Report{}, nerr
	}
	timings["filesystems_and_integrity_tree"] = time.Since(tick).Seconds()
	tick = time.Now()

	tmpDir, terr := os.MkdirTemp("", "nsp-box-control-")
	if terr != nil {
		return Report{}, terr
	}
	defer os.RemoveAll(tmpDir)

	var existingNacp []byte
	if snap.NacpPath != "" {
		existingNacp, rerr = os.ReadFile(snap.NacpPath)
		if rerr != nil {
			return Report{}, rerr
		}
	}
	controlEntries, preserved, cderr := metadata.ControlData(snap.TitleID, opts.DisplayVersion, opts.Title, existingNacp, art)
	if cderr != nil {
		return Report{}, cderr
	}
	for _, kv := range controlEntries {
		if werr := os.WriteFile(filepath.Join(tmpDir, kv.Name), kv.Data, 0o644); werr != nil {
			return Report{}, werr
		}
	}

	phase.lo, phase.hi, phase.label = 22, 23, "Preparing artwork and metadata"
	notify(phase.label, 22)
	var controlManifest []romfs.ManifestEntry
	controlRomfs, crerr := romfs.Build(ctx, tmpDir, &controlManifest, nil, onProgress)
	if crerr != nil {
		return Report{}, crerr
	}
	controlRomFSH, controlPadded, cperr := nca.RomfsSection(ctx, controlRomfs, onProgress)
	if cperr != nil {
		return Report{}, cperr
	}
	control, cncerr := nca.NCA([]nca.Section{{FSH: controlRomFSH, Body: controlPadded}}, snap.TitleID, 2)
	if cncerr != nil {
		return Report{}, cncerr
	}

	phase.lo, phase.hi, phase.label = 23, 44, "Hashing game content"
	notify(phase.label, 23)

	// Capture source SHA-256s during the program-NCA digest pass. The program
	// NCA contains every source file (exefs + romfs), and stream.SHA256 hashes
	// file parts exactly once at read time (Hashes capture is honored by every
	// stream pass).
	var captured map[string]*[]byte
	if opts.Verify {
		captured = make(map[string]*[]byte, len(inv))
		for p := range inv {
			captured[p] = new([]byte)
		}
	}
	programDigest, derr := program.SHA256(&stream.WriteOpts{Ctx: ctx, Progress: onProgress, Hashes: captured})
	if derr != nil {
		return Report{}, derr
	}
	controlDigest, cdigerr := control.SHA256(&stream.WriteOpts{Ctx: ctx, Progress: onProgress})
	if cdigerr != nil {
		return Report{}, cdigerr
	}

	var pdig [32]byte
	copy(pdig[:], programDigest)
	var cdig [32]byte
	copy(cdig[:], controlDigest)
	programName := hex.EncodeToString(programDigest[:16]) + ".nca"
	controlName := hex.EncodeToString(controlDigest[:16]) + ".nca"

	records := []metadata.CNMTRecord{
		{Digest: pdig, Size: program.Size(), ContentType: 1},
		{Digest: cdig, Size: control.Size(), ContentType: 3},
	}
	cnmt := metadata.AppCNMT(snap.TitleID, snap.Version, records)

	cnmtName := fmt.Sprintf("Application_%016x.cnmt", snap.TitleID)
	metaFS, mferr := pfs0.FileSystem([]pfs0.Entry{{Name: cnmtName, Part: stream.Bytes(cnmt)}}, false)
	if mferr != nil {
		return Report{}, mferr
	}
	metaPFSFSH, metaPFSPadded, mperr := nca.PFSSection(ctx, metaFS)
	if mperr != nil {
		return Report{}, mperr
	}
	meta, mnerr := nca.NCA([]nca.Section{{FSH: metaPFSFSH, Body: metaPFSPadded}}, snap.TitleID, 1)
	if mnerr != nil {
		return Report{}, mnerr
	}
	metaDigest, mderr := meta.SHA256(&stream.WriteOpts{Ctx: ctx, Progress: onProgress})
	if mderr != nil {
		return Report{}, mderr
	}
	metaName := hex.EncodeToString(metaDigest[:16]) + ".cnmt.nca"

	result, preserr := packageNSP([]nspFile{
		{name: programName, body: program},
		{name: controlName, body: control},
		{name: metaName, body: meta},
	})
	if preserr != nil {
		return Report{}, preserr
	}
	timings["metadata_and_content_hashing"] = time.Since(tick).Seconds()
	tick = time.Now()

	tempPath := filepath.Join(filepath.Dir(output), filepath.Base(output)+"."+randHex(10)+".partial")
	committed := false
	companionCreated := false
	defer func() {
		if !committed {
			if pathExists(tempPath) {
				os.Remove(tempPath)
			}
			if companionCreated && companion != "" {
				if b, e := os.ReadFile(companion); e == nil && bytes.Equal(b, companionData) {
					os.Remove(companion)
				}
			}
		}
	}()

	f, ferr := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if ferr != nil {
		return Report{}, ferr
	}
	hw := &countingHashWriter{
		w:      f,
		h:      sha256.New(),
		total:  result.Size(),
		lo:     45,
		hi:     74,
		label:  "Writing " + strings.ToUpper(kind),
		notify: notify,
	}
	werr := result.WriteTo(hw, &stream.WriteOpts{Ctx: ctx})
	if werr != nil {
		f.Close()
		return Report{}, werr
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		return Report{}, serr
	}
	if cerr := f.Close(); cerr != nil {
		return Report{}, cerr
	}
	resultSHA := hex.EncodeToString(hw.h.Sum(nil))
	timings["writing"] = time.Since(tick).Seconds()
	tick = time.Now()

	var checked []map[string]any
	if opts.Verify {
		notify("Verifying container integrity", 75)
		verifySnap := snap
		verifySnap.Root = root
		checked, err = VerifyOutput(ctx, tempPath, verifySnap, inv, notify, captured)
		if err != nil {
			return Report{}, err
		}
	}
	timings["verification"] = time.Since(tick).Seconds()

	if serr := snapshot.ValidateSources(inv); serr != nil {
		return Report{}, serr
	}
	binfmt.Check(ctx)

	if companion != "" {
		companionTemp := filepath.Join(filepath.Dir(output), ".main.npdm."+randHex(10)+".partial")
		cf, cferr := os.OpenFile(companionTemp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if cferr != nil {
			return Report{}, cferr
		}
		if _, werr := cf.Write(companionData); werr != nil {
			cf.Close()
			return Report{}, werr
		}
		if serr := cf.Sync(); serr != nil {
			cf.Close()
			return Report{}, serr
		}
		if cerr := cf.Close(); cerr != nil {
			return Report{}, cerr
		}
		if rerr := renameCompanion(companionTemp, companion); rerr != nil {
			if isExistError(rerr) {
				existing, eerr := os.ReadFile(companion)
				if eerr != nil {
					return Report{}, eerr
				}
				if !bytes.Equal(existing, companionData) {
					binfmt.Fail("Output main.npdm changed during packaging. Choose a separate output folder.")
				}
			} else {
				return Report{}, rerr
			}
		} else {
			companionCreated = true
		}
		os.Remove(companionTemp)
	}

	if cerr := commitFile(tempPath, output); cerr != nil {
		return Report{}, cerr
	}
	committed = true

	verificationMethod := "Skipped"
	if opts.Verify {
		verificationMethod = "Source SHA-256 captured during writing; full independent output readback."
	}
	var companionReport *Companion
	if companion != "" {
		sum := sha256.Sum256(companionData)
		companionReport = &Companion{
			File:    "main.npdm",
			SHA256:  hex.EncodeToString(sum[:]),
			Purpose: "Keep beside the package for Eglantine/PokeDinner Switch 2 memory/clock detection.",
		}
	}
	timings["total"] = time.Since(started).Seconds()
	report := Report{
		App:                      "NSP Box",
		Version:                  appVersion,
		Title:                    opts.Title,
		TitleID:                  fmt.Sprintf("%016X", snap.TitleID),
		Snapshot:                 root,
		SnapshotVersion:          snap.Version,
		DisplayVersion:           opts.DisplayVersion,
		Size:                     result.Size(),
		SHA256:                   resultSHA,
		OriginalControlPreserved: preserved,
		SourceFileCount:          len(inv),
		Verified:                 opts.Verify,
		Files:                    checked,
		TimingsSeconds:           timings,
		VerificationMethod:       verificationMethod,
		OutputFormat:             strings.ToUpper(kind),
		Output:                   output,
		PackageFolder:            opts.PackageFolder,
		Switch2Companion:         companionReport,
		Format:                   fmt.Sprintf("Unsigned plaintext emulator %s; one complete snapshot; no automatic DLC merge.", strings.ToUpper(kind)),
	}

	if opts.PackageFolder {
		reportPath := withSuffix(output, ".nsp.report.json")
		if pathExists(reportPath) {
			reportPath = filepath.Join(filepath.Dir(output), filepath.Base(output)+"."+randHex(8)+".report.json")
		}
		data, jerr := json.MarshalIndent(report, "", "  ")
		if jerr != nil {
			return Report{}, jerr
		}
		rf, werr := os.OpenFile(reportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if werr != nil {
			return Report{}, werr
		}
		if _, werr := rf.Write(data); werr != nil {
			rf.Close()
			return Report{}, werr
		}
		if werr := rf.Close(); werr != nil {
			return Report{}, werr
		}
	}

	notify("Complete", 100)
	return report, nil
}

// VerifyOutput opens path and verifies it against snap and inv, the exact
// port of engine.verify_image/_verify_region. It returns the checked file list
// [{path,size,sha256}]. captured (when non-nil) supplies pre-computed source
// SHA-256s keyed by absolute path; otherwise sources are re-hashed.
func VerifyOutput(ctx context.Context, path string, snap snapshot.Snapshot, inv map[string]snapshot.Stamp,
	progress binfmt.Progress, captured map[string]*[]byte) (checked []map[string]any, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	f, ferr := os.Open(path)
	if ferr != nil {
		return nil, ferr
	}
	defer f.Close()
	r, rerr := region.New(f)
	if rerr != nil {
		return nil, rerr
	}
	return verifyRegion(ctx, r, snap, inv, progress, captured)
}

func verifyRegion(ctx context.Context, r *region.Region, snap snapshot.Snapshot, inv map[string]snapshot.Stamp,
	progress binfmt.Progress, captured map[string]*[]byte) (checked []map[string]any, err error) {
	defer binfmt.Recover(&err)

	magic := make([]byte, 4)
	if rerr := r.ReadAt(0, magic); rerr != nil {
		binfmt.Fail("%v", rerr)
	}
	binfmt.Require(string(magic) == "PFS0", "NSP/PFS0 header")

	ncas, perr := pfs0.Partition(ctx, r)
	if perr != nil {
		return nil, perr
	}

	ncaHashes := map[string][]byte{}
	parsed := map[string]*parsedNCA{}
	for name, data := range ncas {
		binfmt.Check(ctx)
		d, derr := data.Byte(0x204)
		if derr != nil {
			binfmt.Fail("%v", derr)
		}
		binfmt.Require(d == 0, "NCA distribution type")
		h, herr := data.Digest(ctx)
		if herr != nil {
			binfmt.Fail("%v", herr)
		}
		ncaHashes[name] = h
		binfmt.Require(hex.EncodeToString(h[:16]) == name[:32], "NCA content hash")
		tid, ctype, secs, serr := nca.Sections(ctx, data, true)
		if serr != nil {
			binfmt.Fail("%v", serr)
		}
		parsed[name] = &parsedNCA{titleID: tid, contentType: ctype, secs: secs}
	}

	program := findByType(parsed, 0)
	binfmt.Require(program != nil, "No program NCA in package")
	binfmt.Require(program.titleID == snap.TitleID, "Program ID mismatch")

	exefsRegion := sectionByKind(program, "pfs")
	binfmt.Require(exefsRegion != nil, "Program NCA has no ExeFS section")
	exefs, eerr := pfs0.Partition(ctx, exefsRegion)
	if eerr != nil {
		binfmt.Fail("%v", eerr)
	}
	romfsRegion := sectionByKind(program, "romfs")
	binfmt.Require(romfsRegion != nil, "Program NCA has no RomFS section")
	romfsFiles, rferr := romfs.Files(ctx, romfsRegion)
	if rferr != nil {
		binfmt.Fail("%v", rferr)
	}

	checked = []map[string]any{}
	var total int64
	for _, st := range inv {
		total += st.Size
	}
	var done int64

	for _, pair := range []struct {
		folder string
		packed map[string]*region.Region
	}{{"exefs", exefs}, {"romfs", romfsFiles}} {
		folderDir := filepath.Join(snap.Root, pair.folder)
		originals := map[string]string{}
		for p := range inv {
			if isWithin(folderDir, p) {
				rel, relerr := filepath.Rel(folderDir, p)
				if relerr != nil {
					continue
				}
				originals[filepath.ToSlash(rel)] = p
			}
		}
		binfmt.Require(len(originals) == len(pair.packed), "Packed file list differs from source")
		for name, p := range originals {
			if _, ok := pair.packed[name]; !ok {
				binfmt.Fail("Packed file list differs from source")
			}
			binfmt.Check(ctx)

			var expected []byte
			if captured == nil {
				h, herr := region.FileDigest(ctx, p)
				if herr != nil {
					binfmt.Fail("%v", herr)
				}
				expected = h
			} else {
				ptr, ok := captured[p]
				binfmt.Require(ok && ptr != nil, "Source hash was not captured: %s/%s", pair.folder, name)
				expected = *ptr
			}

			actual, aerr := pair.packed[name].Digest(ctx)
			if aerr != nil {
				binfmt.Fail("%v", aerr)
			}
			binfmt.Require(bytes.Equal(expected, actual), "Packed file differs: %s/%s", pair.folder, name)

			done += inv[p].Size
			if progress != nil {
				denom := total
				if denom < 1 {
					denom = 1
				}
				progress("Comparing source files", 82+17*float64(done)/float64(denom))
			}
			checked = append(checked, map[string]any{
				"path":   pair.folder + "/" + name,
				"size":   inv[p].Size,
				"sha256": hex.EncodeToString(expected),
			})
		}
	}

	meta := findByType(parsed, 1)
	binfmt.Require(meta != nil, "No meta NCA in package")
	binfmt.Require(len(meta.secs) > 0, "Meta NCA has no sections")
	cnmtFiles, cerr := pfs0.Partition(ctx, meta.secs[0].Data)
	if cerr != nil {
		binfmt.Fail("%v", cerr)
	}
	var cnmt *region.Region
	for _, v := range cnmtFiles {
		cnmt = v
		break
	}
	binfmt.Require(cnmt != nil, "CNMT missing")
	cnmtBytes, sberr := cnmt.Slice()
	if sberr != nil {
		binfmt.Fail("%v", sberr)
	}
	binfmt.Require(len(cnmtBytes) >= 0x20 && len(cnmtBytes) >= 32, "CNMT truncated")
	tid := binfmt.U64At(cnmtBytes, 0)
	ver := binfmt.U32At(cnmtBytes, 8)
	ctype := cnmtBytes[12]
	extra := int(binfmt.U16At(cnmtBytes, 14))
	count := int(binfmt.U16At(cnmtBytes, 16))
	binfmt.Require(tid == snap.TitleID && ver == snap.Version && ctype == 0x80, "CNMT title or version")
	binfmt.Require(bytes.Equal(binfmt.SHA256Sum(cnmtBytes[:len(cnmtBytes)-32]), cnmtBytes[len(cnmtBytes)-32:]), "CNMT digest")
	for i := 0; i < count; i++ {
		at := 0x20 + extra + i*0x38
		binfmt.Require(at >= 0 && at+0x38 <= len(cnmtBytes), "CNMT content record")
		digest := cnmtBytes[at : at+32]
		nid := hex.EncodeToString(cnmtBytes[at+32 : at+48])
		size := int64(le48(cnmtBytes[at+48 : at+54]))
		data, ok := ncas[nid+".nca"]
		binfmt.Require(ok && data.Size() == size && bytes.Equal(ncaHashes[nid+".nca"], digest), "CNMT content record")
	}

	control := findByType(parsed, 2)
	binfmt.Require(control != nil, "No control NCA in package")
	binfmt.Require(len(control.secs) > 0, "Control NCA has no sections")
	cfiles, cferr := romfs.Files(ctx, control.secs[0].Data)
	if cferr != nil {
		binfmt.Fail("%v", cferr)
	}
	_, hasNacp := cfiles["control.nacp"]
	binfmt.Require(hasNacp, "control.nacp missing")

	return checked, nil
}

// countingHashWriter writes through to w, hashing the bytes and reporting
// progress mapped into the [lo,hi] range as data flows.
type countingHashWriter struct {
	w      io.Writer
	h      hash.Hash
	done   int64
	total  int64
	notify binfmt.Progress
	label  string
	lo, hi float64
}

func (cw *countingHashWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	if n > 0 {
		cw.h.Write(p[:n])
		cw.done += int64(n)
		if cw.notify != nil && cw.total > 0 {
			cw.notify(cw.label, cw.lo+(cw.hi-cw.lo)*float64(cw.done)/float64(cw.total))
		}
	}
	return n, err
}

// buildExefs assembles the ExeFS PFS0 from the plain files in root/exefs and
// rejects nested directories with the Python error.
func buildExefs(root string) (s *stream.Stream, err error) {
	defer binfmt.Recover(&err)
	exefsDir := filepath.Join(root, "exefs")
	entries, derr := os.ReadDir(exefsDir)
	if derr != nil {
		binfmt.Fail("%v", derr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	type info struct {
		name   string
		size   int64
		isFile bool
		isDir  bool
	}
	infos := make([]info, 0, len(entries))
	for _, e := range entries {
		full := filepath.Join(exefsDir, e.Name())
		st, serr := os.Stat(full)
		if serr != nil {
			binfmt.Fail("%v", serr)
		}
		infos = append(infos, info{name: e.Name(), size: st.Size(), isFile: st.Mode().IsRegular(), isDir: st.IsDir()})
	}

	fsEntries := make([]pfs0.Entry, 0, len(infos))
	for _, in := range infos {
		if in.isFile {
			fsEntries = append(fsEntries, pfs0.Entry{Name: in.name, Part: stream.File(filepath.Join(exefsDir, in.name), in.size)})
		}
	}
	exblob, ferr := pfs0.FileSystem(fsEntries, false)
	if ferr != nil {
		binfmt.Fail("%v", ferr)
	}
	for _, in := range infos {
		if in.isDir {
			binfmt.Fail("ExeFS must contain files directly, not nested directories.")
		}
	}
	return exblob, nil
}

// packageNSP assembles the final NSP PFS0 from NCA streams. pfs0.Package only
// accepts stream.Part (not *stream.Stream), so the PFS0 framing is reproduced
// here to stream the multi-part NCA bodies directly (identical bytes).
func packageNSP(files []nspFile) (s *stream.Stream, err error) {
	defer binfmt.Recover(&err)

	namesLen := 0
	for _, f := range files {
		namesLen += len(f.name) + 1
	}
	const entrySize = 0x18
	const alignTo = int64(0x10)
	prefix := int64(0x10) + int64(entrySize)*int64(len(files))
	tableSize := binfmt.Align(prefix+int64(namesLen), alignTo) - prefix

	table := make([]byte, 0, tableSize)
	for _, f := range files {
		table = append(table, f.name...)
		table = append(table, 0)
	}
	table = append(table, make([]byte, tableSize-int64(len(table)))...)

	hdr := make([]byte, prefix)
	copy(hdr[0:4], "PFS0")
	binfmt.PutU32(hdr, 4, uint32(len(files)))
	binfmt.PutU32(hdr, 8, uint32(tableSize))
	binfmt.PutU32(hdr, 12, 0)

	parts := []any{hdr, table}
	dataOff := int64(0)
	nameOff := 0
	for i, f := range files {
		at := 0x10 + i*entrySize
		binfmt.PutU64(hdr, at, uint64(dataOff))
		binfmt.PutU64(hdr, at+8, uint64(f.body.Size()))
		binfmt.PutU32(hdr, at+16, uint32(nameOff))
		binfmt.PutU32(hdr, at+20, 0)
		nameOff += len(f.name) + 1
		padded := f.body.Pad(alignTo)
		parts = append(parts, padded)
		dataOff += padded.Size()
	}

	result, nerr := stream.New(parts...)
	if nerr != nil {
		return nil, nerr
	}
	if uint64(result.Size()) > 0xffffffffffffffff {
		binfmt.Fail("NSP size exceeds the PFS0 64-bit size limit.")
	}
	return result, nil
}

func findByType(parsed map[string]*parsedNCA, ctype byte) *parsedNCA {
	for _, v := range parsed {
		if v.contentType == ctype {
			return v
		}
	}
	return nil
}

func sectionByKind(p *parsedNCA, kind string) *region.Region {
	for _, s := range p.secs {
		if s.Kind == kind {
			return s.Data
		}
	}
	return nil
}

func le48(b []byte) uint64 {
	var v uint64
	for i := 0; i < 6; i++ {
		v |= uint64(b[i]) << (8 * i)
	}
	return v
}

func isExistError(err error) bool {
	return errors.Is(err, os.ErrExist)
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i * 8))
		}
	}
	return hex.EncodeToString(b)[:n]
}

func withSuffix(path, suffix string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + suffix
}

func comma(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func resolvePath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	dir := p
	var suffix []string
	for {
		if _, err := os.Lstat(dir); err == nil {
			if r, e := filepath.EvalSymlinks(dir); e == nil {
				out := r
				for i := len(suffix) - 1; i >= 0; i-- {
					out = filepath.Join(out, suffix[i])
				}
				return filepath.Clean(out)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		suffix = append(suffix, filepath.Base(dir))
		dir = parent
	}
	return p
}
