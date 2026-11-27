package diskfree

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeVolume makes the reservations see a volume with free bytes free; the
// real free space of the test machine changes under other writers.
func fakeVolume(t *testing.T, free *uint64) {
	t.Helper()
	old := available
	available = func(string) (uint64, error) { return *free, nil }
	t.Cleanup(func() { available = old })
}

// TestReserveSharesFreeSpace: two jobs on one volume cannot both claim the
// same free space; a released reservation frees it again, and bytes a job
// has already written are not counted twice.
func TestReserveSharesFreeSpace(t *testing.T) {
	dir := t.TempDir()
	free := uint64(1 << 30)
	fakeVolume(t, &free)
	big := int64(free/2) + 1
	tempA := filepath.Join(dir, "a.partial")
	releaseA, err := Reserve(dir, tempA, big)
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := Reserve(dir, filepath.Join(dir, "b.partial"), big); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("second reservation over the free space: err = %v", err)
	}
	// A temp file that already holds most of its size leaves only the rest
	// pending; the volume's free space has shrunk by what it holds.
	f, err := os.Create(tempA)
	if err != nil {
		t.Fatal(err)
	}
	written := big - 64<<20
	if err := f.Truncate(written); err != nil {
		t.Fatal(err)
	}
	f.Close()
	free -= uint64(written)
	// left: free/2+64 MiB; pending for A: 64 MiB; B fits with the headroom
	releaseB, err := Reserve(dir, filepath.Join(dir, "b.partial"), int64(free)-64<<20-headroom)
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

func TestIsFATOnTempDir(t *testing.T) {
	fat, err := IsFAT(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if fat {
		t.Skip("the temporary folder is on a FAT volume")
	}
}

// TestReserveFuncCountsWritten: what a job has written is no longer
// pending, so a second job can claim it.
func TestReserveFuncCountsWritten(t *testing.T) {
	dir := t.TempDir()
	free := uint64(1 << 30)
	fakeVolume(t, &free)
	need := int64(free) - headroom - 1<<20
	var written int64
	release, err := ReserveFunc(dir, func() int64 { return written }, need)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if r, err := Reserve(dir, filepath.Join(dir, "x"), headroom); err == nil {
		r()
		t.Fatal("second reservation fit although the first claims almost everything")
	}
	written = need / 2 // as if written: the OS already counts it
	free -= uint64(written)
	r, err := Reserve(dir, filepath.Join(dir, "x"), 1<<20/2)
	if err != nil {
		t.Fatalf("written bytes still counted as pending: %v", err)
	}
	r()
}
