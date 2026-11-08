// Package metadata ports the title-metadata helpers and CNMT builders from the
// Python reference implementation (engine.py and dlc.py). It produces the
// control NACP, the control.nacp + icon entries for the control NCA RomFS, and
// the Application / AddOnContent CNMT byte images exactly as NSP Box's Python
// build does.
package metadata

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ReadTitleID reads the application title ID from an ExeFS main.npdm image.
func ReadTitleID(npdm []byte) (uint64, error) {
	if len(npdm) < 0x80 || string(npdm[:4]) != "META" {
		return 0, errors.New("main.npdm is not a supported NPDM file.")
	}
	offset := int(binary.LittleEndian.Uint32(npdm[0x70:]))
	size := int(binary.LittleEndian.Uint32(npdm[0x74:]))
	if offset < 0x80 || offset+size > len(npdm) || size < 0x20 || string(npdm[offset:offset+4]) != "ACI0" {
		return 0, errors.New("The NPDM application metadata is incomplete or invalid.")
	}
	tid := binary.LittleEndian.Uint64(npdm[offset+0x10:])
	if tid == 0 {
		return 0, errors.New("The NPDM has no application title ID.")
	}
	return tid, nil
}

var bracketSuffix = regexp.MustCompile(`\s*\[[^\]]*\]`)

// CleanTitle strips bracketed "[...]" groups (and any whitespace immediately
// before them) from a title, trims surrounding whitespace, and falls back to
// "Extracted game" when nothing remains.
func CleanTitle(name string) string {
	if s := strings.TrimSpace(bracketSuffix.ReplaceAllString(name, "")); s != "" {
		return s
	}
	return "Extracted game"
}

// Utf8Field renders text as a NUL-padded UTF-8 field of the given byte size,
// truncating without ever splitting a multi-byte rune (mirrors utf8_field).
func Utf8Field(text string, size int) []byte {
	raw := []byte(text)
	limit := size - 1
	if limit < 0 {
		limit = 0
	}
	if len(raw) > limit {
		raw = raw[:limit]
		// Only the tail can be invalid, because the input was valid UTF-8 and
		// we only truncated the end; drop a partial trailing rune.
		for len(raw) > 0 && !utf8.Valid(raw) {
			raw = raw[:len(raw)-1]
		}
	}
	out := make([]byte, size)
	copy(out, raw)
	return out
}

// KV is a single control-directory file entry.
type KV struct {
	Name string
	Data []byte
}

var iconLanguages = []string{
	"AmericanEnglish", "BritishEnglish", "Japanese", "French", "German",
	"LatinAmericanSpanish", "Spanish", "Italian", "Dutch", "CanadianFrench",
	"Portuguese", "Russian", "Korean", "TraditionalChinese", "SimplifiedChinese",
	"BrazilianPortuguese", "Polish", "Thai",
}

// ControlData builds the control NCA's RomFS contents: a control.nacp plus one
// icon_<lang>.dat per language when artwork is supplied. preserved reports
// whether an existing NACP was carried through (and, when its language block
// was already set, left untouched).
func ControlData(titleID uint64, displayVersion string, title string, existingNacp []byte, artwork []byte) (entries []KV, preserved bool, err error) {
	var nacp []byte
	if existingNacp != nil {
		nacp = append([]byte(nil), existingNacp...)
	} else {
		nacp = make([]byte, 0x4000)
	}

	if existingNacp == nil || nacp[0x3215] == 0 {
		for i := 0; i < 16; i++ {
			copy(nacp[i*0x300:i*0x300+0x200], Utf8Field(title, 0x200))
		}
	}
	if existingNacp == nil {
		nacp[0x3025] = 1
		binary.LittleEndian.PutUint32(nacp[0x302c:], 0xffff)
		binary.LittleEndian.PutUint64(nacp[0x3038:], titleID)
		binary.LittleEndian.PutUint64(nacp[0x3070:], titleID+0x1000)
		binary.LittleEndian.PutUint64(nacp[0x3078:], titleID)
		if titleID>>56 == 4 {
			nacp[0x3214] = 3 // Switch 2 application-error prefix.
			nacp[0x3217] = 1 // ApparentPlatform::Ounce.
		}
	}
	copy(nacp[0x3060:0x3070], Utf8Field(displayVersion, 0x10))

	entries = append(entries, KV{Name: "control.nacp", Data: nacp})
	if artwork != nil {
		for _, lang := range iconLanguages {
			entries = append(entries, KV{Name: "icon_" + lang + ".dat", Data: artwork})
		}
	}
	return entries, existingNacp != nil, nil
}

// CNMTRecord is one content entry in an Application CNMT.
type CNMTRecord struct {
	Digest      [32]byte
	Size        int64
	ContentType byte
}

// AppCNMT builds the Application CNMT image (engine.build inline logic).
func AppCNMT(titleID uint64, version uint32, records []CNMTRecord) []byte {
	buf := make([]byte, 0x20)
	binary.LittleEndian.PutUint64(buf[0:8], titleID)
	binary.LittleEndian.PutUint32(buf[8:12], version)
	buf[12] = 0x80
	buf[13] = 0
	binary.LittleEndian.PutUint16(buf[14:16], 16)
	binary.LittleEndian.PutUint16(buf[16:18], uint16(len(records)))
	binary.LittleEndian.PutUint16(buf[18:20], 0)

	out := append([]byte(nil), buf...)
	out = binary.LittleEndian.AppendUint64(out, titleID+0x800)
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint32(out, 0)

	var sz [8]byte
	for _, r := range records {
		out = append(out, r.Digest[:]...)
		out = append(out, r.Digest[:16]...)
		binary.LittleEndian.PutUint64(sz[:], uint64(r.Size))
		out = append(out, sz[:6]...)
		out = append(out, r.ContentType, 0)
	}

	sum := sha256.Sum256(out)
	return append(out, sum[:]...)
}

// AddOnCNMT builds the AddOnContent CNMT image (dlc.build inline logic).
func AddOnCNMT(titleID uint64, version uint32, dataDigest []byte, dataSize int64) []byte {
	buf := make([]byte, 0x20)
	binary.LittleEndian.PutUint64(buf[0:8], titleID)
	binary.LittleEndian.PutUint32(buf[8:12], version)
	buf[12] = 0x82
	buf[13] = 0
	binary.LittleEndian.PutUint16(buf[14:16], 0x18)
	binary.LittleEndian.PutUint16(buf[16:18], 1)
	binary.LittleEndian.PutUint16(buf[18:20], 0)

	baseID := titleID & 0xffffffffffffe000
	out := append([]byte(nil), buf...)
	out = binary.LittleEndian.AppendUint64(out, baseID)
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = append(out, 0, 0, 0, 0) // B 0 + 3 padding bytes.
	out = binary.LittleEndian.AppendUint64(out, 0)

	out = append(out, dataDigest[:]...)
	out = append(out, dataDigest[:16]...)
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(dataSize))
	out = append(out, sz[:6]...)
	out = append(out, 2, 0)

	sum := sha256.Sum256(out)
	return append(out, sum[:]...)
}
