package nca

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/region"
	"github.com/dannolan/boxcar/internal/stream"
)

const (
	testTitleID     = 0x0100000000010000
	testContentType = 0
)

// testBody is the fixed literal body shared by every test (Python bytes(range(256))).
func testBody() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func mustStream(t *testing.T, b []byte) *stream.Stream {
	t.Helper()
	s, err := stream.New(b)
	if err != nil {
		t.Fatalf("stream.New: %v", err)
	}
	return s
}

func loadHex(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return b
}

// buildTestNCA assembles a two-section (PFS + RomFS) program NCA over testBody.
func buildTestNCA(t *testing.T) *stream.Stream {
	t.Helper()
	ctx := context.Background()
	body := mustStream(t, testBody())

	pfsFSH, pfsBody, err := PFSSection(ctx, body)
	if err != nil {
		t.Fatalf("PFSSection: %v", err)
	}
	romFSH, romBody, err := RomfsSection(ctx, body, nil)
	if err != nil {
		t.Fatalf("RomfsSection: %v", err)
	}

	nca, err := NCA([]Section{{FSH: pfsFSH, Body: pfsBody}, {FSH: romFSH, Body: romBody}}, testTitleID, testContentType)
	if err != nil {
		t.Fatalf("NCA: %v", err)
	}
	return nca
}

// writeTemp streams s to a fresh temp file and returns its path.
func writeTemp(t *testing.T, s *stream.Stream) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "nca-*.bin")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if err := s.WriteTo(f, &stream.WriteOpts{Ctx: context.Background()}); err != nil {
		f.Close()
		t.Fatalf("WriteTo: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return f.Name()
}

func openRegion(t *testing.T, path string) *region.Region {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	r, err := region.New(f)
	if err != nil {
		t.Fatalf("region.New: %v", err)
	}
	return r
}

// TestRoundTrip writes a PFS + RomFS NCA, then parses it back through Sections
// and checks the kind order, content type and title ID.
func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	body := testBody()

	pfsFSH, pfsBody, err := PFSSection(ctx, mustStream(t, body))
	if err != nil {
		t.Fatalf("PFSSection: %v", err)
	}
	romFSH, romBody, err := RomfsSection(ctx, mustStream(t, body), nil)
	if err != nil {
		t.Fatalf("RomfsSection: %v", err)
	}

	nca, err := NCA([]Section{{FSH: pfsFSH, Body: pfsBody}, {FSH: romFSH, Body: romBody}}, testTitleID, testContentType)
	if err != nil {
		t.Fatalf("NCA: %v", err)
	}

	path := writeTemp(t, nca)
	r := openRegion(t, path)

	titleID, contentType, secs, err := Sections(ctx, r, true)
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	if titleID != testTitleID {
		t.Errorf("titleID = %#x, want %#x", titleID, testTitleID)
	}
	if contentType != testContentType {
		t.Errorf("contentType = %d, want %d", contentType, testContentType)
	}
	if len(secs) != 2 {
		t.Fatalf("len(secs) = %d, want 2", len(secs))
	}
	if secs[0].Kind != "pfs" || secs[1].Kind != "romfs" {
		t.Errorf("kinds = [%s, %s], want [pfs, romfs]", secs[0].Kind, secs[1].Kind)
	}

	for i, sec := range secs {
		if got := sec.Data.Size(); got != int64(len(body)) {
			t.Errorf("secs[%d].Data.Size() = %d, want %d", i, got, len(body))
		}
		got, err := sec.Data.Slice()
		if err != nil {
			t.Fatalf("secs[%d].Slice: %v", i, err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("secs[%d].Data != body", i)
		}
	}
}

// TestGoldenVectors compares the PFS/RomFS FSH bytes and the NCA header field
// positions against hex captured from the Python reference implementation
// (see testdata/, generated with:
//
//	python3 -c "sys.path.insert(0,'.../nsp-box-src/src'); import container_format as cf; ...").
func TestGoldenVectors(t *testing.T) {
	ctx := context.Background()
	body := mustStream(t, testBody())

	wantPFS := loadHex(t, "testdata/pfs_fsh.hex")
	wantRomfs := loadHex(t, "testdata/romfs_fsh.hex")

	pfsFSH, pfsBody, err := PFSSection(ctx, body)
	if err != nil {
		t.Fatalf("PFSSection: %v", err)
	}
	if !bytes.Equal(pfsFSH, wantPFS) {
		t.Errorf("PFS fsh mismatch:\n got %x\nwant %x", pfsFSH, wantPFS)
	}

	romFSH, romBody, err := RomfsSection(ctx, body, nil)
	if err != nil {
		t.Fatalf("RomfsSection: %v", err)
	}
	if !bytes.Equal(romFSH, wantRomfs) {
		t.Errorf("RomFS fsh mismatch:\n got %x\nwant %x", romFSH, wantRomfs)
	}

	nca, err := NCA([]Section{{FSH: pfsFSH, Body: pfsBody}, {FSH: romFSH, Body: romBody}}, testTitleID, testContentType)
	if err != nil {
		t.Fatalf("NCA: %v", err)
	}

	var buf bytes.Buffer
	if err := nca.WriteTo(&buf, &stream.WriteOpts{Ctx: ctx}); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	data := buf.Bytes()

	// Full-image byte-for-byte comparison against the Python output.
	wantNCA := loadHex(t, "testdata/nca.hex")
	if !bytes.Equal(data, wantNCA) {
		t.Errorf("NCA image mismatch: got %d bytes, want %d", len(data), len(wantNCA))
		if len(data) == len(wantNCA) {
			for i := range data {
				if data[i] != wantNCA[i] {
					t.Errorf("first differing byte at %#x: got %#x, want %#x", i, data[i], wantNCA[i])
					break
				}
			}
		}
	}

	// Explicit header field positions (see PORTING notes and Python nca()).
	field := func(off, n int, want string) {
		t.Helper()
		if got := hex.EncodeToString(data[off : off+n]); got != want {
			t.Errorf("field @%#x = %s, want %s", off, got, want)
		}
	}
	field(0x200, 4, "4e434133")                          // 'NCA3'
	field(0x204, 4, "00000100")                          // distribution, contentType, 1, 0
	field(0x210, 8, "0000010000000001")                  // title ID (LE)
	field(0x208, 8, "0090010000000000")                  // total size (LE) = 102400
	field(0x21c, 4, "00110c00")                          // 0x000c1100
	field(0x240, 16, "06000000080000000000000000000000") // section 0 begin/end sectors
	field(0x250, 16, "08000000c80000000000000000000000") // section 1 begin/end sectors
	field(0x280, 32, "ec1642a29f5ef768c06d3d74aad4f49c1bc47876388fec1634bf14a9d6f887d3")
	field(0x2a0, 32, "51602359f40bdf48e9cffc9d085d9046cd5c2dcebdebb15f2aa4285d4c6b9a05")
}

// TestSectionsCorruption flips a single byte in various hash/header regions and
// asserts Sections rejects the image.
func TestSectionsCorruption(t *testing.T) {
	ctx := context.Background()
	nca := buildTestNCA(t)

	var buf bytes.Buffer
	if err := nca.WriteTo(&buf, &stream.WriteOpts{Ctx: ctx}); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	good := buf.Bytes()

	cases := []struct {
		name string
		off  int
	}{
		{"NCA3 magic", 0x200},
		{"section 0 fsh digest field", 0x280},
		{"section 0 fsh", 0x400},
		{"section 0 body data (PFS)", 0xc00 + 0x200},
		{"section 1 fsh", 0x600},
		{"section 1 body data (RomFS)", 0x1000 + 5*0x4000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			corrupted := append([]byte(nil), good...)
			corrupted[c.off] ^= 0xff

			path := writeBytesTemp(t, corrupted)
			r := openRegion(t, path)
			if _, _, _, err := Sections(ctx, r, true); err == nil {
				t.Fatalf("Sections accepted corrupted image (byte %#x)", c.off)
			}
		})
	}
}

// TestSectionsVerifyPayloadFlag shows that with verifyPayload=false the payload
// hashes are skipped but structural checks still hold.
func TestSectionsVerifyPayloadFlag(t *testing.T) {
	ctx := context.Background()
	nca := buildTestNCA(t)

	var buf bytes.Buffer
	if err := nca.WriteTo(&buf, &stream.WriteOpts{Ctx: ctx}); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	// Corrupt only the PFS payload data (a payload-hash-protected region).
	corrupted := append([]byte(nil), buf.Bytes()...)
	corrupted[0xc00+0x200] ^= 0xff

	r := openRegion(t, writeBytesTemp(t, corrupted))
	if _, _, _, err := Sections(ctx, r, true); err == nil {
		t.Fatalf("verifyPayload=true should reject corrupted payload")
	}
	if _, _, _, err := Sections(ctx, r, false); err != nil {
		t.Fatalf("verifyPayload=false should skip payload hashes, got: %v", err)
	}
}

func writeBytesTemp(t *testing.T, b []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "nca-*.bin")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return f.Name()
}
