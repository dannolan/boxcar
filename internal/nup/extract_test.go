package nup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractSelection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packages, expected := buildSynthetic(t, dir)
	out := filepath.Join(dir, "game.nup")
	if _, err := Pack(ctx, packages, out, PackOptions{Level: 3, LargeRaw: true, Verify: false}); err != nil {
		t.Fatalf("Pack: %v", err)
	}

	extractDir := filepath.Join(dir, "extracted")
	res, err := Extract(ctx, out, extractDir, []int{1}, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Packages != 1 {
		t.Errorf("Packages = %d, want 1", res.Packages)
	}
	if _, err := os.Stat(filepath.Join(extractDir, "updates/1/exefs/main")); err != nil {
		t.Errorf("update file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, "base")); err == nil {
		t.Errorf("base should not be extracted")
	}
	if _, err := os.Stat(filepath.Join(extractDir, "dlc")); err == nil {
		t.Errorf("dlc should not be extracted")
	}
	for rel, want := range expected {
		if !strings.HasPrefix(rel, "updates/") {
			continue
		}
		got, err := os.ReadFile(filepath.Join(extractDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content mismatch", rel)
		}
	}
}

func TestExtractRefusesExistingOutput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packages, _ := buildSynthetic(t, dir)
	out := filepath.Join(dir, "game.nup")
	if _, err := Pack(ctx, packages, out, PackOptions{Level: 3, Verify: false}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	existing := filepath.Join(dir, "existing")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(existing, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(ctx, out, existing, nil, nil); err == nil {
		t.Errorf("Extract into existing dir: want error")
	}
	if got, _ := os.ReadFile(keep); string(got) != "keep" {
		t.Errorf("existing content was modified")
	}
}

func TestExtractInvalidSelection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	packages, _ := buildSynthetic(t, dir)
	out := filepath.Join(dir, "game.nup")
	if _, err := Pack(ctx, packages, out, PackOptions{Level: 3, Verify: false}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if _, err := Extract(ctx, out, filepath.Join(dir, "e1"), []int{5}, nil); err == nil {
		t.Errorf("out-of-range selection: want error")
	}
	if _, err := Extract(ctx, out, filepath.Join(dir, "e2"), []int{}, nil); err == nil {
		t.Errorf("empty selection: want error")
	}
	if _, err := Extract(ctx, out, filepath.Join(dir, "e3"), []int{0, 0}, nil); err == nil {
		t.Errorf("duplicate selection: want error")
	}
}
