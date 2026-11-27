package nup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"runtime"
	"sync"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/klauspost/compress/zstd"
)

// File-level parallelism for the optimizer. Contents that need CPU work
// (compression or re-encoding) and fit in parChunk are read, checked and
// encoded by workers into memory, ahead of the single writer, which writes
// them in layout order: the output does not depend on the number of
// workers. Each file is encoded independently with a freshly reset encoder,
// exactly as the streaming path does, so both paths emit identical frames.
// Larger contents keep the streaming path (bounded by Block-sized buffers).
//
// Two process-wide budgets keep concurrent archives (--jobs) and their
// workers together within the machine: cpuTokens (one per CPU) is held
// while a worker computes, and memBudget bounds the bytes read and encoded
// ahead of the writers.
var parChunk int64 = 32 << 20 // (a variable so tests can force the streaming path)

var (
	cpuTokens = &budget{limit: int64(runtime.GOMAXPROCS(0))}
	memBudget = &budget{limit: 128 << 20}
)

// budget is a weighted semaphore. A request larger than the limit is
// granted when nothing else is held, so no single job can deadlock.
type budget struct {
	mu    sync.Mutex
	limit int64
	used  int64
	wake  chan struct{}
}

func (b *budget) acquire(ctx context.Context, n int64) error {
	for {
		b.mu.Lock()
		if b.used == 0 || b.used+n <= b.limit {
			b.used += n
			b.mu.Unlock()
			return nil
		}
		if b.wake == nil {
			b.wake = make(chan struct{})
		}
		wake := b.wake
		b.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *budget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	if b.wake != nil {
		close(b.wake)
		b.wake = nil
	}
	b.mu.Unlock()
}

// Worker jobs keep their bytes in chunks of one size (Block) taken from a
// free list, so buffers are always reusable whatever the file sizes, the
// free list never holds more than the memory budget, and steady-state
// encoding allocates and clears almost nothing.
var freeChunks struct {
	sync.Mutex
	list   [][]byte
	active int // parallel writes in progress; the list is emptied when none is
}

func getChunk() []byte {
	freeChunks.Lock()
	if n := len(freeChunks.list); n > 0 {
		b := freeChunks.list[n-1]
		freeChunks.list[n-1] = nil
		freeChunks.list = freeChunks.list[:n-1]
		freeChunks.Unlock()
		return b
	}
	freeChunks.Unlock()
	return make([]byte, Block)
}

func putChunks(bufs [][]byte) {
	freeChunks.Lock()
	defer freeChunks.Unlock()
	for _, b := range bufs {
		if int64(len(freeChunks.list)+1)*Block > memBudget.limit {
			return
		}
		freeChunks.list = append(freeChunks.list, b[:Block])
	}
}

func chunksFor(n int64) int64 { return (n + Block - 1) / Block }

// chunked is a byte sequence kept in Block-sized chunks; writes past limit
// fail with errFrameTooLarge.
type chunked struct {
	bufs  [][]byte
	n     int64
	limit int64
}

func (c *chunked) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.limit {
		return 0, errFrameTooLarge
	}
	done := 0
	for done < len(p) {
		at := int(c.n % Block)
		if at == 0 {
			c.bufs = append(c.bufs, getChunk())
		}
		k := copy(c.bufs[len(c.bufs)-1][at:], p[done:])
		done += k
		c.n += int64(k)
	}
	return done, nil
}

// each calls f with the content's pieces in order.
func (c *chunked) each(f func([]byte) error) error {
	left := c.n
	for _, b := range c.bufs {
		k := min64(left, Block)
		if err := f(b[:k]); err != nil {
			return err
		}
		left -= k
	}
	return nil
}

func (c *chunked) reader() io.Reader {
	rs := make([]io.Reader, 0, len(c.bufs))
	_ = c.each(func(b []byte) error { rs = append(rs, bytes.NewReader(b)); return nil })
	return io.MultiReader(rs...)
}

func (c *chunked) free() {
	putChunks(c.bufs)
	c.bufs, c.n = nil, 0
}

// parJob is one content encoded off the writer.
type parJob struct {
	node  int
	cost  int64
	done  chan struct{}
	data  *chunked   // the extent to write
	held  []*chunked // chunk sets to return once written
	codec int
	stat  *int // the RepackStats counter it adds to
	err   error
}

// parallel runs the workers for one write. jobs holds, per node, the job
// that stores it (nil: the writer stores it itself).
type parallel struct {
	w       *repackWriter
	ctx     context.Context
	cancel  context.CancelFunc
	jobs    map[int]*parJob
	order   []*parJob
	queue   chan *parJob
	wg      sync.WaitGroup
	started bool
}

// parallelEligible reports whether node i is stored by a worker.
func (w *repackWriter) parallelEligible(i int) bool {
	np := w.nodes[i]
	if np.writer != i || (np.act != actCompress && np.act != actReencode) {
		return false
	}
	rec := w.a.files[np.rec]
	return rec.size <= parChunk && rec.stored <= parChunk
}

func (w *repackWriter) startParallel(workers int) *parallel {
	ctx, cancel := context.WithCancel(w.ctx)
	p := &parallel{w: w, ctx: ctx, cancel: cancel, jobs: map[int]*parJob{}, queue: make(chan *parJob)}
	for i := range w.nodes {
		if w.parallelEligible(i) {
			rec := w.a.files[w.nodes[i].rec]
			// the stored bytes and the new frame (at most the file's size)
			cost := (chunksFor(rec.stored) + chunksFor(rec.size)) * Block
			if rec.codec == 1 {
				cost += chunksFor(rec.size) * Block // the decoded bytes
			}
			j := &parJob{node: i, cost: cost, done: make(chan struct{})}
			p.jobs[i] = j
			p.order = append(p.order, j)
		}
	}
	if len(p.order) == 0 {
		cancel()
		return p
	}
	freeChunks.Lock()
	freeChunks.active++
	freeChunks.Unlock()
	p.started = true
	for k := 0; k < workers; k++ {
		p.wg.Add(1)
		go p.worker()
	}
	// The feeder claims memory in layout order, so the writer's next job
	// always holds its share before any later one.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(p.queue)
		for _, j := range p.order {
			if memBudget.acquire(ctx, j.cost) != nil {
				return
			}
			select {
			case p.queue <- j:
			case <-ctx.Done():
				memBudget.release(j.cost)
				return
			}
		}
	}()
	return p
}

// Encoders are shared by every archive in the process: a worker takes one
// per job while it holds a CPU token, so at most one encoder per CPU and
// level is ever built (each holds window-sized history buffers).
var freeEncoders struct {
	sync.Mutex
	byLevel map[int][]*zstd.Encoder
}

func getEncoder(level int) (*zstd.Encoder, error) {
	freeEncoders.Lock()
	if l := freeEncoders.byLevel[level]; len(l) > 0 {
		e := l[len(l)-1]
		freeEncoders.byLevel[level] = l[:len(l)-1]
		freeEncoders.Unlock()
		return e, nil
	}
	freeEncoders.Unlock()
	return newZstdEncoder(io.Discard, level, 0)
}

func putEncoder(level int, e *zstd.Encoder) {
	freeEncoders.Lock()
	defer freeEncoders.Unlock()
	if freeEncoders.byLevel == nil {
		freeEncoders.byLevel = map[int][]*zstd.Encoder{}
	}
	freeEncoders.byLevel[level] = append(freeEncoders.byLevel[level], e)
}

func (p *parallel) worker() {
	defer p.wg.Done()
	var dec *zstd.Decoder
	defer func() {
		if dec != nil {
			dec.Close()
		}
	}()
	for j := range p.queue {
		if cerr := cpuTokens.acquire(p.ctx, 1); cerr != nil {
			j.err = cerr
		} else if enc, eerr := getEncoder(p.w.level); eerr != nil {
			j.err = eerr
			cpuTokens.release(1)
		} else {
			j.err = p.encodeJob(j, enc, &dec)
			putEncoder(p.w.level, enc)
			cpuTokens.release(1)
		}
		close(j.done)
	}
}

// encodeJob reads, checks and encodes one content, choosing what the
// streaming path would: the new frame if it fits the policy limit, else the
// old frame (re-encode) or the raw bytes.
func (p *parallel) encodeJob(j *parJob, enc *zstd.Encoder, dec **zstd.Decoder) (err error) {
	defer binfmt.Recover(&err)
	w := p.w
	np := w.nodes[j.node]
	rec := w.a.files[np.rec]
	stored := &chunked{limit: rec.stored}
	j.held = append(j.held, stored)
	for off := int64(0); off < rec.stored; {
		k := min64(Block, rec.stored-off)
		c := getChunk()
		stored.bufs = append(stored.bufs, c)
		if _, err := w.a.f.ReadAt(c[:k], rec.off+off); err != nil {
			binfmt.Fail("%v", err)
		}
		off += k
		stored.n = off
	}
	raw := stored
	if rec.codec == 1 {
		if *dec == nil {
			d, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(MaxWindow), zstd.WithDecoderConcurrency(1))
			if err != nil {
				binfmt.Fail("%v", err)
			}
			*dec = d
		}
		if err := (*dec).Reset(stored.reader()); err != nil {
			(*dec).Close()
			*dec = nil
			binfmt.Fail("%v", err)
		}
		raw = &chunked{limit: rec.size}
		j.held = append(j.held, raw)
		if _, err := io.Copy(raw, *dec); errors.Is(err, errFrameTooLarge) {
			binfmt.Fail("Decompressed file exceeds its declared size.")
		} else if err != nil {
			binfmt.Fail("%v", err)
		}
		binfmt.Require(raw.n == rec.size, "File SHA-256 mismatch: %s", rec.name)
	}
	binfmt.Check(p.ctx)
	h := sha256.New()
	_ = raw.each(func(b []byte) error { h.Write(b); return nil })
	binfmt.Require(bytes.Equal(h.Sum(nil), rec.digest[:]), "File SHA-256 mismatch: %s", rec.name)

	limit := w.policy.maxStored(rec.size)
	if np.act == actReencode && np.oldOK {
		limit = min64(limit, rec.stored-1)
	}
	frame := &chunked{limit: max64(limit, 0)}
	j.held = append(j.held, frame)
	enc.ResetContentSize(frame, rec.size)
	werr := raw.each(func(b []byte) error { _, err := enc.Write(b); return err })
	if werr == nil {
		werr = enc.Close()
	}
	st := &w.stats
	switch {
	case werr == nil:
		j.data, j.codec = frame, 1
		j.stat = &st.Compressed
		if np.act == actReencode {
			j.stat = &st.Recompressed
		}
	case np.act == actReencode && np.oldOK:
		j.data, j.codec, j.stat = stored, 1, &st.ReusedFrames
	case np.act == actReencode:
		j.data, j.codec, j.stat = raw, 0, &st.Decompressed
	default:
		j.data, j.codec, j.stat = raw, 0, &st.Incompressible
	}
	return nil
}

// take waits for node i's job and returns it, or nil when the writer
// stores node i itself.
func (p *parallel) take(i int) *parJob {
	j := p.jobs[i]
	if j == nil {
		return nil
	}
	select {
	case <-j.done:
	case <-p.ctx.Done():
		binfmt.Check(p.w.ctx)
		<-j.done // (the feeder or a worker is finishing it)
	}
	if j.err != nil {
		if errors.Is(j.err, context.Canceled) {
			binfmt.Check(p.w.ctx)
		}
		binfmt.Fail("%v", j.err)
	}
	return j
}

// written returns job j's memory once its bytes are in the output.
func (p *parallel) written(j *parJob) {
	j.data = nil
	for _, c := range j.held {
		c.free()
	}
	j.held = nil
	memBudget.release(j.cost)
	j.cost = 0
}

// stop ends the workers and returns the memory of jobs that were claimed
// but never written (errors, cancellation).
func (p *parallel) stop() {
	p.cancel()
	p.wg.Wait()
	defer func() {
		if !p.started {
			return
		}
		freeChunks.Lock()
		if freeChunks.active--; freeChunks.active == 0 {
			freeChunks.list = nil // idle memory goes back to the runtime between archives
			freeEncoders.Lock()
			freeEncoders.byLevel = nil
			freeEncoders.Unlock()
		}
		freeChunks.Unlock()
	}()
	for _, j := range p.order {
		select {
		case <-j.done: // (done means the feeder claimed its memory)
			if j.cost > 0 {
				memBudget.release(j.cost)
				j.cost = 0
			}
		default:
		}
	}
}

// parWorkers is the number of encoding workers per archive: one per CPU
// (GOMAXPROCS); the shared CPU tokens keep concurrent archives from
// multiplying that. testWorkers overrides it in tests.
func parWorkers() int {
	if testWorkers > 0 {
		return testWorkers
	}
	return runtime.GOMAXPROCS(0)
}

var testWorkers int
