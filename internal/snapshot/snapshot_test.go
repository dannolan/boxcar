package snapshot

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildNPDM builds a minimal META/ACI0 main.npdm carrying the given title ID,
// the layout metadata.ReadTitleID expects.
func buildNPDM(tid uint64) []byte {
	b := make([]byte, 0x100)
	copy(b[:4], "META")
	off := uint32(0x80)
	binary.LittleEndian.PutUint32(b[0x70:], off)
	binary.LittleEndian.PutUint32(b[0x74:], 0x20)
	copy(b[off:off+4], "ACI0")
	binary.LittleEndian.PutUint64(b[off+0x10:], tid)
	return b
}

// writeSnapshot writes a complete exefs+romfs snapshot at dir.
func writeSnapshot(t *testing.T, dir string, tid uint64, romfsFiles map[string][]byte) {
	t.Helper()
	exefs := filepath.Join(dir, "exefs")
	if err := os.MkdirAll(exefs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exefs, "main"), []byte("main-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exefs, "main.npdm"), buildNPDM(tid), 0o644); err != nil {
		t.Fatal(err)
	}
	romfs := filepath.Join(dir, "romfs")
	if err := os.MkdirAll(romfs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range romfsFiles {
		p := filepath.Join(romfs, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindBasic(t *testing.T) {
	folder := t.TempDir()
	snapDir := filepath.Join(folder, "base", "updates", "123")
	writeSnapshot(t, snapDir, 0x0100000000010000, map[string][]byte{
		"file.bin":       []byte("payload"),
		"sub/nested.bin": []byte("nested"),
	})

	found, skipped, err := Find(context.Background(), folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none", skipped)
	}
	if len(found) != 1 {
		t.Fatalf("found %d snapshots, want 1", len(found))
	}
	s := found[0]
	if s.TitleID != 0x0100000000010000 {
		t.Errorf("TitleID = %#x, want 0x0100000000010000", s.TitleID)
	}
	if s.Version != 123 {
		t.Errorf("Version = %d, want 123", s.Version)
	}
	if want := filepath.Join("base", "updates", "123"); s.Label != want {
		t.Errorf("Label = %q, want %q", s.Label, want)
	}
	if s.Title != filepath.Base(folder) {
		t.Errorf("Title = %q, want %q", s.Title, filepath.Base(folder))
	}
	if s.NacpPath != "" {
		t.Errorf("NacpPath = %q, want empty", s.NacpPath)
	}
}

func TestFindSortsByVersionDesc(t *testing.T) {
	folder := t.TempDir()
	writeSnapshot(t, filepath.Join(folder, "base", "updates", "10"), 0x0100000000010000, map[string][]byte{"a": []byte("1")})
	writeSnapshot(t, filepath.Join(folder, "base", "updates", "300"), 0x0100000000010001, map[string][]byte{"a": []byte("1")})
	writeSnapshot(t, filepath.Join(folder, "base", "updates", "50"), 0x0100000000010002, map[string][]byte{"a": []byte("1")})

	found, _, err := Find(context.Background(), folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Fatalf("found %d, want 3", len(found))
	}
	if found[0].Version != 300 || found[1].Version != 50 || found[2].Version != 10 {
		t.Errorf("order = [%d %d %d], want [300 50 10]", found[0].Version, found[1].Version, found[2].Version)
	}
}

func TestFindSkipsDLC(t *testing.T) {
	folder := t.TempDir()
	writeSnapshot(t, filepath.Join(folder, "base", "updates", "1"), 0x0100000000010000, map[string][]byte{"a": []byte("1")})
	dlcDir := resolvePath(filepath.Join(folder, "dlc"))
	// A dlc directory is reported in skipped and never scanned.
	writeSnapshot(t, filepath.Join(folder, "dlc", "inner"), 0x0100000000010001, map[string][]byte{"b": []byte("2")})

	found, skipped, err := Find(context.Background(), folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d snapshots, want 1 (dlc must be skipped)", len(found))
	}
	if len(skipped) != 1 || skipped[0] != dlcDir {
		t.Errorf("skipped = %v, want [%s]", skipped, dlcDir)
	}
}

func TestFindRejectsPatchTID(t *testing.T) {
	folder := t.TempDir()
	// Bit 0x800 set marks a patch, not a main application.
	writeSnapshot(t, filepath.Join(folder, "base", "updates", "1"), 0x0100000000010800, map[string][]byte{"a": []byte("1")})

	found, _, err := Find(context.Background(), folder)
	if err == nil {
		t.Fatal("expected error for patch-only folder")
	}
	if len(found) != 0 {
		t.Fatalf("found = %v, want none", found)
	}
	if !strings.Contains(err.Error(), "NPDM identifies a patch, not a main application.") {
		t.Errorf("error = %q, want patch detail", err.Error())
	}
}

func TestFindClimbsExefsRomfs(t *testing.T) {
	folder := t.TempDir()
	writeSnapshot(t, filepath.Join(folder, "base", "updates", "1"), 0x0100000000010000, map[string][]byte{"a": []byte("1")})

	// Pointing at the exefs directory should climb to the game folder.
	found, _, err := Find(context.Background(), filepath.Join(folder, "base", "updates", "1", "exefs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].TitleID != 0x0100000000010000 {
		t.Fatalf("found = %v, want the game snapshot", found)
	}
}

func TestFindControlNacp(t *testing.T) {
	folder := t.TempDir()
	snapDir := filepath.Join(folder, "base", "updates", "1")
	writeSnapshot(t, snapDir, 0x0100000000010000, map[string][]byte{"a": []byte("1")})

	nacp := make([]byte, 0x4000)
	copy(nacp, "My Game Title")
	if err := os.WriteFile(filepath.Join(snapDir, "control.nacp"), nacp, 0o644); err != nil {
		t.Fatal(err)
	}

	found, _, err := Find(context.Background(), folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d, want 1", len(found))
	}
	if found[0].NacpPath != filepath.Join(found[0].Root, "control.nacp") {
		t.Errorf("NacpPath = %q, want %q", found[0].NacpPath, filepath.Join(found[0].Root, "control.nacp"))
	}
	if found[0].Title != "My Game Title" {
		t.Errorf("Title = %q, want %q", found[0].Title, "My Game Title")
	}
}

func TestInventoryAndValidate(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, 0x0100000000010000, map[string][]byte{
		"file.bin":       []byte("payload"),
		"sub/nested.bin": []byte("nested"),
	})

	inv, err := Inventory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 4 {
		t.Fatalf("inventory has %d files, want 4 (main, main.npdm, file.bin, sub/nested.bin)", len(inv))
	}
	for p, st := range inv {
		if st.Size <= 0 {
			t.Errorf("%s: size = %d, want > 0", p, st.Size)
		}
		if st.MtimeNS <= 0 {
			t.Errorf("%s: mtime = %d, want > 0", p, st.MtimeNS)
		}
	}

	if err := ValidateSources(inv); err != nil {
		t.Fatalf("ValidateSources on unchanged sources: %v", err)
	}

	// Mutate a romfs file: ValidateSources must detect it.
	mutated := filepath.Join(root, "romfs", "file.bin")
	if err := os.WriteFile(mutated, []byte("CHANGED-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSources(inv); err == nil {
		t.Fatal("ValidateSources must fail after source mutation")
	} else if err.Error() != "A source file changed during packaging. Try again with the dump idle." {
		t.Errorf("error = %q", err.Error())
	}
}

func TestInventoryRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, 0x0100000000010000, map[string][]byte{"file.bin": []byte("payload")})
	target := filepath.Join(root, "romfs", "file.bin")
	link := filepath.Join(root, "romfs", "link.bin")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := Inventory(context.Background(), root, nil); err == nil {
		t.Fatal("Inventory must refuse symlinks")
	} else if !strings.Contains(err.Error(), "Linked files/folders are not supported") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestInventoryEmptyRomfs(t *testing.T) {
	root := t.TempDir()
	exefs := filepath.Join(root, "exefs")
	if err := os.MkdirAll(exefs, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(exefs, "main"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(exefs, "main.npdm"), buildNPDM(0x0100000000010000), 0o644)
	if err := os.MkdirAll(filepath.Join(root, "romfs"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Inventory(context.Background(), root, nil); err == nil {
		t.Fatal("Inventory must fail on empty romfs")
	} else if err.Error() != "The RomFS folder is empty." {
		t.Errorf("error = %q", err.Error())
	}
}
