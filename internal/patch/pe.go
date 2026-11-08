package patch

import (
	"github.com/dannolan/boxcar/internal/binfmt"
)

// section is one PE section header (name, virtualSize, virtualAddress,
// sizeOfRawData, pointerToRawData). The name is retained as raw bytes so the
// null-padded ".s2gpx\0\0" comparison is faithful.
type section struct {
	name   string
	vs, va int // virtualSize, virtualAddress
	rs, rp int // sizeOfRawData, pointerToRawData
}

// PE is a minimal parsed PE32+ (x86-64) image, ported 1:1 from emulator_patch.PE.
type PE struct {
	data         []byte
	pe           int // e_lfanew
	count        int // numberOfSections
	optional     int // offset of optional header
	table        int // offset of section table
	sections     []section
	sectionAlign int
	fileAlign    int
}

// newPE parses data and panics with binfmt.Failure on any validation error,
// matching the Python PE constructor's ValueError messages.
func newPE(data []byte) *PE {
	binfmt.Require(len(data) >= 0x100 && len(data) >= 2 && data[0] == 'M' && data[1] == 'Z',
		"Not a Windows executable.")
	pe := int(binfmt.U32At(data, 0x3c))
	binfmt.Require(pe+4 <= len(data) && string(data[pe:pe+4]) == "PE\x00\x00",
		"Invalid PE header.")
	binfmt.Require(pe+26 <= len(data), "Invalid PE header.")
	machine := int(binfmt.U16At(data, pe+4))
	count := int(binfmt.U16At(data, pe+6))
	optional := pe + 24
	size := int(binfmt.U16At(data, pe+20))
	binfmt.Require(machine == 0x8664 && int(binfmt.U16At(data, optional)) == 0x20b && size >= 240,
		"Only verified Windows x64 builds are supported.")
	table := optional + size
	binfmt.Require(count >= 1 && count <= 32 && table+40*count <= len(data),
		"Invalid section table.")
	sections := make([]section, 0, count)
	for i := 0; i < count; i++ {
		off := table + 40*i
		name := string(data[off : off+8])
		vs := int(binfmt.U32At(data, off+8))
		va := int(binfmt.U32At(data, off+12))
		rs := int(binfmt.U32At(data, off+16))
		rp := int(binfmt.U32At(data, off+20))
		binfmt.Require(rp+rs <= len(data), "Section extends past the executable.")
		sections = append(sections, section{name, vs, va, rs, rp})
	}
	sectionAlign := int(binfmt.U32At(data, optional+32))
	fileAlign := int(binfmt.U32At(data, optional+36))
	binfmt.Require(sectionAlign == 4096 && (fileAlign == 512 || fileAlign == 4096),
		"Unsupported section alignment.")
	_, certSize := directory(data, optional, 4)
	binfmt.Require(certSize == 0, "Signed executables are not supported by this patch.")
	return &PE{
		data:         data,
		pe:           pe,
		count:        count,
		optional:     optional,
		table:        table,
		sections:     sections,
		sectionAlign: sectionAlign,
		fileAlign:    fileAlign,
	}
}

// directory returns the (RVA, size) of the i-th data directory.
func directory(data []byte, optional, index int) (int, int) {
	off := optional + 112 + 8*index
	return int(binfmt.U32At(data, off)), int(binfmt.U32At(data, off+4))
}

func (pe *PE) directory(index int) (int, int) {
	return directory(pe.data, pe.optional, index)
}

// offset maps an RVA to a file offset within a file-backed section.
func (pe *PE) offset(rva, size int) int {
	for _, s := range pe.sections {
		if s.va <= rva && rva+size <= s.va+s.rs {
			return s.rp + rva - s.va
		}
	}
	binfmt.Fail("Patch location is outside a file-backed section.")
	return 0
}

// read returns size bytes at the given RVA.
func (pe *PE) read(rva, size int) []byte {
	off := pe.offset(rva, size)
	return pe.data[off : off+size]
}
