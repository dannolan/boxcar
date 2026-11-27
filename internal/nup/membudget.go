package nup

import (
	"context"
	"sync"
)

// The working-memory budget. Every part of an optimization that holds a
// sizeable buffer reserves it here first: encoders and decoders (their
// window-sized history), the bytes workers read and encode ahead of the
// writer, read buffers, the estimator and verification. Concurrent archives
// (--jobs) share one budget, so the total stays within it however many run.
//
// The budget bounds the memory the optimizer itself holds. It is not the
// process RSS: the Go runtime keeps freed memory for a while before it
// returns it, small per-file metadata (names, the layout) is not counted,
// and a single item larger than the whole budget is still admitted when
// nothing else holds memory, so a tiny budget slows work down instead of
// failing it.
var memBudget = &budget{limit: autoMemoryBudget()}

const (
	minAutoBudget = 256 << 20
	maxAutoBudget = 2 << 30
)

// autoMemoryBudget is an eighth of the physical memory, between 256 MiB and
// 2 GiB (1 GiB when the physical memory is unknown).
func autoMemoryBudget() int64 {
	ram := physicalMemory()
	if ram <= 0 {
		return 1 << 30
	}
	return min64(max64(ram/8, minAutoBudget), maxAutoBudget)
}

// SetMemoryBudget sets the process-wide working-memory budget in bytes; 0
// or less restores the automatic one. Set it before optimizing.
func SetMemoryBudget(n int64) {
	if n <= 0 {
		n = autoMemoryBudget()
	}
	memBudget.setLimit(n)
}

// MemoryBudget returns the current working-memory budget in bytes.
func MemoryBudget() int64 {
	memBudget.mu.Lock()
	defer memBudget.mu.Unlock()
	return memBudget.limit
}

// Measured heap held by one klauspost/compress encoder after use (single
// goroutine, the default 8 MiB window), rounded up: SpeedFastest 8.6 MiB,
// SpeedDefault 17.9, SpeedBetterCompression 20.7, SpeedBestCompression
// 50.9. TestEncoderCosts keeps these honest.
func encoderCost(level int) int64 {
	switch level {
	case 1:
		return 10 << 20
	case 3:
		return 20 << 20
	case 9:
		return 23 << 20
	default:
		return 56 << 20
	}
}

// decoderCost is the heap a decoder holds for frames of the given window:
// the history plus about 1.3 MiB of tables and block buffers.
func decoderCost(window int64) int64 { return window + 2<<20 }

// workerWindow is the largest frame window a worker's long-lived decoder
// keeps; frames with a larger window are decoded by a decoder made for that
// job, whose memory the job reserves.
const workerWindow = 8 << 20

// budget is a weighted semaphore. A request larger than the limit is
// granted when nothing else is held, so no single job can deadlock.
type budget struct {
	mu    sync.Mutex
	limit int64
	used  int64
	peak  int64 // the most ever held (tests)
	wake  chan struct{}
}

func (b *budget) acquire(ctx context.Context, n int64) error {
	for {
		b.mu.Lock()
		if b.used == 0 || b.used+n <= b.limit {
			b.used += n
			b.peak = max64(b.peak, b.used)
			b.mu.Unlock()
			return nil
		}
		wake := b.waiter()
		b.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// waiter returns a channel closed at the next release (b.mu held).
func (b *budget) waiter() chan struct{} {
	if b.wake == nil {
		b.wake = make(chan struct{})
	}
	return b.wake
}

func (b *budget) release(n int64) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	if b.wake != nil {
		close(b.wake)
		b.wake = nil
	}
	b.mu.Unlock()
}

func (b *budget) setLimit(n int64) {
	b.mu.Lock()
	b.limit = n
	if b.wake != nil {
		close(b.wake)
		b.wake = nil
	}
	b.mu.Unlock()
}

// reserveFlexible reserves fixed plus between 1 and want units of unit
// bytes and between minExtra and wantExtra extra bytes, as much as the
// budget has free, waiting while it cannot even give the minimum (granted
// anyway when nothing is held). It is taken while the caller holds nothing
// else from the budget, so waiting cannot deadlock. It returns the units
// and extra bytes granted and the total to release.
func (b *budget) reserveFlexible(ctx context.Context, fixed, unit int64, want int, minExtra, wantExtra int64) (units int, extra, total int64, err error) {
	if want < 1 {
		want, unit = 1, 0
	}
	wantExtra = max64(wantExtra, minExtra)
	for {
		b.mu.Lock()
		free := b.limit - b.used
		need := fixed + unit + minExtra
		if free >= need || b.used == 0 {
			free = max64(free, need)
			// Units (workers) get what is left after the extra (read-ahead)
			// takes up to a quarter of the free memory, so a short budget
			// gets fewer workers rather than workers with nothing to read.
			keep := min64(wantExtra, max64(minExtra, free/4))
			units = 1
			if unit > 0 {
				units = int(max64(1, min64(int64(want), (free-fixed-keep)/unit)))
			}
			extra = min64(wantExtra, free-fixed-int64(units)*unit)
			total = fixed + int64(units)*unit + extra
			b.used += total
			b.peak = max64(b.peak, b.used)
			b.mu.Unlock()
			return units, extra, total, nil
		}
		wake := b.waiter()
		b.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return 0, 0, 0, ctx.Err()
		}
	}
}
