// Package nca builds and verifies Nintendo Content Archive (NCA) containers
// whose sections use conventional plaintext PFS0/SHA256 and RomFS/IVFC hashing.
// It is a 1:1 port of container_format.pfs_section/romfs_section/nca and
// verify_format.nca_sections from the Python reference implementation.
package nca

import (
	"context"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/stream"
)

// Section is one NCA section: its 512-byte filesystem header (FSH) and its
// already-padded body.
type Section struct {
	FSH  []byte
	Body *stream.Stream
}

// blockHashWriter consumes a byte stream and accumulates SHA-256 digests of
// fixed-size blocks, buffering the trailing partial block until Finish. When
// pad is set the final partial block is zero-padded before hashing (romfs
// levels); otherwise it is hashed as-is (pfs body) — exactly like the Python
// chunk loops in pfs_section and romfs_section.
type blockHashWriter struct {
	block    int
	pad      bool
	pending  []byte
	hashes   []byte
	progress binfmt.Progress
	done     int64
	total    int64
}

func (w *blockHashWriter) Write(p []byte) (int, error) {
	w.done += int64(len(p))
	if w.progress != nil && w.total > 0 {
		w.progress("", 100*float64(w.done)/float64(w.total))
	}
	w.pending = append(w.pending, p...)
	for len(w.pending) >= w.block {
		w.hashes = append(w.hashes, binfmt.SHA256Sum(w.pending[:w.block])...)
		w.pending = w.pending[w.block:]
	}
	return len(p), nil
}

func (w *blockHashWriter) finish() {
	if len(w.pending) == 0 {
		return
	}
	if w.pad {
		b := make([]byte, w.block)
		copy(b, w.pending)
		w.hashes = append(w.hashes, binfmt.SHA256Sum(b)...)
	} else {
		w.hashes = append(w.hashes, binfmt.SHA256Sum(w.pending)...)
	}
	w.pending = nil
}

// PFSSection wraps body in a PFS0/SHA256 section. It returns the 0x200-byte
// filesystem header (fsh) and the padded section body: the block-hash list,
// zero padding to a 0x200 boundary, then the body itself, all padded to 0x200.
// The block size doubles while body size exceeds block*block/32.
func PFSSection(ctx context.Context, body *stream.Stream) (fsh []byte, paddedBody *stream.Stream, err error) {
	defer binfmt.Recover(&err)

	block := int64(0x10000)
	for body.Size() > block*block/32 {
		block *= 2
	}

	w := &blockHashWriter{block: int(block)}
	if werr := body.WriteTo(w, &stream.WriteOpts{Ctx: ctx}); werr != nil {
		return nil, nil, werr
	}
	w.finish()
	hashes := w.hashes

	dataOff := binfmt.Align(int64(len(hashes)), 0x200)

	header := make([]byte, 0x200)
	binfmt.PutU16(header, 0, 2)
	header[2] = 1 // SHA256
	header[3] = 2 // block count
	header[4] = 1 // reserved
	copy(header[8:40], binfmt.SHA256Sum(hashes))
	binfmt.PutU32(header, 0x28, uint32(block))
	binfmt.PutU32(header, 0x2c, 2)
	binfmt.PutU64(header, 0x30, 0)
	binfmt.PutU64(header, 0x38, uint64(len(hashes)))
	binfmt.PutU64(header, 0x40, uint64(dataOff))
	binfmt.PutU64(header, 0x48, uint64(body.Size()))

	paddedBody, err = stream.New(hashes, stream.Padding(dataOff-int64(len(hashes))), body)
	if err != nil {
		return nil, nil, err
	}
	return header, paddedBody.Pad(0x200), nil
}

// RomfsSection wraps body in a RomFS/IVFC section with a 6-level hash tree
// (block 0x4000). It returns the 0x200-byte filesystem header (fsh) and the
// padded section body: levels ordered L5..L0, each padded to the block size.
func RomfsSection(ctx context.Context, body *stream.Stream, progress binfmt.Progress) (fsh []byte, paddedBody *stream.Stream, err error) {
	defer binfmt.Recover(&err)

	const block = 0x4000

	levels := []*stream.Stream{body}
	for layer := 0; layer < 5; layer++ {
		w := &blockHashWriter{block: block, pad: true}
		if layer == 0 && progress != nil {
			w.progress = progress
			w.total = levels[0].Size()
		}
		if werr := levels[0].WriteTo(w, &stream.WriteOpts{Ctx: ctx}); werr != nil {
			return nil, nil, werr
		}
		w.finish()
		hashes, err := stream.New(w.hashes)
		if err != nil {
			return nil, nil, err
		}
		levels = append([]*stream.Stream{hashes}, levels...)
	}

	head, err := levels[0].ReadHead(block, &stream.WriteOpts{Ctx: ctx})
	if err != nil {
		return nil, nil, err
	}
	if int64(len(head)) < block {
		b := make([]byte, block)
		copy(b, head)
		head = b
	}
	master := binfmt.SHA256Sum(head)

	header := make([]byte, 0x200)
	binfmt.PutU16(header, 0, 2)
	header[2] = 0 // hash type
	header[3] = 3 // block count
	header[4] = 1 // reserved
	copy(header[8:12], "IVFC")
	binfmt.PutU32(header, 0x0c, 0x20000)
	binfmt.PutU32(header, 0x10, 32)
	binfmt.PutU32(header, 0x14, 7)

	parts := []any{}
	off := int64(0)
	for i, level := range levels {
		binfmt.PutU64(header, 0x18+24*i, uint64(off))
		binfmt.PutU64(header, 0x20+24*i, uint64(level.Size()))
		binfmt.PutU32(header, 0x28+24*i, 14) // order -> block 1<<14
		binfmt.PutU32(header, 0x2c+24*i, 0)
		padded := level.Pad(block)
		parts = append(parts, padded)
		off += padded.Size()
	}
	copy(header[0xc8:0xe8], master)

	paddedBody, err = stream.New(parts...)
	if err != nil {
		return nil, nil, err
	}
	return header, paddedBody, nil
}

// NCA assembles a plaintext NCA3 container from its sections. distribution is
// always 0. It returns the header (0xc00 bytes, with each section's 512-byte
// FSH embedded at 0x400+512*i) followed by the section bodies in order.
func NCA(sections []Section, titleID uint64, contentType byte) (out *stream.Stream, err error) {
	defer binfmt.Recover(&err)
	binfmt.Require(len(sections) <= 4, "NCA supports at most four sections.")

	header := make([]byte, 0xc00)
	copy(header[0x200:0x204], "NCA3")
	header[0x204] = 0 // distribution
	header[0x205] = contentType
	header[0x206] = 1
	header[0x207] = 0
	binfmt.PutU64(header, 0x210, titleID)
	binfmt.PutU32(header, 0x21c, 0x000c1100)

	off := int64(0xc00)
	parts := []any{header}
	for i, sec := range sections {
		binfmt.Require(len(sec.FSH) == 0x200, "section header must be 512 bytes")
		body := sec.Body.Pad(0x200)
		if (off+body.Size())/512 > 0xffffffff {
			binfmt.Fail("An NCA section exceeds the format limit (32-bit sector offsets, approximately 2 TiB per NCA).")
		}
		binfmt.PutU32(header, 0x240+16*i, uint32(off/512))
		binfmt.PutU32(header, 0x244+16*i, uint32((off+body.Size())/512))
		binfmt.PutU32(header, 0x248+16*i, 0)
		binfmt.PutU32(header, 0x24c+16*i, 0)
		copy(header[0x280+32*i:0x2a0+32*i], binfmt.SHA256Sum(sec.FSH))
		copy(header[0x400+512*i:0x600+512*i], sec.FSH)
		parts = append(parts, body)
		off += body.Size()
	}
	binfmt.PutU64(header, 0x208, uint64(off))

	return stream.New(parts...)
}
