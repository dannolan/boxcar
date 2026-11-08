// Package binfmt holds shared low-level helpers for the binary format code:
// little-endian struct packing, alignment, SHA-256, and the Require/Recover
// error style that mirrors the Python reference implementation.
package binfmt

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
)

// Failure is a panic payload converted back into an error by Recover.
// The format packages use panic/Recover so ports of Python's
// require(condition, message) stay 1:1 readable.
type Failure struct{ Err error }

// Recover converts a Failure panic into *err. defer binfmt.Recover(&err) at
// every exported operation boundary. Non-Failure panics propagate.
func Recover(err *error) {
	if r := recover(); r != nil {
		if f, ok := r.(Failure); ok {
			*err = f.Err
		} else {
			panic(r)
		}
	}
}

// Require panics with Failure unless cond holds. Mirrors Python require().
func Require(cond bool, format string, args ...any) {
	if !cond {
		panic(Failure{fmt.Errorf(format, args...)})
	}
}

// Fail panics with Failure unconditionally.
func Fail(format string, args ...any) { panic(Failure{fmt.Errorf(format, args...)}) }

// Check panics with Failure{context.Canceled} when ctx is done. Mirrors the
// Python fmt.checkpoint() cancellation points.
func Check(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		panic(Failure{err})
	}
}

// Progress reports a named operation; done is 0..100.
type Progress func(label string, done float64)

// Align rounds n up to a multiple of a. Mirrors Python align().
func Align(n, a int64) int64 { return (n + a - 1) / a * a }

// U16At/U32At/U64At read little-endian values at an offset.
func U16At(b []byte, off int) uint16     { return binary.LittleEndian.Uint16(b[off:]) }
func U32At(b []byte, off int) uint32     { return binary.LittleEndian.Uint32(b[off:]) }
func U64At(b []byte, off int) uint64     { return binary.LittleEndian.Uint64(b[off:]) }
func PutU16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func PutU32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func PutU64(b []byte, off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }

// AppendU16/U32/U64/I32/I64 append little-endian values to a slice.
func AppendU16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }
func AppendU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }
func AppendU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }
func AppendI32(b []byte, v int32) []byte  { return binary.LittleEndian.AppendUint32(b, uint32(v)) }
func AppendI64(b []byte, v int64) []byte  { return binary.LittleEndian.AppendUint64(b, uint64(v)) }

// SHA256Sum hashes b.
func SHA256Sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// SHA256Copy is a reusable streaming SHA-256.
type SHA256Copy struct{ h hash.Hash }

// NewSHA256 returns a fresh streaming SHA-256.
func NewSHA256() *SHA256Copy { return &SHA256Copy{h: sha256.New()} }

// Write implements io.Writer.
func (s *SHA256Copy) Write(p []byte) (int, error) { return s.h.Write(p) }

// Sum returns the digest; the stream keeps hashing afterwards.
func (s *SHA256Copy) Sum() []byte { return s.h.Sum(nil) }

// CopyWithHash streams src into w, updating h, checking ctx every block and
// reporting progress. Mirrors the Python write+hash loops.
func CopyWithHash(ctx context.Context, w io.Writer, src io.Reader, h hashWriter, block int64,
	progress func(done int64)) (int64, []byte, error) {
	if block <= 0 {
		block = 4 << 20
	}
	buf := make([]byte, block)
	var done int64
	for {
		Check(ctx)
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return done, nil, werr
			}
			if h != nil {
				h.Write(buf[:n])
			}
			done += int64(n)
			if progress != nil {
				progress(done)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return done, nil, rerr
		}
	}
	var sum []byte
	if h != nil {
		sum = h.Sum()
	}
	return done, sum, nil
}

type hashWriter interface {
	io.Writer
	Sum() []byte
}
