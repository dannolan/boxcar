package patch

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dannolan/boxcar/internal/binfmt"
)

// syntheticPE mirrors test_emulator_patch.synthetic_pe.
func syntheticPE() []byte {
	b := make([]byte, 0xa00)
	b[0], b[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(b[0x3c:], 0x80)
	copy(b[0x80:0x84], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[0x84:], 0x8664) // machine
	binary.LittleEndian.PutUint16(b[0x86:], 2)      // numberOfSections
	binary.LittleEndian.PutUint32(b[0x88:], 1)      // timeDateStamp
	binary.LittleEndian.PutUint32(b[0x8c:], 0)      // pointerToSymbolTable
	binary.LittleEndian.PutUint32(b[0x90:], 0)      // numberOfSymbols
	binary.LittleEndian.PutUint16(b[0x94:], 240)    // sizeOfOptionalHeader
	binary.LittleEndian.PutUint16(b[0x96:], 0x22)   // characteristics
	opt := 0x98
	binary.LittleEndian.PutUint16(b[opt:], 0x20b)          // magic PE32+
	binary.LittleEndian.PutUint32(b[opt+16:], 0x1000)      // AddressOfEntryPoint
	binary.LittleEndian.PutUint64(b[opt+24:], 0x140000000) // ImageBase
	binary.LittleEndian.PutUint32(b[opt+32:], 4096)        // SectionAlignment
	binary.LittleEndian.PutUint32(b[opt+36:], 512)         // FileAlignment
	binary.LittleEndian.PutUint32(b[opt+56:], 0x3000)      // SizeOfImage
	binary.LittleEndian.PutUint32(b[opt+60:], 0x400)       // SizeOfHeaders
	binary.LittleEndian.PutUint32(b[opt+108:], 16)         // NumberOfRvaAndSizes
	binary.LittleEndian.PutUint32(b[opt+112+24:], 0x2000)  // exception dir RVA
	binary.LittleEndian.PutUint32(b[opt+112+28:], 12)      // exception dir size
	table := opt + 240
	// .text
	copy(b[table:table+8], ".text\x00\x00\x00")
	binary.LittleEndian.PutUint32(b[table+8:], 0x400)   // virtualSize
	binary.LittleEndian.PutUint32(b[table+12:], 0x1000) // virtualAddress
	binary.LittleEndian.PutUint32(b[table+16:], 0x400)  // sizeOfRawData
	binary.LittleEndian.PutUint32(b[table+20:], 0x400)  // pointerToRawData
	binary.LittleEndian.PutUint32(b[table+36:], 0x60000020)
	// .rdata
	copy(b[table+40:table+48], ".rdata\x00\x00")
	binary.LittleEndian.PutUint32(b[table+48:], 0x200)  // virtualSize
	binary.LittleEndian.PutUint32(b[table+52:], 0x2000) // virtualAddress
	binary.LittleEndian.PutUint32(b[table+56:], 0x200)  // sizeOfRawData
	binary.LittleEndian.PutUint32(b[table+60:], 0x800)  // pointerToRawData
	binary.LittleEndian.PutUint32(b[table+76:], 0x40000040)
	// hook code at RVA 0x1000 (file 0x400)
	code := append([]byte{}, mustHex("48895c2410")...)
	for i := 0; i < 58; i++ {
		code = append(code, 0x90)
	}
	code = append(code, 0xc3)
	copy(b[0x400:0x440], code)
	// original unwind frame at file 0x800 (RVA 0x2000)
	binary.LittleEndian.PutUint32(b[0x800:], 0x1000)
	binary.LittleEndian.PutUint32(b[0x804:], 0x1040)
	binary.LittleEndian.PutUint32(b[0x808:], 0x2010)
	copy(b[0x810:0x814], "\x01\x00\x00\x00")
	marker := []byte("main.npdm\x00Switch 2 program: using the 12 GiB memory layout\x00Guest counter frequency:\x00")
	copy(b[0x840:0x840+len(marker)], marker)
	return b
}

type goldenVector struct {
	Name     string  `json:"name"`
	Revision int     `json:"revision"`
	In       string  `json:"in"`
	Out      string  `json:"out"`
	Profile  Profile `json:"profile"`
}

// TestTransformGoldenVectors asserts byte-identical output against the Python
// reference, for every supported profile kind (plus legacy revision 1).
func TestTransformGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vecs []goldenVector
	if err := json.Unmarshal(raw, &vecs); err != nil {
		t.Fatal(err)
	}
	if len(vecs) == 0 {
		t.Fatal("no golden vectors")
	}
	for _, v := range vecs {
		in, err := os.ReadFile(filepath.Join("testdata", v.In))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("testdata", v.Out))
		if err != nil {
			t.Fatal(err)
		}
		got := transform(in, v.Profile, v.Revision)
		if string(got) != string(want) {
			// locate the first differing byte for diagnostics
			n := len(got)
			if len(want) < n {
				n = len(want)
			}
			i := 0
			for i < n && got[i] == want[i] {
				i++
			}
			t.Errorf("%s: transform mismatch at byte %d (got len %d, want len %d)",
				v.Name, i, len(got), len(want))
			continue
		}
	}
}

func writeIII(b []byte, off int, f [3]int) {
	binary.LittleEndian.PutUint32(b[off:], uint32(f[0]))
	binary.LittleEndian.PutUint32(b[off+4:], uint32(f[1]))
	binary.LittleEndian.PutUint32(b[off+8:], uint32(f[2]))
}

func readFrames(pe *PE) [][3]int {
	rva, size := pe.directory(3)
	raw := pe.read(rva, size)
	var frames [][3]int
	for i := 0; i+12 <= len(raw); i += 12 {
		frames = append(frames, [3]int{
			int(binfmt.U32At(raw, i)),
			int(binfmt.U32At(raw, i+4)),
			int(binfmt.U32At(raw, i+8)),
		})
	}
	return frames
}

// TestTransformSectionAndChecksum mirrors test_transform_has_valid_section_and_checksum.
func TestTransformSectionAndChecksum(t *testing.T) {
	data := syntheticPE()
	profile := Profile{Name: "t", File: "eglantine.exe", ABI: "msvc", Hook: 0x1000, Expected: "48895c2410", Kind: "entry"}
	changed := transform(data, profile, patchRevision)
	pe := newPE(changed)
	if pe.count != 3 {
		t.Fatalf("section count = %d, want 3", pe.count)
	}
	if pe.sections[len(pe.sections)-1].name != ".s2gpx\x00\x00" {
		t.Fatalf("last section name = %q", pe.sections[len(pe.sections)-1].name)
	}
	if got := pe.read(0x1000, 1); got[0] != 0xe9 {
		t.Fatalf("hook not rewritten to jmp: %x", got)
	}
	frames := readFrames(pe)
	if len(frames) <= 1 {
		t.Fatalf("frame count = %d, want >1", len(frames))
	}
	for i := 0; i+1 < len(frames); i++ {
		if frames[i][0] > frames[i+1][0] {
			t.Fatalf("frames not sorted: %v", frames)
		}
		if frames[i][1] > frames[i+1][0] {
			t.Fatalf("overlapping frames: %v", frames)
		}
	}
}

// TestTransformDuplicateUnwind mirrors test_duplicate_original_unwind_records_are_guarded_and_ordered.
func TestTransformDuplicateUnwind(t *testing.T) {
	data := syntheticPE()
	binary.LittleEndian.PutUint32(data[0x98+112+24:], 0x2000)
	binary.LittleEndian.PutUint32(data[0x98+112+28:], 24)
	writeIII(data, 0x800, [3]int{0x1000, 0x1040, 0x2030})
	writeIII(data, 0x80c, [3]int{0x1000, 0x1040, 0x2020})
	copy(data[0x820:0x824], mustHex("01000000"))
	copy(data[0x830:0x834], mustHex("01000000"))

	profile := Profile{Name: "t", File: "eglantine.exe", ABI: "msvc", Hook: 0x1000, Expected: "48895c2410", Kind: "entry"}
	assertOverlap := func(p Profile, label string) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("%s: expected Overlapping unwind functions error", label)
			}
			f, ok := r.(binfmt.Failure)
			if !ok || !strings.Contains(f.Err.Error(), "Overlapping") {
				t.Fatalf("%s: got %v", label, r)
			}
		}()
		transform(data, p, patchRevision)
	}
	assertOverlap(profile, "no preserve")

	profile.PreserveDuplicateUnwind = true
	changed := transform(data, profile, patchRevision)
	frames := readFrames(newPE(changed))
	var dup [][3]int
	for _, f := range frames {
		if f[0] == 0x1000 && f[1] == 0x1040 {
			dup = append(dup, f)
		}
	}
	if len(dup) != 2 || dup[0][2] != 0x2030 || dup[1][2] != 0x2020 {
		t.Fatalf("duplicate unwind order wrong: %v", dup)
	}

	data[0x831] = 1
	assertOverlap(profile, "prolog mismatch")
	data[0x831] = 0
	binary.LittleEndian.PutUint32(data[0x80c:], 0x1010)
	assertOverlap(profile, "different start")
}

// TestReaderABI mirrors test_reader_abi_is_derived_and_unrecognized_slots_refused.
func TestReaderABI(t *testing.T) {
	data := syntheticPE()
	call, target := 0x1020, 0x1100
	copy(data[0x420:0x425], rel(0xe8, call, target))
	body := make([]byte, 40)
	for i := range body {
		body[i] = 0x90
	}
	copy(body[12:15], mustHex("ff5018"))
	copy(body[32:35], mustHex("ff5048"))
	copy(data[0x500:0x528], body)
	mask := make([]byte, 40)
	for i := range mask {
		mask[i] = 0xff
	}
	mask[14], mask[34] = 0, 0
	anchor := make([]byte, 12)
	copy(anchor, body[:12])
	p := &patternData{
		MetadataCallDelta: 32,
		Reader: readerData{
			Anchor:           anchor,
			NormalizedSHA256: digestMasked(body, mask),
			SizeDispOffset:   14,
			ReadDispOffset:   34,
			Mask:             mask,
		},
	}
	frames := [][3]int{{0x1100, 0x1140, 0x2010}}

	if got := readerABI(newPE(data), frames, 0x1000, p); got != "msvc-read9" {
		t.Fatalf("want msvc-read9, got %q", got)
	}
	data[0x522] = 0x40
	if got := readerABI(newPE(data), frames, 0x1000, p); got != "msvc" {
		t.Fatalf("want msvc, got %q", got)
	}
	data[0x522] = 0x70
	if got := readerABI(newPE(data), frames, 0x1000, p); got != "" {
		t.Fatalf("want empty for unknown slot, got %q", got)
	}
	data[0x50f] ^= 1
	if got := readerABI(newPE(data), frames, 0x1000, p); got != "" {
		t.Fatalf("want empty after anchor flip, got %q", got)
	}
}
