package nuptools

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/nup"
	"github.com/dannolan/boxcar/internal/testutil"
)

const cancelTID = 0x0100000000010000

// snapshotFixture is a path-backed game package whose romfs file needs
// several 1 MiB reads, plus an empty output folder for BuildSnapshotNSP.
func snapshotFixture(t *testing.T) (dir, outDir string, pkg nup.Package) {
	t.Helper()
	dir = t.TempDir()
	files := map[string][]byte{
		"exefs/main.npdm": buildNPDM(cancelTID),
		"romfs/big.bin":   bytes.Repeat([]byte{0x5a}, 3<<20),
	}
	pkg = nup.Package{TitleID: cancelTID, Kind: 0, Files: map[string]*nup.Source{}, Directories: []string{"exefs", "romfs"}}
	for name, data := range files {
		p := filepath.Join(dir, "src", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := nup.SourcePath(p)
		if err != nil {
			t.Fatal(err)
		}
		pkg.Files[name] = s
	}
	outDir = filepath.Join(dir, "out")
	if err := os.Mkdir(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, outDir, pkg
}

// assertSnapshotReleased fails if BuildSnapshotNSP left a file open, left its
// temp dir or an output behind, or kept a source from being removed.
func assertSnapshotReleased(t *testing.T, dir, outDir string) {
	t.Helper()
	if open, known := testutil.OpenFilesUnder(dir); known && len(open) > 0 {
		t.Errorf("files still open after BuildSnapshotNSP returned: %v", open)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("failed snapshot left %s behind", e.Name())
	}
	if err := os.RemoveAll(filepath.Join(dir, "src")); err != nil {
		t.Errorf("sources not removable after BuildSnapshotNSP returned: %v", err)
	}
}

func TestSnapshotCancelReleasesFiles(t *testing.T) {
	dir, outDir, pkg := snapshotFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reports := 0
	_, err := BuildSnapshotNSP(ctx, pkg, filepath.Join(outDir, "game.nsp"), false, func(label string, _ float64) {
		if label == "Preparing snapshot" {
			reports++
			cancel() // lands while a source and its copy are open
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildSnapshotNSP err = %v, want context.Canceled", err)
	}
	if reports != 1 {
		t.Errorf("%d chunks copied; cancellation must stop at the next chunk", reports)
	}
	assertSnapshotReleased(t, dir, outDir)
}

func TestSnapshotSourceGrowthReleasesFiles(t *testing.T) {
	dir, outDir, pkg := snapshotFixture(t)
	// Grow both sources after their sizes were captured: the copy then
	// fails its size check (a binfmt panic) with both files open.
	for name := range pkg.Files {
		f, err := os.OpenFile(filepath.Join(dir, "src", filepath.FromSlash(name)), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, werr := f.Write(bytes.Repeat([]byte{1}, 2<<20))
		if cerr := f.Close(); werr != nil || cerr != nil {
			t.Fatal(werr, cerr)
		}
	}
	_, err := BuildSnapshotNSP(context.Background(), pkg, filepath.Join(outDir, "game.nsp"), false, nil)
	if err == nil || !strings.Contains(err.Error(), "Source size changed.") {
		t.Fatalf("BuildSnapshotNSP err = %v, want the size-change error", err)
	}
	assertSnapshotReleased(t, dir, outDir)
}
