// Command boxcar is the single-binary CLI for NSP-Box, porting the argument
// surface of app.py's CLI and nup_cli.py. It uses only the standard library.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/dannolan/boxcar/internal/artwork"
	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/dlc"
	"github.com/dannolan/boxcar/internal/keys"
	"github.com/dannolan/boxcar/internal/nsp"
	"github.com/dannolan/boxcar/internal/nup"
	"github.com/dannolan/boxcar/internal/nuptools"
	"github.com/dannolan/boxcar/internal/patch"
	"github.com/dannolan/boxcar/internal/snapshot"
)

const version = "4.0.0-boxcar.1"

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is the testable entry point; it returns the process exit code.
func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}
	switch args[0] {
	case "-version", "--version", "version":
		fmt.Println(version)
		return 0
	case "build":
		return runBuild(args[1:])
	case "dlc":
		return runDLC(args[1:])
	case "nup":
		return runNup(args[1:])
	case "patch":
		return runPatch(args[1:])
	case "keys":
		return runKeys(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "boxcar: unknown command %q\n", args[0])
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: boxcar <build|dlc|nup|patch|keys> [flags]")
	fmt.Fprintln(os.Stderr, "       boxcar -version")
}

func progress() binfmt.Progress {
	return func(label string, pct float64) {
		fmt.Fprintf(os.Stderr, "%s: %.0f%%\n", label, pct)
	}
}

// normalizeCrop rewrites "--artwork-crop L T R B" into a single comma-joined
// value so the stdlib flag package can consume it.
func normalizeCrop(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--artwork-crop" && i+4 < len(args) {
			out = append(out, "--artwork-crop="+args[i+1]+","+args[i+2]+","+args[i+3]+","+args[i+4])
			i += 4
		} else {
			out = append(out, args[i])
		}
	}
	return out
}

func runBuild(args []string) int {
	args = normalizeCrop(args)
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pkg := fs.String("package", "", "")
	output := fs.String("output", "", "")
	artworkPath := fs.String("artwork", "", "")
	artworkFit := fs.Bool("artwork-fit", false, "")
	artworkCrop := fs.String("artwork-crop", "", "")
	title := fs.String("title", "", "")
	snapshotIdx := fs.Int("snapshot", 0, "")
	displayVersion := fs.String("display-version", "", "")
	packageFolder := fs.Bool("package-folder", true, "")
	noPackageFolder := fs.Bool("no-package-folder", false, "")
	skipVerification := fs.Bool("skip-verification", false, "")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *pkg == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "boxcar build: --package and --output are required")
		return 1
	}
	if (*artworkFit || *artworkCrop != "") && *artworkPath == "" {
		fmt.Fprintln(os.Stderr, "Choose --artwork before specifying its crop mode.")
		return 1
	}
	if *artworkFit && *artworkCrop != "" {
		fmt.Fprintln(os.Stderr, "--artwork-fit and --artwork-crop are mutually exclusive")
		return 1
	}

	ctx := context.Background()
	snapshots, _, err := snapshot.Find(ctx, *pkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *snapshotIdx < 0 || *snapshotIdx >= len(snapshots) {
		fmt.Fprintln(os.Stderr, "Snapshot index is out of range.")
		return 1
	}
	s := snapshots[*snapshotIdx]

	var sel *artwork.Selection
	if *artworkPath != "" {
		sel = &artwork.Selection{Path: *artworkPath, Fit: *artworkFit}
		if *artworkCrop != "" {
			parts := strings.Split(*artworkCrop, ",")
			if len(parts) != 4 {
				fmt.Fprintln(os.Stderr, "--artwork-crop needs LEFT TOP RIGHT BOTTOM")
				return 1
			}
			var box [4]float64
			for i, p := range parts {
				v, err := strconv.ParseFloat(p, 64)
				if err != nil {
					fmt.Fprintf(os.Stderr, "invalid crop coordinate %q\n", p)
					return 1
				}
				box[i] = v
			}
			sel.Box = &box
		}
	}

	t := *title
	if t == "" {
		t = s.Title
	}
	dv := *displayVersion
	if dv == "" {
		if s.Version != 0 {
			dv = fmt.Sprintf("dump-%d", s.Version)
		} else {
			dv = "Snapshot"
		}
	}
	report, err := nsp.Build(ctx, s, nsp.Options{
		Output:         *output,
		Title:          t,
		DisplayVersion: dv,
		Artwork:        sel,
		Verify:         !*skipVerification,
		PackageFolder:  *packageFolder && !*noPackageFolder,
		Progress:       progress(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	printJSON(report)
	return 0
}

func runDLC(args []string) int {
	fs := flag.NewFlagSet("dlc", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pkg := fs.String("package", "", "")
	output := fs.String("output", "", "")
	baseGame := fs.String("base-game", "", "")
	dlcTitleID := fs.String("dlc-title-id", "", "")
	dlcVersion := fs.String("dlc-version", "", "")
	snapshotIdx := fs.Int("snapshot", 0, "")
	skipVerification := fs.Bool("skip-verification", false, "")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *pkg == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "boxcar dlc: --package and --output are required")
		return 1
	}

	ctx := context.Background()
	snapshots, err := dlc.Find(ctx, *pkg, *dlcTitleID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *snapshotIdx < 0 || *snapshotIdx >= len(snapshots) {
		fmt.Fprintln(os.Stderr, "DLC snapshot index is out of range.")
		return 1
	}
	s := snapshots[*snapshotIdx]
	if *dlcVersion != "" {
		v, err := dlc.ParseVersion(*dlcVersion)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		s.Version = v
	}
	report, err := dlc.Build(ctx, s, *output, dlc.Options{
		VerifyOutput: !*skipVerification,
		BaseGame:     *baseGame,
		Progress:     progress(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	printJSON(report)
	return 0
}

func runNup(args []string) int {
	fs := flag.NewFlagSet("nup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	op := fs.String("nup-operation", "", "")
	input := fs.String("input", "", "")
	output := fs.String("output", "", "")
	preset := fs.String("preset", "Balanced", "")
	largeRaw := fs.Bool("large-files-raw", true, "")
	noLargeRaw := fs.Bool("no-large-files-raw", false, "")
	latestSnapshot := fs.Bool("latest-snapshot", true, "")
	noLatestSnapshot := fs.Bool("no-latest-snapshot", false, "")
	verify := fs.Bool("verify", true, "")
	noVerify := fs.Bool("no-verify", false, "")
	packageIndex := fs.Int("package-index", -1, "")
	resultJSON := fs.String("result-json", "", "")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	level, ok := nup.Presets[*preset]
	if !ok {
		fmt.Fprintf(os.Stderr, "boxcar nup: unknown preset %q\n", *preset)
		return 1
	}
	largeRawVal := *largeRaw && !*noLargeRaw
	latestVal := *latestSnapshot && !*noLatestSnapshot
	verifyVal := *verify && !*noVerify

	ctx := context.Background()
	opts := nup.PackOptions{Level: level, LargeRaw: largeRawVal, Verify: verifyVal, Progress: progress()}

	var result any
	code := 0
	switch *op {
	case "pack", "nsp-to-nup", "extract", "nup-to-nsp":
		if *output == "" {
			result = map[string]any{"error": "Choose --output."}
			code = 1
			break
		}
		switch *op {
		case "pack":
			r, err := nuptools.PackFolder(ctx, *input, *output, latestVal, nil, nil, "", nil, opts)
			if err != nil {
				result = map[string]any{"error": err.Error()}
				code = 1
			} else {
				result = packResultMap(r)
			}
		case "nsp-to-nup":
			r, err := nuptools.NspToNup(ctx, *input, *output, nil, opts)
			if err != nil {
				result = map[string]any{"error": err.Error()}
				code = 1
			} else {
				result = packResultMap(r)
			}
		case "extract":
			var selection []int
			if *packageIndex != -1 {
				selection = []int{*packageIndex}
			}
			r, err := nup.Extract(ctx, *input, *output, selection, progress())
			if err != nil {
				result = map[string]any{"error": err.Error()}
				code = 1
			} else {
				result = extractResultMap(r)
			}
		case "nup-to-nsp":
			r, err := nuptools.NupToNsp(ctx, *input, *output, *packageIndex, verifyVal, nil, progress())
			if err != nil {
				result = map[string]any{"error": err.Error()}
				code = 1
			} else {
				result = r
			}
		}
	case "inspect", "verify":
		r, err := nuptools.InspectNup(ctx, *input, *op == "verify", progress())
		if err != nil {
			result = map[string]any{"error": err.Error()}
			code = 1
		} else {
			result = r
		}
	default:
		fmt.Fprintf(os.Stderr, "boxcar nup: unknown operation %q\n", *op)
		return 1
	}

	if *resultJSON != "" {
		f, err := os.OpenFile(*resultJSON, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			f.Close()
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		f.Close()
	}
	printJSON(result)
	return code
}

func runPatch(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: boxcar patch <inspect|apply|remove> FOLDER")
		return 1
	}
	action, folder := args[0], args[1]
	ctx := context.Background()
	var view patch.View
	var err error
	switch action {
	case "inspect":
		view, err = patch.Inspect(ctx, folder)
	case "apply":
		view, err = patch.Apply(ctx, folder)
	case "remove":
		view, err = patch.Remove(ctx, folder)
	default:
		fmt.Fprintf(os.Stderr, "boxcar patch: unknown action %q\n", action)
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	printJSON(view)
	return 0
}

func runKeys(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: boxcar keys <inspect|install> DIR")
		return 1
	}
	action, dir := args[0], args[1]
	switch action {
	case "inspect":
		status, err := keys.Inspect(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printJSON(map[string]any{
			"ready":   status.Ready,
			"missing": status.Missing,
			"invalid": status.Invalid,
		})
	case "install":
		res, err := keys.Install(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printJSON(map[string]any{
			"ready":   res.Ready,
			"missing": res.Missing,
			"invalid": res.Invalid,
			"added":   res.Added,
			"backup":  res.Backup,
		})
	default:
		fmt.Fprintf(os.Stderr, "boxcar keys: unknown action %q\n", action)
		return 1
	}
	return 0
}

func packResultMap(r nup.Result) map[string]any {
	return map[string]any{
		"output":                  r.Output,
		"size":                    r.Size,
		"source_size":             r.SourceSize,
		"saved_bytes":             r.SavedBytes,
		"verified":                r.Verified,
		"packages":                r.Packages,
		"duplicate_files_skipped": r.DuplicateFilesSkipped,
	}
}

func extractResultMap(r nup.Result) map[string]any {
	return map[string]any{
		"output":   r.Output,
		"size":     r.Size,
		"verified": r.Verified,
		"packages": r.Packages,
	}
}

func printJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println(string(data))
}
