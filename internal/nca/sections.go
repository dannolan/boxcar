package nca

import (
	"bytes"
	"context"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/region"
)

// NamedSection is one parsed NCA section: its hash kind ("pfs" or "romfs")
// and the sub-region holding the section's data (the body, not the hash list).
type NamedSection struct {
	Kind string // "pfs" | "romfs"
	Data *region.Region
}

// Sections verifies and parses an NCA image (verify_format.nca_sections).
// When verifyPayload is false only the structural checks run; the section
// payload digests are skipped.
func Sections(ctx context.Context, r *region.Region, verifyPayload bool) (titleID uint64, contentType byte, secs []NamedSection, err error) {
	defer binfmt.Recover(&err)

	binfmt.Require(string(readAt(r, 0x200, 4)) == "NCA3", "NCA magic mismatch")
	binfmt.Require(u64(r, 0x208) == uint64(r.Size()), "NCA size mismatch")
	titleID = u64(r, 0x210)
	contentType = u8(r, 0x205)

	for i := 0; i < 4; i++ {
		base := int64(0x240 + 16*i)
		start := u32(r, base)
		end := u32(r, base+4)
		if start == 0 && end == 0 {
			continue
		}

		fsh := sub(r, int64(0x400+512*i), 0x200)
		binfmt.Require(bytes.Equal(digest(ctx, fsh), readAt(r, int64(0x280+32*i), 32)), "section header hash mismatch")
		binfmt.Require(u8(fsh, 4) == 1, "Expected plaintext section")

		body := sub(r, int64(start)*512, (int64(end)-int64(start))*512)

		switch u8(fsh, 3) {
		case 2: // PFS0 / SHA256
			block := u32(fsh, 0x28)
			count := u32(fsh, 0x2c)
			binfmt.Require(count == 2, "PFS block count")
			ho := int64(u64(fsh, 0x30))
			hs := int64(u64(fsh, 0x38))
			do := int64(u64(fsh, 0x40))
			ds := int64(u64(fsh, 0x48))
			hashes := sub(body, ho, hs)
			data := sub(body, do, ds)
			binfmt.Require(block > 0 && hashes.Size() == hs && data.Size() == ds, "PFS section bounds")
			if verifyPayload {
				binfmt.Require(bytes.Equal(digest(ctx, hashes), readAt(fsh, 8, 32)), "PFS master hash")
				digs, derr := data.BlockDigests(ctx, int64(block), false)
				binfmt.Require(derr == nil, "PFS block digests: %v", derr)
				for k, actual := range digs {
					binfmt.Require(bytes.Equal(actual, readAt(hashes, int64(k)*32, 32)), "PFS block hash")
				}
			}
			secs = append(secs, NamedSection{Kind: "pfs", Data: data})

		case 3: // RomFS / IVFC
			binfmt.Require(string(readAt(fsh, 8, 4)) == "IVFC", "IVFC magic")
			count := int(u32(fsh, 0x14)) - 1
			binfmt.Require(count == 6, "IVFC level count")

			expected := sub(fsh, 0xc8, 32)
			var data *region.Region
			for j := 0; j < count; j++ {
				off := int64(u64(fsh, int64(0x18+24*j)))
				size := int64(u64(fsh, int64(0x20+24*j)))
				order := u32(fsh, int64(0x28+24*j))
				block := int64(1) << order
				data = sub(body, off, size)
				if verifyPayload {
					digs, derr := data.BlockDigests(ctx, block, true)
					binfmt.Require(derr == nil, "IVFC block digests: %v", derr)
					for k, actual := range digs {
						binfmt.Require(bytes.Equal(actual, readAt(expected, int64(k)*32, 32)), "IVFC block hash %d.%d", j, k)
					}
				}
				expected = data
			}
			secs = append(secs, NamedSection{Kind: "romfs", Data: data})

		default:
			binfmt.Fail("Unsupported hash")
		}
	}

	return titleID, contentType, secs, nil
}

// u8/u32/u64 read a little-endian integer at off, panicking on error so the
// validation reads match the Python asserts 1:1.
func u8(r *region.Region, off int64) byte {
	v, err := r.Byte(off)
	binfmt.Require(err == nil, "read byte at %#x: %v", off, err)
	return v
}

func u32(r *region.Region, off int64) uint32 {
	v, err := r.U32(off)
	binfmt.Require(err == nil, "read u32 at %#x: %v", off, err)
	return v
}

func u64(r *region.Region, off int64) uint64 {
	v, err := r.U64(off)
	binfmt.Require(err == nil, "read u64 at %#x: %v", off, err)
	return v
}

// sub returns the [off, off+size) sub-region, panicking when out of range.
func sub(r *region.Region, off, size int64) *region.Region {
	s, err := r.Sub(off, size)
	binfmt.Require(err == nil, "sub-range [%#x,%#x): %v", off, off+size, err)
	return s
}

// readAt reads n bytes at off, panicking on error.
func readAt(r *region.Region, off, n int64) []byte {
	b := make([]byte, n)
	if err := r.ReadAt(off, b); err != nil {
		binfmt.Fail("read %d bytes at %#x: %v", n, off, err)
	}
	return b
}

// digest returns the SHA-256 of the region, panicking on error.
func digest(ctx context.Context, r *region.Region) []byte {
	d, err := r.Digest(ctx)
	binfmt.Require(err == nil, "digest: %v", err)
	return d
}
