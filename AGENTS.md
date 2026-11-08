# AGENTS.md — operating guide for AI coding agents

@meta {
  tool: any-agent (Claude Code, Codex, Cursor, OpenCode, ...)
  language: Go 1.27
  module: github.com/dannolan/boxcar
  reference-implementation: smarkyie/NSP-Box @ c3de7a1 (Python)
  last-updated: 2026-11-08
}

## What this repo is

Boxcar: a Switch 2 game packager (NSP builder, NUP archives, DLC, reversible
emulator patch) — a Go port of smarkyie/NSP-Box. The Python repo is the
**behavioral spec**: where Python and Go differ, that difference is either a
documented deviation (see ARCHITECTURE.md § Deviations) or a bug.

## Commands (CI parity — run before declaring done)

```bash
make check    # build + vet + gofmt + full test suite (what CI runs)
make test     # tests only
make golden   # STRICT cross-validation vs the Python reference (fails if env missing)
make race     # race detector on the concurrent hot packages
make cross    # cross-compile all release targets
```

Plain Go, no cgo: `go build ./... && go vet ./... && go test ./internal/... ./cmd/... -count=1`.

## Non-negotiables

1. **Byte-exactness is the contract.** NSP/DLC output and patch `Transform()`
   output must stay byte-identical to the Python reference for identical
   inputs. NUP container layout must stay cross-compatible (compressed extent
   bytes may differ; everything else may not). After touching any format code,
   run `make golden`.
2. **Never edit:**
   - `internal/patch/data/` — embedded native payloads + pattern masks (data, not code)
   - `internal/patch/testdata/`, `internal/nca/testdata/` — golden fixtures (regenerate via docs/PYREF.md, never hand-edit)
   - `licenses/`, `LICENSE`, `DISCLAIMER.md` — legal files
3. **Keystones may be extended but not reshaped** (`internal/binfmt`,
   `internal/stream`, `internal/region`, `internal/diskfree`, `internal/testutil`):
   every format package builds on them; changing their semantics invalidates
   all golden tests at once.
4. **Pinned APIs live in ARCHITECTURE.md § Contracts.** If you must change a
   public signature of `pfs0`/`romfs`/`nca`/`metadata`/`nup`/`nsp`/`dlc`/
   `nuptools`/`patch`, update ARCHITECTURE.md in the same commit.
5. **Python parity beats tidiness.** Odd-looking code that mirrors the Python
   (odd error strings, exact offsets, weird `require` ordering) is deliberate.
   Do not "clean up" validation order or error messages — tests match on them.
6. **No mmap anywhere.** Random access uses `internal/region` (pread-based).
7. **No overwrite commits.** Outputs go to `name.<10-hex>.partial`, then a
   platform-specific exclusive commit (`commit_*.go` / `platform_*.go` files).
   Never use bare `os.Rename` for commit on Windows — it replaces.
8. **Symlinks/reparse points are refused** with the reference error strings.

## Conventions

- Errors: dense validation uses `binfmt.Require(cond, msg, ...)` (panics) +
  `defer binfmt.Recover(&err)` on exported functions; plain `errors` are fine
  for leaf helpers. Error text mirrors Python wherever a test asserts it.
- Cancellation: `binfmt.Check(ctx)` at the same places Python calls
  `checkpoint()`; plumb `context.Context` as first arg everywhere.
- Progress: `binfmt.Progress` = `func(label string, done float64)` (0–100);
  keep the Python label/percent phase ranges when porting a flow.
- zstd: klauspost levels map 1→SpeedFastest, 3→SpeedDefault, 9→SpeedBetterCompression.
- Versions: bump `cmd/boxcar/main.go` `const version` AND add a CHANGELOG.md
  section in the same commit; releases are tags `v<version>` (workflow builds
  darwin universal/arm64/amd64, windows amd64/arm64, linux amd64/arm64).

## Golden tests — read this before trusting a green suite

Golden cross-validation tests **skip silently** when the Python reference env
is missing. A green `go test ./...` on a fresh clone proves nothing about
byte-exactness.

```bash
scripts/setup-pyref.sh   # clones reference @ pinned SHA + pinned Pillow/zstandard
make golden              # NSPBOX_REQUIRE_PYREF=1 — skips become failures
```

Env vars: `NSPBOX_PYTHON`, `NSPBOX_PYREF`, `NSPBOX_REQUIRE_PYREF` (full docs:
docs/PYREF.md). Resolution is automatic — venv at `<repo>/.pyref-venv`,
sources at `<repo>/../nsp-box-src/src`.

## Map

| Package | Python source | One-liner |
|---|---|---|
| `internal/binfmt` | — | Require/Recover/Check, LE packing, Align |
| `internal/stream` | container_format.Blob | lazy multi-pass composite byte stream |
| `internal/region` | verify_format.Region | bounded pread window (no mmap) |
| `internal/pfs0` | container_format.filesystem/package, verify_format.partition | PFS0/HFS0 writer+reader |
| `internal/romfs` | container_format.romfs, verify_format.romfs_files | RomFS builder + bounded walker |
| `internal/nca` | container_format.{pfs,romfs,nca}, verify_format.nca_sections | NCA sections + IVFC tree |
| `internal/metadata` | engine/dlc CNMT+NACP, NPDM | title metadata builders |
| `internal/snapshot` | engine.find_snapshots/source_inventory | dump discovery + stamps |
| `internal/nsp` | engine.build/verify_image | NSP engine + independent verifier |
| `internal/dlc` | dlc.py | DLC packager |
| `internal/nup` | nup_format.py | NUP v1 format (pack/Archive/extract) |
| `internal/nuptools` | nup_tools.py | discovery/merge/conversion orchestration |
| `internal/patch` | emulator_patch*.py | PE surgery + embedded payloads |
| `internal/artwork` | artwork.py | decode/crop → 256×256 JPEG icons |
| `internal/keys` | compatibility.py | key placeholder setup |
| `cmd/boxcar` | app.py CLI, nup_cli.py | CLI entry |

Full details + contracts: ARCHITECTURE.md. Golden env: docs/PYREF.md.
