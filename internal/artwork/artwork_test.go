package artwork

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

// 40x30 solid (90,140,200) lossless WEBP generated with cwebp.
// golang.org/x/image has no WEBP encoder, so the fixture is embedded.
const testWebP = "UklGRh4AAABXRUJQVlA4TBEAAAAvJ0AHAAdQxmoVuf+BiOh/AAA="

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeBMP(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := bmp.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeTIFF(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := tiff.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func decodeIcon(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not decodable JPEG: %v", err)
	}
	if img.Bounds().Dx() != 256 || img.Bounds().Dy() != 256 {
		t.Fatalf("output size = %v, want 256x256", img.Bounds())
	}
	return img
}

// at returns a pixel relative to the image's top-left corner.
func at(img image.Image, x, y int) color.RGBA {
	r, g, b, a := img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y).RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

func near(c color.RGBA, want color.RGBA, tol int) bool {
	d := func(a, b uint8) int {
		if a > b {
			return int(a - b)
		}
		return int(b - a)
	}
	return d(c.R, want.R) <= tol && d(c.G, want.G) <= tol && d(c.B, want.B) <= tol
}

func TestDefaultCenteredSquare(t *testing.T) {
	green := color.RGBA{0, 200, 0, 255}
	blue := color.RGBA{0, 0, 240, 255}
	img := image.NewRGBA(image.Rect(0, 0, 300, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 300; x++ {
			c := blue
			if x >= 50 && x < 250 {
				c = green
			}
			img.SetRGBA(x, y, c)
		}
	}

	out, err := Bytes(&Selection{Path: writeTemp(t, "center.png", encodePNG(t, img))})
	if err != nil {
		t.Fatal(err)
	}
	icon := decodeIcon(t, out)
	if got := at(icon, 128, 128); !near(got, green, 25) {
		t.Fatalf("center pixel = %v, want green %v (default crop must be the 200x200 center)", got, green)
	}
	for _, p := range [][2]int{{4, 128}, {128, 4}, {251, 128}, {128, 251}} {
		if got := at(icon, p[0], p[1]); !near(got, green, 30) {
			t.Fatalf("pixel %v = %v, want green (blue side bands must be cropped away)", p, got)
		}
	}

	box := defaultBox(300, 200)
	want := [4]float64{1.0 / 6, 0, 5.0 / 6, 1}
	for i := range want {
		if math.Abs(box[i]-want[i]) > 1e-12 {
			t.Fatalf("defaultBox(300,200) = %v, want %v", box, want)
		}
	}
}

func TestExplicitBoxCrop(t *testing.T) {
	red := color.RGBA{220, 20, 20, 255}
	green := color.RGBA{20, 200, 20, 255}
	blue := color.RGBA{20, 20, 220, 255}
	yellow := color.RGBA{230, 220, 20, 255}

	img := image.NewRGBA(image.Rect(0, 0, 300, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 300; x++ {
			c := red
			switch {
			case x >= 150 && y < 150:
				c = green
			case x < 150 && y >= 150:
				c = blue
			case x >= 150 && y >= 150:
				c = yellow
			}
			img.SetRGBA(x, y, c)
		}
	}
	p := writeTemp(t, "quad.png", encodePNG(t, img))

	cases := []struct {
		name string
		box  [4]float64
		want color.RGBA
	}{
		{"top-left", [4]float64{0, 0, 0.5, 0.5}, red},
		{"bottom-right", [4]float64{0.5, 0.5, 1, 1}, yellow},
		{"bottom-left", [4]float64{0, 0.5, 0.5, 1}, blue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			box := tc.box
			out, err := Bytes(&Selection{Path: p, Box: &box})
			if err != nil {
				t.Fatal(err)
			}
			icon := decodeIcon(t, out)
			if got := at(icon, 128, 128); !near(got, tc.want, 30) {
				t.Fatalf("center pixel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFitMode(t *testing.T) {
	red := color.RGBA{220, 20, 20, 255}
	out, err := Bytes(&Selection{
		Path: writeTemp(t, "wide.png", encodePNG(t, solid(400, 200, red))),
		Fit:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	icon := decodeIcon(t, out)

	// Locate the red content box inside the white-padded canvas.
	minX, minY, maxX, maxY := 256, 256, -1, -1
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			c := at(icon, x, y)
			if int(c.R) > int(c.G)+40 && int(c.R) > int(c.B)+40 {
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < 0 {
		t.Fatal("no artwork content found")
	}
	width, height := maxX-minX+1, maxY-minY+1
	if width > 256 || height > 256 {
		t.Fatalf("fit content %dx%d exceeds icon", width, height)
	}
	aspect := float64(width) / float64(height)
	if math.Abs(aspect-2.0) > 0.1 {
		t.Fatalf("fit content %dx%d aspect %.3f, want 2.0", width, height, aspect)
	}
	if math.Abs(float64(minY)-64) > 2 || math.Abs(float64(maxY)-191) > 2 {
		t.Fatalf("fit content rows %d..%d, want centered 64..191", minY, maxY)
	}

	// Pillow thumbnail never upscales.
	if w, h := fitSize(100, 50, 256); w != 100 || h != 50 {
		t.Fatalf("fitSize(100,50) = %dx%d, want 100x50", w, h)
	}
	if w, h := fitSize(400, 200, 256); w != 256 || h != 128 {
		t.Fatalf("fitSize(400,200) = %dx%d, want 256x128", w, h)
	}
}

func TestSupportedFormats(t *testing.T) {
	base := solid(120, 80, color.RGBA{90, 140, 200, 255})

	pal := image.NewPaletted(base.Bounds(), color.Palette{color.RGBA{90, 140, 200, 255}})
	webpData, err := base64.StdEncoding.DecodeString(testWebP)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		ext  string
		data []byte
		w, h int
	}{
		{"png", encodePNG(t, base), 120, 80},
		{"jpg", encodeJPEG(t, base), 120, 80},
		{"gif", encodePNG(t, base), 120, 80}, // replaced below; keeps table shape
		{"bmp", encodeBMP(t, base), 120, 80},
		{"tiff", encodeTIFF(t, base), 120, 80},
		{"webp", webpData, 40, 30},
	}
	var gb bytes.Buffer
	if err := gif.Encode(&gb, pal, nil); err != nil {
		t.Fatal(err)
	}
	cases[2].data = gb.Bytes()

	for _, tc := range cases {
		t.Run(tc.ext, func(t *testing.T) {
			p := writeTemp(t, "art."+tc.ext, tc.data)
			img, err := LoadImage(p)
			if err != nil {
				t.Fatalf("LoadImage(%s): %v", tc.ext, err)
			}
			if img.Bounds().Dx() != tc.w || img.Bounds().Dy() != tc.h {
				t.Fatalf("dims = %v, want %dx%d", img.Bounds(), tc.w, tc.h)
			}
			out, err := Bytes(&Selection{Path: p})
			if err != nil {
				t.Fatalf("Bytes(%s): %v", tc.ext, err)
			}
			icon := decodeIcon(t, out)
			if got := at(icon, 128, 128); !near(got, color.RGBA{90, 140, 200, 255}, 30) {
				t.Fatalf("center pixel = %v, want source colour", got)
			}
		})
	}

	exts := SupportedExtensions()
	joined := strings.Join(exts, " ")
	for _, want := range []string{".png", ".jpg", ".webp", ".bmp", ".tiff"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("SupportedExtensions missing %s: %v", want, exts)
		}
	}
	if strings.Contains(joined, ".avif") || strings.Contains(joined, ".icns") {
		t.Fatalf("SupportedExtensions contains unsupported reduced-scope formats: %v", exts)
	}
}

func TestAnimatedGIFUsesFirstFrame(t *testing.T) {
	red := color.RGBA{230, 20, 20, 255}
	blue := color.RGBA{20, 20, 230, 255}
	palRed := image.NewPaletted(image.Rect(0, 0, 30, 30), color.Palette{red})
	palBlue := image.NewPaletted(image.Rect(0, 0, 30, 30), color.Palette{blue})

	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{
		Image: []*image.Paletted{palRed, palBlue},
		Delay: []int{0, 0},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := Bytes(&Selection{Path: writeTemp(t, "anim.gif", b.Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	icon := decodeIcon(t, out)
	if got := at(icon, 128, 128); !near(got, red, 30) {
		t.Fatalf("center pixel = %v, want first frame red %v", got, red)
	}
}

func TestOversizedImageRejected(t *testing.T) {
	data := oversizedBMP(10000, 6000) // 60 MP
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatalf("bad oversized BMP fixture: %v", err)
	} else if cfg.Width != 10000 || cfg.Height != 6000 {
		t.Fatalf("fixture config = %dx%d", cfg.Width, cfg.Height)
	}
	p := writeTemp(t, "huge.bmp", data)
	_, err := LoadImage(p)
	if err == nil || err.Error() != "Artwork is too large (maximum 50 megapixels)." {
		t.Fatalf("LoadImage error = %v, want 50 megapixel message", err)
	}
	_, err = Bytes(&Selection{Path: p})
	if err == nil || !strings.Contains(err.Error(), "50 megapixels") {
		t.Fatalf("Bytes error = %v, want 50 megapixel message", err)
	}
}

func oversizedBMP(w, h int32) []byte {
	b := make([]byte, 54)
	copy(b, "BM")
	binary.LittleEndian.PutUint32(b[2:], 54)  // file size (header only is enough for DecodeConfig)
	binary.LittleEndian.PutUint32(b[10:], 54) // pixel data offset
	binary.LittleEndian.PutUint32(b[14:], 40) // BITMAPINFOHEADER
	binary.LittleEndian.PutUint32(b[18:], uint32(w))
	binary.LittleEndian.PutUint32(b[22:], uint32(h))
	binary.LittleEndian.PutUint16(b[26:], 1)  // planes
	binary.LittleEndian.PutUint16(b[28:], 24) // bpp
	return b
}

func TestInvalidBoxRejected(t *testing.T) {
	p := writeTemp(t, "valid.png", encodePNG(t, solid(300, 300, color.RGBA{100, 120, 140, 255})))
	nan := math.NaN()

	cases := []struct {
		name string
		box  [4]float64
		want string
	}{
		{"out-of-range", [4]float64{0, 0, 2, 1}, "Crop coordinates must be between 0 and 1."},
		{"negative", [4]float64{-0.001, 0, 1, 1}, "Crop coordinates must be between 0 and 1."},
		{"nan", [4]float64{nan, 0, 1, 1}, "Crop coordinates must be between 0 and 1."},
		{"inf", [4]float64{0, 0, math.Inf(1), 1}, "Crop coordinates must be between 0 and 1."},
		{"not-square", [4]float64{0, 0, 0.5, 0.2}, "Choose a square crop at least one source pixel wide, inside the image."},
		{"empty", [4]float64{0.5, 0.5, 0.5, 0.5}, "Choose a square crop at least one source pixel wide, inside the image."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			box := tc.box
			_, err := Bytes(&Selection{Path: p, Box: &box})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestOrientationTransform(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for i := 0; i < 6; i++ {
		src.SetRGBA(i%3, i/3, color.RGBA{uint8((i + 1) * 10), 0, 0, 255})
	}
	// Expected corrected matrices (row-major values are R/10).
	cases := []struct {
		o    int
		w, h int
		want [][]int
	}{
		{1, 3, 2, [][]int{{1, 2, 3}, {4, 5, 6}}},
		{2, 3, 2, [][]int{{3, 2, 1}, {6, 5, 4}}},
		{3, 3, 2, [][]int{{6, 5, 4}, {3, 2, 1}}},
		{4, 3, 2, [][]int{{4, 5, 6}, {1, 2, 3}}},
		{5, 2, 3, [][]int{{1, 4}, {2, 5}, {3, 6}}},
		{6, 2, 3, [][]int{{4, 1}, {5, 2}, {6, 3}}},
		{7, 2, 3, [][]int{{6, 3}, {5, 2}, {4, 1}}},
		{8, 2, 3, [][]int{{3, 6}, {2, 5}, {1, 4}}},
	}
	for _, tc := range cases {
		got := applyOrientation(src, tc.o)
		if got.Bounds().Dx() != tc.w || got.Bounds().Dy() != tc.h {
			t.Fatalf("orientation %d dims = %v, want %dx%d", tc.o, got.Bounds(), tc.w, tc.h)
		}
		for y := 0; y < tc.h; y++ {
			for x := 0; x < tc.w; x++ {
				if v := int(at(got, x, y).R) / 10; v != tc.want[y][x] {
					t.Fatalf("orientation %d pixel (%d,%d) = %d, want %d", tc.o, x, y, v, tc.want[y][x])
				}
			}
		}
	}
}

func TestJPEGEXIFOrientation(t *testing.T) {
	red := color.RGBA{230, 20, 20, 255}
	white := color.RGBA{255, 255, 255, 255}
	img := solid(40, 20, white)
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			img.SetRGBA(x, y, red)
		}
	}
	plain := encodeJPEG(t, img)
	tagged := append([]byte{}, plain[:2]...)
	tagged = append(tagged, exifOrientationSegment(6)...)
	tagged = append(tagged, plain[2:]...)

	p := writeTemp(t, "rotated.jpg", tagged)
	got, err := LoadImage(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds().Dx() != 20 || got.Bounds().Dy() != 40 {
		t.Fatalf("EXIF orientation 6 dims = %v, want 20x40", got.Bounds())
	}
	// Rotating 90 CW moves the top-left red block to the top-right.
	if c := at(got, 15, 5); !near(c, red, 40) {
		t.Fatalf("pixel (15,5) = %v, want red after rotation", c)
	}
	if c := at(got, 2, 5); !near(c, white, 12) {
		t.Fatalf("pixel (2,5) = %v, want white", c)
	}
	if c := at(got, 15, 35); !near(c, white, 12) {
		t.Fatalf("pixel (15,35) = %v, want white", c)
	}

	// No EXIF -> unchanged dimensions.
	plainImg, err := LoadImage(writeTemp(t, "plain.jpg", plain))
	if err != nil {
		t.Fatal(err)
	}
	if plainImg.Bounds().Dx() != 40 || plainImg.Bounds().Dy() != 20 {
		t.Fatalf("plain JPEG dims = %v, want 40x20", plainImg.Bounds())
	}
}

// exifOrientationSegment builds an APP1 EXIF segment with IFD0 tag 0x0112.
func exifOrientationSegment(orientation uint16) []byte {
	tiff := []byte{'I', 'I', 0x2A, 0x00}
	tiff = binary.LittleEndian.AppendUint32(tiff, 8) // IFD0 offset
	tiff = binary.LittleEndian.AppendUint16(tiff, 1) // entry count
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x0112)
	tiff = binary.LittleEndian.AppendUint16(tiff, 3) // SHORT
	tiff = binary.LittleEndian.AppendUint32(tiff, 1)
	tiff = binary.LittleEndian.AppendUint16(tiff, orientation)
	tiff = append(tiff, 0, 0)                        // value padding
	tiff = binary.LittleEndian.AppendUint32(tiff, 0) // next IFD

	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	return append(seg, payload...)
}

func TestNilSelection(t *testing.T) {
	out, err := Bytes(nil)
	if err != nil || out != nil {
		t.Fatalf("Bytes(nil) = %v, %v; want nil, nil (Python artwork_bytes(None))", out, err)
	}
}

func TestLoadImageReturnsRGBA(t *testing.T) {
	p := writeTemp(t, "rgba.png", encodePNG(t, solid(8, 8, color.RGBA{1, 2, 3, 255})))
	img, err := LoadImage(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := img.(*image.RGBA); !ok {
		t.Fatalf("LoadImage returned %T, want *image.RGBA", img)
	}
}
