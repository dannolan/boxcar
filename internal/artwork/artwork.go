// Package artwork decodes cover images and encodes the 256x256 Switch library
// icon. It is the Go port of the Python reference src/artwork.py.
//
// Format scope is reduced relative to Python/Pillow: only PNG, JPEG, GIF, BMP,
// TIFF and WEBP are supported (via the standard library and golang.org/x/image).
// Animated inputs use their first frame, matching Pillow's source.seek(0).
package artwork

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	"image/jpeg"
	"io"
	"math"
	"os"

	"github.com/dannolan/boxcar/internal/binfmt"
	xdraw "golang.org/x/image/draw"

	// Register decoders. image/jpeg, image/png and image/gif register
	// themselves; the x/image packages need blank imports.
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	_ "image/gif"
	_ "image/png"
)

// IconSize is the output icon edge length (Python ICON_SIZE).
const IconSize = 256

// maxPixels mirrors the Python 50 megapixel guard.
const maxPixels = 50_000_000

// jpegQuality mirrors the Python quality=92.
const jpegQuality = 92

// Selection describes one artwork choice (Python ArtworkSelection).
type Selection struct {
	Path string
	// Box is a normalized 0..1 crop box (left, top, right, bottom).
	// nil means the default centered square, exactly like CropModel.
	Box *[4]float64
	// Fit keeps the whole image, shrinking it to fit inside the icon and
	// padding the remainder with opaque white.
	Fit bool
}

// SupportedExtensions lists the file extensions the decoder understands.
// This is the reduced subset of Python IMAGE_EXTENSIONS actually decodable by
// this package; the CLI uses it for file pickers.
func SupportedExtensions() []string {
	return []string{
		".png",
		".jpg", ".jpeg", ".jpe", ".jfif",
		".webp",
		".bmp", ".dib",
		".gif",
		".tif", ".tiff",
	}
}

// LoadImage decodes path into an RGBA image with EXIF orientation applied.
// It rejects images above 50 megapixels before decoding pixels.
func LoadImage(path string) (img image.Image, err error) {
	defer binfmt.Recover(&err)

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return nil, fmt.Errorf("artwork: decode config %s: %w", path, err)
	}
	binfmt.Require(int64(cfg.Width)*int64(cfg.Height) <= maxPixels,
		"Artwork is too large (maximum 50 megapixels).")
	binfmt.Require(cfg.Width > 0 && cfg.Height > 0,
		"Artwork has invalid dimensions.")

	orientation := 1
	if format == "jpeg" {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			orientation = jpegOrientation(f)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	src, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("artwork: decode %s: %w", path, err)
	}
	return applyOrientation(src, orientation), nil
}

// Bytes loads sel and returns its 256x256 JPEG (quality 92) icon bytes.
// Like Python artwork_bytes(None), a nil selection produces no artwork.
func Bytes(sel *Selection) (out []byte, err error) {
	if sel == nil {
		return nil, nil
	}
	defer binfmt.Recover(&err)

	img, err := LoadImage(sel.Path)
	if err != nil {
		return nil, err
	}
	return encodeArtwork(img, sel)
}

// encodeArtwork mirrors the Python encode_artwork.
func encodeArtwork(src image.Image, sel *Selection) ([]byte, error) {
	rgba := toRGBA(src)
	w, h := rgba.Bounds().Dx(), rgba.Bounds().Dy()

	var canvas *image.RGBA
	if sel.Fit {
		// Pillow's thumbnail only shrinks; keep that behaviour. CatmullRom is
		// the documented approximation of Pillow's LANCZOS resampling.
		tw, th := fitSize(w, h, IconSize)
		thumb := image.NewRGBA(image.Rect(0, 0, tw, th))
		xdraw.CatmullRom.Scale(thumb, thumb.Bounds(), rgba, rgba.Bounds(), stddraw.Src, nil)

		canvas = whiteCanvas()
		x := (IconSize - tw) / 2
		y := (IconSize - th) / 2
		xdraw.Draw(canvas, image.Rect(x, y, x+tw, y+th), thumb, image.Point{}, xdraw.Over)
	} else {
		box := [4]float64{}
		if sel.Box != nil {
			box = *sel.Box
		} else {
			box = defaultBox(w, h)
		}
		for _, n := range box {
			binfmt.Require(!math.IsNaN(n) && !math.IsInf(n, 0) && n >= -1e-6 && n <= 1.000001,
				"Crop coordinates must be between 0 and 1.")
		}

		left, top, right, bottom := box[0], box[1], box[2], box[3]
		fw := (right - left) * float64(w)
		fh := (bottom - top) * float64(h)
		binfmt.Require(fw >= 0.999 && fh >= 0.999 && math.Abs(fw-fh) <= 1.1,
			"Choose a square crop at least one source pixel wide, inside the image.")

		rect := cropRect(box, w, h)
		binfmt.Require(rect.Dx() > 0 && rect.Dy() > 0,
			"Choose a square crop at least one source pixel wide, inside the image.")

		// ImageOps.fit: crop the (nearly square) region to a centered square,
		// then resize to the icon size.
		side := rect.Dx()
		if rect.Dy() < side {
			side = rect.Dy()
		}
		cx := rect.Min.X + rect.Dx()/2
		cy := rect.Min.Y + rect.Dy()/2
		square := image.Rect(cx-side/2, cy-side/2, cx-side/2+side, cy-side/2+side).
			Intersect(image.Rect(0, 0, w, h))
		binfmt.Require(square.Dx() > 0 && square.Dy() > 0,
			"Choose a square crop at least one source pixel wide, inside the image.")

		canvas = whiteCanvas()
		xdraw.CatmullRom.Scale(canvas, canvas.Bounds(), rgba, square, stddraw.Over, nil)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("artwork: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// defaultBox ports CropModel(width, height).box(): a centered square covering
// min(width, height) pixels, normalized to 0..1.
func defaultBox(w, h int) [4]float64 {
	side := math.Min(float64(w), float64(h))
	half := side / 2
	cx, cy := float64(w)/2, float64(h)/2
	return [4]float64{
		(cx - half) / float64(w),
		(cy - half) / float64(h),
		(cx + half) / float64(w),
		(cy + half) / float64(h),
	}
}

// fitSize ports Pillow thumbnail: scale down to fit limit x limit, never up.
func fitSize(w, h, limit int) (int, int) {
	if w <= limit && h <= limit {
		return w, h
	}
	scale := math.Min(float64(limit)/float64(w), float64(limit)/float64(h))
	tw := int(math.Round(float64(w) * scale))
	th := int(math.Round(float64(h) * scale))
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}
	if tw > limit {
		tw = limit
	}
	if th > limit {
		th = limit
	}
	return tw, th
}

// cropRect converts the Python float crop box to pixels. Pillow floors the
// left/top and ceils the right/bottom; clamp to the image.
func cropRect(box [4]float64, w, h int) image.Rectangle {
	x0 := int(math.Floor(box[0]*float64(w) + 1e-9))
	y0 := int(math.Floor(box[1]*float64(h) + 1e-9))
	x1 := int(math.Ceil(box[2]*float64(w) - 1e-9))
	y1 := int(math.Ceil(box[3]*float64(h) - 1e-9))
	x0 = clamp(x0, 0, w)
	x1 = clamp(x1, 0, w)
	y0 = clamp(y0, 0, h)
	y1 = clamp(y1, 0, h)
	return image.Rect(x0, y0, x1, y1)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func whiteCanvas() *image.RGBA {
	canvas := image.NewRGBA(image.Rect(0, 0, IconSize, IconSize))
	stddraw.Draw(canvas, canvas.Bounds(),
		&image.Uniform{C: color.RGBA{R: 255, G: 255, B: 255, A: 255}},
		image.Point{}, stddraw.Src)
	return canvas
}

// toRGBA converts any decoded image to an owned *image.RGBA.
func toRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	stddraw.Draw(dst, dst.Bounds(), src, b.Min, stddraw.Src)
	return dst
}
