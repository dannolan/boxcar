package stream

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cancelOnWrite cancels the pass's context during its first Write, so the
// cancellation lands mid-stream at a known point, with no sleeps.
type cancelOnWrite struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancelOnWrite) Write(b []byte) (int, error) {
	w.writes++
	w.cancel()
	return len(b), nil
}

type failingWriter struct{ writes int }

var errWrite = errors.New("disk full")

func (w *failingWriter) Write(b []byte) (int, error) {
	w.writes++
	return 0, errWrite
}

// openByProcess reports whether this process still holds path open, where
// the OS lists open descriptors (/proc/self/fd); known is false elsewhere.
func openByProcess(t *testing.T, path string) (open, known bool) {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return false, false
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		want = path
	}
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && target == want {
			return true, true
		}
	}
	return false, true
}

// assertReleased fails if the pass left the source file open: the file must
// be removable right away (Windows refuses to delete an open file) and,
// where the OS lists open descriptors, no longer be among them.
func assertReleased(t *testing.T, path string) {
	t.Helper()
	if open, known := openByProcess(t, path); known && open {
		t.Errorf("%s is still open after the pass returned", filepath.Base(path))
	}
	if err := os.Remove(path); err != nil {
		t.Errorf("source not removable after the pass returned: %v", err)
	}
}

// cancelFixture is a 3 MiB file part (several 1 MiB reads) followed by a
// literal, written in 4 KiB blocks: hundreds of output chunks per read.
func cancelFixture(t *testing.T) (string, *Stream) {
	t.Helper()
	data := bytes.Repeat([]byte{0x5a}, 3<<20)
	p := writeFile(t, t.TempDir(), "src.bin", data)
	s, err := New(File(p, int64(len(data))), Bytes([]byte("tail")))
	if err != nil {
		t.Fatal(err)
	}
	return p, s
}

func TestCancelBeforeProcessingReleasesSource(t *testing.T) {
	p, s := cancelFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &failingWriter{}
	if err := s.WriteTo(w, &WriteOpts{Ctx: ctx, Block: 4096}); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteTo err = %v, want context.Canceled", err)
	}
	if w.writes != 0 {
		t.Errorf("%d chunks written after cancellation", w.writes)
	}
	if _, err := s.SHA256(&WriteOpts{Ctx: ctx, Block: 4096}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SHA256 err = %v, want context.Canceled", err)
	}
	if _, err := s.ReadHead(16, &WriteOpts{Ctx: ctx, Block: 4096}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadHead err = %v, want context.Canceled", err)
	}
	assertReleased(t, p)
}

func TestCancelDuringOutputReleasesSource(t *testing.T) {
	p, s := cancelFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelOnWrite{cancel: cancel}
	if err := s.WriteTo(w, &WriteOpts{Ctx: ctx, Block: 4096}); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteTo err = %v, want context.Canceled", err)
	}
	if w.writes != 1 {
		t.Errorf("%d chunks written; cancellation must stop the pass before the next chunk", w.writes)
	}
	assertReleased(t, p)
}

func TestCancelDuringDigestReleasesSource(t *testing.T) {
	p, s := cancelFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reports := 0
	opts := &WriteOpts{Ctx: ctx, Block: 4096, Progress: func(string, float64) { reports++; cancel() }}
	if _, err := s.SHA256(opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("SHA256 err = %v, want context.Canceled", err)
	}
	if reports != 1 {
		t.Errorf("%d chunks digested after cancellation", reports)
	}
	assertReleased(t, p)
}

func TestDeadlineReleasesSource(t *testing.T) {
	p, s := cancelFixture(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := s.WriteTo(&discard{}, &WriteOpts{Ctx: ctx, Block: 4096}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WriteTo err = %v, want context.DeadlineExceeded", err)
	}
	assertReleased(t, p)
}

func TestWriterErrorReleasesSource(t *testing.T) {
	p, s := cancelFixture(t)
	w := &failingWriter{}
	if err := s.WriteTo(w, &WriteOpts{Block: 4096}); !errors.Is(err, errWrite) {
		t.Fatalf("WriteTo err = %v, want the writer's error", err)
	}
	if w.writes != 1 {
		t.Errorf("%d writes after the writer failed", w.writes)
	}
	assertReleased(t, p)
}

func TestCompletedPassReleasesSource(t *testing.T) {
	p, s := cancelFixture(t)
	var out bytes.Buffer
	if err := s.WriteTo(&out, &WriteOpts{Block: 4096}); err != nil {
		t.Fatal(err)
	}
	if int64(out.Len()) != s.Size() {
		t.Fatalf("wrote %d bytes, want %d", out.Len(), s.Size())
	}
	assertReleased(t, p)
}
