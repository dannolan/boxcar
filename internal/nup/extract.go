package nup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/diskfree"
)

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

// Extract extracts selected packages from an archive into output (port of
// nup_format.extract).
func Extract(ctx context.Context, path, output string, selection []int, progress binfmt.Progress) (res Result, err error) {
	defer binfmt.Recover(&err)

	abs, err := filepath.Abs(output)
	if err != nil {
		return res, err
	}
	output = abs
	binfmt.Require(!pathExists(output), "Extract into a new folder; existing files are never overwritten.")
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return res, err
	}
	stage, err := os.MkdirTemp(parent, ".nup-extract-")
	if err != nil {
		return res, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if _, serr := os.Lstat(stage); serr != nil {
			return // already gone
		}
		resolved, err1 := filepath.EvalSymlinks(stage)
		parentResolved, err2 := filepath.EvalSymlinks(parent)
		if err1 != nil || err2 != nil || !strings.HasPrefix(resolved, parentResolved+string(filepath.Separator)) || isLinked(stage) {
			binfmt.Fail("Temporary extraction folder changed unexpectedly.")
		}
		_ = os.RemoveAll(stage)
	}()

	a, err := Open(ctx, path)
	if err != nil {
		return res, err
	}
	defer a.Close()

	var selected []int
	if selection == nil {
		selected = make([]int, len(a.packages))
		for i := range selected {
			selected[i] = i
		}
	} else {
		selected = append([]int(nil), selection...)
	}
	seenSel := map[int]bool{}
	for _, i := range selected {
		binfmt.Require(i >= 0 && i < len(a.packages) && !seenSel[i], "Choose a valid package selection.")
		seenSel[i] = true
	}
	binfmt.Require(len(selected) > 0, "Choose a valid package selection.")

	var total int64
	for _, k := range selected {
		for _, index := range a.trees[k] {
			total += a.files[index].size
		}
	}

	free, err := diskfree.Available(stage)
	if err != nil {
		return res, err
	}
	binfmt.Require(free > uint64(total)+32*1024*1024, "Not enough extraction disk space.")

	var done int64
	manifest := []manifestPkg{}
	destinations := map[string]bool{}
	windows := runtime.GOOS == "windows"

	for _, k := range selected {
		p := a.packages[k]
		relative := PackagePath(p)
		binfmt.Require(!destinations[relative], "Duplicate extraction package path.")
		destinations[relative] = true
		base := filepath.Join(stage, filepath.FromSlash(relative))
		if err := os.MkdirAll(base, 0o755); err != nil {
			return res, err
		}

		all := map[string]bool{}
		for _, d := range p.Directories {
			all[d] = true
		}
		for f := range p.Files {
			all[f] = true
		}
		items := make([]string, 0, len(all))
		for it := range all {
			items = append(items, it)
		}
		sort.Strings(items)

		collision := map[string]bool{}
		for _, item := range items {
			parts := pathParts(item)
			for _, part := range parts {
				safeName(part, windows)
			}
			key := item
			if windows {
				key = casefold(item)
			}
			binfmt.Require(!collision[key], "Case-colliding names cannot be extracted on this filesystem.")
			collision[key] = true
		}

		dirs := append([]string(nil), p.Directories...)
		sort.Strings(dirs)
		for _, directory := range dirs {
			binfmt.Check(ctx)
			if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(directory)), 0o755); err != nil {
				return res, err
			}
		}

		for item, index := range a.trees[k] {
			target := filepath.Join(base, filepath.FromSlash(item))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return res, err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
			if err != nil {
				return res, err
			}
			werr := a.streamFile(ctx, index, f, func(n int) {
				done += int64(n)
				if progress != nil {
					progress("Extracting verified files", 99*float64(done)/float64(max64(total, 1)))
				}
			})
			f.Close()
			if werr != nil {
				return res, werr
			}
		}

		meta := make([]any, 0, len(p.Metadata))
		for _, t := range p.Metadata {
			meta = append(meta, []any{int(t.Kind), base64.StdEncoding.EncodeToString(t.Data)})
		}
		manifest = append(manifest, manifestPkg{
			Path:     relative,
			TitleID:  fmt.Sprintf("%016X", p.TitleID),
			Version:  p.Version,
			Kind:     p.Kind,
			Flags:    p.Flags,
			Metadata: meta,
		})
	}

	if err := a.unchanged(); err != nil {
		return res, err
	}

	doc := manifestDoc{Format: "NSP Box NUP extraction v1", Packages: manifest}
	manifestBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return res, err
	}
	if err := os.WriteFile(filepath.Join(stage, "nup-manifest.json"), manifestBytes, 0o644); err != nil {
		return res, err
	}

	binfmt.Check(ctx)
	if err := os.Rename(stage, output); err != nil {
		return res, err
	}
	committed = true
	if progress != nil {
		progress("Extraction complete", 100)
	}
	return Result{Output: output, Size: total, Verified: true, Packages: len(selected)}, nil
}

// casefold is a UTF-8 case-folding approximation of Python str.casefold
// (strings.ToLower covers the ASCII cases exercised here; Python additionally
// folds e.g. 'ß'→'ss').
func casefold(s string) string {
	return strings.ToLower(s)
}
