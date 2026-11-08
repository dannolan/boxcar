package nup

import (
	"bytes"
	"context"
	"fmt"
	"github.com/dannolan/boxcar/internal/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func havePython(t *testing.T) (python, src string) {
	return testutil.SkipUnlessPyRef(t)
}

// crossFile is one synthetic source file shared by the Go and Python builders.
type crossFile struct {
	pkg     int // 0=base, 1=update, 2=dlc
	path    string
	content []byte
}

func crosscheckFiles() []crossFile {
	tid := uint64(0x0100000000010000)
	return []crossFile{
		{0, "exefs/main.npdm", buildNPDM(tid)},
		{0, "exefs/main", append([]byte("NSO0"), bytes.Repeat([]byte{1}, 256)...)},
		{0, "romfs/hello.txt", []byte("hello world")},
		{0, "romfs/sub/nested.bin", []byte("nested payload data")},
		{1, "exefs/main", append([]byte("NSO0"), bytes.Repeat([]byte{2}, 256)...)},
		{1, "romfs/update.bin", []byte("update content")},
		{2, "romfs/addon.bin", []byte("addon content")},
	}
}

// writeCrosscheckSources writes the shared source files to disk and returns
// the Go package set plus the expected extraction layout.
func writeCrosscheckSources(t *testing.T, dir string) ([]Package, map[string][]byte) {
	t.Helper()
	const tid = 0x0100000000010000
	pkgDir := map[int]string{0: "base", 1: "update", 2: "dlc"}
	for _, f := range crosscheckFiles() {
		writeFile(t, dir, filepath.Join(pkgDir[f.pkg], f.path), f.content)
	}
	base := Package{TitleID: tid, Version: 0, Kind: 0, Files: map[string]*Source{}, Directories: []string{"exefs", "romfs", "romfs/sub"}}
	update := Package{TitleID: tid, Version: 1, Kind: 1, Files: map[string]*Source{}, Directories: []string{"exefs", "romfs"}}
	dlc := Package{TitleID: tid | 0x1000, Version: 0, Kind: 2, Files: map[string]*Source{}, Directories: []string{"romfs"}}
	expected := map[string][]byte{}
	for _, f := range crosscheckFiles() {
		src := mustSourcePath(t, filepath.Join(dir, pkgDir[f.pkg], f.path))
		var rel string
		switch f.pkg {
		case 0:
			base.Files[f.path] = src
			rel = "base/" + f.path
		case 1:
			update.Files[f.path] = src
			rel = "updates/1/" + f.path
		case 2:
			dlc.Files[f.path] = src
			rel = "dlc/0100000000011000/0/" + f.path
		}
		expected[rel] = f.content
	}
	return []Package{base, update, dlc}, expected
}

// runPython writes script to a temp file and runs it with the reference venv.
func runPython(t *testing.T, dir, script string, args ...string) {
	t.Helper()
	python, _ := testutil.SkipUnlessPyRef(t)
	scriptPath := filepath.Join(dir, "script.py")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, append([]string{scriptPath}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python failed: %v\n%s", err, out)
	}
}

func compareExtracted(t *testing.T, extractDir string, expected map[string][]byte) {
	t.Helper()
	for rel, want := range expected {
		got, err := os.ReadFile(filepath.Join(extractDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content mismatch", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(extractDir, "nup-manifest.json")); err != nil {
		t.Errorf("nup-manifest.json missing: %v", err)
	}
}

// pythonBuildScript packs the shared source files into a NUP using the Python
// reference implementation.
func pythonBuildScript(src string) string {
	return fmt.Sprintf(`import sys, os
sys.path.insert(0, %q)
import nup_format as nf

src = sys.argv[1]
out = sys.argv[2]
TID = 0x0100000000010000

def p(rel):
    return nf.Source.path(os.path.join(src, rel))

base = nf.Package(TID, 0, 0, files={
    'exefs/main.npdm': p('base/exefs/main.npdm'),
    'exefs/main': p('base/exefs/main'),
    'romfs/hello.txt': p('base/romfs/hello.txt'),
    'romfs/sub/nested.bin': p('base/romfs/sub/nested.bin'),
}, directories={'exefs', 'romfs', 'romfs/sub'})

update = nf.Package(TID, 1, 1, files={
    'exefs/main': p('update/exefs/main'),
    'romfs/update.bin': p('update/romfs/update.bin'),
}, directories={'exefs', 'romfs'})

dlc = nf.Package(TID | 0x1000, 0, 2, files={
    'romfs/addon.bin': p('dlc/romfs/addon.bin'),
}, directories={'romfs'})

nf.pack([base, update, dlc], out, level=3, large_raw=True, verify=True)
print('OK')
`, src)
}

// pythonVerifyExtractScript verifies and extracts a NUP using the Python
// reference implementation.
func pythonVerifyExtractScript(src string) string {
	return fmt.Sprintf(`import sys
sys.path.insert(0, %q)
import nup_format as nf

nup = sys.argv[1]
out = sys.argv[2]

with nf.Archive(nup) as a:
    a.verify()
nf.extract(nup, out)
print('OK')
`, src)
}

func TestCrossValidation(t *testing.T) {
	_, pyrefSrc := havePython(t)
	ctx := context.Background()
	dir := t.TempDir()
	packages, expected := writeCrosscheckSources(t, dir)

	// (a) Python builds gold.nup; Go opens, verifies and extracts it.
	gold := filepath.Join(dir, "gold.nup")
	runPython(t, dir, pythonBuildScript(pyrefSrc), dir, gold)

	a, err := Open(ctx, gold)
	if err != nil {
		t.Fatalf("Open(gold.nup): %v", err)
	}
	if _, err := a.Verify(ctx, nil); err != nil {
		t.Fatalf("Verify(gold.nup): %v", err)
	}
	if _, _, err := a.Inspect(ctx); err != nil {
		t.Fatalf("Inspect(gold.nup): %v", err)
	}
	a.Close()

	goExtract := filepath.Join(dir, "go-extract")
	if _, err := Extract(ctx, gold, goExtract, nil, nil); err != nil {
		t.Fatalf("Extract(gold.nup): %v", err)
	}
	compareExtracted(t, goExtract, expected)

	// (b) Go packs gold-go.nup; Python verifies and extracts it.
	goldGo := filepath.Join(dir, "gold-go.nup")
	if _, err := Pack(ctx, packages, goldGo, PackOptions{Level: 3, LargeRaw: true, Verify: true}); err != nil {
		t.Fatalf("Pack(gold-go.nup): %v", err)
	}
	pyExtract := filepath.Join(dir, "py-extract")
	runPython(t, dir, pythonVerifyExtractScript(pyrefSrc), goldGo, pyExtract)
	compareExtracted(t, pyExtract, expected)
}
