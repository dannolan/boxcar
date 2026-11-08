package nup

import (
	"encoding/binary"
	"errors"
)

// zstdFrameParams mirrors Python zstandard.get_frame_parameters +
// frame_header_size for the frame header walk in Archive parsing.
type zstdFrameParams struct {
	contentSize int64 // -1 when not stored
	dictID      uint32
	windowSize  int64
	hasChecksum bool
	headerSize  int
}

var zstdMagic = [4]byte{0x28, 0xb5, 0x2f, 0xfd}

// parseZstdFrameHead decodes a Zstandard frame header (RFC 8878 §3.1.1.1.3).
// It mirrors zstandard's get_frame_parameters: for single-segment frames the
// window size equals the content size; otherwise it is derived from the window
// descriptor. contentSize is -1 when the frame does not store a content size.
func parseZstdFrameHead(head []byte) (zstdFrameParams, error) {
	var p zstdFrameParams
	p.contentSize = -1
	if len(head) < 5 {
		return p, errors.New("truncated Zstandard frame header")
	}
	if [4]byte(head[:4]) != zstdMagic {
		return p, errors.New("not a Zstandard frame")
	}
	fhd := head[4]
	if fhd&0x18 != 0 { // unused bit 4 and reserved bit 3 must be zero
		return p, errors.New("invalid Zstandard frame header descriptor")
	}
	single := fhd&0x20 != 0
	p.hasChecksum = fhd&0x04 != 0
	didFlag := fhd & 0x03
	fcsFlag := fhd >> 6

	pos := 5
	if !single {
		if pos >= len(head) {
			return p, errors.New("truncated Zstandard window descriptor")
		}
		wd := head[pos]
		pos++
		windowLog := 10 + int(wd>>3)
		windowBase := int64(1) << uint(windowLog)
		p.windowSize = windowBase + (windowBase/8)*int64(wd&7)
	}

	if didFlag != 0 {
		didSize := int(didFlag)
		if didSize == 3 {
			didSize = 4
		}
		if pos+didSize > len(head) {
			return p, errors.New("truncated Zstandard dictionary ID")
		}
		switch didSize {
		case 1:
			p.dictID = uint32(head[pos])
		case 2:
			p.dictID = uint32(head[pos]) | uint32(head[pos+1])<<8
		case 4:
			p.dictID = binary.LittleEndian.Uint32(head[pos:])
		}
		pos += didSize
	}

	fcsSize := 0
	switch fcsFlag {
	case 0:
		if single {
			fcsSize = 1
		}
	default:
		fcsSize = 1 << fcsFlag // 2, 4 or 8
	}
	if fcsSize > 0 {
		if pos+fcsSize > len(head) {
			return p, errors.New("truncated Zstandard content size")
		}
		switch fcsSize {
		case 1:
			p.contentSize = int64(head[pos])
		case 2:
			p.contentSize = int64(binary.LittleEndian.Uint16(head[pos:])) + 256
		case 4:
			p.contentSize = int64(binary.LittleEndian.Uint32(head[pos:]))
		case 8:
			p.contentSize = int64(binary.LittleEndian.Uint64(head[pos:]))
		}
		pos += fcsSize
	}

	if single && p.contentSize >= 0 {
		p.windowSize = p.contentSize
	}
	p.headerSize = pos
	return p, nil
}
