// Package testutil resolves the optional Python reference environment used
// by the golden cross-validation tests.
//
// Resolution order for the interpreter (NSPBOX_PYTHON):
//  1. $NSPBOX_PYTHON explicitly
//  2. <repo root>/.pyref-venv/bin/python  (created by scripts/setup-pyref.sh)
//
// Resolution order for the reference sources (NSPBOX_PYREF):
//  1. $NSPBOX_PYREF explicitly
//  2. <repo root>/../nsp-box-src/src
//  3. <repo root>/nsp-box-src/src
//
// Set NSPBOX_REQUIRE_PYREF=1 to turn a missing reference environment from a
// graceful skip into a hard failure — used by `make golden` and CI so that
// byte-exactness claims stay verified rather than silently skipped.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// Python returns the path to the Python interpreter with Pillow/zstandard
// installed, if one can be found.
func Python() (string, bool) {
	if p := os.Getenv("NSPBOX_PYTHON"); p != "" {
		return p, true
	}
	root, ok := repoRoot()
	if !ok {
		return "", false
	}
	candidates := []string{
		filepath.Join(root, ".pyref-venv", "bin", "python"),
		filepath.Join(root, ".pyref-venv", "Scripts", "python.exe"), // windows venvs
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, true
		}
	}
	return "", false
}

// PyRef returns the directory containing the Python reference sources
// (the smarkyie/NSP-Box checkout's src/), if one can be found.
func PyRef() (string, bool) {
	if p := os.Getenv("NSPBOX_PYREF"); p != "" {
		return p, true
	}
	root, ok := repoRoot()
	if !ok {
		return "", false
	}
	candidates := []string{
		filepath.Join(root, "..", "nsp-box-src", "src"),
		filepath.Join(root, "nsp-box-src", "src"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "engine.py")); err == nil && !st.IsDir() {
			return c, true
		}
	}
	return "", false
}

// SkipUnlessPyRef skips t unless the full reference environment (interpreter
// AND sources) is available — or fails hard when NSPBOX_REQUIRE_PYREF=1.
func SkipUnlessPyRef(t testing.TB) (python, pyref string) {
	require := os.Getenv("NSPBOX_REQUIRE_PYREF") == "1"
	python, hasPython := Python()
	pyref, hasRef := PyRef()
	if hasPython && hasRef {
		if _, err := os.Stat(filepath.Join(pyref, "engine.py")); err != nil {
			if require {
				t.Fatalf("NSPBOX_REQUIRE_PYREF=1 but %s is not a reference checkout", pyref)
			}
			t.Skipf("reference sources incomplete at %s", pyref)
		}
		return python, pyref
	}
	missing := ""
	if !hasPython {
		missing = "interpreter"
	}
	if !hasRef {
		if missing != "" {
			missing += " + sources"
		} else {
			missing = "sources"
		}
	}
	if require {
		t.Fatalf("NSPBOX_REQUIRE_PYREF=1 but reference %s unavailable — run scripts/setup-pyref.sh", missing)
	}
	t.Skipf("python reference %s unavailable (see docs/PYREF.md)", missing)
	return "", ""
}

// repoRoot walks up from the current directory to the directory containing go.mod.
func repoRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for ; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
	}
	return "", false
}

// EnvOr is a small helper for other env lookups in tests.
func EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
