// Package region provides a bounded, random-access read window over a file,
// the Go port of the Python verify_format.Region mmap range. Unlike Python it
// uses pread (ReadAt) rather than mmap, so it is portable and memory-safe.
package region

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/dannolan/boxcar/internal/binfmt"
)

// Region is a bounded range [off, off+size) of an open file.
type Region struct {
	f    *os.File
	off  int64
	size int64
}

// New wraps the whole file. The caller owns f and must keep it open while any
// Region derived from it is in use.
func New(f *os.File) (*Region, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return &Region{f: f, off: 0, size: st.Size()}, nil
}

// Size returns the region length.
func (r *Region) Size() int64 { return r.size }

// Sub returns the sub-region [off, off+size) relative to r. Mirrors Python
// Region.__getitem__ slicing and its range validation.
func (r *Region) Sub(off, size int64) (*Region, error) {
	if off < 0 || size < 0 || off+size > r.size {
		return nil, fmt.Errorf("Range is outside the container")
	}
	return &Region{f: r.f, off: r.off + off, size: size}, nil
}

// Slice reads the whole region into memory. Only for small regions.
func (r *Region) Slice() ([]byte, error) {
	b := make([]byte, r.size)
	if _, err := r.f.ReadAt(b, r.off); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

// Read reads size bytes at rel into dst (which must be >= size).
func (r *Region) ReadAt(rel int64, dst []byte) error {
	if rel < 0 || int64(len(dst)) > r.size-rel {
		return fmt.Errorf("Range is outside the container")
	}
	_, err := r.f.ReadAt(dst, r.off+rel)
	if err == io.EOF && len(dst) > 0 {
		// short read
		return io.ErrUnexpectedEOF
	}
	return err
}

// Byte returns one byte at rel (Python Region.__getitem__).
func (r *Region) Byte(rel int64) (byte, error) {
	var b [1]byte
	if err := r.ReadAt(rel, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// U16/U32/U64 read little-endian integers at rel.
func (r *Region) U16(rel int64) (uint16, error) {
	var b [2]byte
	if err := r.ReadAt(rel, b[:]); err != nil {
		return 0, err
	}
	return binfmt.U16At(b[:], 0), nil
}

func (r *Region) U32(rel int64) (uint32, error) {
	var b [4]byte
	if err := r.ReadAt(rel, b[:]); err != nil {
		return 0, err
	}
	return binfmt.U32At(b[:], 0), nil
}

func (r *Region) U64(rel int64) (uint64, error) {
	var b [8]byte
	if err := r.ReadAt(rel, b[:]); err != nil {
		return 0, err
	}
	return binfmt.U64At(b[:], 0), nil
}

// Digest hashes the region contents, checking ctx every 4 MiB block
// (Python verify_format.digest on a Region).
func (r *Region) Digest(ctx context.Context) ([]byte, error) {
	h := sha256.New()
	buf := make([]byte, 4<<20)
	for off := int64(0); off < r.size; off += int64(len(buf)) {
		binfmt.Check(ctx)
		n := int64(len(buf))
		if r.size-off < n {
			n = r.size - off
		}
		if err := r.ReadAt(off, buf[:n]); err != nil {
			return nil, err
		}
		h.Write(buf[:n])
	}
	return h.Sum(nil), nil
}

// BlockDigests returns SHA-256 of each block_size chunk; when padded, the
// final partial chunk is zero-padded to block_size before hashing
// (Python verify_format.block_digests). Checks ctx every 128 blocks.
func (r *Region) BlockDigests(ctx context.Context, blockSize int64, padded bool) ([][]byte, error) {
	if blockSize <= 0 {
		return nil, fmt.Errorf("Invalid block size")
	}
	var out [][]byte
	buf := make([]byte, blockSize)
	for k, off := 0, int64(0); off < r.size; k, off = k+1, off+blockSize {
		if k%128 == 0 {
			binfmt.Check(ctx)
		}
		n := blockSize
		if r.size-off < n {
			n = r.size - off
		}
		if padded {
			// Zero the tail so stale bytes from earlier full blocks cannot
			// leak into the final partial block's padding.
			for i := n; i < blockSize; i++ {
				buf[i] = 0
			}
		}
		if err := r.ReadAt(off, buf[:n]); err != nil {
			return nil, err
		}
		if padded && n < blockSize {
			h := sha256.Sum256(buf)
			out = append(out, h[:])
		} else {
			h := sha256.Sum256(buf[:n])
			out = append(out, h[:])
		}
	}
	return out, nil
}

// FileDigest hashes an on-disk file in 4 MiB chunks (Python file_digest).
func FileDigest(ctx context.Context, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 4<<20)
	for {
		binfmt.Check(ctx)
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	return h.Sum(nil), nil
}
