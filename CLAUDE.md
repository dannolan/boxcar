# CLAUDE.md

Read AGENTS.md first — it is this repo's agent operating guide (commands,
non-negotiables, conventions, package map).

The short version for Claude Code sessions:

- Run `make check` before declaring anything done; run `make golden` after
  touching any format code (`internal/pfs0`, `romfs`, `nca`, `metadata`,
  `nup`, `nsp`, `dlc`, `patch`).
- Byte-exactness vs the Python reference is the contract; "Python parity beats
  tidiness" — do not refactor validation order, error strings, or offsets.
- Golden tests skip silently without the reference env (see docs/PYREF.md,
  `scripts/setup-pyref.sh`).
- Never edit `internal/patch/data/`, `internal/patch/testdata/`,
  `internal/nca/testdata/`, `licenses/`.
