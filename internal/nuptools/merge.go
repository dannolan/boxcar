package nuptools

import (
	"path"
	"sort"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/nup"
)

// MergeGame merges a base game and its highest update into one runnable
// snapshot, the exact port of nup_tools.merge_game (Eglantine semantics:
// highest update, whole update ExeFS, RomFS shadowing, base-only file
// retention). selection -1 means "none" (auto-select highest update).
func MergeGame(packages []nup.Package, selection int) (merged nup.Package, err error) {
	defer binfmt.Recover(&err)

	var base *nup.Package
	for i := range packages {
		if packages[i].Kind == 0 {
			base = &packages[i]
			break
		}
	}
	var update *nup.Package
	for i := range packages {
		if packages[i].Kind == 1 {
			if update == nil || packages[i].Version > update.Version {
				update = &packages[i]
			}
		}
	}
	if selection != -1 {
		binfmt.Require(selection >= 0 && selection < len(packages), "Select a valid package.")
		chosen := &packages[selection]
		binfmt.Require(chosen.Kind == 0 || chosen.Kind == 1, "Selected package is not a game/update.")
		if chosen.Kind == 0 {
			update = nil
			base = chosen
		} else {
			update = chosen
		}
	}

	var program *nup.Package
	if base != nil {
		program = base
	} else {
		program = update
	}
	binfmt.Require(program != nil, "No base game or standalone update snapshot found.")

	exefs := base
	if update != nil {
		if _, ok := update.Files["exefs/main"]; ok {
			exefs = update
		}
	}
	binfmt.Require(exefs != nil, "No complete ExeFS found.")

	files := map[string]*nup.Source{}
	for n, s := range program.Files {
		if !stringsHasPrefix(n, "romfs/") && !stringsHasPrefix(n, "exefs/") {
			files[n] = s
		}
	}
	for n, s := range exefs.Files {
		if stringsHasPrefix(n, "exefs/") {
			files[n] = s
		}
	}
	if base != nil {
		for n, s := range base.Files {
			if stringsHasPrefix(n, "romfs/") {
				files[n] = s
			}
		}
	}
	if update != nil {
		binfmt.Require(update.TitleID&^0x1fff == program.TitleID&^0x1fff, "Unrelated game update.")
		for n, s := range update.Files {
			if stringsHasPrefix(n, "romfs/") {
				files[n] = s
			}
		}
	}
	binfmt.Require(files["exefs/main.npdm"] != nil && files["exefs/main"] != nil && hasRomfsPrefix(files),
		"Conversion requires a complete game snapshot, not a delta-only update.")

	npdmData := mustReadSmall(files["exefs/main.npdm"])
	tid, terr := NpdmID(npdmData)
	binfmt.Require(terr == nil, "%v", terr)
	binfmt.Require(tid == program.TitleID, "NPDM ID differs from the application ID; conversion would require changing original metadata.")

	for name := range files {
		parent := path.Dir(name)
		for parent != "." {
			_, ok := files[parent]
			binfmt.Require(!ok, "Update file/directory collision cannot be represented in NSP.")
			parent = path.Dir(parent)
		}
	}

	dirSet := map[string]bool{"exefs": true, "romfs": true}
	for _, p := range []*nup.Package{program, update} {
		if p == nil {
			continue
		}
		for _, n := range p.Directories {
			if !stringsHasPrefix(n, "exefs/") {
				if _, isFile := files[n]; !isFile {
					dirSet[n] = true
				}
			}
		}
	}
	for _, n := range exefs.Directories {
		if stringsHasPrefix(n, "exefs/") {
			dirSet[n] = true
		}
	}
	directories := make([]string, 0, len(dirSet))
	for d := range dirSet {
		directories = append(directories, d)
	}
	sort.Strings(directories)

	metadata := filterTLV(program.Metadata, 1)
	metadata = filterTLV(metadata, 7)
	metadata = append(metadata, nup.TLV{Kind: 1, Data: npdmData})
	head := mustReadHead(files["exefs/main"], 0x60)
	if len(head) == 0x60 && (string(head[:4]) == "NSO0" || string(head[0x10:0x14]) == "NRO0") {
		metadata = append(metadata, nup.TLV{Kind: 7, Data: head[0x40:0x60]})
	}
	if update != nil {
		var display []byte
		for _, t := range update.Metadata {
			if t.Kind == 4 {
				display = t.Data
				break
			}
		}
		if display != nil {
			metadata = filterTLV(metadata, 4)
			metadata = append(metadata, nup.TLV{Kind: 4, Data: display})
		}
	}

	version := program.Version
	if update != nil {
		version = update.Version
	}
	return nup.Package{
		TitleID:     tid,
		Version:     version,
		Kind:        0,
		Files:       files,
		Directories: directories,
		Metadata:    metadata,
	}, nil
}

// CompactPackages collapses base+updates into one latest runnable game tree
// while retaining DLC, the port of nup_tools.compact_packages.
func CompactPackages(packages []nup.Package) (result []nup.Package, err error) {
	defer binfmt.Recover(&err)
	var games []nup.Package
	for _, p := range packages {
		if p.Kind == 0 || p.Kind == 1 {
			games = append(games, p)
		}
	}
	if len(games) == 0 {
		return packages, nil
	}
	if len(games) == 1 && games[0].Kind == 0 {
		return packages, nil
	}
	merged, merr := MergeGame(packages, -1)
	if merr != nil {
		return nil, merr
	}
	result = append(result, merged)
	for _, p := range packages {
		if p.Kind != 0 && p.Kind != 1 {
			result = append(result, p)
		}
	}
	return result, nil
}

func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func hasRomfsPrefix(files map[string]*nup.Source) bool {
	for n := range files {
		if stringsHasPrefix(n, "romfs/") {
			return true
		}
	}
	return false
}
