package nup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/testutil"
)

// pythonOpenScript prints the reference reader's verdict on a NUP.
func pythonOpenScript(src string) string {
	return fmt.Sprintf(`import sys
sys.path.insert(0, %q)
import nup_format as nf
try:
    with nf.Archive(sys.argv[1]) as a:
        a.verify()
    print('ACCEPTED')
except ValueError as e:
    print('REJECTED', e)
`, src)
}

func pythonVerdict(t *testing.T, dir, path string) string {
	t.Helper()
	python, src := testutil.SkipUnlessPyRef(t)
	script := filepath.Join(dir, "open.py")
	if err := os.WriteFile(script, []byte(pythonOpenScript(src)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, script, path).CombinedOutput()
	if err != nil {
		t.Fatalf("python failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestCrossOptimize: the Python reference must verify and extract optimizer
// output, whether it reuses python-zstandard frames, recompresses them with
// klauspost, or rewrites a KaiDump archive the reference itself rejects.
func TestCrossOptimize(t *testing.T) {
	_, pyrefSrc := havePython(t)
	ctx := context.Background()
	dir := t.TempDir()
	_, expected := writeCrosscheckSources(t, dir)
	gold := filepath.Join(dir, "gold.nup")
	runPython(t, dir, pythonBuildScript(pyrefSrc), dir, gold)

	for _, recompress := range []bool{false, true} {
		out := filepath.Join(dir, fmt.Sprintf("optimized-%v.nup", recompress))
		res, err := Optimize(ctx, gold, OptimizeOptions{
			RepackOptions: RepackOptions{Level: 9, LargeRaw: true, Recompress: recompress},
			Output:        out, Force: true,
		})
		if err != nil {
			t.Fatalf("Optimize(recompress=%v): %v", recompress, err)
		}
		if !recompress && res.Stats.ReusedFrames == 0 {
			t.Errorf("python frames were not reused: %+v", res.Stats)
		}
		extract := filepath.Join(dir, fmt.Sprintf("py-extract-%v", recompress))
		runPython(t, dir, pythonVerifyExtractScript(pyrefSrc), out, extract)
		compareExtracted(t, extract, expected)
	}

	kai := filepath.Join(dir, "kaidump.nup")
	kaiExpected := writeKaiDump(t, kai)
	if v := pythonVerdict(t, dir, kai); !strings.Contains(v, "Invalid empty-file extent.") {
		t.Fatalf("reference reader on the KaiDump fixture: %s", v)
	}
	fixed := filepath.Join(dir, "kaidump-optimized.nup")
	if _, err := Optimize(ctx, kai, OptimizeOptions{RepackOptions: RepackOptions{Level: 3, LargeRaw: true}, Output: fixed}); err != nil {
		t.Fatalf("Optimize(kaidump): %v", err)
	}
	extract := filepath.Join(dir, "py-extract-kaidump")
	runPython(t, dir, pythonVerifyExtractScript(pyrefSrc), fixed, extract)
	compareExtracted(t, extract, kaiExpected)
}
