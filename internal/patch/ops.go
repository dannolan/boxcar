package patch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dannolan/boxcar/internal/binfmt"
)

// record is one entry in state.json's "files" map.
type record struct {
	Original string `json:"original"`
	Patched  string `json:"patched"`
	Revision *int   `json:"revision,omitempty"`
}

// filePair tracks a file that has been replaced during apply/remove, for
// rollback when a later write fails.
type filePair struct {
	path string
	data []byte
}

// rootFolder resolves folder to the emulator root and derives the state dir.
func rootFolder(folder string) (root, state string) {
	r, err := filepath.EvalSymlinks(folder)
	binfmt.Require(err == nil, "Choose an emulator folder.")
	fi, err := os.Stat(r)
	binfmt.Require(err == nil && fi.IsDir(), "Choose an emulator folder.")
	root = r
	if !hasExe(root) {
		entries, _ := os.ReadDir(root)
		var children []string
		for _, e := range entries {
			p := filepath.Join(root, e.Name())
			if linked(p) {
				continue
			}
			if cfi, err := os.Stat(p); err == nil && cfi.IsDir() && hasExe(p) {
				children = append(children, p)
			}
		}
		if len(children) == 1 {
			root = children[0]
		}
	}
	state = filepath.Join(root, stateDir)
	if st, err := os.Lstat(state); err == nil {
		binfmt.Require(!linked(state) && st.IsDir(), "Patch backup folder is linked or invalid.")
	}
	return root, state
}

// hasExe reports whether dir contains any *.exe entry (non-recursive,
// case-sensitive, matching Python Path.glob('*.exe')).
func hasExe(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".exe") {
			return true
		}
	}
	return false
}

// exeFiles returns root's *.exe entries sorted case-insensitively by name.
func exeFiles(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".exe") {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(filepath.Base(out[i])) < strings.ToLower(filepath.Base(out[j]))
	})
	return out
}

// records validates and returns state.json's files map (empty when absent).
func records(state string) map[string]record {
	path := filepath.Join(state, "state.json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return map[string]record{}
	}
	binfmt.Require(!linked(path), "Linked patch manifest refused.")
	raw, err := os.ReadFile(path)
	binfmt.Require(err == nil, "Unrecognized patch manifest.")
	var value struct {
		PatchID string            `json:"patch_id"`
		Files   map[string]record `json:"files"`
	}
	binfmt.Require(json.Unmarshal(raw, &value) == nil, "Unrecognized patch manifest.")
	binfmt.Require(value.PatchID == patchID && value.Files != nil, "Unrecognized patch manifest.")
	files := value.Files
	for name, info := range files {
		binfmt.Require(filepath.Base(name) == name && !strings.ContainsAny(name, "/\\:") &&
			strings.HasSuffix(strings.ToLower(name), ".exe"),
			"Invalid backup filename.")
		rev := 1
		if info.Revision != nil {
			rev = *info.Revision
		}
		binfmt.Require(rev == 1 || rev == patchRevision, "Unsupported saved patch revision.")
		binfmt.Require(isHex64(info.Original) && isHex64(info.Patched), "Invalid backup fingerprint.")
	}
	return files
}

// inspectResolved scans an already-resolved root, mirroring emulator_patch.inspect.
func inspectResolved(root, state string) View {
	saved := records(state)
	var files []FileStatus
	seen := map[string]bool{}
	for _, path := range exeFiles(root) {
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		resolved := path
		if r, err := filepath.EvalSymlinks(path); err == nil {
			resolved = r
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		binfmt.Require(!linked(path), "Linked emulator executables are not supported.")

		data, err := os.ReadFile(path)
		binfmt.Require(err == nil, "Unable to read executable.")
		sha := digest(data)
		name := filepath.Base(path)
		info, hasInfo := saved[name]

		var profile *Profile
		if hasInfo && sha == info.Patched {
			profile = nil
		} else {
			profile = profileFor(data, name, false)
		}
		if profile == nil && !hasInfo {
			lower := strings.ToLower(name)
			recognized := false
			for _, n := range names {
				if strings.ToLower(n) == lower {
					recognized = true
					break
				}
			}
			if !recognized {
				continue
			}
		}

		status := "unsupported"
		if profile != nil {
			if profile.Verified {
				status = "supported"
			} else {
				status = "experimental"
			}
		}

		if hasInfo {
			backup := filepath.Join(state, name+".original")
			bfi, err := os.Stat(backup)
			binfmt.Require(err == nil && bfi.Mode().IsRegular() && !linked(backup),
				"Original backup is missing or changed; no files will be modified.")
			bdata, err := os.ReadFile(backup)
			binfmt.Require(err == nil, "Original backup is missing or changed; no files will be modified.")
			binfmt.Require(digest(bdata) == info.Original,
				"Original backup is missing or changed; no files will be modified.")
			switch {
			case sha == info.Patched:
				rev := 1
				if info.Revision != nil {
					rev = *info.Revision
				}
				known := profileFor(bdata, name, rev == 1)
				binfmt.Require(known != nil, "Original backup loader is not recognized.")
				binfmt.Require(digest(transform(bdata, *known, rev)) == sha,
					"Patch manifest does not match this patch version.")
				if rev < patchRevision {
					status = "outdated"
				} else {
					status = "patched"
				}
				profile = known
			case sha == info.Original:
				if profile != nil {
					if profile.Verified {
						status = "supported"
					} else {
						status = "experimental"
					}
				} else {
					status = "unsupported"
				}
			default:
				status = "changed"
			}
		}

		build := "Unknown build"
		if profile != nil {
			build = profile.Name
		}
		files = append(files, FileStatus{File: name, SHA256: sha, Status: status, Build: build})
	}

	binfmt.Require(len(files) > 0,
		"No compatible Switch 2 loader was recognized. Select the folder containing the emulator EXE.")
	blocked := false
	canApply := false
	canRemove := false
	for _, f := range files {
		if f.Status == "changed" {
			blocked = true
		}
		if f.Status == "supported" || f.Status == "experimental" {
			canApply = true
		}
		if f.Status == "patched" || f.Status == "outdated" {
			canRemove = true
		}
	}
	return View{Folder: root, Files: files, CanApply: !blocked && canApply, CanRemove: !blocked && canRemove}
}

// inspectFolder resolves folder then inspects.
func inspectFolder(folder string) View {
	root, state := rootFolder(folder)
	return inspectResolved(root, state)
}

// atomicWrite writes data to path atomically (or exclusively when requested).
// It is a package variable so tests can inject failures.
var atomicWrite = atomicWriteImpl

func atomicWriteImpl(path string, data []byte, exclusive bool) {
	if _, err := os.Lstat(path); err == nil && linked(path) {
		binfmt.Fail("Linked output refused.")
	}
	if exclusive {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		binfmt.Require(err == nil, "Unable to create backup.")
		if _, err := f.Write(data); err != nil {
			f.Close()
			binfmt.Fail("Unable to write backup.")
		}
		_ = f.Sync()
		_ = f.Close()
		return
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".s2gp-*.tmp")
	binfmt.Require(err == nil, "Unable to write file.")
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		binfmt.Fail("Unable to write file.")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		binfmt.Fail("Unable to write file.")
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		binfmt.Fail("Unable to write file.")
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		binfmt.Fail("Unable to write file.")
	}
	if _, err := os.Lstat(tmpName); err == nil {
		os.Remove(tmpName)
	}
}

// locked acquires the exclusive operation lock, returning the resolved root,
// state dir and an unlock function.
func locked(folder string) (root, state string, unlock func()) {
	root, state = rootFolder(folder)
	if err := os.Mkdir(state, 0o755); err != nil && !os.IsExist(err) {
		binfmt.Fail("Unable to create backup folder.")
	}
	lockPath := filepath.Join(state, "operation.lock")
	if _, err := os.Lstat(lockPath); err == nil {
		binfmt.Require(!linked(lockPath), "Linked operation lock refused.")
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	binfmt.Require(err == nil, "Unable to open operation lock.")
	fi, err := f.Stat()
	binfmt.Require(err == nil, "Unable to stat operation lock.")
	if fi.Size() == 0 {
		_, _ = f.Write([]byte{0})
		_ = f.Sync()
	}
	_, _ = f.Seek(0, 0)
	if err := flock(f, true); err != nil {
		_ = f.Close()
		binfmt.Fail("A patch operation is already running. Try again when it finishes.")
	}
	unlock = func() {
		_ = flock(f, false)
		_ = f.Close()
	}
	return root, state, unlock
}

// applyFolder mirrors emulator_patch.apply.
func applyFolder(folder string) View {
	if !inspectFolder(folder).CanApply {
		binfmt.Fail("No recognizable unpatched loader found, or executable files changed after patching.")
	}
	root, state, unlock := locked(folder)
	defer unlock()

	view := inspectResolved(root, state)
	binfmt.Require(view.CanApply, "This folder is patched, changed, or contains an unsupported build.")

	saved := records(state)
	type pendingT struct {
		path    string
		data    []byte
		patched []byte
	}
	var pending []pendingT
	for _, item := range view.Files {
		if item.Status != "supported" && item.Status != "experimental" {
			continue
		}
		path := filepath.Join(root, item.File)
		data, err := os.ReadFile(path)
		binfmt.Require(err == nil, "Unable to read executable.")
		binfmt.Require(digest(data) == item.SHA256, "Emulator changed while checking. Try again.")
		profile := profileFor(data, item.File, false)
		binfmt.Require(profile != nil, "Loader code is no longer recognized.")
		patched := transform(data, *profile, patchRevision)
		backup := filepath.Join(state, item.File+".original")
		if _, err := os.Lstat(backup); err == nil {
			binfmt.Require(!linked(backup), "An unrelated backup already exists; it was preserved.")
			bdata, err := os.ReadFile(backup)
			binfmt.Require(err == nil, "An unrelated backup already exists; it was preserved.")
			binfmt.Require(string(bdata) == string(data), "An unrelated backup already exists; it was preserved.")
		} else {
			atomicWrite(backup, data, true)
		}
		rev := patchRevision
		saved[item.File] = record{Original: digest(data), Patched: digest(patched), Revision: &rev}
		pending = append(pending, pendingT{path: path, data: data, patched: patched})
	}

	stateJSON, err := json.MarshalIndent(map[string]any{"patch_id": patchID, "files": saved}, "", "  ")
	binfmt.Require(err == nil, "Unable to serialize patch manifest.")
	atomicWrite(filepath.Join(state, "state.json"), stateJSON, false)

	var changed []filePair
	func() {
		defer func() {
			if r := recover(); r != nil {
				for i := len(changed) - 1; i >= 0; i-- {
					atomicWrite(changed[i].path, changed[i].data, false)
				}
				panic(r)
			}
		}()
		for _, p := range pending {
			cur, err := os.ReadFile(p.path)
			binfmt.Require(err == nil, "Unable to read executable.")
			binfmt.Require(digest(cur) == digest(p.data), "Emulator changed while applying. Try again.")
			atomicWrite(p.path, p.patched, false)
			changed = append(changed, filePair{p.path, p.data})
		}
	}()

	return inspectResolved(root, state)
}

// removeFolder mirrors emulator_patch.remove.
func removeFolder(folder string) View {
	if !inspectFolder(folder).CanRemove {
		binfmt.Fail("No safely removable patch found, or the emulator changed after patching.")
	}
	root, state, unlock := locked(folder)
	defer unlock()

	view := inspectResolved(root, state)
	binfmt.Require(view.CanRemove, "No safely removable patch found, or the emulator changed after patching.")

	records(state) // validate the manifest before modifying anything

	var changed []filePair
	func() {
		defer func() {
			if r := recover(); r != nil {
				for i := len(changed) - 1; i >= 0; i-- {
					atomicWrite(changed[i].path, changed[i].data, false)
				}
				panic(r)
			}
		}()
		for _, item := range view.Files {
			if item.Status != "patched" && item.Status != "outdated" {
				continue
			}
			path := filepath.Join(root, item.File)
			current, err := os.ReadFile(path)
			binfmt.Require(err == nil, "Unable to read executable.")
			binfmt.Require(digest(current) == item.SHA256, "Emulator changed while restoring. Try again.")
			original, err := os.ReadFile(filepath.Join(state, item.File+".original"))
			binfmt.Require(err == nil, "Unable to read backup.")
			atomicWrite(path, original, false)
			changed = append(changed, filePair{path, current})
		}
	}()

	return inspectResolved(root, state)
}
