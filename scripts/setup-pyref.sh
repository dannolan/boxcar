#!/usr/bin/env bash
# Set up the Python reference environment used by the golden cross-validation
# tests. Creates:
#   <repo>/.pyref-venv      — Python venv with pinned Pillow + zstandard
#   ../nsp-box-src          — the reference implementation checkout (pinned)
# Prints the env vars the tests honor. Idempotent: re-running repairs/updates.
#
# Usage: scripts/setup-pyref.sh
set -euo pipefail

PYREF_COMMIT="c3de7a120cb4b3c86cd5b5bcdbf5f33bbffe0fc0" # smarkyie/NSP-Box main
REPO="$(cd "$(dirname "$0")/.." && pwd)"
SRC="${NSPBOX_PYREF:-$REPO/../nsp-box-src}"
VENV="$REPO/.pyref-venv"

echo "==> reference sources: $SRC"
if [ ! -f "$SRC/src/engine.py" ]; then
  git clone https://github.com/smarkyie/NSP-Box "$SRC"
fi
if [ ! -f "$SRC/src/engine.py" ]; then
  echo "engine.py not found in $SRC/src — set NSPBOX_PYREF to a checkout's src/ dir" >&2
  exit 1
fi
if ! (cd "$SRC" && git rev-parse HEAD) | grep -q "$PYREF_COMMIT"; then
  echo "==> pinning reference checkout to $PYREF_COMMIT"
  git -C "$SRC" fetch origin main >/dev/null 2>&1 || true
  git -C "$SRC" checkout -q "$PYREF_COMMIT"
fi

echo "==> virtualenv: $VENV"
PYBIN="${NSPBOX_PYTHON_BIN:-python3}"
"$PYBIN" -m venv "$VENV"
"$VENV/bin/pip" install --disable-pip-version-check -q 'Pillow==12.2.0' 'zstandard==0.25.0'
"$VENV/bin/python" -c "import PIL, zstandard; print('==> reference env OK:', PIL.__version__, zstandard.__version__)"

cat <<EOF

==> Add to your shell (or export in your editor/agent config):
    export NSPBOX_PYTHON="$VENV/bin/python"
    export NSPBOX_PYREF="$SRC/src"
    export NSPBOX_REQUIRE_PYREF=1   # optional: fail instead of skip when missing

==> Verify strictly:   make golden
EOF
