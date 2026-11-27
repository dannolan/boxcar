package nup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dannolan/boxcar/internal/nup/nuptest"
)

// TestBenchFixtures runs every benchmark scenario (scripts/bench) at a tiny
// scale through each profile: the output keeps the contents and verifies,
// and optimizing the result again changes nothing (idempotence).
// The second run writes no output.
func TestBenchFixtures(t *testing.T) {
	dir := t.TempDir()
	for _, fx := range nuptest.Fixtures(0.01) {
		for _, ar := range fx.Archives {
			in := filepath.Join(dir, ar.Name)
			if err := nuptest.WriteKaiDump(in, ar.Build(), nuptest.Options{}); err != nil {
				t.Fatalf("%s: %v", ar.Name, err)
			}
			for _, profile := range []string{"runtime", "archive", "fast"} {
				ro, err := ProfileOptions(profile)
				if err != nil {
					t.Fatal(err)
				}
				out := filepath.Join(dir, profile+"-"+ar.Name)
				res := optimizeT(t, in, OptimizeOptions{RepackOptions: ro, Output: out, MinGain: 0})
				if res.Status != "optimized" && res.Status != "normalized" {
					t.Fatalf("%s/%s: status %s (%s)", ar.Name, profile, res.Status, res.Reason)
				}
				assertSameContent(t, in, out, nil)
				// The archive profile cannot tell a level-19 frame from any
				// other without re-encoding it, so its second run rewrites
				// and then discards the result.
				again := optimizeT(t, out, OptimizeOptions{RepackOptions: ro, Output: filepath.Join(dir, "again-"+profile+"-"+ar.Name), MinGain: 0})
				if again.Status != "skipped" {
					t.Errorf("%s/%s: second run %s (%s), want skipped", ar.Name, profile, again.Status, again.Reason)
				}
				if _, err := os.Stat(filepath.Join(dir, "again-"+profile+"-"+ar.Name)); !os.IsNotExist(err) {
					t.Errorf("%s/%s: second run left an output (%v)", ar.Name, profile, err)
				}
			}
		}
	}
}
