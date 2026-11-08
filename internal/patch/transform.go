package patch

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/dannolan/boxcar/internal/binfmt"
)

const patchRevision = 2

// readerABI validates the metadata reader and derives the ABI from the
// size/read vtable slots. Returns "" when the reader does not verify.
func readerABI(pe *PE, frames [][3]int, start int, p *patternData) string {
	call := start + p.MetadataCallDelta
	code := pe.read(call, 5)
	if code[0] != 0xe8 {
		return ""
	}
	target := call + 5 + int(int32(binary.LittleEndian.Uint32(code[1:])))
	spec := p.Reader
	mask := spec.Mask
	inFrame := false
	for _, f := range frames {
		if f[0] == target && f[1]-f[0] >= len(mask) {
			inFrame = true
			break
		}
	}
	if !inFrame {
		return ""
	}
	body := pe.read(target, len(mask))
	if !bytes.HasPrefix(body, spec.Anchor) || digestMasked(body, mask) != spec.NormalizedSHA256 {
		return ""
	}
	size := int(body[spec.SizeDispOffset])
	read := int(body[spec.ReadDispOffset])
	if size%8 != 0 || read%8 != 0 {
		return ""
	}
	for _, k := range payloadKeys {
		v := payloads[k]
		if v.SizeSlot == size/8 && v.ReadSlot == read/8 {
			return k
		}
	}
	return ""
}

// profileFor recognizes a build either by exact digest or by a unique
// normalized loader-code fingerprint. It returns nil when no single pattern
// matches, mirroring the Python try/except returning None on invalid data.
func profileFor(data []byte, name string, legacy bool) (p *Profile) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(binfmt.Failure); ok {
				p = nil
				return
			}
			panic(r)
		}
	}()

	if known, ok := profiles[digest(data)]; ok {
		if strings.EqualFold(known.File, name) {
			k := known
			k.Verified = true
			return &k
		}
	}

	markers := [][]byte{
		[]byte("main.npdm\x00"),
		[]byte("Switch 2 program: using the 12 GiB memory layout"),
		[]byte("Guest counter frequency:"),
	}
	for _, m := range markers {
		if !bytes.Contains(data, m) {
			return nil
		}
	}

	pe := newPE(data)
	rva, size := pe.directory(3)
	if size == 0 || size%12 != 0 {
		return nil
	}
	raw := pe.read(rva, size)
	frames := make([][3]int, 0, size/12)
	for i := 0; i+12 <= len(raw); i += 12 {
		frames = append(frames, [3]int{
			int(binfmt.U32At(raw, i)),
			int(binfmt.U32At(raw, i+4)),
			int(binfmt.U32At(raw, i+8)),
		})
	}

	var matches []*Profile
	for i := range patterns {
		pattern := &patterns[i]
		mask := pattern.Mask
		anchor := pattern.Anchor
		for _, f := range frames {
			start, end := f[0], f[1]
			if end-start < len(mask) || string(pe.read(start, len(anchor))) != string(anchor) {
				continue
			}
			body := pe.read(start, len(mask))
			if digestMasked(body, mask) != pattern.NormalizedSHA256 {
				continue
			}
			hook := start + pattern.HookDelta
			expected := pattern.Expected
			expBytes := mustHex(expected)
			if hex.EncodeToString(pe.read(hook, len(expBytes))) != expected {
				continue
			}
			abi := ""
			if legacy {
				if pattern.Kind == "inline" {
					abi = "gnu"
				} else {
					abi = "msvc"
				}
			} else {
				abi = readerABI(pe, frames, start, pattern)
			}
			if abi == "" {
				continue
			}
			m := &Profile{
				Name:     "Compatible loader · experimental build",
				File:     name,
				ABI:      abi,
				Hook:     hook,
				Expected: expected,
				Kind:     pattern.Kind,
				Verified: false,
			}
			if pattern.PreserveDuplicateUnwind {
				m.PreserveDuplicateUnwind = true
			}
			matches = append(matches, m)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return nil
}

// transform is the pure, byte-exact port of emulator_patch.transform.
// It panics with binfmt.Failure on any validation error.
func transform(data []byte, profile Profile, revision int) []byte {
	pe := newPE(data)
	hook := profile.Hook
	saved := mustHex(profile.Expected)
	binfmt.Require(string(pe.read(hook, len(saved))) == string(saved),
		"Expected patch instructions do not match.")

	firstRaw := -1
	for _, s := range pe.sections {
		if s.rs > 0 && (firstRaw == -1 || s.rp < firstRaw) {
			firstRaw = s.rp
		}
	}
	binfmt.Require(firstRaw != -1, "Invalid section table.")
	if pe.table+40*(pe.count+1) > firstRaw {
		binfmt.Fail("No room for the patch section header.")
	}

	end := 0
	for _, s := range pe.sections {
		m := s.vs
		if s.rs > m {
			m = s.rs
		}
		if s.va+m > end {
			end = s.va + m
		}
	}
	base := align(end, pe.sectionAlign)

	binfmt.Require(revision == 1 || revision == patchRevision, "Unsupported patch revision.")

	legacyABI := "msvc"
	if profile.Kind == "inline" {
		legacyABI = "gnu"
	}
	var payload *payloadData
	if revision == 1 {
		payload = legacyPayloads[legacyABI]
	} else {
		payload = payloads[profile.ABI]
	}
	binfmt.Require(payload != nil, "Unknown patch payload.")

	blob := make([]byte, len(payload.Image))
	copy(blob, payload.Image)
	if digest(blob) != payload.SHA256 {
		binfmt.Fail("Included native probe failed its integrity check.")
	}

	functions := make([][3]int, 0, len(payload.Unwind)+2)
	for _, u := range payload.Unwind {
		functions = append(functions, [3]int{base + u[0], base + u[1], base + u[2]})
	}

	originalRVA, originalSize := pe.directory(3)
	binfmt.Require(originalSize != 0 && originalSize%12 == 0, "Invalid executable unwind table.")
	original := pe.read(originalRVA, originalSize)
	originalFunctions := make([][3]int, 0, originalSize/12)
	for i := 0; i+12 <= len(original); i += 12 {
		originalFunctions = append(originalFunctions, [3]int{
			int(binfmt.U32At(original, i)),
			int(binfmt.U32At(original, i+4)),
			int(binfmt.U32At(original, i+8)),
		})
	}

	// put pads blob to alignment a, appends value and returns its RVA.
	put := func(value []byte, a int) int {
		for len(blob)%a != 0 {
			blob = append(blob, 0)
		}
		off := len(blob)
		blob = append(blob, value...)
		return base + off
	}

	wrapper := base + len(blob)
	code := []byte{}
	call := func(target int) {
		code = append(code, rel(0xe8, wrapper+len(code), target)...)
	}

	switch profile.Kind {
	case "entry":
		// stack + shared_ptr save
		code = append(code, mustHex("4883ec3848894c2420")...)
		call(base + payload.Entry)
		code = append(code, mustHex("85c074054883c438c3")...)
		code = append(code, mustHex("488b4c2420")...)
		trampolineCall := len(code)
		code = append(code, 0, 0, 0, 0, 0)
		code = append(code, mustHex("4883c438c3")...)
		trampoline := wrapper + len(code)
		copy(code[trampolineCall:trampolineCall+5], rel(0xe8, wrapper+trampolineCall, trampoline))
		code = append(code, saved...)
		code = append(code, rel(0xe9, wrapper+len(code), hook+len(saved))...)
		blob = append(blob, code...)
		uw := put(mustHex("0104010004620000"), 4)  // ALLOC_SMALL 56
		tuw := put(mustHex("0105020005340200"), 4) // original SAVE_NONVOL RBX, +16
		functions = append(functions,
			[3]int{wrapper, trampoline, uw},
			[3]int{trampoline, wrapper + len(code), tuw})
	case "inline", "inline-pokedinner", "inline-selevet":
		var frame [3]int
		found := false
		for _, f := range originalFunctions {
			if f[0] <= hook && hook < f[1] {
				frame = f
				found = true
				break
			}
		}
		binfmt.Require(found, "Original PGO unwind frame not found.")
		pokedinner := profile.Kind == "inline-pokedinner"
		selevet := profile.Kind == "inline-selevet"
		if pokedinner || selevet {
			code = append(code, mustHex("488d8da8030000")...)
		} else {
			code = append(code, mustHex("488d8c2408010000")...)
		}
		call(base + payload.Entry)
		switch {
		case selevet:
			code = append(code, mustHex("4109c5")...)
		case pokedinner:
			code = append(code, mustHex("4109c4")...)
		default:
			code = append(code, mustHex("09c5")...)
		}
		code = append(code, saved...)
		code = append(code, rel(0xe9, wrapper+len(code), hook+len(saved))...)
		blob = append(blob, code...)
		frameRegister := pe.read(frame[2], 4)[3]
		uwVal := append([]byte{0x21, 0, 0, frameRegister}, packIII(frame)...)
		uw := put(uwVal, 4)
		functions = append(functions, [3]int{wrapper, wrapper + len(code), uw})
	default:
		binfmt.Fail("Unknown patch profile kind.")
	}

	// Keep equal-start original records in their existing order.
	combined := make([][3]int, 0, len(originalFunctions)+len(functions))
	combined = append(combined, originalFunctions...)
	combined = append(combined, functions...)
	sort.SliceStable(combined, func(i, j int) bool { return combined[i][0] < combined[j][0] })
	originalSet := make(map[[3]int]bool, len(originalFunctions))
	for _, f := range originalFunctions {
		originalSet[f] = true
	}
	for i := 0; i+1 < len(combined); i++ {
		a, b := combined[i], combined[i+1]
		if a[1] <= b[0] {
			continue
		}
		duplicate := profile.PreserveDuplicateUnwind && a[0] == b[0] && a[1] == b[1] &&
			originalSet[a] && originalSet[b]
		if duplicate {
			first := pe.read(a[2], 4)
			second := pe.read(b[2], 4)
			duplicate = (first[0]&7) == 1 && (second[0]&7) == 1 &&
				string(first[1:]) == string(second[1:]) &&
				(first[0]|second[0])&0x20 == 0 &&
				string(pe.read(a[2]+4, int(first[2])*2)) == string(pe.read(b[2]+4, int(second[2])*2))
		}
		binfmt.Require(duplicate, "Overlapping unwind functions.")
	}

	var exc []byte
	for _, f := range combined {
		exc = binfmt.AppendU32(exc, uint32(f[0]))
		exc = binfmt.AppendU32(exc, uint32(f[1]))
		exc = binfmt.AppendU32(exc, uint32(f[2]))
	}
	exception := put(exc, 4)

	raw := align(len(data), pe.fileAlign)
	rawSize := align(len(blob), pe.fileAlign)
	result := make([]byte, 0, raw+rawSize)
	result = append(result, data...)
	for len(result) < raw {
		result = append(result, 0)
	}
	result = append(result, blob...)
	for len(result) < raw+rawSize {
		result = append(result, 0)
	}

	binfmt.PutU16(result, pe.pe+6, uint16(pe.count+1))
	off := pe.table + 40*pe.count
	copy(result[off:off+8], []byte(".s2gpx\x00\x00"))
	binfmt.PutU32(result, off+8, uint32(len(blob)))                                         // virtualSize
	binfmt.PutU32(result, off+12, uint32(base))                                             // virtualAddress
	binfmt.PutU32(result, off+16, uint32(rawSize))                                          // sizeOfRawData
	binfmt.PutU32(result, off+20, uint32(raw))                                              // pointerToRawData
	binfmt.PutU32(result, off+24, 0)                                                        // relocations
	binfmt.PutU32(result, off+28, 0)                                                        // lineNumbers
	binfmt.PutU16(result, off+32, 0)                                                        // numRelocations
	binfmt.PutU16(result, off+34, 0)                                                        // numLineNumbers
	binfmt.PutU32(result, off+36, 0x60000020)                                               // characteristics
	binfmt.PutU32(result, pe.optional+56, uint32(align(base+len(blob), pe.sectionAlign)))   // SizeOfImage
	binfmt.PutU32(result, pe.optional+4, binfmt.U32At(data, pe.optional+4)+uint32(rawSize)) // SizeOfCode
	binfmt.PutU32(result, pe.optional+112+24, uint32(exception))
	binfmt.PutU32(result, pe.optional+112+28, uint32(len(combined)*12))

	hoff := pe.offset(hook, len(saved))
	jmp := rel(0xe9, hook, wrapper)
	copy(result[hoff:hoff+len(saved)], jmp)
	for i := len(jmp); i < len(saved); i++ {
		result[hoff+i] = 0x90
	}

	binfmt.PutU32(result, pe.optional+64, 0)
	var checksum uint64
	for i := 0; i+1 < len(result); i += 2 {
		checksum += uint64(binary.LittleEndian.Uint16(result[i:]))
	}
	if len(result)%2 == 1 {
		checksum += uint64(result[len(result)-1])
	}
	for checksum>>16 != 0 {
		checksum = (checksum & 0xffff) + (checksum >> 16)
	}
	binfmt.PutU32(result, pe.optional+64, uint32(checksum+uint64(len(result))))

	return result
}
