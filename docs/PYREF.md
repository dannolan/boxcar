# Python Reference Environment (golden cross-validation)

Boxcar's byte-exactness claims are enforced by tests that run the original
Python implementation (NSP-Box) side-by-side with the Go code. The Python
environment is optional for development but **required** for the golden tests,
and CI always runs them.

## What the golden tests check

| Test | Direction |
|---|---|
| `internal/nsp` `TestGoldenCrossCheck` | Python builds an NSP → Go output must be byte-identical; Python's verifier must accept the Go NSP |
| `internal/dlc` `TestCrossCheckPython` | Python `dlc.build` → byte-identical NSP; Python `dlc.verify` accepts Go output |
| `internal/nup` `TestCrossValidation` | Python packs a NUP → Go verifies/extracts it; Go packs → Python verifies/extracts it |

## Setup

```bash
scripts/setup-pyref.sh
```

This clones the reference repo (pinned to commit
`c3de7a120cb4b3c86cd5b5bcdbf5f33bbffe0fc0` of `smarkyie/NSP-Box`) to
`../nsp-box-src` and creates `.pyref-venv` with `Pillow==12.2.0` and
`zstandard==0.25.0`. It prints the env vars to export.

## Environment variables

| Variable | Meaning |
|---|---|
| `NSPBOX_PYTHON` | Python interpreter with Pillow + zstandard (default: `<repo>/.pyref-venv/bin/python`) |
| `NSPBOX_PYREF` | Reference checkout's `src/` directory (default: `<repo>/../nsp-box-src/src`) |
| `NSPBOX_REQUIRE_PYREF=1` | **Fail instead of skip** when the reference env is missing |

Resolution is automatic: tests discover the venv and the sibling checkout
without any env vars. The one rule to remember:

> **Golden tests skip silently when the reference env is absent.**
> A green `go test ./...` on a fresh clone proves nothing about byte-exactness.
> Run `make golden` (sets `NSPBOX_REQUIRE_PYREF=1`) to make skips loud.

## Strict verification

```bash
make golden    # = NSPBOX_REQUIRE_PYREF=1 go test -run 'Cross|Golden' -count=1 ./internal/...
```

CI runs `make golden` on every push — if a change breaks byte-exactness,
CI is red, not silently green.

## Regenerating golden fixtures

The committed fixtures under `internal/patch/testdata/` and
`internal/nca/testdata/` were generated from the Python reference during the
port (`emulator_patch.transform` for patch pairs; `container_format` for the
NCA image). They are the contract — **never hand-edit the bytes**. If the
reference implementation's behavior intentionally changes, regenerate by
driving the Python reference directly and committing the new input/output
pairs together; `make golden` and CI will hold the Go code to them.

