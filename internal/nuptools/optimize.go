package nuptools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	// Profile names the profile RepackOptions came from (reported only).
	Profile string
	// Jobs is how many archives are optimized at once; 0 chooses
	// automatically (see autoJobs).
	Jobs int
	// MemoryBudget sets the process-wide working-memory budget in bytes
	// (nup.SetMemoryBudget); 0 keeps the current one (automatic by default).
	MemoryBudget int64
	// Output is a .nup path when exactly one archive is selected, otherwise
	// a directory that receives the archives under their relative paths.
	// Leave it empty with InPlace or DryRun.
	Output  string
	InPlace bool
	DryRun  bool
	Force   bool
	// MinGain is the minimum estimated saving in percent. Negative means
	// automatic: 1% for archives found in folders, any saving for archives
	// named explicitly. Explicitly named archives are also measured by a
	// real rewrite when the estimate is uncertain (nup.OptimizeOptions.Measure).
	MinGain float64
	// SplitSize is nup.OptimizeOptions.SplitSize: 0 writes parts of
	// nup.FAT32PartSize only on FAT volumes, a positive size writes parts of
	// that size, a negative one never splits.
	SplitSize int64
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
	Normalized   int     `json:"normalized"`
	Skipped      int     `json:"skipped"`
	Planned      int     `json:"planned"`
	Failed       int     `json:"failed"`
	SizeBefore   int64   `json:"size_before"`
	SizeAfter    int64   `json:"size_after"`
	SavedBytes   int64   `json:"saved_bytes"`
	SavedPercent float64 `json:"saved_percent"`
}

// Policy is the storage policy a run used, after profile and overrides.
type Policy struct {
	Profile            string  `json:"profile,omitempty"`
	Level              int     `json:"level"`
	LargeFilesRaw      bool    `json:"large_files_raw"`
	LargeFileThreshold int64   `json:"large_file_threshold"`
	MinSavings         float64 `json:"min_savings"`
	Recompress         bool    `json:"recompress"`
	Jobs               int     `json:"jobs"`
	SplitSize          int64   `json:"split_size"`    // 0: parts only on FAT volumes; -1: never
	MemoryBudget       int64   `json:"memory_budget"` // bytes of working memory shared by all archives
}

// BatchResult is the outcome of Optimize.
type BatchResult struct {
	DryRun   bool            `json:"dry_run"`
	InPlace  bool            `json:"in_place"`
	Policy   Policy          `json:"policy"`
	Archives []ArchiveResult `json:"archives"`
	Summary  BatchSummary    `json:"summary"`
}

type optimizeTarget struct {
	explicit bool // named on the command line rather than found in a folder
	path     string
	rel      string
	output   string
	size     int64
	err      error
}

// Optimize optimizes every NUP named by inputs (files, or directories
// searched for *.nup), Jobs at a time. A failing archive is reported in its
// result and does not stop the others; the returned error covers only
// unusable arguments.
func Optimize(ctx context.Context, inputs []string, opts OptimizeOptions) (BatchResult, error) {
	result := BatchResult{DryRun: opts.DryRun, InPlace: opts.InPlace && !opts.DryRun}
	var outputDir string
	if opts.Output != "" && !opts.InPlace && !strings.EqualFold(filepath.Ext(opts.Output), ".nup") {
		if abs, err := filepath.Abs(opts.Output); err == nil {
			outputDir = abs
		}
	}
	targets, err := findArchives(inputs, opts.Recursive, outputDir)
	if err != nil {
		return result, err
	}
	if err := assignOutputs(targets, opts); err != nil {
		return result, err
	}

	if opts.MemoryBudget > 0 {
		nup.SetMemoryBudget(opts.MemoryBudget)
	}
	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = autoJobs(targets, opts)
	}
	jobs = max(min(jobs, len(targets)), 1)
	threshold := opts.LargeFileThreshold
	if threshold == 0 {
		threshold = nup.LargeFile
	}
	level := opts.Level
	if level == 0 {
		level = 3
	}
	result.Policy = Policy{Profile: opts.Profile, Level: level, LargeFilesRaw: opts.LargeRaw, LargeFileThreshold: threshold,
		MinSavings: opts.MinSavings, Recompress: opts.Recompress, Jobs: jobs, SplitSize: opts.SplitSize,
		MemoryBudget: nup.MemoryBudget()}
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
				Measure:       t.explicit,
				SplitSize:     opts.SplitSize,
			}
			if o.MinGain < 0 {
				o.MinGain = 1
				if t.explicit {
					o.MinGain = 0
				}
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
		case "normalized":
			s.Normalized++
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

// findArchives expands inputs into archive targets, skipping the output
// folder when it lies inside a searched folder. Problems with a single entry
// (missing, linked) become failed targets; finding nothing at all is an
// error.
func findArchives(inputs []string, recursive bool, outputDir string) ([]optimizeTarget, error) {
	if len(inputs) == 0 {
		return nil, errors.New("Choose at least one .nup file or folder.")
	}
	var targets []optimizeTarget
	seen := map[string]int{}
	add := func(path, rel string, err error) int {
		if i, ok := seen[path]; ok {
			return i
		}
		seen[path] = len(targets)
		t := optimizeTarget{path: path, rel: rel, err: err}
		targets = append(targets, t)
		return len(targets) - 1
	}
	// archive adds the archive a path stands for: a whole file, the first
	// part of an archive in parts, or a folder of one archive's parts (see
	// nup.FindParts). Its output is named after the whole file.
	archive := func(path, rel string) int {
		parts, name, err := nup.FindParts(path)
		if err != nil {
			return add(path, rel, err)
		}
		i := add(path, filepath.Join(filepath.Dir(rel), name), nil)
		var size int64
		for _, p := range parts {
			if st, serr := os.Stat(p); serr == nil {
				size += st.Size()
			}
		}
		targets[i].size = size
		return i
	}
	isNup := func(name string) bool { return strings.EqualFold(filepath.Ext(name), ".nup") }
	linkErr := errors.New("Linked input files are not supported.")
	// gameFolder: a folder that is one archive's parts: named *.nup (the
	// console's own way), or holding nameless parts 00, 01, ...
	gameFolder := func(path string) bool {
		if isNup(path) {
			return true
		}
		parts, _, err := nup.FindParts(path)
		return err == nil && len(parts) > 0 && strings.Trim(filepath.Base(parts[0]), "0123456789") == ""
	}
	// fileEntry: a file found in a folder that is an archive's whole file or
	// first part; later parts, and names that only look like parts, are not.
	fileEntry := func(path string) bool {
		parts, _, err := nup.FindParts(path)
		if errors.Is(err, nup.ErrLaterPart) {
			return false
		}
		return isNup(path) || err == nil && len(parts) > 1
	}

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
			targets[archive(abs, filepath.Base(abs))].explicit = true
		case gameFolder(abs):
			targets[archive(abs, filepath.Base(abs))].explicit = true
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
				if d.IsDir() && p != abs && outputDir != "" && sameDir(p, outputDir) {
					return fs.SkipDir
				}
				if d.IsDir() && p != abs && gameFolder(p) {
					archive(p, rel)
					return fs.SkipDir
				}
				if d.IsDir() || !fileEntry(p) {
					return nil
				}
				if d.Type()&fs.ModeSymlink != 0 {
					add(p, rel, linkErr)
				} else if d.Type().IsRegular() {
					archive(p, rel)
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
				case e.IsDir():
					if isNup(e.Name()) {
						archive(p, e.Name())
					}
				case !fileEntry(p):
				case e.Type()&fs.ModeSymlink != 0:
					add(p, e.Name(), linkErr)
				case e.Type().IsRegular():
					archive(p, e.Name())
				}
			}
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("No .nup archives found.")
	}
	return targets, nil
}

func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// autoJobs picks how many archives run at once when --jobs is 0: up to
// four, and at most half the CPUs. Each archive already compresses and
// verifies its files on every CPU under process-wide CPU and memory
// budgets, but its planning, writer and commit run on one goroutine, so
// small archives leave most CPUs idle. Measured (8 CPUs): four 125 MB
// archives 2.3 s -> 1.4 s (2 jobs) -> 1.0 s (4 jobs) with no more peak
// memory; two real archives (2.9 + 6.7 GB) 14.8 s -> 13.3 s (2 jobs) for
// ~23% more peak memory. See docs/OPTIMIZE.md "Concurrency".
func autoJobs(targets []optimizeTarget, _ OptimizeOptions) int {
	valid := 0
	for _, t := range targets {
		if t.err == nil {
			valid++
		}
	}
	return max(1, min(valid, 4, runtime.NumCPU()/2))
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
		if t.err != nil {
			continue
		}
		t.output = filepath.Join(out, t.rel)
		key := strings.ToLower(t.output)
		if claimed[key] {
			t.err = fmt.Errorf("Another archive is already written to %s.", t.output)
		}
		claimed[key] = true
	}
	return nil
}
