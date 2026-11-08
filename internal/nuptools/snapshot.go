package nuptools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/diskfree"
	"github.com/dannolan/boxcar/internal/dlc"
	"github.com/dannolan/boxcar/internal/metadata"
	"github.com/dannolan/boxcar/internal/nsp"
	"github.com/dannolan/boxcar/internal/nup"
	"github.com/dannolan/boxcar/internal/snapshot"
)

// BuildSnapshotNSP rebuilds a single package into an NSP, honoring explicit
// presentation metadata overrides, the port of nup_tools.build_snapshot_nsp.
// The game path reuses internal/nsp.Build by materializing the snapshot into a
// temp dir (deviation: Python rebuilds inline; the CNMT version/kind match).
func BuildSnapshotNSP(ctx context.Context, pkg nup.Package, output string, verify bool, progress binfmt.Progress) (result map[string]any, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	binfmt.Require(!pathExists(output), "NSP output already exists.")

	tempDir, terr := os.MkdirTemp(filepath.Dir(output), ".nup-nsp-")
	binfmt.Require(terr == nil, "%v", terr)
	defer os.RemoveAll(tempDir)

	var total int64
	for _, s := range pkg.Files {
		total += s.Size()
	}
	var done int64
	for name, source := range pkg.Files {
		if !strings.HasPrefix(name, "exefs/") && !strings.HasPrefix(name, "romfs/") &&
			!strings.HasPrefix(name, "data/control/") && !strings.HasPrefix(name, "control/") && name != "control.nacp" {
			continue
		}
		parts, perr := nup.PathParts(name)
		binfmt.Require(perr == nil, "%v", perr)
		target := filepath.Join(append([]string{tempDir}, parts...)...)
		if merr := os.MkdirAll(filepath.Dir(target), 0o755); merr != nil {
			return nil, merr
		}
		out, oerr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if oerr != nil {
			return nil, oerr
		}
		r, rerr := source.Open()
		if rerr != nil {
			out.Close()
			return nil, rerr
		}
		count := int64(0)
		buf := make([]byte, 1<<20)
		for {
			binfmt.Check(ctx)
			n, rerr := r.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					r.Close()
					out.Close()
					return nil, werr
				}
				count += int64(n)
				done += int64(n)
				binfmt.Require(count <= source.Size(), "Source size changed.")
				if progress != nil {
					progress("Preparing snapshot", 20*float64(done)/float64(max64(total, 1)))
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				r.Close()
				out.Close()
				return nil, rerr
			}
		}
		r.Close()
		out.Close()
		binfmt.Require(count == source.Size(), "Incomplete snapshot file.")
		if verr := source.Validate(); verr != nil {
			return nil, verr
		}
	}

	if pkg.Kind == 2 {
		snap := dlc.Snapshot{Root: tempDir, TitleID: pkg.TitleID, Version: pkg.Version, Label: "NUP DLC"}
		report, berr := dlc.Build(ctx, snap, output, dlc.Options{VerifyOutput: verify, Progress: progress})
		if berr != nil {
			return nil, berr
		}
		return map[string]any{"output": output, "size": report.Size, "verified": verify}, nil
	}

	control := filepath.Join(tempDir, "data", "control")
	original := findTLV(pkg.Metadata, 2)
	if original != nil {
		binfmt.Require(len(original) == 0x4000, "Original NACP is not a supported 0x4000-byte control record.")
		if merr := os.MkdirAll(control, 0o755); merr != nil {
			return nil, merr
		}
		if werr := os.WriteFile(filepath.Join(control, "control.nacp"), original, 0o644); werr != nil {
			return nil, werr
		}
	}
	if !isFile(filepath.Join(control, "control.nacp")) {
		if merr := os.MkdirAll(control, 0o755); merr != nil {
			return nil, merr
		}
		original = findTLV(pkg.Metadata, 2)
		if original == nil {
			for _, p := range []string{filepath.Join(tempDir, "control", "control.nacp"), filepath.Join(tempDir, "control.nacp")} {
				if isFile(p) {
					original, _ = os.ReadFile(p)
					break
				}
			}
		}
		if original == nil {
			title := tlvString(pkg.Metadata, 5, "NUP game")
			display := tlvString(pkg.Metadata, 4, "Snapshot")
			entries, _, cerr := metadata.ControlData(pkg.TitleID, display, title, nil, nil)
			binfmt.Require(cerr == nil, "%v", cerr)
			for _, kv := range entries {
				if kv.Name == "control.nacp" {
					original = kv.Data
					break
				}
			}
		}
		binfmt.Require(len(original) == 0x4000, "Original NACP is not a supported 0x4000-byte control record.")
		if werr := os.WriteFile(filepath.Join(control, "control.nacp"), original, 0o644); werr != nil {
			return nil, werr
		}
	}
	if icon := findTLV(pkg.Metadata, 3); icon != nil {
		if werr := os.WriteFile(filepath.Join(control, "icon_AmericanEnglish.dat"), icon, 0o644); werr != nil {
			return nil, werr
		}
	}

	title := tlvString(pkg.Metadata, 5, "NUP game")
	display := tlvString(pkg.Metadata, 4, "Snapshot")
	snap := snapshot.Snapshot{
		Root:     tempDir,
		TitleID:  pkg.TitleID,
		Version:  pkg.Version,
		NacpPath: filepath.Join(control, "control.nacp"),
	}
	report, berr := nsp.Build(ctx, snap, nsp.Options{
		Output:         output,
		Title:          title,
		DisplayVersion: display,
		Verify:         verify,
		Progress:       progress,
	})
	if berr != nil {
		return nil, berr
	}
	return map[string]any{"output": output, "size": report.Size, "verified": verify}, nil
}

// NupToNsp converts a NUP archive into NSP(s), the port of nup_tools.nup_to_nsp.
// selection -1 means whole-archive mode (game+highest update and separate DLC).
func NupToNsp(ctx context.Context, path, output string, selection int, verify bool, includeKinds []int, progress binfmt.Progress) (result map[string]any, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	output, aerr := filepath.Abs(output)
	binfmt.Require(aerr == nil, "%v", aerr)
	binfmt.Require(!pathExists(output), "Output already exists; choose a new path.")
	if merr := os.MkdirAll(filepath.Dir(output), 0o755); merr != nil {
		return nil, merr
	}

	stampSize, stampMtime, serr := statStamp(path)
	binfmt.Require(serr == nil, "%v", serr)

	a, oerr := nup.Open(ctx, path)
	if oerr != nil {
		return nil, oerr
	}
	defer a.Close()

	if _, verr := a.Verify(ctx, func(label string, v float64) {
		if progress != nil {
			progress(label, v*0.1)
		}
	}); verr != nil {
		return nil, verr
	}

	var packages []nup.Package
	if selection == -1 {
		selected, serr := SelectPackages(a.Packages(), includeKinds)
		if serr != nil {
			return nil, serr
		}
		hasGame := false
		for _, p := range selected {
			if p.Kind == 0 || p.Kind == 1 {
				hasGame = true
				break
			}
		}
		if hasGame {
			merged, merr := MergeGame(selected, -1)
			if merr != nil {
				return nil, merr
			}
			packages = append(packages, merged)
		}
		for _, p := range selected {
			if p.Kind == 2 {
				packages = append(packages, p)
			}
		}
		for _, p := range selected {
			binfmt.Require(p.Kind != 3 && p.Kind != 4, "Reserved/system packages cannot be converted to NSP.")
		}
	} else {
		binfmt.Require(selection >= 0 && selection < len(a.Packages()), "Select a valid package.")
		p := a.Packages()[selection]
		if p.Kind == 2 {
			packages = []nup.Package{p}
		} else {
			merged, merr := MergeGame(a.Packages(), selection)
			if merr != nil {
				return nil, merr
			}
			packages = []nup.Package{merged}
		}
	}
	binfmt.Require(len(packages) > 0, "No convertible content packages found.")

	var total int64
	for _, p := range packages {
		for _, s := range p.Files {
			total += s.Size()
		}
	}
	free, ferr := diskfree.Available(filepath.Dir(output))
	binfmt.Require(ferr == nil, "%v", ferr)
	binfmt.Require(free > uint64(float64(total)*2.1+64*1024*1024), "NSP conversion needs space for a temporary extracted snapshot and the output.")
	binfmt.Require(len(packages) == 1 || !strings.EqualFold(filepath.Ext(output), ".nsp"),
		"A game plus DLC needs a new output folder, not one .nsp filename.")

	single := strings.EqualFold(filepath.Ext(output), ".nsp")
	if single {
		res, berr := BuildSnapshotNSP(ctx, packages[0], output, verify, progress)
		if berr != nil {
			return nil, berr
		}
		if err := checkUnchanged(path, stampSize, stampMtime); err != nil {
			return nil, err
		}
		if progress != nil {
			progress("NSP ready", 100)
		}
		return res, nil
	}

	stage, terr := os.MkdirTemp(filepath.Dir(output), ".nup-convert-")
	binfmt.Require(terr == nil, "%v", terr)
	committed := false
	defer func() {
		if !committed {
			if resolved, e1 := filepath.EvalSymlinks(stage); e1 == nil {
				parentResolved, e2 := filepath.EvalSymlinks(filepath.Dir(output))
				if e2 == nil && strings.HasPrefix(resolved, parentResolved+string(filepath.Separator)) && !isLinked(stage) {
					_ = os.RemoveAll(stage)
				}
			}
		}
	}()

	var results []map[string]any
	for i, p := range packages {
		binfmt.Check(ctx)
		kindName := "Game"
		if p.Kind == 2 {
			kindName = "DLC"
		}
		name := fmt.Sprintf("%s [%016X] v%d.nsp", kindName, p.TitleID, p.Version)
		subProgress := func(label string, v float64) {
			if progress != nil {
				progress(label, 10+89*(float64(i)+v/100)/float64(len(packages)))
			}
		}
		res, berr := BuildSnapshotNSP(ctx, p, filepath.Join(stage, name), verify, subProgress)
		if berr != nil {
			return nil, berr
		}
		results = append(results, res)
	}
	if err := checkUnchanged(path, stampSize, stampMtime); err != nil {
		return nil, err
	}
	binfmt.Check(ctx)
	if rerr := os.Rename(stage, output); rerr != nil {
		return nil, rerr
	}
	committed = true
	if progress != nil {
		progress("NSP conversion complete", 100)
	}
	var size int64
	for _, r := range results {
		size += r["size"].(int64)
	}
	return map[string]any{"output": output, "size": size, "verified": verify, "packages": len(results)}, nil
}

// InspectNup opens a NUP and returns its inspect result dict, the port of
// nup_tools.inspect.
func InspectNup(ctx context.Context, path string, verify bool, progress binfmt.Progress) (result map[string]any, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	a, oerr := nup.Open(ctx, path)
	if oerr != nil {
		return nil, oerr
	}
	defer a.Close()
	stats, infos, ierr := a.Inspect(ctx)
	if ierr != nil {
		return nil, ierr
	}
	if verify {
		if _, verr := a.Verify(ctx, progress); verr != nil {
			return nil, verr
		}
	}
	result = map[string]any{}
	for k, v := range stats {
		result[k] = v
	}
	pi := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		pi = append(pi, map[string]any{
			"index":          info.Index,
			"kind":           nup.KindName(info.Kind),
			"title_id":       fmt.Sprintf("%016X", info.TitleID),
			"version":        info.Version,
			"flags":          info.Flags,
			"files":          info.Files,
			"metadata_types": info.MetadataTypes,
			"title":          info.Title,
		})
	}
	result["packages_info"] = pi
	result["verified"] = verify
	result["output"] = path
	result["size"] = result["archive_size"]
	return result, nil
}

// --- helpers ---

func findTLV(items []nup.TLV, kind uint16) []byte {
	for _, t := range items {
		if t.Kind == kind {
			return t.Data
		}
	}
	return nil
}

func tlvString(items []nup.TLV, kind uint16, fallback string) string {
	for _, t := range items {
		if t.Kind == kind {
			return strings.ToValidUTF8(string(t.Data), "\uFFFD")
		}
	}
	return fallback
}

func statStamp(p string) (size, mtime int64, err error) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, 0, err
	}
	return st.Size(), st.ModTime().UnixNano(), nil
}

func checkUnchanged(p string, size, mtime int64) error {
	s2, m2, err := statStamp(p)
	if err != nil {
		return err
	}
	if s2 != size || m2 != mtime {
		return errors.New("NUP changed while reading.")
	}
	return nil
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
