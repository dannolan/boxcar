// Package nuptools ports nup_tools.py: NUP discovery, folder packing, and the
// NSP<->NUP conversions. It is the W3-A owner of the NSP-Box Go port.
//
// Contract (pinned; other code may rely on these signatures):
//
//	func NpdmID(data []byte) (uint64, error)
//	type DirStamp struct{ MtimeNS int64; Ino uint64 }
//	func TreeSources(ctx context.Context, root string, stamps map[string]DirStamp, notify func(label string, found int)) (files map[string]*nup.Source, dirs []string, err error)
//	func MetadataFor(files map[string]*nup.Source, title string) []nup.TLV
//	func Discover(ctx context.Context, folder string, stamps map[string]DirStamp, notify func(label string, found int)) ([]nup.Package, error)
//	type FolderIndex struct{ Root string; Packages []nup.Package; DirStamps map[string]DirStamp }
//	func (i *FolderIndex) Validate() error
//	func PrepareFolder(ctx context.Context, folder string, notify func(label string, found int)) (*FolderIndex, error)
//	func SelectPackages(packages []nup.Package, includeKinds []int) ([]nup.Package, error)
//	func Presentation(packages []nup.Package, title string, artwork []byte) ([]nup.Package, error)
//	func PackFolder(ctx context.Context, folder, output string, latestSnapshot bool, prepared *FolderIndex, includeKinds []int, title string, artwork []byte, opts nup.PackOptions) (nup.Result, error)
//	func NspPackages(ctx context.Context, path string, deep bool, notify binfmt.Progress) ([]nup.Package, func() error, error)
//	func NspToNup(ctx context.Context, path, output string, includeKinds []int, opts nup.PackOptions) (nup.Result, error)
//	func MergeGame(packages []nup.Package, selection int) (nup.Package, error)
//	func CompactPackages(packages []nup.Package) ([]nup.Package, error)
//	func BuildSnapshotNSP(ctx context.Context, pkg nup.Package, output string, verify bool, progress binfmt.Progress) (result map[string]any, err error)
//	func NupToNsp(ctx context.Context, path, output string, selection int, verify bool, includeKinds []int, progress binfmt.Progress) (map[string]any, error)
//	func InspectNup(ctx context.Context, path string, verify bool, progress binfmt.Progress) (map[string]any, error)
//
// Deviations from the Python reference:
//   - NSP->NUP conversion materializes region-backed files into temporary
//     files (nup.Source has no exported opener constructor); Python streams
//     directly from the mmap. The temp files are removed by the returned
//     source-validate closure.
//   - PackFolder returns nup.Result, which cannot carry the Python
//     latest_snapshot/original_bundle_bytes/omitted_version_bytes extras; they
//     are computed internally but not returned.
package nuptools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/metadata"
	"github.com/dannolan/boxcar/internal/nup"
)

// DirStamp is a captured directory stat (mtime nanoseconds + inode), the port
// of the Python (st_mtime_ns, st_ino) tuple.
type DirStamp struct {
	MtimeNS int64
	Ino     uint64
}

// dirStamp captures a directory's (mtime_ns, ino) via lstat.
func dirStamp(p string) (DirStamp, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return DirStamp{}, err
	}
	return DirStamp{MtimeNS: fi.ModTime().UnixNano(), Ino: fileIno(fi)}, nil
}

// NpdmID reads the application title ID from an ExeFS main.npdm image, the
// exact port of nup_tools.npdm_id (distinct error text from metadata.ReadTitleID).
func NpdmID(data []byte) (tid uint64, err error) {
	defer binfmt.Recover(&err)
	binfmt.Require(len(data) >= 0x80 && string(data[:4]) == "META", "Invalid NPDM header.")
	off := int(binfmt.U32At(data, 0x70))
	size := int(binfmt.U32At(data, 0x74))
	binfmt.Require(off >= 0x80 && size >= 0x20 && off+size <= len(data) && string(data[off:off+4]) == "ACI0",
		"Invalid NPDM application metadata.")
	return binfmt.U64At(data, off+0x10), nil
}

// TreeSources walks root, returning the file map and directory list. It is the
// exact port of nup_tools.tree_sources: sorted UTF-8 names, '.listing' skipped,
// links refused, depth<256, and directory stamps collected into stamps.
func TreeSources(ctx context.Context, root string, stamps map[string]DirStamp, notify func(label string, found int)) (files map[string]*nup.Source, dirs []string, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	binfmt.Require(isDir(root) && !isLinked(root), "Choose an unlinked source folder.")
	files = map[string]*nup.Source{}
	dirSet := map[string]bool{}

	var visit func(folder string, depth int)
	visit = func(folder string, depth int) {
		binfmt.Check(ctx)
		binfmt.Require(depth < 256, "Source tree is too deep.")
		if stamps != nil {
			st, serr := dirStamp(folder)
			binfmt.Require(serr == nil, "%v", serr)
			stamps[folder] = st
		}
		entries, derr := os.ReadDir(folder)
		binfmt.Require(derr == nil, "%v", derr)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			binfmt.Check(ctx)
			if err := nup.SafeName(entry.Name(), false); err != nil {
				binfmt.Fail("%v", err)
			}
			if entry.Name() == ".listing" {
				continue
			}
			p := filepath.Join(folder, entry.Name())
			st, serr := os.Lstat(p)
			binfmt.Require(serr == nil, "%v", serr)
			binfmt.Require(st.Mode()&os.ModeSymlink == 0, "Linked files/folders are not supported: %s", entry.Name())
			name := filepath.ToSlash(relPath(p, root))
			if st.IsDir() {
				dirSet[name] = true
				visit(p, depth+1)
			} else if st.Mode().IsRegular() {
				src, serr := nup.SourcePath(p)
				binfmt.Require(serr == nil, "%v", serr)
				files[name] = src
				if len(files)%1024 == 0 && notify != nil {
					notify(fmt.Sprintf("Indexing source files · %s", comma(len(files))), 1)
				}
			} else {
				binfmt.Fail("Special files are not supported: %s", entry.Name())
			}
		}
	}
	visit(root, 0)

	dirs = make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return files, dirs, nil
}

// MetadataFor builds the metadata TLV list for a package, the exact port of
// nup_tools.metadata_for. TLV kinds: 1 npdm, 7 NSO/NRO head, 2 nacp, 6
// publisher, 4 display, 3 icon, 5 title. It panics (binfmt.Failure) on read
// errors; callers wrap with binfmt.Recover.
func MetadataFor(files map[string]*nup.Source, title string) []nup.TLV {
	var items []nup.TLV
	if npdm, ok := files["exefs/main.npdm"]; ok {
		items = append(items, nup.TLV{Kind: 1, Data: mustReadSmall(npdm)})
	}
	if main, ok := files["exefs/main"]; ok {
		head := mustReadHead(main, 0x60)
		if len(head) == 0x60 && (string(head[:4]) == "NSO0" || string(head[0x10:0x14]) == "NRO0") {
			items = append(items, nup.TLV{Kind: 7, Data: head[0x40:0x60]})
		}
	}
	var nacp *nup.Source
	for _, p := range []string{"data/control/control.nacp", "control/control.nacp", "control.nacp"} {
		if s, ok := files[p]; ok {
			nacp = s
			break
		}
	}
	if nacp != nil {
		data := mustReadSmall(nacp)
		items = append(items, nup.TLV{Kind: 2, Data: data})
		if len(data) == 0x4000 {
			if data[0x3215] == 0 {
				name := strings.ToValidUTF8(string(cstr(data[:0x200])), "\uFFFD")
				publisher := strings.ToValidUTF8(string(cstr(data[0x200:0x300])), "\uFFFD")
				if name != "" {
					title = name
				}
				if publisher != "" {
					items = append(items, nup.TLV{Kind: 6, Data: []byte(publisher)})
				}
			}
			display := cstr(data[0x3060:0x3070])
			if len(display) != 0 {
				items = append(items, nup.TLV{Kind: 4, Data: display})
			}
		}
	}
	var icons []string
	for p := range files {
		base := path.Base(p)
		if strings.HasPrefix(base, "icon_") || strings.ToLower(base) == "icon.jpg" || strings.ToLower(base) == "icon.png" {
			icons = append(icons, p)
		}
	}
	sort.Strings(icons)
	var icon string
	for _, p := range icons {
		if strings.Contains(p, "AmericanEnglish") {
			icon = p
			break
		}
	}
	if icon == "" && len(icons) > 0 {
		icon = icons[0]
	}
	if icon != "" {
		items = append(items, nup.TLV{Kind: 3, Data: mustReadSmall(files[icon])})
	}
	if title != "" {
		items = append(items, nup.TLV{Kind: 5, Data: []byte(title)})
	}
	return items
}

// Discover locates conventional bundles or a lossless extraction manifest, the
// exact port of nup_tools.discover.
func Discover(ctx context.Context, folder string, stamps map[string]DirStamp, notify func(label string, found int)) (packages []nup.Package, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	folder, aerr := filepath.Abs(folder)
	binfmt.Require(aerr == nil, "%v", aerr)
	binfmt.Require(isDir(folder) && !isLinked(folder), "Choose an existing, unlinked folder.")
	if strings.EqualFold(filepath.Base(folder), "exefs") || strings.EqualFold(filepath.Base(folder), "romfs") {
		folder = filepath.Dir(folder)
	}

	manifestPath := filepath.Join(folder, "nup-manifest.json")
	if isFile(manifestPath) {
		st, serr := os.Stat(manifestPath)
		binfmt.Require(serr == nil, "%v", serr)
		binfmt.Require(st.Size() <= 64*1024*1024, "Extraction manifest is too large.")
		data, rerr := os.ReadFile(manifestPath)
		binfmt.Require(rerr == nil, "%v", rerr)
		var obj manifestDoc
		binfmt.Require(json.Unmarshal(data, &obj) == nil, "Invalid extraction manifest.")
		binfmt.Require(obj.Format == "NSP Box NUP extraction v1", "Unknown extraction manifest.")
		for _, rec := range obj.Packages {
			root := filepath.Join(folder, filepath.FromSlash(rec.Path))
			for parent := root; ; parent = filepath.Dir(parent) {
				if parent == filepath.Dir(folder) {
					break
				}
				binfmt.Require(!isLinked(parent), "Linked manifest source folder.")
			}
			files, dirs, terr := TreeSources(ctx, root, stamps, notify)
			binfmt.Require(terr == nil, "%v", terr)
			var items []nup.TLV
			for _, m := range rec.Metadata {
				pair, ok := m.([]any)
				binfmt.Require(ok && len(pair) == 2, "Invalid manifest metadata.")
				kind, ok := pair[0].(float64)
				binfmt.Require(ok, "Invalid manifest metadata kind.")
				b64, ok := pair[1].(string)
				binfmt.Require(ok, "Invalid manifest metadata data.")
				dec, derr := base64.StdEncoding.DecodeString(b64)
				binfmt.Require(derr == nil, "Invalid manifest metadata base64.")
				items = append(items, nup.TLV{Kind: uint16(kind), Data: dec})
			}
			tid, perr := strconv.ParseUint(rec.TitleID, 16, 64)
			binfmt.Require(perr == nil, "Invalid manifest title ID.")
			packages = append(packages, nup.Package{
				TitleID:     tid,
				Version:     rec.Version,
				Kind:        rec.Kind,
				Files:       files,
				Directories: dirs,
				Metadata:    items,
				Flags:       rec.Flags,
				Label:       rec.Path,
			})
		}
		recomputeUpdateFlags(packages)
		return packages, nil
	}

	content := func(p string) bool {
		if stamps != nil && isDir(p) {
			st, serr := dirStamp(p)
			binfmt.Require(serr == nil, "%v", serr)
			stamps[p] = st
		}
		return isDir(filepath.Join(p, "exefs")) || isDir(filepath.Join(p, "romfs"))
	}

	add := func(root string, kind int, version uint32, tid uint64) {
		files, dirs, terr := TreeSources(ctx, root, stamps, notify)
		binfmt.Require(terr == nil, "%v", terr)
		binfmt.Require(len(files) > 0, "Empty content package: %s", filepath.Base(root))
		if npdmSrc := files["exefs/main.npdm"]; npdmSrc != nil {
			data, rerr := npdmSrc.ReadSmall(0)
			binfmt.Require(rerr == nil, "%v", rerr)
			tid, terr = NpdmID(data)
			binfmt.Require(terr == nil, "%v", terr)
		}
		if tid == 0 {
			for cur := root; ; cur = filepath.Dir(cur) {
				if v, ok := findHex16(filepath.Base(cur)); ok {
					tid = v
					break
				}
				if cur == folder {
					break
				}
			}
		}
		binfmt.Require(tid != 0, "No original title ID found in NPDM/folder: %s", filepath.Base(root))
		label := relPath(root, folder)
		if label == "" {
			label = "."
		}
		packages = append(packages, nup.Package{
			TitleID:     tid,
			Version:     version,
			Kind:        kind,
			Files:       files,
			Directories: dirs,
			Metadata:    MetadataFor(files, metadata.CleanTitle(filepath.Base(folder))),
			Label:       label,
		})
	}

	bundle := (isDir(filepath.Join(folder, "base")) || isDir(filepath.Join(folder, "updates")) || isDir(filepath.Join(folder, "dlc")) ||
		strings.EqualFold(filepath.Base(folder), "updates") || strings.EqualFold(filepath.Base(folder), "dlc")) && !content(folder)

	if bundle {
		base := filepath.Join(folder, "base")
		if content(base) {
			add(base, 0, 0, 0)
		}
		updates := folder
		if !strings.EqualFold(filepath.Base(folder), "updates") {
			updates = filepath.Join(folder, "updates")
		}
		if isDir(updates) {
			binfmt.Require(!isLinked(updates), "Linked updates folder.")
			entries, derr := os.ReadDir(updates)
			binfmt.Require(derr == nil, "%v", derr)
			sort.Slice(entries, func(i, j int) bool {
				ni, nj := entries[i].Name(), entries[j].Name()
				ki, kj := decimalSortValue(ni), decimalSortValue(nj)
				if ki != kj {
					return ki < kj
				}
				return ni < nj
			})
			for _, e := range entries {
				name := e.Name()
				root := filepath.Join(updates, name)
				if isDir(root) && isASCIIDecimal(name) && content(root) {
					add(root, 1, parseVersion(name), 0)
				}
			}
		}
		dlcroot := folder
		if !strings.EqualFold(filepath.Base(folder), "dlc") {
			dlcroot = filepath.Join(folder, "dlc")
		}
		if isDir(dlcroot) {
			binfmt.Require(!isLinked(dlcroot), "Linked DLC folder.")
			entries, derr := os.ReadDir(dlcroot)
			binfmt.Require(derr == nil, "%v", derr)
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, e := range entries {
				title := filepath.Join(dlcroot, e.Name())
				if !isDir(title) {
					continue
				}
				binfmt.Require(!isLinked(title), "Linked DLC folder.")
				tid, ok := findHex16(e.Name())
				binfmt.Require(ok, "DLC folder needs its original 16-digit title ID.")
				if content(title) {
					add(title, 2, 0, tid)
				} else {
					sub, derr := os.ReadDir(title)
					binfmt.Require(derr == nil, "%v", derr)
					sort.Slice(sub, func(i, j int) bool { return sub[i].Name() < sub[j].Name() })
					for _, se := range sub {
						root := filepath.Join(title, se.Name())
						if isDir(root) && content(root) {
							ver := uint32(0)
							if isASCIIDecimal(se.Name()) {
								ver = parseVersion(se.Name())
							}
							add(root, 2, ver, tid)
						}
					}
				}
			}
		}
	} else if content(folder) {
		isDLC := !isFile(filepath.Join(folder, "exefs", "main.npdm"))
		version := uint32(0)
		if isASCIIDecimal(filepath.Base(folder)) && len(filepath.Base(folder)) <= 10 {
			version = parseVersion(filepath.Base(folder))
		}
		isUpdate := strings.EqualFold(filepath.Base(filepath.Dir(folder)), "updates")
		kind := 0
		if isDLC {
			kind = 2
		} else if isUpdate {
			kind = 1
		}
		add(folder, kind, version, 0)
	} else {
		binfmt.Fail("No extracted exefs/romfs package or base/updates/dlc bundle found.")
	}

	recomputeUpdateFlags(packages)
	binfmt.Require(len(packages) > 0, "No content packages found. Empty base folders are skipped.")
	return packages, nil
}

// recomputeUpdateFlags sets each update package's standalone flag from whether
// a base package is present (port of the discover tail loop).
func recomputeUpdateFlags(packages []nup.Package) {
	hasBase := false
	for _, p := range packages {
		if p.Kind == 0 {
			hasBase = true
			break
		}
	}
	for i := range packages {
		if packages[i].Kind == 1 {
			if hasBase {
				packages[i].Flags = 0
			} else {
				packages[i].Flags = 1
			}
		}
	}
}

// FolderIndex is a prepared folder: its resolved root, discovered packages and
// the directory stamps captured during inspection.
type FolderIndex struct {
	Root      string
	Packages  []nup.Package
	DirStamps map[string]DirStamp
}

// Validate re-stats every recorded directory and fails on any change, the port
// of FolderIndex.validate.
func (i *FolderIndex) Validate() (err error) {
	defer binfmt.Recover(&err)
	for p, expected := range i.DirStamps {
		st, serr := dirStamp(p)
		binfmt.Require(serr == nil, "%v", serr)
		binfmt.Require(!isLinked(p) && st.MtimeNS == expected.MtimeNS && st.Ino == expected.Ino,
			"Source folder changed since inspection. Inspect it again.")
	}
	return nil
}

// PrepareFolder discovers a folder and records stamps for each package root
// and its ancestors, the port of nup_tools.prepare_folder.
func PrepareFolder(ctx context.Context, folder string, notify func(label string, found int)) (index *FolderIndex, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	root := resolvePath(folder)
	if strings.EqualFold(filepath.Base(root), "exefs") || strings.EqualFold(filepath.Base(root), "romfs") {
		root = filepath.Dir(root)
	}
	stamps := map[string]DirStamp{}
	packages, derr := Discover(ctx, root, stamps, notify)
	binfmt.Require(derr == nil, "%v", derr)
	for _, p := range packages {
		current := filepath.Join(root, filepath.FromSlash(p.Label))
		for isWithin(root, current) {
			if isDir(current) {
				st, serr := dirStamp(current)
				binfmt.Require(serr == nil, "%v", serr)
				stamps[current] = st
			}
			if current == root {
				break
			}
			current = filepath.Dir(current)
		}
	}
	return &FolderIndex{Root: root, Packages: packages, DirStamps: stamps}, nil
}

// SelectPackages filters packages by kind and recomputes update flags, the
// port of nup_tools.select_packages. includeKinds == nil selects all.
func SelectPackages(packages []nup.Package, includeKinds []int) (selected []nup.Package, err error) {
	defer binfmt.Recover(&err)
	include := map[int]bool{}
	if includeKinds != nil {
		for _, k := range includeKinds {
			include[k] = true
		}
	}
	for _, p := range packages {
		if includeKinds == nil || include[p.Kind] {
			selected = append(selected, p)
		}
	}
	binfmt.Require(len(selected) > 0, "No matching packages selected. Include Base, Updates or DLC present in this source.")
	hasBase := false
	for _, p := range selected {
		if p.Kind == 0 {
			hasBase = true
			break
		}
	}
	for i := range selected {
		if selected[i].Kind == 1 {
			if hasBase {
				selected[i].Flags = 0
			} else {
				selected[i].Flags = 1
			}
		}
	}
	return selected, nil
}

// Presentation overrides presentation TLVs (title/nacp/icon) without touching
// the archived filesystem bytes, the port of nup_tools.presentation.
func Presentation(packages []nup.Package, title string, artwork []byte) (result []nup.Package, err error) {
	defer binfmt.Recover(&err)
	title = strings.TrimSpace(title)
	binfmt.Require(len([]byte(title)) <= 0x1ff, "Title is too long (maximum 511 UTF-8 bytes).")
	if title == "" && artwork == nil {
		return packages, nil
	}
	hasGame := false
	for _, p := range packages {
		if p.Kind == 0 || p.Kind == 1 {
			hasGame = true
			break
		}
	}
	result = make([]nup.Package, 0, len(packages))
	for _, p := range packages {
		if (p.Kind != 0 && p.Kind != 1) && hasGame {
			result = append(result, p)
			continue
		}
		md := append([]nup.TLV(nil), p.Metadata...)
		if title != "" {
			md = filterTLV(md, 5)
			md = append(md, nup.TLV{Kind: 5, Data: []byte(title)})
			var original []byte
			for _, t := range md {
				if t.Kind == 2 {
					original = t.Data
					break
				}
			}
			if original != nil && len(original) == 0x4000 && original[0x3215] == 0 {
				nacp := append([]byte(nil), original...)
				for lang := 0; lang < 16; lang++ {
					start := lang * 0x300
					for i := 0; i < 0x200; i++ {
						nacp[start+i] = 0
					}
					copy(nacp[start:start+0x200], []byte(title))
				}
				md = filterTLV(md, 2)
				md = append(md, nup.TLV{Kind: 2, Data: nacp})
			}
		}
		if artwork != nil {
			md = filterTLV(md, 3)
			md = append(md, nup.TLV{Kind: 3, Data: artwork})
		}
		p.Metadata = md
		result = append(result, p)
	}
	return result, nil
}

// PackFolder packs a folder into a NUP, the port of nup_tools.pack_folder.
func PackFolder(ctx context.Context, folder, output string, latestSnapshot bool, prepared *FolderIndex, includeKinds []int, title string, artwork []byte, opts nup.PackOptions) (res nup.Result, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	folder = resolvePath(folder)
	output = resolvePath(output)
	if strings.EqualFold(filepath.Base(folder), "exefs") || strings.EqualFold(filepath.Base(folder), "romfs") {
		folder = filepath.Dir(folder)
	}
	binfmt.Require(!isWithin(folder, output), "Save the NUP outside the source folder.")

	notify := func(label string, found int) {
		if opts.Progress != nil {
			opts.Progress(label, float64(found))
		}
	}

	var index *FolderIndex
	if prepared != nil {
		index = prepared
	} else {
		index, err = PrepareFolder(ctx, folder, notify)
		if err != nil {
			return res, err
		}
	}
	binfmt.Require(index.Root == folder, "Inspected source does not match the selected folder.")
	if err := index.Validate(); err != nil {
		return res, err
	}
	packages := index.Packages
	packages, err = SelectPackages(packages, includeKinds)
	if err != nil {
		return res, err
	}
	var before int64
	for _, p := range packages {
		for _, s := range p.Files {
			before += s.Size()
		}
	}
	if latestSnapshot {
		packages, err = CompactPackages(packages)
		if err != nil {
			return res, err
		}
	}
	packages, err = Presentation(packages, title, artwork)
	if err != nil {
		return res, err
	}
	opts.SourceGuard = index.Validate
	res, err = nup.Pack(ctx, packages, output, opts)
	if err != nil {
		return res, err
	}
	// Python adds latest_snapshot/original_bundle_bytes/omitted_version_bytes
	// here; nup.Result cannot carry them (documented deviation).
	_ = before
	return res, nil
}

// --- helpers ---

type manifestPkg struct {
	Path     string `json:"path"`
	TitleID  string `json:"title_id"`
	Version  uint32 `json:"version"`
	Kind     int    `json:"kind"`
	Flags    int    `json:"flags"`
	Metadata []any  `json:"metadata"`
}

type manifestDoc struct {
	Format   string        `json:"format"`
	Packages []manifestPkg `json:"packages"`
}

func filterTLV(items []nup.TLV, kind uint16) []nup.TLV {
	out := make([]nup.TLV, 0, len(items))
	for _, t := range items {
		if t.Kind != kind {
			out = append(out, t)
		}
	}
	return out
}

func mustReadSmall(src *nup.Source) []byte {
	data, err := src.ReadSmall(0)
	if err != nil {
		binfmt.Fail("%v", err)
	}
	return data
}

func mustReadHead(src *nup.Source, n int64) []byte {
	r, err := src.Open()
	if err != nil {
		binfmt.Fail("%v", err)
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		binfmt.Fail("%v", err)
	}
	return data
}

// cstr returns the NUL-terminated prefix of b decoded as UTF-8 (replace).
func cstr(b []byte) []byte {
	if i := indexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return b
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// findHex16 finds the first 16-hex-digit run not surrounded by hex digits,
// matching the Python negative-lookaround regex.
func findHex16(s string) (uint64, bool) {
	for i := 0; i+16 <= len(s); i++ {
		v, err := strconv.ParseUint(s[i:i+16], 16, 64)
		if err != nil {
			continue
		}
		if i > 0 && isHexDigit(s[i-1]) {
			continue
		}
		if i+16 < len(s) && isHexDigit(s[i+16]) {
			continue
		}
		return v, true
	}
	return 0, false
}

func isASCIIDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func parseVersion(name string) uint32 {
	v, err := strconv.ParseUint(name, 10, 32)
	binfmt.Require(err == nil, "Version number is out of range.")
	return uint32(v)
}

func decimalSortValue(name string) int64 {
	if !isASCIIDecimal(name) {
		return -1
	}
	v, err := strconv.ParseUint(name, 10, 64)
	if err != nil {
		return 1<<63 - 1
	}
	return int64(v)
}

func isLinked(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func relPath(root, folder string) string {
	rel, err := filepath.Rel(folder, root)
	if err != nil {
		return "."
	}
	return filepath.ToSlash(rel)
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
