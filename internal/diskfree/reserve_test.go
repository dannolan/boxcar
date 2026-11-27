package diskfree

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestReserveSharesFreeSpace: two jobs on one volume cannot both claim the
// same free space; a released reservation frees it again, and bytes a job
// has already written are not counted twice.
func TestReserveSharesFreeSpace(t *testing.T) {
	dir := t.TempDir()
	free, err := Available(dir)
	if err != nil {
		t.Fatal(err)
	}
	if free < 4*headroom {
		t.Skip("too little free space")
	}
	big := int64(free/2) + 1
	tempA := filepath.Join(dir, "a.partial")
	releaseA, err := Reserve(dir, tempA, big)
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := Reserve(dir, filepath.Join(dir, "b.partial"), big); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("second reservation over the free space: err = %v", err)
	}
	// A temp file that already holds its whole size leaves nothing pending.
	f, err := os.Create(tempA)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(big); err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(tempA)
	// (sparse: the free space did not shrink, so the second job fits now)
	releaseB, err := Reserve(dir, filepath.Join(dir, "b.partial"), big)
	if err != nil {
		t.Fatalf("written bytes counted twice: %v", err)
	}
	releaseB()
	releaseA()
	releaseB, err = Reserve(dir, filepath.Join(dir, "b.partial"), 1)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	releaseB()
	if _, err := Reserve(dir, "x", int64(free)+1); !errors.Is(err, ErrNoSpace) {
		t.Errorf("reservation larger than the volume: err = %v", err)
	}
}
