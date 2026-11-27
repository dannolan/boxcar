//go:build linux || darwin

// Command bench measures Boxcar's NUP optimizer on deterministic synthetic
// fixtures (internal/nup/nuptest.Fixtures) and, optionally, on real archives
// given with -real (never committed). It compares any number of boxcar
// binaries:
//
//   - binaries with "optimize --profile" run each profile (runtime, archive,
//     fast), then optimize their own output again (idempotence);
//   - binaries with "optimize" but no profiles run their default settings;
//   - older binaries without "optimize" run the only upstream route, extract
//     and pack again (temporary storage includes the extracted tree).
//
// Every run records wall time, CPU time (user+system), peak RSS, bytes read
// and written (Linux /proc io accounting, page cache included), sampled peak
// disk use of the work folder, output size and ratio, and the time the
// binary's own verify takes on the output (logical MB/s).
//
//	go run ./scripts/bench -work /tmp/bench -bin new=./boxcar \
//	    -bin pr=/path/boxcar-bc81fee -bin upstream=/path/boxcar-9702ddc
//
// Results are printed as a Markdown table and written to -json.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dannolan/boxcar/internal/nup/nuptest"
)

type binFlag []string

func (b *binFlag) String() string     { return strings.Join(*b, ",") }
func (b *binFlag) Set(v string) error { *b = append(*b, v); return nil }

// Run is one measured command.
type Run struct {
	Scenario   string  `json:"scenario"`
	Binary     string  `json:"binary"`
	Variant    string  `json:"variant"`
	Status     string  `json:"status"`
	InputSize  int64   `json:"input_bytes"`
	OutputSize int64   `json:"output_bytes"`
	Ratio      float64 `json:"ratio"`
	WallSec    float64 `json:"wall_s"`
	CPUSec     float64 `json:"cpu_s"`
	PeakRSSMB  float64 `json:"peak_rss_mib"`
	ReadMB     float64 `json:"read_mib"`
	WrittenMB  float64 `json:"written_mib"`
	PeakTempMB float64 `json:"peak_disk_mib"`
	VerifySec  float64 `json:"verify_s"`
	VerifyMBs  float64 `json:"verify_logical_mib_s"`
	Error      string  `json:"error,omitempty"`
}

type measured struct {
	wall, cpu         float64
	rssKB             int64
	read, written     int64
	peakDisk          int64
	stdout, stderrOut []byte
	err               error
}

func main() {
	if len(os.Args) > 3 && os.Args[1] == "-child" {
		child(os.Args[2], os.Args[3:])
		return
	}
	work := flag.String("work", "", "work folder (fixtures and outputs; needs several GB)")
	scale := flag.Float64("scale", 1, "fixture scale (1 = about 2.6 GB of fixtures)")
	only := flag.String("only", "", "comma-separated scenarios to run (default all)")
	profiles := flag.String("profiles", "runtime,archive,fast", "profiles to run on binaries that have them")
	jsonOut := flag.String("json", "", "write all runs to this JSON file")
	warm := flag.Bool("warm", true, "read the input once before each run so every binary starts with the same page cache")
	var bins, real binFlag
	flag.Var(&bins, "bin", "label=path of a boxcar binary (repeatable)")
	flag.Var(&real, "real", "a real .nup to include as its own scenario (repeatable)")
	flag.Parse()
	if *work == "" || len(bins) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*work, 0o755); err != nil {
		fatal(err)
	}
	want := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		if s != "" {
			want[s] = true
		}
	}

	type scenario struct {
		name   string
		inputs []string // archives, or one folder for multi
		folder bool
	}
	var scenarios []scenario
	fixDir := filepath.Join(*work, "fixtures-"+strconv.FormatFloat(*scale, 'f', -1, 64))
	for _, fx := range nuptest.Fixtures(*scale) {
		if len(want) > 0 && !want[fx.Name] {
			continue
		}
		dir := filepath.Join(fixDir, fx.Name)
		if _, err := os.Stat(filepath.Join(dir, ".done")); err != nil {
			fmt.Fprintf(os.Stderr, "generating %s...\n", fx.Name)
			_ = os.RemoveAll(dir)
			must(os.MkdirAll(dir, 0o755))
			for _, a := range fx.Archives {
				must(nuptest.WriteKaiDump(filepath.Join(dir, a.Name), a.Build(), nuptest.Options{}))
			}
			must(os.WriteFile(filepath.Join(dir, ".done"), nil, 0o644))
		}
		if len(fx.Archives) > 1 {
			scenarios = append(scenarios, scenario{fx.Name, []string{dir}, true})
		} else {
			scenarios = append(scenarios, scenario{fx.Name, []string{filepath.Join(dir, fx.Archives[0].Name)}, false})
		}
	}
	for _, r := range real {
		abs, err := filepath.Abs(r)
		must(err)
		scenarios = append(scenarios, scenario{"real:" + filepath.Base(r), []string{abs}, false})
	}

	var runs []Run
	for _, sc := range scenarios {
		in := inputSize(sc.inputs[0])
		for _, b := range bins {
			label, path, ok := strings.Cut(b, "=")
			if !ok {
				fatal(fmt.Errorf("-bin %q: want label=path", b))
			}
			path, _ = filepath.Abs(path)
			kind := capability(path)
			out := filepath.Join(*work, "out")
			var variants []string
			switch kind {
			case "profiles":
				for _, p := range strings.Split(*profiles, ",") {
					variants = append(variants, "profile="+p)
				}
				if sc.folder {
					variants = append(variants, "profile=runtime,jobs=2")
				}
			case "optimize":
				variants = []string{"default"}
			default:
				if sc.folder {
					continue // extract+pack of a folder is not one command
				}
				variants = []string{"extract+pack"}
			}
			for _, v := range variants {
				_ = os.RemoveAll(out)
				must(os.MkdirAll(out, 0o755))
				r := Run{Scenario: sc.name, Binary: label, Variant: v, InputSize: in}
				fmt.Fprintf(os.Stderr, "%s / %s / %s\n", sc.name, label, v)
				if *warm {
					warmUp(sc.inputs[0])
				}
				var m measured
				target := filepath.Join(out, "o.nup")
				if sc.folder {
					target = filepath.Join(out, "folder")
				}
				if v == "extract+pack" {
					tree := filepath.Join(out, "tree")
					m = measure(out, path, "nup", "--nup-operation", "extract", "--input", sc.inputs[0], "--output", tree)
					if m.err == nil {
						m2 := measure(out, path, "nup", "--nup-operation", "pack", "--input", tree, "--output", target, "--no-latest-snapshot")
						m = combine(m, m2)
					}
					r.Status = "packed"
				} else {
					args := []string{"optimize", "--output", target, "--result-json", filepath.Join(*work, "result.json")}
					_ = os.Remove(filepath.Join(*work, "result.json"))
					for _, kv := range strings.Split(v, ",") {
						if k, val, ok := strings.Cut(kv, "="); ok {
							args = append(args, "--"+k, val)
						}
					}
					args = append(args, sc.inputs...)
					m = measure(out, path, args...)
					r.Status = statusOf(filepath.Join(*work, "result.json"))
				}
				r.fill(m)
				if m.err == nil {
					r.OutputSize = inputSize(target)
					if r.OutputSize == 0 {
						r.OutputSize = in // skipped: nothing written
					}
					r.Ratio = float64(r.OutputSize) / float64(max(in, 1))
					if pathExists(target) && !sc.folder {
						vm, logical := verify(path, target)
						if vm.err != nil {
							r.Error = "verify: " + vm.err.Error()
						} else {
							r.VerifySec = vm.wall
							r.VerifyMBs = float64(logical) / (1 << 20) / vm.wall
						}
					}
				}
				runs = append(runs, r)
				if kind == "profiles" && m.err == nil && pathExists(target) && !sc.folder {
					againDir := filepath.Join(*work, "again")
					_ = os.RemoveAll(againDir)
					must(os.MkdirAll(againDir, 0o755))
					again := filepath.Join(againDir, "o.nup")
					_ = os.Remove(filepath.Join(*work, "result.json"))
					args := []string{"optimize", "--output", again, "--result-json", filepath.Join(*work, "result.json")}
					for _, kv := range strings.Split(v, ",") {
						if k, val, ok := strings.Cut(kv, "="); ok {
							args = append(args, "--"+k, val)
						}
					}
					args = append(args, target)
					m2 := measure(againDir, path, args...)
					r2 := Run{Scenario: sc.name, Binary: label, Variant: v + " (2nd run)", InputSize: r.OutputSize,
						Status: statusOf(filepath.Join(*work, "result.json"))}
					r2.fill(m2)
					r2.OutputSize = inputSize(again)
					if r2.OutputSize == 0 {
						r2.OutputSize = r.OutputSize
					}
					r2.Ratio = float64(r2.OutputSize) / float64(max(r2.InputSize, 1))
					runs = append(runs, r2)
					_ = os.RemoveAll(againDir)
				}
			}
		}
	}
	_ = os.RemoveAll(filepath.Join(*work, "out"))
	printTable(runs)
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(map[string]any{"scale": *scale, "cpus": runtime.NumCPU(), "runs": runs}, "", "  ")
		must(os.WriteFile(*jsonOut, b, 0o644))
	}
}

func (r *Run) fill(m measured) {
	r.WallSec, r.CPUSec = m.wall, m.cpu
	r.PeakRSSMB = float64(m.rssKB) / 1024
	r.ReadMB, r.WrittenMB = float64(m.read)/(1<<20), float64(m.written)/(1<<20)
	r.PeakTempMB = float64(m.peakDisk) / (1 << 20)
	if m.err != nil {
		r.Error = m.err.Error() + ": " + lastLine(m.stderrOut)
		r.Status = "error"
	}
}

func combine(a, b measured) measured {
	a.wall += b.wall
	a.cpu += b.cpu
	a.rssKB = max(a.rssKB, b.rssKB)
	a.read += b.read
	a.written += b.written
	a.peakDisk = max(a.peakDisk, b.peakDisk)
	a.err = b.err
	a.stderrOut = b.stderrOut
	return a
}

// capability tells what a boxcar binary can do.
func capability(bin string) string {
	out, _ := exec.Command(bin, "optimize", "-h").CombinedOutput()
	switch {
	case bytes.Contains(out, []byte("-profile")):
		return "profiles"
	case bytes.Contains(out, []byte("usage: boxcar optimize")):
		return "optimize"
	}
	return "pack"
}

// measure runs bin args through a fresh copy of this program (-child), so
// the measured process is forked from a small parent: a child forked from
// this process would inherit its peak RSS. The helper reaps the command and
// reports its rusage and, on Linux, its own /proc io counters, which include
// the reaped child.
func measure(diskDir, bin string, args ...string) measured {
	tmp, err := os.MkdirTemp("", "bench")
	must(err)
	defer os.RemoveAll(tmp)
	self, err := os.Executable()
	must(err)
	cmd := exec.Command(self, append([]string{"-child", tmp, bin}, args...)...)
	var peak atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // sample disk use of diskDir
		defer wg.Done()
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			if s := inputSize(diskDir); s > peak.Load() {
				peak.Store(s)
			}
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	out, runErr := cmd.CombinedOutput()
	close(stop)
	wg.Wait()
	if s := inputSize(diskDir); s > peak.Load() {
		peak.Store(s)
	}
	m := measured{peakDisk: peak.Load()}
	var rep childReport
	if runErr != nil || json.Unmarshal(out, &rep) != nil {
		m.err = fmt.Errorf("helper failed: %v: %s", runErr, out)
		return m
	}
	m.wall, m.cpu, m.rssKB, m.read, m.written = rep.Wall, rep.CPU, rep.RSSKB, rep.Read, rep.Written
	if rep.Err != "" {
		m.err = errors.New(rep.Err)
	}
	m.stdout, _ = os.ReadFile(filepath.Join(tmp, "out"))
	m.stderrOut, _ = os.ReadFile(filepath.Join(tmp, "err"))
	return m
}

type childReport struct {
	Wall, CPU     float64
	RSSKB         int64
	Read, Written int64
	Err           string
}

// child is the -child helper: run the command, then report.
func child(tmp string, argv []string) {
	stdout, err := os.Create(filepath.Join(tmp, "out"))
	must(err)
	stderr, err := os.Create(filepath.Join(tmp, "err"))
	must(err)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	runErr := cmd.Run()
	rep := childReport{Wall: time.Since(start).Seconds()}
	if runErr != nil {
		rep.Err = runErr.Error()
	}
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok && ru != nil {
		rep.CPU = tv(ru.Utime) + tv(ru.Stime)
		rep.RSSKB = int64(ru.Maxrss)
		if runtime.GOOS == "darwin" {
			rep.RSSKB /= 1024 // bytes there
		}
	}
	if io, err := os.ReadFile("/proc/self/io"); err == nil {
		for _, line := range strings.Split(string(io), "\n") {
			k, v, _ := strings.Cut(line, ": ")
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			switch k {
			case "rchar":
				rep.Read = n
			case "wchar":
				rep.Written = n
			}
		}
	}
	b, _ := json.Marshal(rep)
	os.Stdout.Write(b)
}

func verify(bin, archive string) (measured, int64) {
	m := measure(filepath.Dir(archive), bin, "nup", "--nup-operation", "verify", "--input", archive)
	var res struct {
		Uncompressed int64 `json:"uncompressed"`
		Verified     bool  `json:"verified"`
	}
	if m.err == nil {
		if i := bytes.IndexByte(m.stdout, '{'); i >= 0 {
			_ = json.Unmarshal(m.stdout[i:], &res)
		}
		if !res.Verified {
			m.err = fmt.Errorf("not verified")
		}
	}
	return m, res.Uncompressed
}

func statusOf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "?"
	}
	var res struct {
		Status  string `json:"status"`
		Results []struct {
			Status string `json:"status"`
		} `json:"archives"`
	}
	_ = json.Unmarshal(b, &res)
	if len(res.Results) > 0 {
		count := map[string]int{}
		var keys []string
		for _, r := range res.Results {
			if count[r.Status] == 0 {
				keys = append(keys, r.Status)
			}
			count[r.Status]++
		}
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%d %s", count[k], k))
		}
		return strings.Join(parts, ", ")
	}
	return res.Status
}

func printTable(runs []Run) {
	fmt.Println("| scenario | binary | variant | status | in MiB | out MiB | ratio | wall s | CPU s | RSS MiB | read MiB | written MiB | peak disk MiB | verify s (MiB/s) |")
	fmt.Println("|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, r := range runs {
		v := "–"
		if r.VerifySec > 0 {
			v = fmt.Sprintf("%.2f (%.0f)", r.VerifySec, r.VerifyMBs)
		}
		status := r.Status
		if r.Error != "" {
			status = "error: " + r.Error
		}
		fmt.Printf("| %s | %s | %s | %s | %.1f | %.1f | %.3f | %.2f | %.2f | %.0f | %.0f | %.0f | %.0f | %s |\n",
			r.Scenario, r.Binary, r.Variant, status, float64(r.InputSize)/(1<<20), float64(r.OutputSize)/(1<<20), r.Ratio,
			r.WallSec, r.CPUSec, r.PeakRSSMB, r.ReadMB, r.WrittenMB, r.PeakTempMB, v)
	}
}

// inputSize is the size of a file, or the total size of a folder's files.
func inputSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// warmUp reads every file under path once.
func warmUp(path string) {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if f, err := os.Open(p); err == nil {
				_, _ = io.Copy(io.Discard, f)
				f.Close()
			}
		}
		return nil
	})
}

func pathExists(p string) bool { _, err := os.Stat(p); return err == nil }

func tv(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(1)
}
