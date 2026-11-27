package nup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dannolan/boxcar/internal/nup/nuptest"
	"github.com/klauspost/compress/zstd"
)

func heapInUse() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse)
}

// TestEncoderCosts: the reserved encoder and decoder sizes cover what the
// encoders and decoders really hold after use.
func TestEncoderCosts(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates several hundred MiB")
	}
	data := compressibleText(16 << 20)
	for _, level := range []int{1, 3, 9, LevelBest} {
		const n = 3
		base := heapInUse()
		encs := make([]*zstd.Encoder, n)
		for k := range encs {
			e, err := newZstdEncoder(io.Discard, level, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			e.Write(data)
			e.Close()
			encs[k] = e
		}
		got := (heapInUse() - base) / n
		runtime.KeepAlive(encs)
		if got > encoderCost(level) {
			t.Errorf("level %d encoder holds %d bytes, reserved %d", level, got, encoderCost(level))
		}
	}
	var frame bytes.Buffer
	e, _ := newZstdEncoder(&frame, 3, int64(len(data)))
	e.Write(data)
	e.Close()
	base := heapInUse()
	d, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(workerWindow), zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	d.Reset(bytes.NewReader(frame.Bytes()))
	io.Copy(io.Discard, d)
	got := heapInUse() - base
	runtime.KeepAlive(d)
	d.Close()
	if got > decoderCost(workerWindow) {
		t.Errorf("decoder holds %d bytes, reserved %d", got, decoderCost(workerWindow))
	}
}

// TestReserveFlexible: a reservation takes what is free down to its
// minimum, waits when even that is not free, gives up on cancellation, and
// is granted alone when nothing is held however large.
func TestReserveFlexible(t *testing.T) {
	b := &budget{limit: 100}
	units, extra, total, err := b.reserveFlexible(t.Context(), 10, 20, 8, 15, 50)
	if err != nil || units != 3 || extra != 30 || total != 100 {
		t.Fatalf("got %d units, %d extra, %d total, %v", units, extra, total, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, _, _, err := b.reserveFlexible(ctx, 10, 20, 1, 0, 0); err == nil {
		t.Fatal("reserved over a full budget")
	}
	done := make(chan int)
	go func() {
		u, _, _, _ := b.reserveFlexible(t.Context(), 10, 20, 8, 0, 0)
		done <- u
	}()
	time.Sleep(10 * time.Millisecond)
	b.release(total)
	if u := <-done; u != 4 {
		t.Errorf("after release: %d units, want 4", u)
	}
	b.release(90)
	// zstd 19 under 512 MiB: read-ahead first (up to a quarter), then as
	// many 56 MiB workers as fit
	c := &budget{limit: 512}
	if u, x, _, _ := c.reserveFlexible(t.Context(), 57, 56, 8, 2, 128); u != 5 || x != 128 {
		t.Errorf("512 budget: %d workers, %d read-ahead; want 5, 128", u, x)
	}
	if _, _, total, _ := b.reserveFlexible(t.Context(), 500, 20, 2, 0, 0); total != 520 || b.used != 520 {
		t.Errorf("oversize reservation alone: total %d used %d", total, b.used)
	}
}

// TestOptimizeWithinMemoryBudget: concurrent optimizations under small
// budgets finish (no deadlock), produce the same bytes as an ample budget,
// return everything, and never hold more than the budget once it can hold
// their minimums.
func TestOptimizeWithinMemoryBudget(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.nup")
	small := filepath.Join(dir, "small.nup") // (zstd 19 is slow under the race detector)
	var files []nuptest.File
	for i := 0; i < 24; i++ {
		files = append(files, nuptest.File{Path: fmt.Sprintf("romfs/t%02d.txt", i), Data: compressibleText(300 << 10)})
	}
	if err := nuptest.WriteKaiDump(small, []nuptest.Package{{TitleID: testTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	files = append(files, nuptest.File{Path: "romfs/big.bin", Data: compressibleText(33 << 20)}) // streamed by the writer
	if err := nuptest.WriteKaiDump(in, []nuptest.Package{{TitleID: testTID, Kind: 0, Files: files}}, nuptest.Options{}); err != nil {
		t.Fatal(err)
	}
	old := MemoryBudget()
	defer SetMemoryBudget(old)
	var want [32]byte
	for _, c := range []struct {
		limit int64
		level int
	}{{4 << 30, 3}, {1, 3}, {96 << 20, 3}, {160 << 20, 3}, {200 << 20, LevelBest}} {
		SetMemoryBudget(c.limit)
		memBudget.mu.Lock()
		memBudget.peak = 0
		memBudget.mu.Unlock()
		var wg sync.WaitGroup
		errs := make([]error, 3)
		for k := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				opts := defaultOptions()
				opts.Level = c.level
				opts.Output = filepath.Join(dir, fmt.Sprintf("out-%d-%d-%d.nup", c.limit, c.level, k))
				src := in
				if c.level == LevelBest {
					src = small
				}
				_, errs[k] = Optimize(t.Context(), src, opts)
			}()
		}
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(5 * time.Minute):
			t.Fatalf("budget %d: optimizations did not finish (deadlock?)", c.limit)
		}
		for k, err := range errs {
			if err != nil {
				t.Fatalf("budget %d: %v", c.limit, err)
			}
			if c.level == 3 {
				got := fileSHA(t, filepath.Join(dir, fmt.Sprintf("out-%d-%d-%d.nup", c.limit, c.level, k)))
				if want == ([32]byte{}) {
					want = got
				} else if got != want {
					t.Errorf("budget %d: output differs", c.limit)
				}
			}
		}
		memBudget.mu.Lock()
		used, peak := memBudget.used, memBudget.peak
		memBudget.mu.Unlock()
		if used != 0 {
			t.Errorf("budget %d: still holds %d", c.limit, used)
		}
		if c.limit >= 96<<20 && peak > c.limit {
			t.Errorf("budget %d: held %d", c.limit, peak)
		}
	}
}
