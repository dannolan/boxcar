# Changelog

## Unreleased

- **NUP optimizer: `boxcar optimize` / `nup --nup-operation optimize`.**
  Rewrites existing NUPs without extracting them: reuses zstd frames,
  compresses raw files with the presets, stores identical contents once and
  applies the large-file RAW rule (`--recompress` re-encodes frames).
  Lossless by default; outputs are verified file by file and compared with
  the source's logical content (reported as a logical SHA-256) before an
  exclusive commit or, with `--in-place`, an atomic replacement. Batch
  mode: files or folders, `--recursive`, `--jobs`, per-archive and overall
  progress, size statistics, skips already-optimized archives (`--min-gain`,
  `--force`), keeps going past failures. `--dry-run` previews with size
  estimates. `--latest-snapshot` is an opt-in consolidation. See
  docs/OPTIMIZE.md for KaiDump and nx2-flow workflows.
- **NUP deviations are reported.** Archives Boxcar accepts although the spec
  or the reference reader does not (KaiDump empty-file offsets, metadata
  before the blob region) get a note from `optimize` and from
  `nup --nup-operation verify`/`inspect` (`notes` in their JSON), and
  `optimize` rewrites them into compliant archives.
- **Faster NUP verification and extraction.** Read buffers and zstd
  decoders are reused across files, and alignment padding no longer
  allocates 1 MiB per extent; on a 67,000-file archive, verification went
  from 8.5 s to 1.4 s and optimizing from 14.4 s / 240 MB RSS to
  4.6 s / 128 MB.
- **NUP reader: accept third-party-packed archives.** Package metadata may
  live anywhere in the file (in-bounds, non-overlapping) instead of requiring
  `blobOff <= metaOff`. Real-world NUPs from other packers were rejected by
  the reference-implementation rule; Boxcar now reads them and verifies every
  extent. Regression-tested; golden cross-validation unaffected.
- **NUP packing: configurable storage policy.** `--large-file-threshold`
  (default `256MiB`, the reference value) sets the size above which
  `--large-files-raw` keeps files uncompressed, and `--min-savings PCT`
  (default `0`) stores a file raw unless zstd saves at least that share of it.
  Defaults reproduce the previous output byte for byte.
- **NUP reader: accept KaiDump empty files.** KaiDump stores the blob cursor
  as a zero-length file's extent offset; the reader now ignores that offset
  (size and codec must still be zero) instead of rejecting the archive.

## 4.0.0-boxcar.1 — 2026-11-08

Initial release. Full Go port of [NSP-Box 4.0.0](https://github.com/smarkyie/NSP-Box/releases/tag/v4.0.0) ("Meet NUP").

### Ported
- NSP builder for complete ExeFS/RomFS snapshots (Game tab feature parity, CLI)
- DLC packager for RomFS-only add-ons with base-game validation
- NUP v1 tools: pack, NSP↔NUP conversion, extract, inspect, verify (Fast/Balanced/Smaller presets, dedup, large-file raw rule)
- Snapshot discovery (base/updates/<version> bundles, NPDM title IDs, control.nacp preservation)
- Reversible emulator patch for reviewed Selevet/Eglantine/PokeDinner Windows x64 builds (byte-exact transform, backups, rollback)
- Plaintext-repack key placeholder setup
- Artwork: square-crop/fit to 256×256 JPEG library icons

### Differences from the original
- CLI only; no Tkinter GUI yet
- macOS/Linux supported (patch targets remain Windows x64 executables)
- Artwork decoders: PNG/JPEG/GIF/BMP/TIFF/WEBP (no AVIF/ICNS/JPEG2000)
- NUP compression uses Go's zstd backend; archives remain cross-compatible with the reference implementation
- NSP→NUP conversion stages region-backed files through temp files

### Validation
Byte-identical NSP/DLC output vs the Python reference; byte-exact patch transforms; NUP cross-verified in both directions; NSP→NUP→NSP round trip byte-identical. See README.
