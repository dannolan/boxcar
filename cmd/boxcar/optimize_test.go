package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dannolan/boxcar/internal/nup"
	"github.com/dannolan/boxcar/internal/nup/nuptest"
)

// TestMain lets the CLI tests run this test binary as the boxcar command.
func TestMain(m *testing.M) {
	if os.Getenv("BOXCAR_TEST_MAIN") == "1" {
		os.Exit(run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

const cliTID = 0x0100000000010000

// wordStream yields deterministic, compressible text.
type wordStream struct {
	rng  *rand.Rand
	left int64
}

func (s *wordStream) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), s.left))
	words := [...]string{"mario ", "zelda ", "kirby ", "samus ", "link ", "fox ", "ness\n"}
	for i := 0; i < n; {
		i += copy(p[i:n], words[s.rng.Intn(len(words))])
	}
	s.left -= int64(n)
	return n, nil
}

// writeDump writes a KaiDump-style (raw, undeduplicated) archive; extra
// adds a streamed text file of that size.
func writeDump(t *testing.T, path string, extra int64) {
	t.Helper()
	text := bytes.Repeat([]byte("hello optimizer\n"), 5000)
	files := []nuptest.File{
		{Path: "exefs/main.npdm", Data: buildNPDM(cliTID)},
		{Path: "exefs/main", Data: []byte("main-binary")},
		{Path: "romfs/a.txt", Data: text},
		{Path: "romfs/b.txt", Data: text},
		{Path: "romfs/empty"},
	}
	if extra > 0 {
		files = append(files, nuptest.File{Path: "romfs/big.txt", Size: extra, Source: &wordStream{rng: rand.New(rand.NewSource(7)), left: extra}})
	}
	pkgs := []nuptest.Package{{TitleID: cliTID, Kind: 0, Files: files, Meta: nuptest.TLV(8, []byte("KaiDump"))}}
	if err := nuptest.WriteKaiDump(path, pkgs, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
}

func sha(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func verifyArchive(t *testing.T, path string) string {
	t.Helper()
	a, err := nup.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer a.Close()
	if _, err := a.Verify(context.Background(), nil); err != nil {
		t.Fatalf("Verify(%s): %v", path, err)
	}
	return nup.LogicalDigest(a.Contents())
}

func partials(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*.partial"))
	return m
}

func TestOptimizeCommand(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	writeDump(t, in, 0)
	digest := verifyArchive(t, in)
	out := filepath.Join(dir, "out", "game.nup")
	report := filepath.Join(dir, "result.json")

	if code := run([]string{"optimize", in, "--output", out, "--preset", "Smaller", "--result-json", report}); code != 0 {
		t.Fatalf("optimize = %d", code)
	}
	if verifyArchive(t, out) != digest {
		t.Errorf("logical content changed")
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Archives []struct {
			Status, Output string
			SizeBefore     int64  `json:"size_before"`
			SizeAfter      int64  `json:"size_after"`
			Logical        string `json:"logical_sha256"`
			OutputLogical  string `json:"output_logical_sha256"`
			Verified       bool
		}
		Summary struct{ Optimized int }
	}
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	a := res.Archives[0]
	if res.Summary.Optimized != 1 || a.Status != "optimized" || !a.Verified || a.Output != out ||
		a.SizeAfter >= a.SizeBefore || a.Logical != digest || a.OutputLogical != digest {
		t.Errorf("result = %s", data)
	}

	// The nup-operation spelling, a dry run, and flags after paths.
	if code := run([]string{"nup", "--nup-operation=optimize", "--input", in, "--input", out, "--dry-run"}); code != 0 {
		t.Errorf("nup --nup-operation optimize --dry-run = %d", code)
	}
	if code := run([]string{"optimize", dir, "--recursive", "--dry-run", "--jobs", "2"}); code != 0 {
		t.Errorf("optimize DIR --recursive --dry-run = %d", code)
	}

	before := sha(t, in)
	if code := run([]string{"optimize", in, "--in-place"}); code != 0 {
		t.Fatalf("--in-place = %d", code)
	}
	if sha(t, in) == before || verifyArchive(t, in) != digest {
		t.Errorf("in-place rewrite missing or changed content")
	}
	if p := partials(t, dir); len(p) != 0 {
		t.Errorf("leftover partial files: %v", p)
	}
}

func TestOptimizeCommandErrors(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	writeDump(t, in, 0)
	broken := filepath.Join(dir, "broken.nup")
	if err := os.WriteFile(broken, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"optimize", in},
		{"optimize"},
		{"optimize", in, "--in-place", "--output", filepath.Join(dir, "x.nup")},
		{"optimize", in, "--dry-run", "--preset", "Huge"},
		{"optimize", in, "--dry-run", "--large-file-threshold", "big"},
		{"optimize", in, "--dry-run", "--jobs", "-1"},
		{"optimize", in, "--dry-run", "--profile", "tiny"},
		{"optimize", in, "--dry-run", "--split", "100"},
		{"optimize", in, "--in-place", "--split", "1MiB"},
		{"optimize", dir, "--dry-run"},
	} {
		if code := run(args); code != 1 {
			t.Errorf("%v = %d, want 1", args, code)
		}
	}
	if code := run([]string{"optimize", in, "--dry-run"}); code != 0 {
		t.Errorf("dry run of a good archive = %d", code)
	}
}

// TestOptimizeNoLatestSnapshotWins: --no-latest-snapshot overrides an
// earlier --latest-snapshot, as it does for pack.
func TestOptimizeNoLatestSnapshotWins(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "bundle.nup")
	files := func(main string) map[string]*nup.Source {
		return map[string]*nup.Source{
			"exefs/main.npdm": nup.SourceData(buildNPDM(cliTID)),
			"exefs/main":      nup.SourceData([]byte(main)),
			"romfs/a.bin":     nup.SourceData(bytes.Repeat([]byte(main), 100)),
		}
	}
	pkgs := []nup.Package{
		{TitleID: cliTID, Kind: 0, Files: files("base")},
		{TitleID: cliTID | 0x800, Version: 65536, Kind: 1, Files: files("update")},
	}
	if _, err := nup.Pack(context.Background(), pkgs, in, nup.PackOptions{Level: 1, LargeRaw: true}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.nup")
	if code := run([]string{"optimize", in, "--output", out, "--latest-snapshot", "--no-latest-snapshot", "--force"}); code != 0 {
		t.Fatalf("optimize = %d", code)
	}
	a, err := nup.Open(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if n := len(a.Packages()); n != 2 {
		t.Errorf("packages = %d, want 2 (no consolidation)", n)
	}
}

func TestNupOperationParsing(t *testing.T) {
	for want, args := range map[string][]string{
		"optimize": {"--input", "x", "--nup-operation", "optimize"},
		"verify":   {"-nup-operation=verify"},
		"":         {"--input", "nup-operation"},
	} {
		if got := nupOperation(args); got != want {
			t.Errorf("nupOperation(%v) = %q, want %q", args, got, want)
		}
	}
}

// startOptimize runs the CLI in a child process on a large in-place job and
// returns once the child has started writing its partial file.
func startOptimize(t *testing.T) (cmd *exec.Cmd, dir, in string, before [32]byte, stderr *bytes.Buffer) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals")
	}
	if testing.Short() {
		t.Skip("large archive")
	}
	dir = t.TempDir()
	in = filepath.Join(dir, "game.nup")
	writeDump(t, in, 192<<20)
	before = sha(t, in)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(exe, "optimize", in, "--in-place", "--preset", "Smaller")
	cmd.Env = append(os.Environ(), "BOXCAR_TEST_MAIN=1")
	stderr = &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for len(partials(t, dir)) == 0 {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("no partial file appeared; stderr:\n%s", stderr)
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	return cmd, dir, in, before, stderr
}

// TestOptimizeSurvivesKill: a hard kill in the middle of an in-place rewrite
// leaves the original byte-identical and valid; only the partial file is
// left behind.
func TestOptimizeSurvivesKill(t *testing.T) {
	cmd, dir, in, before, stderr := startOptimize(t)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatalf("process finished before it was killed; stderr:\n%s", stderr)
	}
	if sha(t, in) != before {
		t.Fatalf("killed rewrite modified the original")
	}
	verifyArchive(t, in)
	if p := partials(t, dir); len(p) != 1 {
		t.Errorf("partial files after a hard kill: %v", p)
	}
}

// TestOptimizeInterrupt: Ctrl-C cancels cleanly, removing the partial file.
func TestOptimizeInterrupt(t *testing.T) {
	cmd, dir, in, before, stderr := startOptimize(t)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v, want status 1; stderr:\n%s", err, stderr)
	}
	if !strings.Contains(stderr.String(), "FAILED: context canceled") {
		t.Errorf("stderr does not report the cancellation:\n%s", stderr)
	}
	if sha(t, in) != before {
		t.Fatalf("interrupted rewrite modified the original")
	}
	if p := partials(t, dir); len(p) != 0 {
		t.Errorf("partial files after an interrupt: %v", p)
	}
}

// TestOptimizeProfiles: --profile selects the storage policy, single flags
// override it, and the JSON reports the policy that was used.
func TestOptimizeProfiles(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	writeDump(t, in, 0)
	cases := []struct {
		args      []string
		level     int
		raw       bool
		threshold int64
		savings   float64
		recompr   bool
	}{
		{nil, 3, true, 128 << 20, 5, false},
		{[]string{"--profile", "archive"}, 19, false, 256 << 20, 0, true},
		{[]string{"--profile", "Fast"}, 1, true, 128 << 20, 5, false},
		{[]string{"--profile", "archive", "--preset", "Balanced", "--recompress=false"}, 3, false, 256 << 20, 0, false},
		{[]string{"--min-savings", "0", "--large-file-threshold", "32MiB"}, 3, true, 32 << 20, 0, false},
		{[]string{"--no-large-files-raw"}, 3, false, 128 << 20, 5, false},
	}
	for i, c := range cases {
		report := filepath.Join(dir, fmt.Sprintf("r%d.json", i))
		args := append([]string{"optimize", in, "--dry-run", "--result-json", report}, c.args...)
		if code := run(args); code != 0 {
			t.Fatalf("%v = %d", c.args, code)
		}
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		var res struct {
			Policy struct {
				Level      int     `json:"level"`
				Raw        bool    `json:"large_files_raw"`
				Threshold  int64   `json:"large_file_threshold"`
				MinSavings float64 `json:"min_savings"`
				Recompress bool    `json:"recompress"`
			}
		}
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		p := res.Policy
		if p.Level != c.level || p.Raw != c.raw || p.Threshold != c.threshold || p.MinSavings != c.savings || p.Recompress != c.recompr {
			t.Errorf("%v: policy = %+v", c.args, p)
		}
	}
}

// TestOptimizeSplitCommand: --split writes nx2-flow-style parts, and the
// first part is accepted wherever an archive is (verify, optimize).
func TestOptimizeSplitCommand(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "game.nup")
	noise := make([]byte, 100000)
	rand.New(rand.NewSource(1)).Read(noise)
	files := []nuptest.File{
		{Path: "exefs/main.npdm", Data: buildNPDM(cliTID)},
		{Path: "romfs/noise.bin", Data: noise},
		{Path: "romfs/text.txt", Data: bytes.Repeat([]byte("hello optimizer\n"), 5000)},
	}
	if err := nuptest.WriteKaiDump(in, []nuptest.Package{{TitleID: cliTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out", "game.nup")
	report := filepath.Join(dir, "r.json")
	if code := run([]string{"optimize", in, "--output", out, "--split", "32KiB", "--result-json", report}); code != 0 {
		t.Fatalf("optimize --split = %d", code)
	}
	var res struct {
		Policy struct {
			SplitSize int64 `json:"split_size"`
		}
		Archives []struct {
			Output string
			Parts  []string
		}
	}
	data, _ := os.ReadFile(report)
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Policy.SplitSize != 32<<10 || len(res.Archives) != 1 || len(res.Archives[0].Parts) != 4 || res.Archives[0].Output != out+".001" {
		t.Fatalf("result = %s", data)
	}
	if code := run([]string{"nup", "--nup-operation", "verify", "--input", out + ".001"}); code != 0 {
		t.Errorf("verify of the first part = %d", code)
	}
	if code := run([]string{"optimize", out + ".001", "--dry-run"}); code != 0 {
		t.Errorf("dry run of the first part = %d", code)
	}
	if code := run([]string{"optimize", out + ".002", "--dry-run"}); code != 1 {
		t.Errorf("dry run of a later part = %d, want 1", code)
	}
}
