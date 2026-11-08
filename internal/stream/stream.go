// Package stream is the Go port of the Python reference Blob type: a lazy,
// multi-pass composite byte stream assembled from literal bytes, zero padding
// and on-disk files. Streams are immutable once built; every pass re-opens
// file parts, exactly like the Python implementation re-iterates chunks().
package stream

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/dannolan/boxcar/internal/binfmt"
)

// Part is one component of a Stream: literal bytes, File of the given size,
// or Padding zero bytes. Construct via Bytes, File, Padding. Zero-value Size
// with empty Data and Path is a zero-length part.
type Part struct {
	Data []byte // literal bytes when Path == ""
	Path string // file source when non-empty
	Size int64
}

// Bytes returns a literal part.
func Bytes(b []byte) Part { return Part{Data: b, Size: int64(len(b))} }

// Padding returns n zero bytes. Negative panics like Python ('Negative padding').
func Padding(n int64) Part {
	if n < 0 {
		binfmt.Fail("Negative padding")
	}
	return Part{Size: n}
}

// File returns a file-backed part; size is captured at construction time
// (Python captures st_size when the Blob is built).
func File(path string, size int64) Part { return Part{Path: path, Size: size} }

// WriteOpts configure a write or digest pass.
type WriteOpts struct {
	Ctx      context.Context    // cancellation; nil means context.Background
	Progress binfmt.Progress    // reports done 0..100 over the whole stream
	Hashes   map[string]*[]byte // capture SHA-256 of file parts by absolute path
	// Block is the chunk size; default 4 MiB.
	Block int64
}

func (o *WriteOpts) ctx() context.Context {
	if o == nil || o.Ctx == nil {
		return context.Background()
	}
	return o.Ctx
}

func (o *WriteOpts) block() int64 {
	if o == nil || o.Block <= 0 {
		return 4 << 20
	}
	return o.Block
}

// Stream is an immutable composite of parts.
type Stream struct {
	parts []Part
	size  int64
}

// New builds a Stream and flattens nested streams (Python Blob(*parts)).
func New(parts ...any) (*Stream, error) {
	s := &Stream{}
	var add func(p Part) error
	add = func(p Part) error {
		s.parts = append(s.parts, p)
		s.size += p.Size
		return nil
	}
	var walk func(v any) error
	walk = func(v any) error {
		switch t := v.(type) {
		case *Stream:
			for _, p := range t.parts {
				if err := walk(p); err != nil {
					return err
				}
			}
		case Part:
			return add(t)
		case []Part:
			for _, p := range t {
				if err := walk(p); err != nil {
					return err
				}
			}
		case []byte:
			return add(Bytes(t))
		case string:
			return add(File(t, -1))
		default:
			return fmt.Errorf("stream.New: unsupported part %T", v)
		}
		return nil
	}
	for _, v := range parts {
		if err := walk(v); err != nil {
			return nil, err
		}
	}
	// Resolve implicit file sizes via stat (mirrors Blob(Path) behavior).
	for i := range s.parts {
		p := &s.parts[i]
		if p.Size < 0 {
			st, err := os.Stat(p.Path)
			if err != nil {
				return nil, err
			}
			p.Size = st.Size()
		}
	}
	s.size = 0
	for _, p := range s.parts {
		s.size += p.Size
	}
	return s, nil
}

// Size returns the total byte length of the stream.
func (s *Stream) Size() int64 { return s.size }

// Pad returns a new Stream extended with zero bytes up to a multiple of a
// (Python Blob.pad).
func (s *Stream) Pad(a int64) *Stream {
	extra := binfmt.Align(s.size, a) - s.size
	out, _ := New(s, Padding(extra))
	return out
}

// WriteTo streams the whole composite into w, checking cancellation every
// block and reporting progress. File parts whose path appears in opts.Hashes
// get their SHA-256 captured into the map (Python capture_sources).
func (s *Stream) WriteTo(w io.Writer, opts *WriteOpts) (err error) {
	defer binfmt.Recover(&err)
	return s.pass(opts, func(chunk []byte) error {
		_, werr := w.Write(chunk)
		return werr
	})
}

// ReadHead returns the first n bytes (Python Blob.read_head). n may exceed
// the stream size.
func (s *Stream) ReadHead(n int64, opts *WriteOpts) (head []byte, err error) {
	defer binfmt.Recover(&err)
	if n > s.size {
		n = s.size
	}
	var buf []byte
	err = s.pass(opts, func(chunk []byte) error {
		if int64(len(buf)) < n {
			take := n - int64(len(buf))
			if take > int64(len(chunk)) {
				take = int64(len(chunk))
			}
			buf = append(buf, chunk[:take]...)
		}
		return nil
	})
	return buf, err
}

// SHA256 digests the whole stream (Python Blob.digest) with progress.
func (s *Stream) SHA256(opts *WriteOpts) (sum []byte, err error) {
	defer binfmt.Recover(&err)
	h := sha256.New()
	var done int64
	err = s.pass(opts, func(chunk []byte) error {
		h.Write(chunk)
		done += int64(len(chunk))
		if opts != nil && opts.Progress != nil && s.size > 0 {
			opts.Progress("", 100*float64(done)/float64(s.size))
		}
		return nil
	})
	return h.Sum(nil), err
}

type hashTracker struct {
	h hash256
}

type hash256 interface {
	io.Writer
	Sum(b []byte) []byte
}

// pass iterates every part in block-sized chunks, keeping a partial block
// buffered across part boundaries exactly like Blob.chunks. File parts whose
// path is a key of opts.Hashes are hashed as their bytes are read (Python
// source_hash.update(b)), and the digest stored under that key once the part
// is fully consumed.
func (s *Stream) pass(opts *WriteOpts, emit func(chunk []byte) error) error {
	ctx := opts.ctx()
	block := opts.block()
	var pending []byte
	var active *hashTracker
	var activePath string
	flush := func(force bool) error {
		binfmt.Check(ctx)
		for int64(len(pending)) >= block || (force && len(pending) > 0) {
			n := block
			if int64(len(pending)) < n {
				n = int64(len(pending))
			}
			chunk := pending[:n]
			if err := emit(chunk); err != nil {
				return err
			}
			pending = pending[n:]
		}
		return nil
	}
	finishTracker := func() {
		if active != nil {
			if opts != nil && opts.Hashes != nil {
				sum := active.h.Sum(nil)
				if dst, ok := opts.Hashes[activePath]; ok && dst != nil {
					*dst = sum
				}
			}
			active = nil
			activePath = ""
		}
	}
	for i := range s.parts {
		p := &s.parts[i]
		var src *hashTracker
		if p.Path != "" && opts != nil && opts.Hashes != nil {
			if _, want := opts.Hashes[p.Path]; want {
				src = &hashTracker{h: sha256.New()}
				active, activePath = src, p.Path
			}
		}
		if p.Path != "" {
			f, err := os.Open(p.Path)
			if err != nil {
				return err
			}
			remaining := p.Size
			buf := make([]byte, 1<<20)
			for remaining > 0 {
				n := int64(len(buf))
				if remaining < n {
					n = remaining
				}
				read, rerr := io.ReadFull(f, buf[:n])
				if rerr != nil && !(rerr == io.EOF && int64(read) == n) {
					f.Close()
					return fmt.Errorf("Source changed or short read: %w", rerr)
				}
				if int64(read) != n {
					f.Close()
					return fmt.Errorf("Source changed or short read")
				}
				pending = append(pending, buf[:read]...)
				remaining -= int64(read)
				if src != nil {
					src.h.Write(buf[:read])
				}
				if err := flush(false); err != nil {
					f.Close()
					return err
				}
			}
			f.Close()
			finishTracker()
		} else {
			data := p.Data
			if len(data) == 0 && p.Size > 0 {
				// zero padding in chunks
				zero := make([]byte, 1<<20)
				remaining := p.Size
				for remaining > 0 {
					n := int64(len(zero))
					if remaining < n {
						n = remaining
					}
					pending = append(pending, zero[:n]...)
					remaining -= n
					if err := flush(false); err != nil {
						return err
					}
				}
			} else {
				pending = append(pending, data...)
				if err := flush(false); err != nil {
					return err
				}
			}
		}
	}
	if err := flush(true); err != nil {
		return err
	}
	finishTracker()
	return nil
}
