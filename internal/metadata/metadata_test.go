package metadata

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

func TestReadTitleID(t *testing.T) {
	const tid = uint64(0x0100f2c011c5c000)
	npdm := make([]byte, 0x100)
	copy(npdm[0:4], "META")
	binary.LittleEndian.PutUint32(npdm[0x70:], 0x90)
	binary.LittleEndian.PutUint32(npdm[0x74:], 0x40)
	copy(npdm[0x90:0x94], "ACI0")
	binary.LittleEndian.PutUint64(npdm[0x90+0x10:], tid)

	got, err := ReadTitleID(npdm)
	if err != nil {
		t.Fatalf("ReadTitleID: %v", err)
	}
	if got != tid {
		t.Fatalf("tid = %#x, want %#x", got, tid)
	}

	short := append([]byte(nil), npdm[:0x7f]...)
	if _, err := ReadTitleID(short); err == nil {
		t.Fatal("expected error for short NPDM")
	}
	badMagic := append([]byte(nil), npdm...)
	copy(badMagic[0:4], "XXXX")
	if _, err := ReadTitleID(badMagic); err == nil {
		t.Fatal("expected error for bad magic")
	}
	badAci0 := append([]byte(nil), npdm...)
	copy(badAci0[0x90:0x94], "NOPE")
	if _, err := ReadTitleID(badAci0); err == nil {
		t.Fatal("expected error for bad ACI0")
	}
	zeroTid := append([]byte(nil), npdm...)
	binary.LittleEndian.PutUint64(zeroTid[0x90+0x10:], 0)
	if _, err := ReadTitleID(zeroTid); err == nil {
		t.Fatal("expected error for zero title ID")
	}
}

func TestCleanTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"My Game [v1.2] [USA]", "My Game"},
		{"[DLC] Mario", "Mario"},
		{"Game", "Game"},
		{"", "Extracted game"},
		{"  [only brackets]  ", "Extracted game"},
		{"Title [with] text [tail]", "Title text"},
	}
	for _, c := range cases {
		if got := CleanTitle(c.in); got != c.want {
			t.Errorf("CleanTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUtf8Field(t *testing.T) {
	cases := []struct {
		text string
		size int
		want []byte
	}{
		{"Hello", 8, []byte("Hello\x00\x00\x00")},
		{"abcdef", 4, []byte("abc\x00")},
		{"ab", 5, []byte("ab\x00\x00\x00")},
		{"hé", 3, []byte{'h', 0, 0}},    // truncation splits the 2-byte é
		{"héllo", 5, []byte("hél\x00")}, // 4-byte prefix lands on a boundary
		{"日本語", 10, []byte("日本語\x00")},  // 3x3 bytes fits
	}
	for _, c := range cases {
		if got := Utf8Field(c.text, c.size); !bytes.Equal(got, c.want) {
			t.Errorf("Utf8Field(%q, %d) = %q, want %q", c.text, c.size, got, c.want)
		}
	}
}

func TestControlData(t *testing.T) {
	const tid = uint64(0x0400f2c011c5c000) // top byte 4 => Switch 2
	entries, preserved, err := ControlData(tid, "1.2.3", "My Game", nil, []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("ControlData: %v", err)
	}
	if preserved {
		t.Fatal("preserved should be false when generating")
	}
	if len(entries) != 1+18 {
		t.Fatalf("len(entries) = %d, want %d", len(entries), 1+18)
	}
	if entries[0].Name != "control.nacp" {
		t.Fatalf("entries[0].Name = %q", entries[0].Name)
	}
	nacp := entries[0].Data
	if len(nacp) != 0x4000 {
		t.Fatalf("len(nacp) = %d", len(nacp))
	}
	if !bytes.Equal(nacp[0x3060:0x3070], Utf8Field("1.2.3", 0x10)) {
		t.Errorf("display version at 0x3060 = %q", nacp[0x3060:0x3070])
	}
	if binary.LittleEndian.Uint64(nacp[0x3038:]) != tid {
		t.Errorf("tid at 0x3038 = %#x", binary.LittleEndian.Uint64(nacp[0x3038:]))
	}
	if binary.LittleEndian.Uint64(nacp[0x3070:]) != tid+0x1000 {
		t.Errorf("dlc base at 0x3070 = %#x", binary.LittleEndian.Uint64(nacp[0x3070:]))
	}
	if binary.LittleEndian.Uint64(nacp[0x3078:]) != tid {
		t.Errorf("tid at 0x3078 = %#x", binary.LittleEndian.Uint64(nacp[0x3078:]))
	}
	if nacp[0x3025] != 1 {
		t.Errorf("nacp[0x3025] = %d", nacp[0x3025])
	}
	if binary.LittleEndian.Uint32(nacp[0x302c:]) != 0xffff {
		t.Errorf("nacp[0x302c] = %#x", binary.LittleEndian.Uint32(nacp[0x302c:]))
	}
	if nacp[0x3214] != 3 || nacp[0x3217] != 1 {
		t.Errorf("Switch 2 flags = (%d, %d), want (3, 1)", nacp[0x3214], nacp[0x3217])
	}
	if !bytes.Equal(nacp[0x0000:0x0200], Utf8Field("My Game", 0x200)) {
		t.Errorf("title not written to first language field")
	}
	if !bytes.Equal(nacp[0x0300:0x0500], Utf8Field("My Game", 0x200)) {
		t.Errorf("title not written to second language field")
	}
	if entries[1].Name != "icon_AmericanEnglish.dat" {
		t.Errorf("entries[1].Name = %q", entries[1].Name)
	}
	if entries[18].Name != "icon_Thai.dat" {
		t.Errorf("entries[18].Name = %q", entries[18].Name)
	}
	if !bytes.Equal(entries[1].Data, []byte{1, 2, 3}) {
		t.Errorf("icon data not carried through")
	}
}

func TestControlDataNonSwitch2(t *testing.T) {
	const tid = uint64(0x0100f2c011c5c000)
	entries, preserved, err := ControlData(tid, "1.0", "Game", nil, nil)
	if err != nil {
		t.Fatalf("ControlData: %v", err)
	}
	if preserved {
		t.Fatal("preserved should be false")
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1 (no artwork)", len(entries))
	}
	nacp := entries[0].Data
	if nacp[0x3214] != 0 || nacp[0x3217] != 0 {
		t.Errorf("non-Switch2 flags = (%d, %d), want (0, 0)", nacp[0x3214], nacp[0x3217])
	}
}

func TestControlDataPreserved(t *testing.T) {
	existing := make([]byte, 0x4000)
	existing[0x3215] = 1
	for i := 0; i < 0x200; i++ {
		existing[i] = 0xAB
	}
	entries, preserved, err := ControlData(0x0100000000000000, "9.9.9", "IGNORED", existing, nil)
	if err != nil {
		t.Fatalf("ControlData: %v", err)
	}
	if !preserved {
		t.Fatal("preserved should be true when an existing NACP is provided")
	}
	nacp := entries[0].Data
	if nacp[0] != 0xAB || nacp[0x1ff] != 0xAB {
		t.Errorf("title field was modified despite preserved NACP")
	}
	if !bytes.Equal(nacp[0x3060:0x3070], Utf8Field("9.9.9", 0x10)) {
		t.Errorf("display version was not written to 0x3060")
	}
}

func TestControlDataExistingButEmpty(t *testing.T) {
	existing := make([]byte, 0x4000)
	existing[0x3215] = 0
	existing[0x123] = 0xCD // sentinel inside the first language field
	entries, preserved, err := ControlData(0x0100000000000000, "1.0", "New Title", existing, nil)
	if err != nil {
		t.Fatalf("ControlData: %v", err)
	}
	if !preserved {
		t.Fatal("preserved should be true when an existing NACP is provided")
	}
	nacp := entries[0].Data
	if !bytes.Equal(nacp[0x0000:0x0200], Utf8Field("New Title", 0x200)) {
		t.Errorf("title field not overwritten when language block was empty")
	}
	// Existing NACP keeps its (zero) title ID block untouched.
	if binary.LittleEndian.Uint64(nacp[0x3038:]) != 0 {
		t.Errorf("existing tid block was modified")
	}
}

func TestAppCNMT(t *testing.T) {
	d1 := sha256.Sum256([]byte("program-nca"))
	d2 := sha256.Sum256([]byte("control-nca"))
	records := []CNMTRecord{
		{Digest: d1, Size: 0x123456, ContentType: 1},
		{Digest: d2, Size: 0xabcdef012, ContentType: 3},
	}
	got := AppCNMT(0x0100f2c011c5c000, 0x00010000, records)

	want := mustHex(t, "00c0c511c0f2000100000100800010000200000000000000000000000000000000"+
		"c8c511c0f200010000000000000000"+
		"9d80031124602f6d0103e194cb8c11253704f242b7d71de67003573f8b01ea74"+
		"9d80031124602f6d0103e194cb8c11255634120000000100"+
		"906ecd26cc617e59541904d5d44d00aff9e18ce7663e051e9679ce2769f2e0"+
		"01906ecd26cc617e59541904d5d44d00af12f0debc0a000300"+
		"ab50e002929363ca9aed0d48ee6b8ae29b4a82110abfaf4fc82b779024ff209b")
	if !bytes.Equal(got, want) {
		t.Fatalf("AppCNMT mismatch:\n got %x\nwant %x", got, want)
	}
}

func TestAddOnCNMT(t *testing.T) {
	dd := sha256.Sum256([]byte("dlc-data-nca"))
	got := AddOnCNMT(0x0100f2c011c5d000, 0x00010001, dd[:], 0x987654321)

	want := mustHex(t, "00d0c511c0f2000101000100820018000100000000000000000000000000000000"+
		"c0c511c0f2000100000000000000000000000000000000"+
		"cb7cdf2d7d395e0905eea73a55209a67e69aa45b9020647ce18ecd24dce068cf"+
		"cb7cdf2d7d395e0905eea73a55209a672143658709000200"+
		"06b22fb2cf214d2fa523f1e1dab1e10295cfab5133b14c56232b82410d5fd122")
	if !bytes.Equal(got, want) {
		t.Fatalf("AddOnCNMT mismatch:\n got %x\nwant %x", got, want)
	}
}
