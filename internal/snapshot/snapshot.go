// Package snapshot locates complete extracted-title snapshots on disk and
// inventories their source files, porting engine.find_snapshots and
// engine.source_inventory from the Python reference implementation.
package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/metadata"
)

// Snapshot describes one complete exefs + romfs title snapshot.
type Snapshot struct {
	Root     string
	TitleID  uint64
	Version  uint32
	Label    string
	Title    string
	NacpPath string
}

// Stamp is a (size, mtime-ns) pair captured at inventory time and re-checked
// before a commit to detect source mutation.
type Stamp struct {
	Size    int64
	MtimeNS int64
}

// Find scans folder (and up to four levels below it) for complete snapshots:
// a directory holding exefs/main, exefs/main.npdm and a romfs directory. It
// returns snapshots sorted by (version, label) descending, plus any skipped
// dlc/aoc/addons/add_on_content directories. See engine.find_snapshots.
func Find(ctx context.Context, folder string) (found []Snapshot, skipped []string, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}

	folder = resolvePath(folder)
	st, serr := os.Stat(folder)
	if serr != nil || !st.IsDir() {
		binfmt.Fail("Choose an existing game folder.")
	}

	if name := strings.ToLower(filepath.Base(folder)); name == "exefs" || name == "romfs" {
		folder = filepath.Dir(folder)
	}

	var invalid []string
	var scan func(path string, depth int)
	scan = func(path string, depth int) {
		if depth > 4 {
			return
		}
		binfmt.Check(ctx)
		if isFile(filepath.Join(path, "exefs", "main")) &&
			isFile(filepath.Join(path, "exefs", "main.npdm")) &&
			isDir(filepath.Join(path, "romfs")) {
			if snap, perr := parseSnapshot(path, folder); perr != nil {
				invalid = append(invalid, fmt.Sprintf("%s: %v", path, perr))
			} else {
				found = append(found, *snap)
			}
		}
		if depth == 4 {
			return
		}
		entries, derr := os.ReadDir(path)
		if derr != nil {
			return
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			name := e.Name()
			lower := strings.ToLower(name)
			if lower == "romfs" || lower == "exefs" || lower == "control" {
				continue
			}
			full := filepath.Join(path, name)
			if lower == "dlc" || lower == "aoc" || lower == "addons" || lower == "add_on_content" {
				if isDir(full) {
					skipped = append(skipped, full)
				}
				continue
			}
			// Only descend into real directories: symlinks are silently
			// skipped. The Windows reparse-point attribute check is not
			// performed here (deviation; see PORTING.md rule 4).
			lst, lerr := os.Lstat(full)
			if lerr != nil {
				continue
			}
			if lst.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if lst.IsDir() {
				scan(full, depth+1)
			}
		}
	}
	scan(folder, 0)

	sort.Slice(found, func(i, j int) bool {
		if found[i].Version != found[j].Version {
			return found[i].Version > found[j].Version
		}
		return strings.ToLower(found[i].Label) > strings.ToLower(found[j].Label)
	})

	if len(found) == 0 {
		msg := "No complete exefs + romfs snapshot found. Select the folder containing both, or its main game folder."
		if len(invalid) > 0 {
			msg += "\n" + invalid[0]
		}
		return nil, skipped, errors.New(msg)
	}
	return found, skipped, nil
}

// parseSnapshot extracts one Snapshot from a candidate path. The returned
// error mirrors the Python engine.find_snapshots try/except detail string.
func parseSnapshot(path, folder string) (*Snapshot, error) {
	tid, err := readTitleID(filepath.Join(path, "exefs", "main.npdm"))
	if err != nil {
		return nil, err
	}
	if tid&0x800 != 0 {
		return nil, errors.New("NPDM identifies a patch, not a main application.")
	}

	rel, _ := filepath.Rel(folder, path)
	ver, ok := versionFromRel(rel)
	if !ok {
		return nil, errors.New("Version number is out of range.")
	}

	label := rel
	if rel == "." {
		label = "Selected folder"
	}

	title := metadata.CleanTitle(filepath.Base(folder))
	nacp := ""
	for _, parent := range uniquePaths([]string{
		path,
		filepath.Join(path, "control"),
		folder,
		filepath.Join(folder, "control"),
	}) {
		p := filepath.Join(parent, "control.nacp")
		if isFile(p) && fileSize(p) == 0x4000 {
			nacp = p
			if data, rerr := os.ReadFile(p); rerr == nil && len(data) > 0x3215 && data[0x3215] == 0 {
				if text := extractTitle(data); text != "" {
					title = text
				}
			}
			break
		}
	}
	return &Snapshot{Root: path, TitleID: tid, Version: ver, Label: label, Title: title, NacpPath: nacp}, nil
}

// versionFromRel returns the last decimal path component of rel, or 0 when
// none exists. ok is false when the component exceeds the 32-bit range.
func versionFromRel(rel string) (ver uint32, ok bool) {
	if rel == "." {
		return 0, true
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if isDecimal(part) {
			v, inRange := parseDecimalVersion(part)
			if !inRange {
				return 0, false
			}
			ver = v
		}
	}
	return ver, true
}

// parseDecimalVersion parses an ASCII decimal string into a uint32, reporting
// whether it fits. Python uses arbitrary-precision int() so oversized decimals
// simply fail the 0..0xffffffff range check.
func parseDecimalVersion(s string) (uint32, bool) {
	t := strings.TrimLeft(s, "0")
	if t == "" {
		t = "0"
	}
	if len(t) > 10 || (len(t) == 10 && t > "4294967295") {
		return 0, false
	}
	v, err := strconv.ParseUint(t, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// extractTitle mirrors data[:0x200].split(b'\0')[0].decode('utf-8',
// errors='replace').strip().
func extractTitle(data []byte) string {
	chunk := data
	if len(chunk) > 0x200 {
		chunk = chunk[:0x200]
	}
	first := chunk
	if i := bytes.IndexByte(chunk, 0); i >= 0 {
		first = chunk[:i]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(first), "\uFFFD"))
}

func uniquePaths(ps []string) []string {
	seen := make(map[string]bool, len(ps))
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func readTitleID(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return metadata.ReadTitleID(data)
}

// Inventory walks root/exefs and root/romfs recursively, refusing linked
// entries and recording the size and modification time of every regular file.
// It reports progress every 256 files and fails when no romfs files exist.
func Inventory(ctx context.Context, root string, notify func(label string, found int)) (inv map[string]Stamp, err error) {
	defer binfmt.Recover(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	inv = make(map[string]Stamp)

	var scan func(folder string)
	scan = func(folder string) {
		entries, derr := os.ReadDir(folder)
		if derr != nil {
			binfmt.Fail("%v", derr)
		}
		for _, entry := range entries {
			binfmt.Check(ctx)
			p := filepath.Join(folder, entry.Name())
			lst, lerr := os.Lstat(p)
			if lerr != nil {
				binfmt.Fail("%v", lerr)
			}
			if lst.Mode()&os.ModeSymlink != 0 {
				binfmt.Fail("Linked files/folders are not supported: %s", p)
			}
			if lst.IsDir() {
				scan(p)
			} else if lst.Mode().IsRegular() {
				inv[p] = Stamp{Size: lst.Size(), MtimeNS: mtimeNS(lst)}
				if notify != nil && len(inv)%256 == 0 {
					notify(fmt.Sprintf("Checking source files · %s found", comma(len(inv))), 1)
				}
			}
		}
	}
	scan(filepath.Join(root, "exefs"))
	scan(filepath.Join(root, "romfs"))

	romfsDir := filepath.Join(root, "romfs")
	empty := true
	for p := range inv {
		if isWithin(romfsDir, p) {
			empty = false
			break
		}
	}
	if empty {
		binfmt.Fail("The RomFS folder is empty.")
	}
	return inv, nil
}

// ValidateSources re-stats every recorded path and compares size and mtime.
// It returns the engine.build "source changed" error on any mismatch.
func ValidateSources(paths map[string]Stamp) error {
	for p, expected := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if st.Size() != expected.Size || mtimeNS(st) != expected.MtimeNS {
			return errors.New("A source file changed during packaging. Try again with the dump idle.")
		}
	}
	return nil
}

func comma(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return st.Size()
}

// resolvePath makes p absolute and resolves symlinks where possible, matching
// Python pathlib.Path.resolve(strict=False).
func resolvePath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	dir := p
	var suffix []string
	for {
		if _, err := os.Lstat(dir); err == nil {
			if r, e := filepath.EvalSymlinks(dir); e == nil {
				out := r
				for i := len(suffix) - 1; i >= 0; i-- {
					out = filepath.Join(out, suffix[i])
				}
				return filepath.Clean(out)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		suffix = append(suffix, filepath.Base(dir))
		dir = parent
	}
	return p
}
