package patch

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// digest returns the lower-case hex SHA-256 of data, mirroring Python
// hashlib.sha256(data).hexdigest().
func digest(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// align rounds n up to a multiple of a, mirroring Python align().
func align(n, a int) int { return (n + a - 1) / a * a }

// rel builds a single x86-64 relative branch/jump: [opcode] + rel32 where
// rel32 = target - source - 5.
func rel(opcode byte, source, target int) []byte {
	b := make([]byte, 5)
	b[0] = opcode
	binary.LittleEndian.PutUint32(b[1:], uint32(int32(target-source-5)))
	return b
}

// mustHex decodes a hex string, panicking on error. Only used with constant
// byte strings from the embedded manifest/patterns.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("patch: bad hex constant: " + s)
	}
	return b
}

// digestMasked hashes body masked per-byte with mask (len(body) == len(mask)),
// mirroring digest(bytes(b & m for b, m in zip(body, mask))).
func digestMasked(body, mask []byte) string {
	h := sha256.New()
	buf := make([]byte, len(body))
	for i := range body {
		buf[i] = body[i] & mask[i]
	}
	h.Write(buf)
	return hex.EncodeToString(h.Sum(nil))
}

// packIII packs a (start, end, unwindRVA) triple as three little-endian u32s.
func packIII(f [3]int) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b[0:], uint32(f[0]))
	binary.LittleEndian.PutUint32(b[4:], uint32(f[1]))
	binary.LittleEndian.PutUint32(b[8:], uint32(f[2]))
	return b
}

// isHex64 reports whether s is exactly 64 lower-case hex digits.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
