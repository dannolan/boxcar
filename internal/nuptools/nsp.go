package nuptools

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/nca"
	"github.com/dannolan/boxcar/internal/nup"
	"github.com/dannolan/boxcar/internal/pfs0"
	"github.com/dannolan/boxcar/internal/region"
	"github.com/dannolan/boxcar/internal/romfs"
)

var ncaNameRe = regexp.MustCompile(`^[0-9a-f]{32}(\.cnmt)?\.nca$`)

type parsedNCA struct {
	titleID     uint64
	contentType byte
	secs        []nca.NamedSection
}

// NspPackages parses a plaintext NSP into NUP packages, the port of the
// nsp_packages context manager. It returns the packages plus a source-validate
// closure that re-validates the NSP source stamp and removes the temporary
// files materialized for region-backed sources; the caller must invoke it.
func NspPackages(ctx context.Context, path string, deep bool, notify binfmt.Progress) (packages []nup.Package, validate func() error, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	source, serr := nup.SourcePath(path)
	if serr != nil {
		return nil, nil, serr
	}
	f, ferr := os.Open(path)
	if ferr != nil {
		return nil, nil, ferr
	}
	defer f.Close()
	r, rerr := region.New(f)
	if rerr != nil {
		return nil, nil, rerr
	}

	tempDir, terr := os.MkdirTemp("", "nspbox-nsp-")
	if terr != nil {
		return nil, nil, terr
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tempDir)
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			if f, ok := r.(binfmt.Failure); ok {
				err = fmt.Errorf("Malformed or unsupported plaintext NSP: %v", f.Err)
			} else {
				panic(r)
			}
		}
	}()

	var magic [4]byte
	binfmt.Require(r.ReadAt(0, magic[:]) == nil && string(magic[:]) == "PFS0", "Expected a plaintext NSP/PFS0 container.")
	ncas, perr := pfs0.Partition(ctx, r)
	binfmt.Require(perr == nil, "%v", perr)

	parsed := map[string]*parsedNCA{}
	hashes := map[string][]byte{}
	for name, ncaRegion := range ncas {
		binfmt.Check(ctx)
		binfmt.Require(ncaRegion.Size() >= 0xc00 && string(readAt(ncaRegion, 0x200, 4)) == "NCA3" && readAt(ncaRegion, 0x204, 1)[0] == 0,
			"Only plaintext NSP-Box NSPs are supported; encrypted/retail content is not supported.")
		binfmt.Require(ncaNameRe.MatchString(name), "Unsupported NCA filename.")
		var digest []byte
		if deep {
			if notify != nil {
				notify("Checking source NCA integrity", 1)
			}
			d, derr := ncaRegion.Digest(ctx)
			binfmt.Require(derr == nil, "%v", derr)
			digest = d
			binfmt.Require(name[:32] == hex.EncodeToString(digest[:16]), "NCA content hash does not match its name.")
		}
		for i := 0; i < 4; i++ {
			start := int64(u32(ncaRegion, int64(0x240+i*16)))
			end := int64(u32(ncaRegion, int64(0x244+i*16)))
			if start == 0 && end == 0 {
				continue
			}
			binfmt.Require(6 <= start && start < end && end*512 <= ncaRegion.Size(), "NCA section outside container.")
			header := readAt(ncaRegion, int64(0x400+i*512), 0x200)
			if header[3] == 2 {
				block := binfmt.U32At(header, 0x28)
				binfmt.Require(block > 0 && block <= 16*1024*1024, "Unsupported NCA integrity block size.")
			} else if header[3] == 3 {
				for j := 0; j < 6; j++ {
					binfmt.Require(binfmt.U32At(header, 0x18+j*24+16) <= 24, "Unsupported IVFC block size.")
				}
			}
		}
		tid, ctype, secs, serr := nca.Sections(ctx, ncaRegion, deep)
		binfmt.Require(serr == nil, "%v", serr)
		parsed[name] = &parsedNCA{titleID: tid, contentType: ctype, secs: secs}
		hashes[name] = digest
	}

	var metas []string
	for name, p := range parsed {
		if p.contentType == 1 {
			metas = append(metas, name)
		}
	}
	binfmt.Require(len(metas) > 0, "NSP has no title metadata.")

	claimed := map[string]bool{}
	for _, metaname := range metas {
		meta := parsed[metaname]
		binfmt.Require(len(meta.secs) > 0, "NSP has no title metadata.")
		cnmts, perr := pfs0.Partition(ctx, meta.secs[0].Data)
		binfmt.Require(perr == nil, "%v", perr)
		binfmt.Require(len(cnmts) == 1, "Expected one CNMT per metadata NCA.")
		var cnmtName string
		var cnmtRegion *region.Region
		for n, reg := range cnmts {
			cnmtName, cnmtRegion = n, reg
		}
		cnmtBytes, serr := cnmtRegion.Slice()
		binfmt.Require(serr == nil, "%v", serr)
		tid := binfmt.U64At(cnmtBytes, 0)
		version := binfmt.U32At(cnmtBytes, 8)
		ctype := cnmtBytes[12]
		extra := int(binfmt.U16At(cnmtBytes, 14))
		count := int(binfmt.U16At(cnmtBytes, 16))
		binfmt.Require(ctype == 0x80 || ctype == 0x82, "Only application snapshots and DLC NSPs from NSP Box can be converted.")
		binfmt.Require(bytes.Equal(binfmt.SHA256Sum(cnmtBytes[:len(cnmtBytes)-32]), cnmtBytes[len(cnmtBytes)-32:]), "CNMT hash mismatch.")

		files := map[string]*nup.Source{}
		dirSet := map[string]bool{}
		claimed[metaname] = true

		addFiles := func(prefix string, views map[string]*region.Region) {
			for name, view := range views {
				if _, perr := nup.PathParts(name); perr != nil {
					binfmt.Fail("%v", perr)
				}
				key := prefix + "/" + name
				_, dup := files[key]
				binfmt.Require(!dup, "Duplicate NSP file path.")
				tmpPath, merr := materializeRegion(ctx, view, tempDir)
				binfmt.Require(merr == nil, "%v", merr)
				src, sperr := nup.SourcePath(tmpPath)
				binfmt.Require(sperr == nil, "%v", sperr)
				files[key] = src
			}
		}

		for j := 0; j < count; j++ {
			at := 0x20 + extra + j*0x38
			digest := cnmtBytes[at : at+32]
			name := hex.EncodeToString(cnmtBytes[at+32:at+48]) + ".nca"
			size := le48(cnmtBytes[at+48 : at+54])
			ncaRegion, ok := ncas[name]
			binfmt.Require(ok && ncaRegion.Size() == size && name[:32] == hex.EncodeToString(digest[:16]) &&
				(!deep || bytes.Equal(hashes[name], digest)), "CNMT content record mismatch.")
			binfmt.Require(!claimed[name], "Shared NCA records are unsupported.")
			claimed[name] = true
			content := parsed[name]
			binfmt.Require(content.titleID == tid, "NSP content title ID mismatch.")
			switch content.contentType {
			case 0:
				exefsRegion := findSection(content.secs, "pfs")
				romfsRegion := findSection(content.secs, "romfs")
				binfmt.Require(exefsRegion != nil && romfsRegion != nil, "NSP content title ID mismatch.")
				exefsFiles, perr := pfs0.Partition(ctx, exefsRegion)
				binfmt.Require(perr == nil, "%v", perr)
				addFiles("exefs", exefsFiles)
				romfsFiles, rerr := romfs.Files(ctx, romfsRegion)
				binfmt.Require(rerr == nil, "%v", rerr)
				addFiles("romfs", romfsFiles)
				dirSet["exefs"] = true
				dirSet["romfs"] = true
			case 2:
				binfmt.Require(len(content.secs) > 0, "NSP content title ID mismatch.")
				romfsFiles, rerr := romfs.Files(ctx, content.secs[0].Data)
				binfmt.Require(rerr == nil, "%v", rerr)
				addFiles("data/control", romfsFiles)
			case 4:
				binfmt.Require(len(content.secs) > 0, "NSP content title ID mismatch.")
				romfsFiles, rerr := romfs.Files(ctx, content.secs[0].Data)
				binfmt.Require(rerr == nil, "%v", rerr)
				addFiles("romfs", romfsFiles)
				dirSet["romfs"] = true
			default:
				binfmt.Fail("Unsupported NSP content type.")
			}
		}

		files["data/nsp/"+cnmtName] = nup.SourceData(cnmtBytes)
		if ctype == 0x80 {
			binfmt.Require(files["exefs/main.npdm"] != nil && files["exefs/main"] != nil && hasRomfsPrefix(files),
				"NSP does not contain a complete game snapshot.")
		}
		if npdmSrc := files["exefs/main.npdm"]; npdmSrc != nil {
			npdmData, rerr := npdmSrc.ReadSmall(0)
			binfmt.Require(rerr == nil, "%v", rerr)
			npdmTid, nerr := NpdmID(npdmData)
			binfmt.Require(nerr == nil, "%v", nerr)
			binfmt.Require(npdmTid == tid, "NPDM/title identity mismatch.")
		}

		kind := 0
		if ctype == 0x82 {
			kind = 2
		}
		directories := make([]string, 0, len(dirSet))
		for d := range dirSet {
			directories = append(directories, d)
		}
		sort.Strings(directories)
		packages = append(packages, nup.Package{
			TitleID:     tid,
			Version:     version,
			Kind:        kind,
			Files:       files,
			Directories: directories,
			Metadata:    MetadataFor(files, ""),
		})
	}

	binfmt.Require(len(ncas) == len(claimed), "Unrecognized/unreferenced NCA content; refusing to drop it.")
	if err := source.Validate(); err != nil {
		return nil, nil, err
	}
	if deep {
		if notify != nil {
			notify("Plaintext NSP integrity checked", 2)
		}
	} else if notify != nil {
		notify("NSP package index ready · full integrity checked during conversion", 2)
	}

	var once sync.Once
	cleanup := func() { once.Do(func() { _ = os.RemoveAll(tempDir) }) }
	validate = func() error {
		if err := source.Validate(); err != nil {
			return err
		}
		cleanup()
		return nil
	}
	committed = true
	return packages, validate, nil
}

// NspToNsp converts a plaintext NSP into a NUP, the port of nsp_to_nup.
func NspToNup(ctx context.Context, path, output string, includeKinds []int, opts nup.PackOptions) (res nup.Result, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	binfmt.Require(resolvePath(path) != resolvePath(output), "Input and output must differ.")
	packages, validate, perr := NspPackages(ctx, path, true, opts.Progress)
	if perr != nil {
		return res, perr
	}
	defer validate()
	selected, serr := SelectPackages(packages, includeKinds)
	if serr != nil {
		return res, serr
	}
	opts.SourceGuard = validate
	return nup.Pack(ctx, selected, output, opts)
}

// materializeRegion streams a region into a fresh temp file under dir.
func materializeRegion(ctx context.Context, r *region.Region, dir string) (string, error) {
	tmp, err := os.CreateTemp(dir, "f-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	buf := make([]byte, 4<<20)
	for off := int64(0); off < r.Size(); off += int64(len(buf)) {
		binfmt.Check(ctx)
		n := int64(len(buf))
		if r.Size()-off < n {
			n = r.Size() - off
		}
		if err := r.ReadAt(off, buf[:n]); err != nil {
			tmp.Close()
			return "", err
		}
		if _, err := tmp.Write(buf[:n]); err != nil {
			tmp.Close()
			return "", err
		}
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return name, nil
}

func findSection(secs []nca.NamedSection, kind string) *region.Region {
	for _, s := range secs {
		if s.Kind == kind {
			return s.Data
		}
	}
	return nil
}

func u32(r *region.Region, off int64) uint32 {
	v, err := r.U32(off)
	binfmt.Require(err == nil, "read u32: %v", err)
	return v
}

func readAt(r *region.Region, off, n int64) []byte {
	b := make([]byte, n)
	if err := r.ReadAt(off, b); err != nil {
		binfmt.Fail("read: %v", err)
	}
	return b
}

func le48(b []byte) int64 {
	var v uint64
	for i := 0; i < 6; i++ {
		v |= uint64(b[i]) << (8 * i)
	}
	return int64(v)
}
