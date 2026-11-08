package patch

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dannolan/boxcar/internal/binfmt"
)

// setupPatch builds a synthetic exe, registers a known profile for it, and
// returns the emulator folder.
func setupPatch(t *testing.T, name string, exe []byte) string {
	t.Helper()
	dir := t.TempDir()
	profiles[digest(exe)] = Profile{
		Name:     "Synthetic test emulator",
		File:     name,
		ABI:      "msvc",
		Hook:     0x1000,
		Expected: "48895c2410",
		Kind:     "entry",
	}
	t.Cleanup(func() { delete(profiles, digest(exe)) })
	if err := os.WriteFile(filepath.Join(dir, name), exe, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestApplyRemoveExactAndRepeat mirrors test_apply_remove_exact_and_repeat.
func TestApplyRemoveExactAndRepeat(t *testing.T) {
	original := syntheticPE()
	dir := setupPatch(t, "eglantine.exe", original)
	exePath := filepath.Join(dir, "eglantine.exe")

	for i := 0; i < 2; i++ {
		view, err := Inspect(t.Context(), dir)
		if err != nil || !view.CanApply {
			t.Fatalf("iter %d: inspect can_apply = %v (%v)", i, view.CanApply, err)
		}
		view, err = Apply(t.Context(), dir)
		if err != nil || !view.CanRemove {
			t.Fatalf("iter %d: apply can_remove = %v (%v)", i, view.CanRemove, err)
		}
		got, _ := os.ReadFile(exePath)
		if bytes.Equal(got, original) {
			t.Fatalf("iter %d: exe not patched", i)
		}
		backup, _ := os.ReadFile(filepath.Join(dir, stateDir, "eglantine.exe.original"))
		if !bytes.Equal(backup, original) {
			t.Fatalf("iter %d: backup != original", i)
		}
		view, err = Remove(t.Context(), dir)
		if err != nil || !view.CanApply {
			t.Fatalf("iter %d: remove can_apply = %v (%v)", i, view.CanApply, err)
		}
		got, _ = os.ReadFile(exePath)
		if !bytes.Equal(got, original) {
			t.Fatalf("iter %d: exe not restored", i)
		}
	}
}

// TestUnknownLoaderUntouched mirrors test_unknown_loader_is_untouched.
func TestUnknownLoaderIsUntouched(t *testing.T) {
	original := syntheticPE()
	dir := setupPatch(t, "eglantine.exe", original)
	exePath := filepath.Join(dir, "eglantine.exe")
	garbage := append([]byte("MZ"), make([]byte, 100)...)
	if err := os.WriteFile(exePath, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(t.Context(), dir)
	if err != nil || view.CanApply {
		t.Fatalf("inspect can_apply = %v (%v)", view.CanApply, err)
	}
	if _, err := Apply(t.Context(), dir); err == nil {
		t.Fatal("apply should fail on unknown loader")
	}
	got, _ := os.ReadFile(exePath)
	if !bytes.Equal(got, garbage) {
		t.Fatal("unknown loader was modified")
	}
	if _, err := os.Stat(filepath.Join(dir, stateDir)); !os.IsNotExist(err) {
		t.Fatal("state dir should not exist")
	}
}

// TestChangedExecutableOrBackupNotOverwritten mirrors test_changed_executable_or_backup_is_not_overwritten.
func TestChangedExecutableOrBackupNotOverwritten(t *testing.T) {
	original := syntheticPE()
	dir := setupPatch(t, "eglantine.exe", original)
	exePath := filepath.Join(dir, "eglantine.exe")

	if _, err := Apply(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	// Append to the *patched* exe, matching the Python test which appends to current.
	current, _ := os.ReadFile(exePath)
	current = append(current, []byte("changed")...)
	if err := os.WriteFile(exePath, current, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(t.Context(), dir); err == nil {
		t.Fatal("remove should fail on changed exe")
	}
	got, _ := os.ReadFile(exePath)
	if !bytes.Equal(got, current) {
		t.Fatal("changed exe was overwritten")
	}

	backup := filepath.Join(dir, stateDir, "eglantine.exe.original")
	if err := os.WriteFile(backup, []byte("bad backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), dir); err == nil {
		t.Fatal("inspect should fail on corrupt backup")
	}
	got, _ = os.ReadFile(backup)
	if string(got) != "bad backup" {
		t.Fatal("corrupt backup was overwritten")
	}
}

// TestPartialWriteFailureRollsBack mirrors test_partial_write_failure_rolls_back.
func TestPartialWriteFailureRollsBack(t *testing.T) {
	original := syntheticPE()
	dir := setupPatch(t, "eglantine.exe", original)
	eglantine := filepath.Join(dir, "eglantine.exe")

	// Second recognized exe: differs by a single header byte.
	cliData := append([]byte{}, original...)
	cliData[0x88] ^= 1
	cliPath := filepath.Join(dir, "eden-cli.exe")
	profiles[digest(cliData)] = Profile{
		Name:     "Synthetic test emulator",
		File:     "eden-cli.exe",
		ABI:      "msvc",
		Hook:     0x1000,
		Expected: "48895c2410",
		Kind:     "entry",
	}
	t.Cleanup(func() { delete(profiles, digest(cliData)) })
	if err := os.WriteFile(cliPath, cliData, 0o644); err != nil {
		t.Fatal(err)
	}

	realWrite := atomicWrite
	failed := false
	atomicWrite = func(path string, data []byte, exclusive bool) {
		if filepath.Base(path) == "eglantine.exe" && !bytes.Equal(data, original) && !failed {
			failed = true
			panic(binfmt.Failure{Err: os.ErrInvalid})
		}
		realWrite(path, data, exclusive)
	}
	defer func() { atomicWrite = realWrite }()

	if _, err := Apply(t.Context(), dir); err == nil {
		t.Fatal("apply should fail")
	}
	got, _ := os.ReadFile(eglantine)
	if !bytes.Equal(got, original) {
		t.Fatal("eglantine.exe not rolled back")
	}
	gotCli, _ := os.ReadFile(cliPath)
	if !bytes.Equal(gotCli, cliData) {
		t.Fatal("eden-cli.exe not rolled back")
	}
	view, err := Inspect(t.Context(), dir)
	if err != nil || !view.CanApply {
		t.Fatalf("after rollback inspect can_apply = %v (%v)", view.CanApply, err)
	}
}

// TestManifestPathInjectionAndLock mirrors test_manifest_path_injection_and_lock.
func TestManifestPathInjectionAndLock(t *testing.T) {
	original := syntheticPE()
	dir := setupPatch(t, "eglantine.exe", original)
	state := filepath.Join(dir, stateDir)

	root, s, unlock := locked(dir)
	resolvedState, _ := filepath.EvalSymlinks(state)
	if s != resolvedState {
		t.Fatalf("locked state = %q, want %q", s, resolvedState)
	}
	if _, err := Apply(t.Context(), dir); err == nil {
		unlock()
		t.Fatal("apply should fail while lock held")
	}
	unlock()
	_ = root

	if err := os.WriteFile(filepath.Join(state, "state.json"),
		[]byte(`{"patch_id":"embedded-npdm-v1","files":{"../outside.exe":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), dir); err == nil {
		t.Fatal("inspect should fail on path injection")
	}
}

// TestLegacyPatchRemoveThenUpdate mirrors test_legacy_patch_can_be_removed_then_updated.
func TestLegacyPatchRemoveThenUpdate(t *testing.T) {
	original := syntheticPE()
	dir := setupPatch(t, "eglantine.exe", original)
	state := filepath.Join(dir, stateDir)
	exePath := filepath.Join(dir, "eglantine.exe")

	old := transform(original, Profile{Name: "t", File: "eglantine.exe", ABI: "msvc", Hook: 0x1000, Expected: "48895c2410", Kind: "entry"}, 1)
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "eglantine.exe.original"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"patch_id": patchID,
		"files": map[string]any{
			"eglantine.exe": map[string]any{
				"original": digest(original),
				"patched":  digest(old),
			},
		},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(state, "state.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exePath, old, 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := Inspect(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if view.Files[0].Status != "outdated" {
		t.Fatalf("status = %q, want outdated", view.Files[0].Status)
	}
	if !view.CanRemove || view.CanApply {
		t.Fatalf("can_remove=%v can_apply=%v", view.CanRemove, view.CanApply)
	}
	if _, err := Remove(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exePath)
	if !bytes.Equal(got, original) {
		t.Fatal("legacy patch not restored")
	}
	view, err = Apply(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if view.Files[0].Status != "patched" {
		t.Fatalf("status = %q, want patched", view.Files[0].Status)
	}
	if rec := records(state)["eglantine.exe"]; rec.Revision == nil || *rec.Revision != 2 {
		t.Fatalf("revision = %v, want 2", rec.Revision)
	}
}

// TestParentFolderSelection mirrors test_parent_folder_selection.
func TestParentFolderSelection(t *testing.T) {
	original := syntheticPE()
	dir := setupPatch(t, "eglantine.exe", original)
	view, err := Inspect(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	if view.Folder != resolved {
		t.Fatalf("folder = %q, want %q", view.Folder, resolved)
	}
}
