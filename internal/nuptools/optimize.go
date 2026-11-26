package nuptools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dannolan/boxcar/internal/nup"
)

// OptimizeOptions controls Optimize.
type OptimizeOptions struct {
	// Storage policy and compression level (RepackOptions.Progress is
	// ignored; use Progress below).
	nup.RepackOptions
	// Recursive also searches subdirectories of directory inputs.
	Recursive bool
	// Jobs is how many archives are optimized at once (minimum 1).
	Jobs int
	// Output is a .nup path when exactly one archive is selected, otherwise
	// a directory that receives the archives under their relative paths.
	// Leave it empty with InPlace or DryRun.
	Output  string
	InPlace bool
	DryRun  bool
	Force   bool
	MinGain float64
	// LatestSnapshot consolidates base and updates into one runnable game
	// (content-altering, so it is opt-in), as pack --latest-snapshot does.
	LatestSnapshot bool
	// Progress receives per-archive and size-weighted overall progress;
	// Finished receives each archive's result as soon as it is known. Both
	// are called from one goroutine at a time.
	Progress func(BatchProgress)
	Finished func(index, count int, result ArchiveResult)
}

// BatchProgress is one progress update of an Optimize run.
type BatchProgress struct {
	Index, Count int // 1-based archive number and number of archives
	Path         string
	Label        string
	Percent      float64 // this archive, 0..100
	Overall      float64 // every archive, weighted by size, 0..100
}

// ArchiveResult is one archive's outcome; Status "failed" carries Error.
type ArchiveResult struct {
	nup.OptimizeResult
	Error string `json:"error,omitempty"`
}

// BatchSummary aggregates an Optimize run. Sizes cover archives that were
// read; SizeAfter counts unchanged archives at their current size and dry
// runs at their estimated size.
type BatchSummary struct {
	Archives     int     `json:"archives"`
	Optimized    int     `json:"optimized"`
	Skipped      int     `json:"skipped"`
	Planned      int     `json:"planned"`
	Failed       int     `json:"failed"`
	SizeBefore   int64   `json:"size_before"`
	SizeAfter    int64   `json:"size_after"`
	SavedBytes   int64   `json:"saved_bytes"`
	SavedPercent float64 `json:"saved_percent"`
}

// BatchResult is the outcome of Optimize.
type BatchResult struct {
	DryRun   bool            `json:"dry_run"`
	InPlace  bool            `json:"in_place"`
	Archives []ArchiveResult `json:"archives"`
	Summary  BatchSummary    `json:"summary"`
}

type optimizeTarget struct {
	path   string
	rel    string
	output string
	size   int64
	err    error
}

// Optimize optimizes every NUP named by inputs (files, or directories
// searched for *.nup), Jobs at a time. A failing archive is reported in its
// result and does not stop the others; the returned error covers only
// unusable arguments.
func Optimize(ctx context.Context, inputs []string, opts OptimizeOptions) (BatchResult, error) {
	result := BatchResult{DryRun: opts.DryRun, InPlace: opts.InPlace && !opts.DryRun}
	targets, err := findArchives(inputs, opts.Recursive)
	if err != nil {
		return result, err
	}
	if err := assignOutputs(targets, opts); err != nil {
		return result, err
	}

	jobs := max(opts.Jobs, 1)
	jobs = min(jobs, len(targets))
	var total int64
	for _, t := range targets {
		total += t.size
	}
	var mu sync.Mutex
	percent := make([]float64, len(targets))
	overall := func() float64 {
		if total == 0 {
			return 0
		}
		var done float64
		for i := range targets {
			done += float64(targets[i].size) * percent[i] / 100
		}
		return 100 * done / float64(total)
	}

	results := make([]ArchiveResult, len(targets))
	run := func(i int) {
		t := targets[i]
		var r ArchiveResult
		switch {
		case t.err != nil:
			r = failed(t.path, t.err)
		case ctx.Err() != nil:
			r = failed(t.path, errors.New("Cancelled before it started."))
		default:
			o := nup.OptimizeOptions{
				RepackOptions: opts.RepackOptions,
				Output:        t.output,
				InPlace:       opts.InPlace,
				DryRun:        opts.DryRun,
				Force:         opts.Force,
				MinGain:       opts.MinGain,
			}
			o.Progress = func(label string, pct float64) {
				mu.Lock()
				defer mu.Unlock()
				percent[i] = pct
				if opts.Progress != nil {
					opts.Progress(BatchProgress{Index: i + 1, Count: len(targets), Path: t.path, Label: label, Percent: pct, Overall: overall()})
				}
			}
			if opts.LatestSnapshot {
				o.Transform = func(_ context.Context, packages []nup.Package) ([]nup.Package, error) {
					return CompactPackages(packages)
				}
			}
			res, oerr := nup.Optimize(ctx, t.path, o)
			r = ArchiveResult{OptimizeResult: res}
			if oerr != nil {
				r = failed(t.path, oerr)
				r.SizeBefore, r.SizeAfter = res.SizeBefore, res.SizeBefore
				r.LogicalSHA256 = res.LogicalSHA256
			}
		}
		mu.Lock()
		defer mu.Unlock()
		percent[i] = 100
		results[i] = r
		if opts.Finished != nil {
			opts.Finished(i+1, len(targets), r)
		}
	}

	queue := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				run(i)
			}
		}()
	}
	for i := range targets {
		queue <- i
	}
	close(queue)
	wg.Wait()

	result.Archives = results
	s := &result.Summary
	s.Archives = len(results)
	for _, r := range results {
		switch r.Status {
		case "optimized":
			s.Optimized++
		case "skipped":
			s.Skipped++
		case "planned":
			s.Planned++
		default:
			s.Failed++
		}
		if r.SizeBefore > 0 {
			s.SizeBefore += r.SizeBefore
			s.SizeAfter += r.SizeAfter
		}
	}
	s.SavedBytes = s.SizeBefore - s.SizeAfter
	if s.SizeBefore > 0 {
		s.SavedPercent = 100 * float64(s.SavedBytes) / float64(s.SizeBefore)
	}
	return result, nil
}

func failed(path string, err error) ArchiveResult {
	r := ArchiveResult{Error: err.Error()}
	r.Input = path
	r.Status = "failed"
	return r
}

// findArchives expands inputs into archive targets. Problems with a single
// entry (missing, linked) become failed targets; finding nothing at all is
// an error.
func findArchives(inputs []string, recursive bool) ([]optimizeTarget, error) {
	if len(inputs) == 0 {
		return nil, errors.New("Choose at least one .nup file or folder.")
	}
	var targets []optimizeTarget
	seen := map[string]bool{}
	add := func(path, rel string, err error) {
		if seen[path] {
			return
		}
		seen[path] = true
		t := optimizeTarget{path: path, rel: rel, err: err}
		if err == nil {
			if st, serr := os.Stat(path); serr == nil {
				t.size = st.Size()
			}
		}
		targets = append(targets, t)
	}
	isNup := func(name string) bool { return strings.EqualFold(filepath.Ext(name), ".nup") }
	linkErr := errors.New("Linked input files are not supported.")
	splitErr := errors.New("This .nup is a folder (a NUP split into parts); join the parts into one file first.")

	for _, input := range inputs {
		abs, err := filepath.Abs(input)
		if err != nil {
			return nil, err
		}
		st, err := os.Lstat(abs)
		switch {
		case err != nil:
			add(abs, filepath.Base(abs), err)
		case st.Mode()&os.ModeSymlink != 0 || isLinked(abs):
			add(abs, filepath.Base(abs), linkErr)
		case !st.IsDir():
			add(abs, filepath.Base(abs), nil)
		case isNup(abs):
			add(abs, filepath.Base(abs), splitErr)
		case recursive:
			werr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
				rel, _ := filepath.Rel(abs, p)
				if err != nil {
					add(p, rel, err)
					if d != nil && d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if d.IsDir() && p != abs && isNup(d.Name()) {
					add(p, rel, splitErr)
					return fs.SkipDir
				}
				if d.IsDir() || !isNup(d.Name()) {
					return nil
				}
				if d.Type()&fs.ModeSymlink != 0 {
					add(p, rel, linkErr)
				} else if d.Type().IsRegular() {
					add(p, rel, nil)
				}
				return nil
			})
			if werr != nil {
				return nil, werr
			}
		default:
			entries, err := os.ReadDir(abs)
			if err != nil {
				add(abs, filepath.Base(abs), err)
				continue
			}
			for _, e := range entries {
				p := filepath.Join(abs, e.Name())
				switch {
				case !isNup(e.Name()):
				case e.IsDir():
					add(p, e.Name(), splitErr)
				case e.Type()&fs.ModeSymlink != 0:
					add(p, e.Name(), linkErr)
				case e.Type().IsRegular():
					add(p, e.Name(), nil)
				}
			}
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("No .nup archives found.")
	}
	return targets, nil
}

// assignOutputs maps each target to its output path.
func assignOutputs(targets []optimizeTarget, opts OptimizeOptions) error {
	if opts.DryRun || opts.InPlace {
		return nil
	}
	if opts.Output == "" {
		return errors.New("Choose --output, --in-place or --dry-run.")
	}
	out, err := filepath.Abs(opts.Output)
	if err != nil {
		return err
	}
	if strings.EqualFold(filepath.Ext(out), ".nup") {
		if len(targets) != 1 {
			return fmt.Errorf("--output names one .nup file but %d archives were selected; choose a folder.", len(targets))
		}
		targets[0].output = out
		return nil
	}
	claimed := map[string]bool{}
	for i := range targets {
		t := &targets[i]
		t.output = filepath.Join(out, t.rel)
		key := strings.ToLower(t.output)
		if claimed[key] && t.err == nil {
			t.err = fmt.Errorf("Another archive is already written to %s.", t.output)
		}
		claimed[key] = true
	}
	return nil
}
