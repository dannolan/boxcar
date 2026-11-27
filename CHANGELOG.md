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
- **Optimizer safety hardening** (review fixes): in-place runs take an
  advisory lock (macOS/Linux), refuse a source that was swapped for another
  file even with the same size and time, and write the partial copy with
  the original's permissions; outputs on exFAT/FAT32 are published with an
  exclusive rename; policy violations in duplicate extents are rewritten;
  rewrites match Pack's alignment byte for byte; `--no-latest-snapshot`
  overrides `--latest-snapshot`.
- **Optimizer publication and in-place safety (merge-blocking fixes).**
  The placeholder-file fallback for filesystems without hard links is gone:
  new archives are published with a hard link or an exclusive rename, and
  where neither exists the run fails with nothing published. In-place runs
  refuse to proceed unlocked; Windows gets a real lock (no-share,
  delete-on-close sidecar) and `ReplaceFileW`, which keeps ACLs and
  attributes. On macOS/Linux the replacement keeps the original's mode,
  owner and extended attributes (ACLs), and a failure to carry them over
  keeps the original instead of being ignored. Concurrent jobs share one
  free-space reservation per drive. `Pack` registers its temp-file cleanup
  only after the exclusive create succeeded and also publishes through the
  exclusive commit (works on exFAT/FAT32).
- **Optimizer: best duplicate extent, profiles, planning.** When one
  content is stored several times (raw, fast frame, strong frame), the
  output is built from the best copy the policy allows instead of the
  first; dropped copies are still checked. `--profile runtime` (default;
  nx2-flow: Balanced, files over 128 MiB raw, 5% minimum per-file saving),
  `archive` (zstd 19 "Best", no large-file rule, frames re-encoded) and
  `fast`; explicit flags override single settings. Plans report
  `savings_basis` (exact / estimated / lower_bound) and `exact_savings`.
  Explicitly named archives get any saving (`--min-gain` auto: 1% only for
  archives found in folders), and an uncertain skip is measured by a real
  rewrite instead of trusted; a recompression that saves nothing is
  discarded, so repeated runs write nothing. New status `normalized`
  (rewritten for a policy fix or spec deviation without getting smaller).
- **Optimizer throughput.** Files up to 32 MiB are read, checked and
  encoded by one worker per CPU ahead of a single in-order writer (output
  identical for any worker count) under process-wide CPU and 128 MiB
  memory budgets; raw fallbacks no longer re-read the source; verification
  runs on all CPUs; `--jobs 0` runs up to four archives at once. Mario
  Party Superstars (2.9 GB) 10.9 s -> about 3.5 s, Animal Crossing
  (6.7 GB, 67k files) 39.6 s -> about 9-10 s on 8 CPUs.
- **Optimizer working-memory budget.** Encoders (all levels, including
  zstd 19), decoders, read-ahead, estimation, verification and `Pack`
  reserve from one process-wide budget shared by concurrent archives:
  automatic (1/8 of RAM within 256 MiB-2 GiB) or `--memory SIZE`. Writes
  adapt their worker count to it, cannot deadlock each other, and return
  everything on cancellation. The budget bounds working memory, not RSS.
- **Windows in-place replacement handles `ReplaceFileW` partial
  failures.** The original is always moved to a backup name and the
  outcome is checked by file ID: 1176-style failures keep the original,
  1177 (original moved, replacement not) moves it back, and if that fails
  nothing is deleted and the error names both files. Windows tests now run
  in CI (with simulated failures), which also fixed Pack publishing a still
  open file on Windows.
- **Estimation and measurement.** Sampled estimates apply the per-file
  storage policy (minimum savings) as the writer will; dry runs report
  action `measure` when a real run would decide by measuring; rewrites that
  are likely to be discarded are measured with a counting writer first, so
  already-optimized second runs write nothing (Mario Party 4.2 -> 1.7 s,
  Animal Crossing 10.5 -> 2.8 s).
- **`fast` profile uses `runtime`'s storage policy** (128 MiB, 5%) at level
  1: the policy decides how nx2-flow loads the archive, so only the
  compression effort differs.
- **NUP archives in parts (nx2-flow's convention).** Reading accepts the
  first part or a folder of parts in every naming nx2-flow's loader
  accepts; `optimize --split SIZE|fat32|auto|none` writes `<name>.001`,
  `<name>.002`, ... like nx2-flow's receiver, automatically on FAT
  volumes, published last part first so a game appears only complete.
- **Benchmarks.** `go run ./scripts/bench` compares boxcar binaries on
  deterministic synthetic fixtures (`internal/nup/nuptest.Fixtures`) and
  optionally real archives: wall, CPU, peak RSS, bytes read/written, peak
  disk, ratio, verify throughput and idempotence.
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
