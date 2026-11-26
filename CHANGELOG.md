# Changelog

## Unreleased

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
