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
// Workers share the process-wide CPU tokens (one per CPU, held while a
// worker computes) and reserve their memory from the working-memory budget
// (membudget.go): each write reserves its writer's and workers' encoders
// and decoders and an allowance for the bytes read and encoded ahead of the
// writer before it starts, adapting the number of workers to what the
// budget has free.
var parChunk int64 = 32 << 20 // (a variable so tests can force the streaming path)

var cpuTokens = &budget{limit: int64(runtime.GOMAXPROCS(0))}

// readAhead is the allowance a write would like for jobs read and encoded
// ahead of its writer; it gets less (down to its largest job) when the
// budget is short.
const readAhead = 128 << 20

// chunkPool keeps a write's job bytes in chunks of one size (Block), so
// buffers are reusable whatever the file sizes and steady-state encoding
// allocates and clears almost nothing. It never holds more than the write's
// job allowance, and goes with the write.
type chunkPool struct {
	mu   sync.Mutex
	list [][]byte
	max  int
}

func (p *chunkPool) get() []byte {
	p.mu.Lock()
	if n := len(p.list); n > 0 {
		b := p.list[n-1]
		p.list[n-1] = nil
		p.list = p.list[:n-1]
		p.mu.Unlock()
		return b
	}
	p.mu.Unlock()
	return make([]byte, Block)
}

func (p *chunkPool) put(bufs [][]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range bufs {
		if len(p.list) >= p.max {
			return
		}
		p.list = append(p.list, b[:Block])
	}
}

func chunksFor(n int64) int64 { return (n + Block - 1) / Block }

// chunked is a byte sequence kept in Block-sized chunks; writes past limit
// fail with errFrameTooLarge.
type chunked struct {
	pool  *chunkPool
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
			c.bufs = append(c.bufs, c.pool.get())
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
	c.pool.put(c.bufs)
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
	w        *repackWriter
	ctx      context.Context
	cancel   context.CancelFunc
	jobs     map[int]*parJob
	order    []*parJob
	queue    chan *parJob
	wg       sync.WaitGroup
	pool     *chunkPool
	ahead    *budget // the job allowance, out of reserved
	reserved int64   // held in memBudget until stop
	workers  int
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

// jobCost is the memory a worker job holds until its bytes are written:
// the stored bytes, the decoded bytes, the new frame (at most the file's
// size) and, for a frame whose window is larger than a worker decoder
// keeps, a decoder of its own.
func jobCost(rec fileRecord) int64 {
	cost := (chunksFor(rec.stored) + chunksFor(rec.size)) * Block
	if rec.codec == 1 {
		cost += chunksFor(rec.size) * Block
		if rec.window > workerWindow {
			cost += decoderCost(rec.window)
		}
	}
	return cost
}

// startParallel reserves the write's memory and starts its workers. It
// waits while the budget cannot give even the minimum: the writer, one
// worker and the largest job.
func (w *repackWriter) startParallel() (*parallel, error) {
	ctx, cancel := context.WithCancel(w.ctx)
	p := &parallel{w: w, ctx: ctx, cancel: cancel, jobs: map[int]*parJob{}, queue: make(chan *parJob)}
	var maxJob, window int64
	decodes := false
	for i := range w.nodes {
		rec := w.a.files[w.nodes[i].rec]
		if !w.parallelEligible(i) {
			if rec.codec == 1 {
				window = max64(window, rec.window)
			}
			continue
		}
		j := &parJob{node: i, cost: jobCost(rec), done: make(chan struct{})}
		p.jobs[i] = j
		p.order = append(p.order, j)
		maxJob = max64(maxJob, j.cost)
		decodes = decodes || rec.codec == 1
	}
	// The writer streams the other contents: an encoder, a decoder for the
	// largest window among them, its read buffer.
	writer := encoderCost(w.level) + Block
	if window > 0 {
		writer += decoderCost(window)
	}
	perWorker, want := int64(0), 0
	if len(p.order) > 0 {
		perWorker, want = encoderCost(w.level), parWorkers()
		if decodes {
			perWorker += decoderCost(workerWindow)
		}
	}
	units, ahead, total, err := memBudget.reserveFlexible(w.ctx, writer, perWorker, want, maxJob, max64(readAhead, maxJob))
	if err != nil {
		cancel()
		return nil, err
	}
	p.reserved = total
	if len(p.order) == 0 {
		return p, nil
	}
	p.workers = units
	p.ahead = &budget{limit: ahead}
	p.pool = &chunkPool{max: int(ahead / Block)}
	for k := 0; k < p.workers; k++ {
		p.wg.Add(1)
		go p.worker()
	}
	// The feeder claims job memory in layout order, so the writer's next job
	// always holds its share before any later one. The allowance holds at
	// least the largest job, so it never waits for memory another archive
	// holds.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(p.queue)
		for _, j := range p.order {
			if p.ahead.acquire(ctx, j.cost) != nil {
				return
			}
			select {
			case p.queue <- j:
			case <-ctx.Done():
				p.ahead.release(j.cost)
				return
			}
		}
	}()
	return p, nil
}

func (p *parallel) worker() {
	defer p.wg.Done()
	// The worker's encoder and decoder are its own for the write (their
	// memory is part of the write's reservation) and go with it.
	var enc *zstd.Encoder
	var dec *zstd.Decoder
	defer func() {
		if dec != nil {
			dec.Close()
		}
	}()
	for j := range p.queue {
		if cerr := cpuTokens.acquire(p.ctx, 1); cerr != nil {
			j.err = cerr
		} else {
			if enc == nil {
				enc, j.err = newZstdEncoder(io.Discard, p.w.level, 0)
			}
			if j.err == nil {
				j.err = p.encodeJob(j, enc, &dec)
			}
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
	stored := &chunked{pool: p.pool, limit: rec.stored}
	j.held = append(j.held, stored)
	for off := int64(0); off < rec.stored; {
		k := min64(Block, rec.stored-off)
		c := p.pool.get()
		stored.bufs = append(stored.bufs, c)
		if _, err := w.a.f.ReadAt(c[:k], rec.off+off); err != nil {
			binfmt.Fail("%v", err)
		}
		off += k
		stored.n = off
	}
	raw := stored
	if rec.codec == 1 {
		// A frame with a larger window than the worker's decoder keeps gets
		// a decoder of its own (reserved in the job's cost), closed after.
		d := dec
		var own *zstd.Decoder
		if rec.window > workerWindow {
			d = &own
			defer func() {
				if own != nil {
					own.Close()
				}
			}()
		}
		if *d == nil {
			max := uint64(workerWindow)
			if rec.window > workerWindow {
				max = MaxWindow
			}
			nd, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(max), zstd.WithDecoderConcurrency(1))
			if err != nil {
				binfmt.Fail("%v", err)
			}
			*d = nd
		}
		if err := (*d).Reset(stored.reader()); err != nil {
			(*d).Close()
			*d = nil
			binfmt.Fail("%v", err)
		}
		raw = &chunked{pool: p.pool, limit: rec.size}
		j.held = append(j.held, raw)
		if _, err := io.Copy(raw, *d); errors.Is(err, errFrameTooLarge) {
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
	frame := &chunked{pool: p.pool, limit: max64(limit, 0)}
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
	p.ahead.release(j.cost)
	j.cost = 0
}

// stop ends the workers and returns the write's reservation, also after
// errors and cancellation (the job allowance goes with it).
func (p *parallel) stop() {
	p.cancel()
	p.wg.Wait()
	p.pool = nil
	memBudget.release(p.reserved)
	p.reserved = 0
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
