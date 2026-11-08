// Package nup is a streaming implementation of the public NUP v1 archive
// format. It is a 1:1 port of nup_format.py from the Python reference
// implementation (format reference: a55uka/nup, SPEC.md at 744fb7e).
//
// All public operations use bounded reads, cancellation and exclusive final
// commits. Compressed extents use github.com/klauspost/compress/zstd; the
// compressed bytes differ from the Python backend but the container verifies
// with the exact same rules.
package nup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dannolan/boxcar/internal/binfmt"
)

// Format constants (mirror nup_format.py).
const (
	Block     = 1 << 20   // 1 MiB read/write block
	LargeFile = 256 << 20 // 256 MiB large-raw threshold
	MaxWindow = 1 << 30   // 1 GiB decoder window cap
)

const none uint32 = 0xffffffff // sentinel for absent directory index

// Presets maps friendly preset names to the three accepted compression levels.
var Presets = map[string]int{"Fast": 1, "Balanced": 3, "Smaller": 9}

var kindNames = []string{"base", "update", "addon", "addon_update", "system"}

// KindName returns the package-kind label for a kind index (0..4).
func KindName(kind int) string {
	if kind >= 0 && kind < len(kindNames) {
		return kindNames[kind]
	}
	return ""
}

// TLV is one metadata type-length-value pair (port of a (kind, data) tuple).
type TLV struct {
	Kind uint16
	Data []byte
}

// Package describes one NUP package (port of nup_format.Package).
type Package struct {
	TitleID     uint64
	Version     uint32
	Kind        int
	Files       map[string]*Source
	Directories []string
	Metadata    []TLV
	Flags       int
	Label       string
}

// PackageInfo is the typed form of a Python inspect() packages_info entry.
type PackageInfo struct {
	Index         int
	Kind          int
	TitleID       uint64
	Version       uint32
	Flags         uint32
	Files         int
	MetadataTypes []string
	Title         string
}

// PackOptions controls Pack (port of nup_format.pack keyword arguments).
type PackOptions struct {
	Level       int
	LargeRaw    bool
	Verify      bool
	Progress    binfmt.Progress
	SourceGuard func() error
}

// Result is the Pack/Extract result (port of the Python result dicts).
type Result struct {
	Output                string
	Size                  int64
	SourceSize            int64
	SavedBytes            int64
	Verified              bool
	Packages              int
	DuplicateFilesSkipped int
}

// Stats is the port of the Python stats dict.
type Stats map[string]any

// Source is a path- or data-backed byte source with an optional change
// validator (port of nup_format.Source).
type Source struct {
	size      int64
	opener    func() (io.ReadCloser, error)
	validator func() error
}

type sourceStamp struct {
	size  int64
	mtime int64
	ctime int64
	ino   int64
}

// SourcePath builds a path-backed Source, refusing symlinks/reparse points and
// capturing a size+mtime(+ctime+ino) stamp used by Validate.
func SourcePath(path string) (*Source, error) {
	size, mtime, ctime, ino, linked, err := lstatSource(path)
	if err != nil {
		return nil, err
	}
	if linked {
		return nil, errors.New("Linked input files are not supported.")
	}
	stamp := sourceStamp{size: size, mtime: mtime, ctime: ctime, ino: ino}
	base := filepath.Base(path)
	return &Source{
		size: size,
		opener: func() (io.ReadCloser, error) {
			return os.Open(path)
		},
		validator: func() error {
			s2, m2, c2, i2, l2, err := lstatSource(path)
			if err != nil {
				return err
			}
			if l2 {
				return errors.New("Source became a link.")
			}
			if s2 != stamp.size || m2 != stamp.mtime || c2 != stamp.ctime {
				return fmt.Errorf("Source changed during packaging: %s", base)
			}
			if stamp.ino != 0 && i2 != stamp.ino {
				return fmt.Errorf("Source changed during packaging: %s", base)
			}
			return nil
		},
	}, nil
}

// SourceData builds an in-memory Source (port of Source.data).
func SourceData(data []byte) *Source {
	return &Source{
		size: int64(len(data)),
		opener: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

// Size returns the source length in bytes.
func (s *Source) Size() int64 { return s.size }

// Open returns a fresh re-openable reader over the source contents.
func (s *Source) Open() (io.ReadCloser, error) { return s.opener() }

// Validate re-checks the source against its captured stamp.
func (s *Source) Validate() error {
	if s.validator != nil {
		return s.validator()
	}
	return nil
}

// ReadSmall reads the whole source (bounded by limit). A limit <= 0 uses the
// Python default of 32 MiB.
func (s *Source) ReadSmall(limit int64) (data []byte, err error) {
	defer binfmt.Recover(&err)
	if limit <= 0 {
		limit = 32 << 20
	}
	binfmt.Require(s.size <= limit, "Metadata is too large.")
	if err := s.Validate(); err != nil {
		return nil, err
	}
	r, err := s.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err = io.ReadAll(io.LimitReader(r, s.size+1))
	if err != nil {
		return nil, err
	}
	binfmt.Require(int64(len(data)) == s.size, "Short metadata read.")
	return data, nil
}

// PackagePath returns the extraction layout path for a package.
func PackagePath(p Package) string {
	switch p.Kind {
	case 0:
		return "base"
	case 1:
		return fmt.Sprintf("updates/%d", p.Version)
	case 2:
		return fmt.Sprintf("dlc/%016X/%d", p.TitleID, p.Version)
	default:
		return fmt.Sprintf("%s/%016X/%d", KindName(p.Kind), p.TitleID, p.Version)
	}
}

// ValidatePackages validates a package set (port of validate_packages).
func ValidatePackages(packages []Package) (err error) {
	defer binfmt.Recover(&err)
	validatePackages(packages)
	return nil
}

func validatePackages(packages []Package) {
	binfmt.Require(len(packages) > 0 && len(packages) <= 4096, "NUP needs 1–4096 packages.")
	baseCount := 0
	for _, p := range packages {
		if p.Kind == 0 {
			baseCount++
		}
	}
	binfmt.Require(baseCount <= 1, "NUP v1 permits only one base package.")
	var base *Package
	for i := range packages {
		if packages[i].Kind == 0 {
			base = &packages[i]
			break
		}
	}
	identities := map[identityKey]bool{}
	for i := range packages {
		p := &packages[i]
		binfmt.Require(p.TitleID != 0 && p.Kind >= 0 && p.Kind < 5, "Invalid package identity.")
		ident := identityKey{tid: p.TitleID, version: p.Version, kind: p.Kind}
		binfmt.Require(!identities[ident], "Duplicate package identity.")
		identities[ident] = true
		binfmt.Require(p.Flags == 0 || p.Flags == 1, "Unknown package flags.")
		binfmt.Require(p.Flags == 0 || p.Kind == 1, "Unknown package flags.")
		if p.Kind == 1 {
			standalone := base == nil
			want := 0
			if standalone {
				want = 1
			}
			binfmt.Require(p.Flags == want, "Invalid standalone-update flag.")
		}
		const mask = ^uint64(0x1fff)
		if base != nil && p.Kind == 1 {
			binfmt.Require(p.TitleID&mask == base.TitleID&mask, "Update belongs to a different game.")
		}
		if base != nil && p.Kind == 2 {
			binfmt.Require(p.TitleID&mask == base.TitleID&mask, "DLC belongs to a different game.")
		}
	}
}

type identityKey struct {
	tid     uint64
	version uint32
	kind    int
}

// SafeName validates a single path component (port of safe_name).
func SafeName(name string, windows bool) (err error) {
	defer binfmt.Recover(&err)
	safeName(name, windows)
	return nil
}

func safeName(name string, windows bool) {
	binfmt.Require(name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00"),
		"Unsafe filename in archive/source.")
	if windows {
		binfmt.Require(!strings.ContainsAny(name, "<>:\"|?*") && !hasLowChar(name) &&
			!strings.ContainsAny(name[len(name)-1:], ". "),
			"A filename cannot be extracted safely on Windows: %s", pyRepr(name))
		binfmt.Require(!isReservedWindows(name), "Reserved Windows filename: %s", name)
	}
}

func hasLowChar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 32 {
			return true
		}
	}
	return false
}

func isReservedWindows(name string) bool {
	base := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		base = name[:i]
	}
	base = strings.ToUpper(base)
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 {
		prefix := base[:3]
		if (prefix == "COM" || prefix == "LPT") && base[3] >= '1' && base[3] <= '9' {
			return true
		}
	}
	return false
}

// pyRepr approximates Python's str.__repr__ for the error messages.
func pyRepr(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString("\\\\")
		case c == '\'':
			b.WriteString("\\'")
		case c == '\n':
			b.WriteString("\\n")
		case c == '\r':
			b.WriteString("\\r")
		case c == '\t':
			b.WriteString("\\t")
		case c < 0x20 || c > 0x7e:
			fmt.Fprintf(&b, "\\x%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// PathParts splits a POSIX-style path and validates each part (port of
// path_parts).
func PathParts(path string) (parts []string, err error) {
	defer binfmt.Recover(&err)
	return pathParts(path), nil
}

func pathParts(path string) []string {
	binfmt.Require(!strings.HasPrefix(path, "/"), "Absolute file paths are not allowed.")
	parts := strings.Split(path, "/")
	for _, name := range parts {
		safeName(name, false)
	}
	return parts
}

// tlvBytes serializes metadata TLV pairs (port of tlv_bytes).
func tlvBytes(items []TLV) []byte {
	var out []byte
	for _, t := range items {
		out = binfmt.AppendU16(out, t.Kind)
		out = binfmt.AppendU16(out, 0)
		out = binfmt.AppendU32(out, uint32(len(t.Data)))
		out = append(out, t.Data...)
	}
	return out
}

// parseTlvs parses metadata TLV pairs with bounds checks (port of parse_tlvs).
func parseTlvs(data []byte) (items []TLV, err error) {
	defer binfmt.Recover(&err)
	at := 0
	for at < len(data) {
		binfmt.Require(at+8 <= len(data), "Truncated metadata TLV.")
		kind := binfmt.U16At(data, at)
		res := binfmt.U16At(data, at+2)
		size := int(binfmt.U32At(data, at+4))
		at += 8
		binfmt.Require(res == 0 && size <= len(data)-at, "Invalid metadata TLV bounds.")
		items = append(items, TLV{Kind: kind, Data: data[at : at+size]})
		at += size
	}
	return items, nil
}

// chooseCodec returns whether a file of the given size should be compressed.
func chooseCodec(size int64, largeRaw bool) bool {
	return size > 0 && !(largeRaw && size > LargeFile)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// validUTF8 checks the byte slice decodes as strict UTF-8 (Python .decode).
func validUTF8(b []byte) bool {
	return utf8.Valid(b)
}
