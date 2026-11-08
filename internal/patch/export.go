package patch

import (
	"context"

	"github.com/dannolan/boxcar/internal/binfmt"
)

const (
	patchID  = "embedded-npdm-v1"
	stateDir = ".s2gp-patch"
)

// names are the recognized emulator executable names (case-insensitive match).
var names = []string{"eglantine.exe", "pokedinner.exe", "PokeDinner.exe", "eden.exe", "eden-cli.exe"}

// Profile describes a recognized (or experimentally matched) loader build.
type Profile struct {
	Name                    string `json:"name"`
	File                    string `json:"file"`
	ABI                     string `json:"abi"`
	Hook                    int    `json:"hook"`
	Expected                string `json:"expected"`
	Kind                    string `json:"kind"`
	Verified                bool   `json:"verified,omitempty"`
	PreserveDuplicateUnwind bool   `json:"preserve_duplicate_unwind,omitempty"`
}

// FileStatus is one inspected executable in a View.
type FileStatus struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Status string `json:"status"`
	Build  string `json:"build"`
}

// View is the result of Inspect/Apply/Remove.
type View struct {
	Folder    string       `json:"folder"`
	Files     []FileStatus `json:"files"`
	CanApply  bool         `json:"can_apply"`
	CanRemove bool         `json:"can_remove"`
}

// Transform applies the patch to a verified Windows x64 executable, returning
// the patched bytes. It is byte-identical to the Python reference for a given
// exe + profile.
func Transform(exe []byte, profile Profile) (result []byte, err error) {
	defer binfmt.Recover(&err)
	return transform(exe, profile, patchRevision), nil
}

// Inspect scans an emulator folder and reports each executable's status.
func Inspect(ctx context.Context, folder string) (view View, err error) {
	defer binfmt.Recover(&err)
	binfmt.Check(ctx)
	return inspectFolder(folder), nil
}

// Apply patches every supported unpatched executable in the folder.
func Apply(ctx context.Context, folder string) (view View, err error) {
	defer binfmt.Recover(&err)
	binfmt.Check(ctx)
	return applyFolder(folder), nil
}

// Remove restores every patched executable to its original bytes.
func Remove(ctx context.Context, folder string) (view View, err error) {
	defer binfmt.Recover(&err)
	binfmt.Check(ctx)
	return removeFolder(folder), nil
}
