package artwork

import (
	"encoding/binary"
	"image"
	"io"
)

// jpegOrientation scans JPEG marker segments for the EXIF APP1 block and
// returns the orientation tag (0x0112) value, or 1 when absent/unparseable.
// This is a minimal stdlib-only replacement for Pillow's EXIF handling.
func jpegOrientation(r io.ReadSeeker) int {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 1
	}
	var soi [2]byte
	if _, err := io.ReadFull(r, soi[:]); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return 1
	}

	var b [2]byte
	for {
		// Every marker starts with 0xFF; tolerate fill bytes.
		c, err := readByte(r)
		if err != nil || c != 0xFF {
			return 1
		}
		for {
			c, err = readByte(r)
			if err != nil {
				return 1
			}
			if c != 0xFF {
				break
			}
		}
		marker := c

		switch {
		case marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			continue // standalone markers, no payload
		case marker == 0xD9 || marker == 0xDA:
			return 1 // EOI / start of scan: no EXIF before image data
		}

		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(b[:]))
		if segLen < 2 {
			return 1
		}
		n := segLen - 2

		if marker == 0xE1 && n >= 6 {
			payload := make([]byte, n)
			if _, err := io.ReadFull(r, payload); err != nil {
				return 1
			}
			if string(payload[:6]) == "Exif\x00\x00" {
				return tiffOrientation(payload[6:])
			}
			continue
		}
		if _, err := r.Seek(int64(n), io.SeekCurrent); err != nil {
			return 1
		}
	}
}

func readByte(r io.Reader) (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r, b[:])
	return b[0], err
}

// tiffOrientation reads IFD0 of a TIFF/EXIF block and returns tag 0x0112.
func tiffOrientation(b []byte) int {
	if len(b) < 8 {
		return 1
	}
	little := false
	switch string(b[:2]) {
	case "II":
		little = true
	case "MM":
	default:
		return 1
	}
	u16 := func(p []byte) uint16 {
		if little {
			return binary.LittleEndian.Uint16(p)
		}
		return binary.BigEndian.Uint16(p)
	}
	u32 := func(p []byte) uint32 {
		if little {
			return binary.LittleEndian.Uint32(p)
		}
		return binary.BigEndian.Uint32(p)
	}

	if u16(b[2:]) != 42 {
		return 1
	}
	off := int(u32(b[4:]))
	if off < 8 || off+2 > len(b) {
		return 1
	}
	count := int(u16(b[off:]))
	off += 2
	if count < 0 || count > (len(b)-off)/12 {
		return 1
	}
	for i := 0; i < count; i++ {
		e := off + i*12
		if u16(b[e:]) != 0x0112 {
			continue
		}
		typ := u16(b[e+2:])
		cnt := u32(b[e+4:])
		if typ != 3 || cnt < 1 { // SHORT
			return 1
		}
		v := u16(b[e+8:])
		if v >= 1 && v <= 8 {
			return int(v)
		}
		return 1
	}
	return 1
}

// applyOrientation returns an owned RGBA copy of src transformed per the EXIF
// orientation value (1..8). Values outside that range are treated as 1.
func applyOrientation(src image.Image, orientation int) *image.RGBA {
	rgba := toRGBA(src)
	if orientation < 2 || orientation > 8 {
		return rgba
	}

	w := rgba.Bounds().Dx()
	h := rgba.Bounds().Dy()
	outW, outH := w, h
	if orientation >= 5 {
		outW, outH = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, outW, outH))

	for y := 0; y < outH; y++ {
		for x := 0; x < outW; x++ {
			var sx, sy int
			switch orientation {
			case 2: // mirror horizontal
				sx, sy = w-1-x, y
			case 3: // rotate 180
				sx, sy = w-1-x, h-1-y
			case 4: // mirror vertical
				sx, sy = x, h-1-y
			case 5: // transpose
				sx, sy = y, x
			case 6: // rotate 90 CW
				sx, sy = y, h-1-x
			case 7: // transverse
				sx, sy = w-1-y, h-1-x
			case 8: // rotate 270 CW
				sx, sy = w-1-y, x
			}
			si := sy*rgba.Stride + sx*4
			di := y*out.Stride + x*4
			copy(out.Pix[di:di+4], rgba.Pix[si:si+4])
		}
	}
	return out
}
