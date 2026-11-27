package nup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dannolan/boxcar/internal/testutil"
)

// testdata/cancel.nup is one Kind 0 package (testTID) holding
// exefs/main.npdm (buildNPDM(testTID)) and romfs/zero.bin (4 MiB of zeros,
// zstd), written by Pack at level 3. It is checked in rather than packed per
// test so the cancellation tests do not depend on Pack.

func TestExtractCancelReleasesPartialOutput(t *testing.T) {
	dir := t.TempDir()
	fixture, err := os.ReadFile(filepath.Join("testdata", "cancel.nup"))
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "cancel.nup")
	if err := os.WriteFile(archive, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := 0
	output := filepath.Join(dir, "extracted")
	_, err = Extract(ctx, archive, output, nil, func(label string, _ float64) {
		if label == "Extracting verified files" {
			chunks++
			cancel() // lands while an output file is open, at a known point
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Extract err = %v, want context.Canceled", err)
	}
	if chunks != 1 {
		t.Errorf("%d chunks extracted; cancellation must stop at the next chunk", chunks)
	}
	if open, known := testutil.OpenFilesUnder(dir); known && len(open) > 0 {
		t.Errorf("files still open after Extract returned: %v", open)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Errorf("output exists after a cancelled extract (Lstat err %v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "cancel.nup" {
			t.Errorf("cancelled extract left %s behind", e.Name())
		}
	}
	if err := os.Remove(archive); err != nil {
		t.Errorf("archive not removable after Extract returned: %v", err)
	}
}
