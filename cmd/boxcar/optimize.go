package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dannolan/boxcar/internal/nup"
	"github.com/dannolan/boxcar/internal/nuptools"
)

type pathList []string

func (p *pathList) String() string     { return strings.Join(*p, ", ") }
func (p *pathList) Set(v string) error { *p = append(*p, v); return nil }

// nupOperation returns the --nup-operation value in args, if any.
func nupOperation(args []string) string {
	for i, a := range args {
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "nup-operation" {
			continue
		}
		if hasValue {
			return value
		}
		if i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// runOptimize implements both "boxcar optimize PATH..." and
// "boxcar nup --nup-operation optimize --input PATH".
func runOptimize(args []string) int {
	fs := flag.NewFlagSet("optimize", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: boxcar optimize [flags] PATH...   (or: boxcar nup --nup-operation optimize --input PATH ...)")
		fmt.Fprintln(os.Stderr, "Rewrites NUP archives without extracting them; see docs/OPTIMIZE.md.")
		fs.PrintDefaults()
	}
	fs.String("nup-operation", "optimize", "accepted for 'boxcar nup' compatibility")
	var inputs pathList
	fs.Var(&inputs, "input", "a .nup file or a folder of them (repeatable; bare paths work too)")
	output := fs.String("output", "", "new .nup file (one archive) or folder that receives the optimized archives")
	inPlace := fs.Bool("in-place", false, "replace each archive after its rewrite is verified")
	dryRun := fs.Bool("dry-run", false, "only report what would change; write nothing")
	recursive := fs.Bool("recursive", false, "also search subfolders")
	jobs := fs.Int("jobs", 1, "archives optimized at the same time")
	preset := fs.String("preset", "Balanced", "zstd preset for files that get compressed: Fast, Balanced or Smaller")
	recompress := fs.Bool("recompress", false, "re-encode already-compressed files with --preset (keeps the smaller result)")
	largeRaw := fs.Bool("large-files-raw", true, "keep files above --large-file-threshold uncompressed")
	noLargeRaw := fs.Bool("no-large-files-raw", false, "compress large files too")
	largeThreshold := fs.String("large-file-threshold", "256MiB", "size above which files stay raw (e.g. 64MiB)")
	minSavings := fs.Float64("min-savings", 0, "store a file raw unless zstd saves at least this percent of it")
	minGain := fs.Float64("min-gain", 1, "skip archives whose estimated savings are below this percent")
	force := fs.Bool("force", false, "rewrite archives even when they look already optimized")
	latest := fs.Bool("latest-snapshot", false, "merge base and updates into one runnable game (changes content; opt-in)")
	fs.Bool("no-latest-snapshot", false, "keep every package (default)")
	resultJSON := fs.String("result-json", "", "also write the JSON result to this new file")

	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() == 0 {
			break
		}
		inputs = append(inputs, fs.Arg(0))
		rest = fs.Args()[1:]
	}

	level, ok := nup.Presets[*preset]
	if !ok {
		fmt.Fprintf(os.Stderr, "boxcar optimize: unknown preset %q (Fast, Balanced or Smaller)\n", *preset)
		return 1
	}
	threshold, err := parseSize(*largeThreshold)
	if err != nil || threshold < 1 {
		fmt.Fprintf(os.Stderr, "boxcar optimize: invalid --large-file-threshold %q\n", *largeThreshold)
		return 1
	}
	if *inPlace && *output != "" {
		fmt.Fprintln(os.Stderr, "boxcar optimize: --in-place and --output are mutually exclusive")
		return 1
	}
	if *jobs < 1 {
		fmt.Fprintln(os.Stderr, "boxcar optimize: --jobs must be at least 1")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // a second interrupt ends the process immediately
	}()

	printer := newProgressPrinter()
	opts := nuptools.OptimizeOptions{
		RepackOptions: nup.RepackOptions{
			Level:              level,
			LargeRaw:           *largeRaw && !*noLargeRaw,
			LargeFileThreshold: threshold,
			MinSavings:         *minSavings,
			Recompress:         *recompress,
		},
		Recursive:      *recursive,
		Jobs:           *jobs,
		Output:         *output,
		InPlace:        *inPlace,
		DryRun:         *dryRun,
		Force:          *force,
		MinGain:        *minGain,
		LatestSnapshot: *latest,
		Progress:       printer.progress,
		Finished:       printer.finished,
	}
	res, err := nuptools.Optimize(ctx, inputs, opts)
	var result any = res
	code := 0
	if err != nil {
		result = map[string]any{"error": err.Error()}
		code = 1
	} else {
		printer.summary(res)
		if res.Summary.Failed > 0 {
			code = 1
		}
	}
	if *resultJSON != "" {
		f, ferr := os.OpenFile(*resultJSON, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, ferr)
			return 1
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		werr := enc.Encode(result)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			fmt.Fprintln(os.Stderr, werr)
			return 1
		}
	}
	printJSON(result)
	return code
}

// progressPrinter writes progress to stderr: one rewritten line on a
// terminal, otherwise a line per 10% step.
type progressPrinter struct {
	tty     bool
	last    map[int]string
	pending bool
}

func newProgressPrinter() *progressPrinter {
	st, err := os.Stderr.Stat()
	return &progressPrinter{tty: err == nil && st.Mode()&os.ModeCharDevice != 0, last: map[int]string{}}
}

func (p *progressPrinter) progress(ev nuptools.BatchProgress) {
	step := int(ev.Percent)
	if !p.tty {
		step = step / 10 * 10
	}
	key := fmt.Sprintf("%s %d", ev.Label, step)
	if p.last[ev.Index] == key {
		return
	}
	p.last[ev.Index] = key
	line := fmt.Sprintf("[%d/%d] %s: %s %d%% (overall %d%%)", ev.Index, ev.Count, filepath.Base(ev.Path), ev.Label, step, int(ev.Overall))
	if p.tty {
		fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
		p.pending = true
	} else {
		fmt.Fprintln(os.Stderr, line)
	}
}

func (p *progressPrinter) println(line string) {
	if p.pending {
		fmt.Fprint(os.Stderr, "\r\033[K")
		p.pending = false
	}
	fmt.Fprintln(os.Stderr, line)
}

func (p *progressPrinter) finished(index, count int, r nuptools.ArchiveResult) {
	head := fmt.Sprintf("[%d/%d] %s:", index, count, filepath.Base(r.Input))
	change := func(before, after int64) string {
		saved := before - after
		return fmt.Sprintf("%s -> %s (%s %s, %.1f%%)", humanBytes(before), humanBytes(after),
			map[bool]string{true: "saved", false: "grew by"}[saved >= 0], humanBytes(abs64(saved)),
			100*float64(abs64(saved))/float64(max(before, 1)))
	}
	switch r.Status {
	case "optimized":
		where := "replaced in place"
		if !r.InPlace {
			where = "written to " + r.Output
		}
		p.println(fmt.Sprintf("%s optimized %s; verified, %s", head, change(r.SizeBefore, r.SizeAfter), where))
	case "planned":
		verdict := "would optimize"
		if r.Action == "skip" {
			verdict = "would skip"
		}
		p.println(fmt.Sprintf("%s %s: %s\n    %s; estimated %s", head, verdict, r.Reason, planSummary(r.Plan), change(r.SizeBefore, r.SizeAfter)))
	case "skipped":
		p.println(fmt.Sprintf("%s skipped: %s", head, r.Reason))
	default:
		p.println(fmt.Sprintf("%s FAILED: %s", head, r.Error))
	}
}

func planSummary(pl nup.Plan) string {
	parts := []string{fmt.Sprintf("%d files, %d distinct", pl.Files, pl.UniqueContents)}
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(pl.DuplicateFiles, "duplicates")
	add(pl.Compress, "to compress")
	add(pl.ReuseFrames, "frames reused")
	add(pl.Reencode, "to re-encode")
	add(pl.Decode, "to store raw (policy)")
	add(pl.KeepRaw, "large files kept raw")
	return strings.Join(parts, ", ")
}

func (p *progressPrinter) summary(res nuptools.BatchResult) {
	s := res.Summary
	verb := "saved"
	if res.DryRun {
		verb = "would save"
	}
	p.println(fmt.Sprintf("%d archive(s): %d optimized, %d skipped, %d planned, %d failed; %s %s of %s (%.1f%%)",
		s.Archives, s.Optimized, s.Skipped, s.Planned, s.Failed, verb, humanBytes(s.SavedBytes), humanBytes(s.SizeBefore), s.SavedPercent))
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
