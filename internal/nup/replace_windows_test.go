//go:build windows

package nup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// replaceFixture writes an original and its replacement and returns their
// paths and contents.
func replaceFixture(t *testing.T) (dir, orig, temp string, before, after []byte) {
	t.Helper()
	dir = t.TempDir()
	orig, temp = filepath.Join(dir, "game.nup"), filepath.Join(dir, "game.nup.x.partial")
	before, after = []byte("original archive bytes"), []byte("optimized")
	if err := os.WriteFile(orig, before, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, after, 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

func readOr(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return b
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*.boxcar-backup"))
	return m
}

func withReplace(t *testing.T, f func(replaced, replacement, backup string) error) {
	t.Helper()
	old := replaceFileW
	replaceFileW = f
	t.Cleanup(func() { replaceFileW = old })
}

func move(t *testing.T, from, to string) {
	t.Helper()
	a, _ := windows.UTF16PtrFromString(from)
	b, _ := windows.UTF16PtrFromString(to)
	if err := windows.MoveFileEx(a, b, 0); err != nil {
		t.Fatal(err)
	}
}

// TestReplaceFileSuccess: the real ReplaceFileW path leaves the new file at
// the name, no backup and no temporary file.
func TestReplaceFileSuccess(t *testing.T) {
	dir, orig, temp, _, after := replaceFixture(t)
	if err := replaceFile(temp, orig); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readOr(t, orig), after) || readOr(t, temp) != nil || len(backups(t, dir)) != 0 {
		t.Errorf("state after success: %q, temp %q, backups %v", readOr(t, orig), readOr(t, temp), backups(t, dir))
	}
}

// TestReplaceFileUnableToMove (1176 with a backup name): both files keep
// their names, so the original is intact and the run fails.
func TestReplaceFileUnableToMove(t *testing.T) {
	dir, orig, temp, before, _ := replaceFixture(t)
	withReplace(t, func(string, string, string) error { return errUnableToMoveReplacement })
	err := replaceFile(temp, orig)
	var keep *keepFilesError
	if err == nil || errors.As(err, &keep) || !strings.Contains(err.Error(), "original was kept") {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(readOr(t, orig), before) || len(backups(t, dir)) != 0 {
		t.Errorf("original changed or backup left")
	}
}

// TestReplaceFileUnableToMove2 (1177): the original was already moved to the
// backup name and nothing is at the path; it is moved back.
func TestReplaceFileUnableToMove2(t *testing.T) {
	dir, orig, temp, before, _ := replaceFixture(t)
	withReplace(t, func(replaced, _, backup string) error {
		move(t, replaced, backup)
		return errUnableToMoveReplacement2
	})
	err := replaceFile(temp, orig)
	if err == nil || !strings.Contains(err.Error(), "moved back") {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(readOr(t, orig), before) || len(backups(t, dir)) != 0 {
		t.Errorf("original not restored: %q, backups %v", readOr(t, orig), backups(t, dir))
	}
}

// TestReplaceFileUnableToMove2RestoreFails: when moving the original back
// fails too, nothing is deleted: the backup and the optimized copy stay,
// the error names both, and Optimize must not remove the temporary file.
func TestReplaceFileUnableToMove2RestoreFails(t *testing.T) {
	dir, orig, temp, before, after := replaceFixture(t)
	withReplace(t, func(replaced, _, backup string) error {
		move(t, replaced, backup)
		return errUnableToMoveReplacement2
	})
	old := restoreBackup
	restoreBackup = func(string, string) error { return errors.New("simulated") }
	t.Cleanup(func() { restoreBackup = old })
	err := replaceFile(temp, orig)
	var keep *keepFilesError
	if !errors.As(err, &keep) {
		t.Fatalf("err = %v, want keepFilesError", err)
	}
	b := backups(t, dir)
	if len(b) != 1 || !bytes.Equal(readOr(t, b[0]), before) || !bytes.Equal(readOr(t, temp), after) {
		t.Fatalf("backups %v, temp %q", b, readOr(t, temp))
	}
	if !strings.Contains(err.Error(), b[0]) || !strings.Contains(err.Error(), temp) {
		t.Errorf("error does not name the kept files: %v", err)
	}
}

// TestReplaceFileErrorAfterSuccess: a reported error is not trusted either
// way; when the new file is in place it is a success.
func TestReplaceFileErrorAfterSuccess(t *testing.T) {
	dir, orig, temp, _, after := replaceFixture(t)
	real := replaceFileW
	withReplace(t, func(a, b, c string) error {
		if err := real(a, b, c); err != nil {
			return err
		}
		return errUnableToRemoveReplaced
	})
	if err := replaceFile(temp, orig); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readOr(t, orig), after) || len(backups(t, dir)) != 0 {
		t.Errorf("state: %q backups %v", readOr(t, orig), backups(t, dir))
	}
}

// TestOptimizeInPlaceKeepsFilesWhenRecoveryFails: end to end, the verified
// optimized copy survives a replacement that could not be recovered.
func TestOptimizeInPlaceKeepsFilesWhenRecoveryFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kaidump.nup")
	writeKaiDump(t, path)
	before := readOr(t, path)
	withReplace(t, func(replaced, _, backup string) error {
		move(t, replaced, backup)
		return errUnableToMoveReplacement2
	})
	old := restoreBackup
	restoreBackup = func(string, string) error { return errors.New("simulated") }
	t.Cleanup(func() { restoreBackup = old })
	opts := defaultOptions()
	opts.InPlace = true
	_, err := Optimize(ctxT(), path, opts)
	var keep *keepFilesError
	if !errors.As(err, &keep) {
		t.Fatalf("err = %v", err)
	}
	b := backups(t, dir)
	partials, _ := filepath.Glob(filepath.Join(dir, "*.partial"))
	if len(b) != 1 || !bytes.Equal(readOr(t, b[0]), before) || len(partials) != 1 {
		t.Fatalf("backups %v partials %v", b, partials)
	}
	if b, err := Open(ctxT(), partials[0]); err != nil {
		t.Errorf("kept optimized copy does not open: %v", err)
	} else {
		b.Close()
	}
}
