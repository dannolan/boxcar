# Optimizer benchmarks

`go run ./scripts/bench` measures any number of boxcar binaries on the
deterministic fixtures in `internal/nup/nuptest/fixtures.go` (generated on
first use; nothing binary is committed) and, with `-real FILE`, on real
archives you supply. Real-game data never enters the repository.

```bash
go build -o /tmp/boxcar-new ./cmd/boxcar
go run ./scripts/bench -work /tmp/bench -scale 1 \
    -bin upstream=/path/boxcar-9702ddc -bin pr=/path/boxcar-bc81fee -bin new=/tmp/boxcar-new \
    [-real /path/Game.nup] -json results.json
```

How each binary is run: binaries with `optimize --profile` run every
profile and then optimize their own output again (the "2nd run" rows:
idempotence); binaries with plain `optimize` (the PR baseline `bc81fee`)
run their defaults (Balanced, large files raw above 256 MiB, any saving);
upstream `9702ddc` has no optimizer, so it runs the only route it offers,
`nup extract` followed by `nup pack`. Before every run the input is read
once so all binaries start with the same page cache.

Columns: wall and CPU (user+system) seconds; peak RSS of the measured
process; bytes read and written through system calls (`/proc` io
accounting, page cache included, so re-reads count); peak disk use of the
output folder sampled every 50 ms (temporary files and the extracted tree
included); output size and ratio; time of the binary's own `verify` on the
output and its logical throughput. A "skipped" second run writes nothing
(out = in). Machine: 8 vCPUs, 15 GB RAM, Linux, one virtual disk; Go
1.27; numbers vary by a few percent between runs.

## Synthetic fixtures (scale 1, about 2.3 GB)

| Fixture | What it holds |
|---|---|
| large-raw | a 384 MiB random file, a 320 MiB 40%-random file, 100 small files |
| incompressible | 1,500 random files (32-512 KiB) and 500 files already stored as zstd-19 frames |
| many-small | 40,000 compressible files of 0.5-8 KiB in 400 folders |
| duplicates | 200 contents stored raw (base), as zstd-1 frames (update) and as zstd-19 frames (DLC) |
| mixed | base of 800 files, update replacing a fifth, DLC; 35% random blocks |
| multi | a folder of four smaller mixed archives |

| scenario | binary | variant | status | in MiB | out MiB | ratio | wall s | CPU s | RSS MiB | read MiB | written MiB | peak disk MiB | verify s (MiB/s) |
|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| large-raw | upstream | extract+pack | packed | 729.0 | 717.8 | 0.985 | 3.65 | 3.57 | 62 | 2176 | 1447 | 1447 | 0.71 (1028) |
| large-raw | pr | default | 1 optimized | 729.0 | 717.8 | 0.985 | 2.07 | 1.83 | 15 | 1472 | 718 | 718 | 0.57 (1287) |
| large-raw | new | profile=runtime | 1 optimized | 729.0 | 717.8 | 0.985 | 1.72 | 2.31 | 82 | 1472 | 718 | 718 | 0.31 (2388) |
| large-raw | new | profile=runtime (2nd run) | 1 skipped | 717.8 | 717.8 | 1.000 | 0.02 | 0.02 | 25 | 1 | 0 | 0 | – |
| large-raw | new | profile=archive | 1 optimized | 729.0 | 581.0 | 0.797 | 12.06 | 13.49 | 403 | 1758 | 965 | 581 | 0.71 (1030) |
| large-raw | new | profile=archive (2nd run) | 1 skipped | 581.0 | 581.0 | 1.000 | 11.76 | 13.32 | 444 | 1794 | 1149 | 581 | – |
| large-raw | new | profile=fast | 1 optimized | 729.0 | 717.3 | 0.984 | 1.57 | 1.75 | 56 | 1471 | 717 | 717 | 0.29 (2503) |
| large-raw | new | profile=fast (2nd run) | 1 skipped | 717.3 | 717.3 | 1.000 | 0.01 | 0.01 | 8 | 1 | 0 | 0 | – |
| incompressible | upstream | extract+pack | packed | 420.9 | 421.5 | 1.001 | 7.61 | 9.80 | 70 | 1715 | 1294 | 897 | 0.87 (544) |
| incompressible | pr | default | 1 skipped | 420.9 | 420.9 | 1.000 | 0.04 | 0.04 | 12 | 59 | 0 | 0 | – |
| incompressible | new | profile=runtime | 1 optimized | 420.9 | 420.9 | 1.000 | 0.60 | 1.30 | 102 | 901 | 421 | 421 | 0.08 (6079) |
| incompressible | new | profile=runtime (2nd run) | 1 skipped | 420.9 | 420.9 | 1.000 | 0.49 | 1.36 | 102 | 901 | 421 | 421 | – |
| incompressible | new | profile=archive | 1 optimized | 420.9 | 420.9 | 1.000 | 1.15 | 6.18 | 375 | 901 | 421 | 421 | 0.08 (6148) |
| incompressible | new | profile=archive (2nd run) | 1 skipped | 420.9 | 420.9 | 1.000 | 1.10 | 5.76 | 379 | 901 | 421 | 421 | – |
| incompressible | new | profile=fast | 1 optimized | 420.9 | 420.9 | 1.000 | 0.50 | 1.24 | 90 | 901 | 421 | 421 | 0.08 (6062) |
| incompressible | new | profile=fast (2nd run) | 1 skipped | 420.9 | 420.9 | 1.000 | 0.51 | 1.27 | 93 | 901 | 421 | 421 | – |
| many-small | upstream | extract+pack | packed | 169.9 | 74.2 | 0.437 | 31.64 | 94.83 | 98 | 426 | 264 | 240 | 5.70 (29) |
| many-small | pr | default | 1 optimized | 169.9 | 74.2 | 0.437 | 2.74 | 2.77 | 70 | 262 | 78 | 74 | 1.07 (155) |
| many-small | new | profile=runtime | 1 optimized | 169.9 | 74.2 | 0.437 | 1.21 | 4.77 | 117 | 246 | 78 | 74 | 0.38 (437) |
| many-small | new | profile=runtime (2nd run) | 1 skipped | 74.2 | 74.2 | 1.000 | 0.19 | 0.25 | 54 | 20 | 0 | 0 | – |
| many-small | new | profile=archive | 1 optimized | 169.9 | 73.2 | 0.431 | 2.58 | 13.19 | 418 | 245 | 77 | 73 | 0.40 (417) |
| many-small | new | profile=archive (2nd run) | 1 skipped | 73.2 | 73.2 | 1.000 | 2.91 | 14.00 | 444 | 163 | 77 | 73 | – |
| many-small | new | profile=fast | 1 optimized | 169.9 | 75.8 | 0.446 | 0.98 | 3.61 | 83 | 247 | 79 | 76 | 0.34 (483) |
| many-small | new | profile=fast (2nd run) | 1 skipped | 75.8 | 75.8 | 1.000 | 0.16 | 0.21 | 51 | 20 | 0 | 0 | – |
| duplicates | upstream | extract+pack | packed | 90.3 | 18.6 | 0.206 | 1.89 | 2.32 | 63 | 270 | 180 | 180 | 0.22 (727) |
| duplicates | pr | default | 1 optimized | 90.3 | 18.6 | 0.206 | 1.11 | 1.10 | 20 | 163 | 19 | 19 | 0.12 (1336) |
| duplicates | new | profile=runtime | 1 optimized | 90.3 | 18.1 | 0.200 | 0.22 | 0.35 | 27 | 108 | 18 | 18 | 0.03 (6110) |
| duplicates | new | profile=runtime (2nd run) | 1 skipped | 18.1 | 18.1 | 1.000 | 0.01 | 0.01 | 9 | 0 | 0 | 0 | – |
| duplicates | new | profile=archive | 1 optimized | 90.3 | 18.1 | 0.200 | 0.58 | 3.33 | 356 | 108 | 18 | 18 | 0.03 (6252) |
| duplicates | new | profile=archive (2nd run) | 1 skipped | 18.1 | 18.1 | 1.000 | 0.45 | 3.21 | 348 | 36 | 18 | 18 | – |
| duplicates | new | profile=fast | 1 optimized | 90.3 | 18.1 | 0.200 | 0.21 | 0.33 | 30 | 108 | 18 | 18 | 0.02 (6807) |
| duplicates | new | profile=fast (2nd run) | 1 skipped | 18.1 | 18.1 | 1.000 | 0.01 | 0.01 | 17 | 0 | 0 | 0 | – |
| mixed | upstream | extract+pack | packed | 378.9 | 212.2 | 0.560 | 6.65 | 7.80 | 66 | 973 | 595 | 591 | 1.26 (300) |
| mixed | pr | default | 1 optimized | 378.9 | 212.2 | 0.560 | 3.32 | 3.18 | 23 | 656 | 213 | 212 | 0.67 (566) |
| mixed | new | profile=runtime | 1 optimized | 378.9 | 212.2 | 0.560 | 0.85 | 3.40 | 100 | 653 | 212 | 212 | 0.11 (3572) |
| mixed | new | profile=runtime (2nd run) | 1 skipped | 212.2 | 212.2 | 1.000 | 0.01 | 0.02 | 12 | 4 | 0 | 0 | – |
| mixed | new | profile=archive | 1 optimized | 378.9 | 207.1 | 0.547 | 3.13 | 13.33 | 359 | 648 | 207 | 207 | 0.10 (3668) |
| mixed | new | profile=archive (2nd run) | 1 skipped | 207.1 | 207.1 | 1.000 | 1.82 | 12.83 | 381 | 417 | 207 | 207 | – |
| mixed | new | profile=fast | 1 optimized | 378.9 | 207.0 | 0.546 | 0.65 | 2.66 | 66 | 648 | 207 | 207 | 0.11 (3523) |
| mixed | new | profile=fast (2nd run) | 1 skipped | 207.0 | 207.0 | 1.000 | 0.01 | 0.02 | 10 | 3 | 0 | 0 | – |
| multi | pr | default | 4 optimized | 500.1 | 280.2 | 0.560 | 5.37 | 5.25 | 54 | 1032 | 281 | 280 | – |
| multi | new | profile=runtime | 4 optimized | 500.1 | 280.2 | 0.560 | 1.00 | 5.77 | 244 | 1028 | 280 | 280 | – |
| multi | new | profile=archive | 4 optimized | 500.1 | 273.4 | 0.547 | 3.89 | 22.64 | 629 | 1021 | 274 | 273 | – |
| multi | new | profile=fast | 4 optimized | 500.1 | 273.2 | 0.546 | 0.70 | 4.13 | 214 | 1021 | 273 | 273 | – |
| multi | new | profile=runtime,jobs=2 | 4 optimized | 500.1 | 280.2 | 0.560 | 1.40 | 5.64 | 297 | 1028 | 280 | 280 | – |

## Real archives (KaiDump exports, not redistributable)

Mario Party Superstars (2.9 GB, 820 files) and Animal Crossing: New
Horizons (6.7 GB, 67,000 files), both raw KaiDump NUP exports.

| scenario | binary | variant | status | in MiB | out MiB | ratio | wall s | CPU s | RSS MiB | read MiB | written MiB | peak disk MiB | verify s (MiB/s) |
|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Mario Party Superstars | upstream | extract+pack | packed | 2781.9 | 2732.1 | 0.982 | 17.62 | 18.21 | 64 | 8721 | 5940 | 5514 | 3.45 (806) |
| Mario Party Superstars | pr | default | 1 optimized | 2781.9 | 2732.1 | 0.982 | 8.88 | 8.50 | 43 | 6003 | 3135 | 2732 | 2.97 (935) |
| Mario Party Superstars | new | profile=runtime | 1 optimized | 2781.9 | 2742.8 | 0.986 | 3.97 | 10.12 | 341 | 5935 | 3084 | 2743 | 0.51 (5491) |
| Mario Party Superstars | new | profile=runtime (2nd run) | 1 skipped | 2742.8 | 2742.8 | 1.000 | 4.15 | 9.01 | 335 | 5896 | 3084 | 2743 | – |
| Mario Party Superstars | new | profile=archive | 1 optimized | 2781.9 | 2710.8 | 0.974 | 20.94 | 54.03 | 684 | 5557 | 2711 | 2711 | 0.46 (6096) |
| Mario Party Superstars | new | profile=archive (2nd run) | 1 skipped | 2710.8 | 2710.8 | 1.000 | 105.40 | 210.40 | 768 | 5843 | 3069 | 3757 | – |
| Mario Party Superstars | new | profile=fast | 1 optimized | 2781.9 | 2738.7 | 0.984 | 3.37 | 8.81 | 270 | 5638 | 2792 | 2739 | 0.47 (5911) |
| Mario Party Superstars | new | profile=fast (2nd run) | 1 skipped | 2738.7 | 2738.7 | 1.000 | 3.00 | 6.03 | 208 | 5594 | 2792 | 2739 | – |
| Animal Crossing NH | upstream | extract+pack | error: upstream rejects KaiDump empty-file offsets | 6373.4 | 0.0 | 0.000 | 0.07 | 0.08 | 47 | 8 | 0 | 0 | – |
| Animal Crossing NH | pr | default | 1 optimized | 6373.4 | 6167.7 | 0.968 | 30.77 | 28.37 | 138 | 17595 | 9905 | 6168 | 6.58 (967) |
| Animal Crossing NH | new | profile=runtime | 1 optimized | 6373.4 | 6178.0 | 0.969 | 8.65 | 26.88 | 536 | 12653 | 6230 | 6178 | 1.16 (5464) |
| Animal Crossing NH | new | profile=runtime (2nd run) | 1 skipped | 6178.0 | 6178.0 | 1.000 | 9.64 | 25.53 | 515 | 12456 | 6230 | 6178 | – |
| Animal Crossing NH | new | profile=archive | 1 optimized | 6373.4 | 6078.7 | 0.954 | 35.99 | 154.10 | 875 | 12507 | 6086 | 6079 | 1.26 (5032) |
| Animal Crossing NH | new | profile=archive (2nd run) | 1 skipped | 6078.7 | 6078.7 | 1.000 | 30.43 | 156.68 | 968 | 12257 | 6133 | 6079 | – |
| Animal Crossing NH | new | profile=fast | 1 optimized | 6373.4 | 6198.9 | 0.973 | 10.06 | 26.88 | 330 | 12628 | 6206 | 6199 | 1.28 (4969) |
| Animal Crossing NH | new | profile=fast (2nd run) | 1 skipped | 6198.9 | 6198.9 | 1.000 | 7.96 | 19.83 | 414 | 12452 | 6206 | 6199 | – |

### Final round: memory budget, counting measurement, `fast` policy

`before` is 9748b50 (the code of the table above), `after` is this
branch's head; same machine, same method. First runs are within noise
of each other (same bytes for `runtime` and `archive`; `fast` now uses
`runtime`'s policy, see OPTIMIZE.md). Second runs no longer write
anything: a likely-discarded rewrite is first measured with a writer
that only counts.

| archive | profile | before: time / RSS | after: time / RSS | after: size | 2nd run before: time / written | 2nd run after: time / written |
|---|---|---|---|---|---|---|
| Mario Party | runtime | 4.07 s / 340 MB | 3.29 s / 357 MB | 2743 MiB | 4.07 s / 3084 MiB | **1.57 s / 0** |
| Mario Party | archive | 25.9 s / 687 MB | 25.7 s / 702 MB | 2711 MiB | 34.2 s / 3069 MiB | **27.6 s / 0** |
| Mario Party | fast | 3.03 s / 270 MB | 3.17 s / 258 MB | 2748 MiB (was 2739) | 3.10 s / 2792 MiB | **1.39 s / 0** |
| Animal Crossing | runtime | 9.87 s / 558 MB | 9.07 s / 564 MB | 6178 MiB | 8.34 s / 6230 MiB | **1.63 s / 0** |
| Animal Crossing | archive | 29.3 s / 681 MB | 32.0 s / 908 MB | 6079 MiB | 33.7 s / 6133 MiB | **30.7 s / 0** |
| Animal Crossing | fast | 9.17 s / 486 MB | 8.64 s / 342 MB | 6209 MiB (was 6199) | 7.99 s / 6206 MiB | **1.56 s / 0** |

RSS of the same run varies by up to ~30% between runs (the archive
profile on Animal Crossing measured 681, 875 and 908 MB on the same
code), because how much freed memory the Go runtime still holds when the
peak is sampled varies. The automatic budget on this 16 GB machine is
2 GiB, which these runs never reach, so it does not change them; a
smaller budget does (`--memory`, same outputs byte for byte):

| archive | profile | `--memory auto` (2 GiB) | `--memory 512MiB` | `--memory 256MiB` |
|---|---|---|---|---|
| Mario Party | runtime | 4.27 s / 366 MB | 3.62 s / 386 MB | 4.27 s / 319 MB |
| Mario Party | archive | 23.9 s / 717 MB | 25.3 s / 556 MB | 33.0 s / 285 MB |
| Animal Crossing | runtime | 9.12 s / 544 MB | 9.28 s / 369 MB | 9.01 s / 411 MB |
| Animal Crossing | archive | 27.8 s / 912 MB | 35.1 s / 545 MB | 73.6 s / 514 MB |

The budget bounds the optimizer's working memory (encoders, decoders,
read-ahead, estimation, verification), not RSS: Animal Crossing's 67,000-
file table and plan and the runtime's unreturned memory come on top, so
its RSS stays around 400-500 MB however small the budget. Under 256 MiB
the archive profile gets 2 zstd-19 workers instead of 8 and takes about
2.6x as long.

## What the numbers say

- **Throughput** (runtime profile vs the PR baseline): Mario Party 8.9 s
  -> 4.0 s, Animal Crossing 30.8 s -> 8.7 s, many-small 2.7 s -> 1.2 s,
  mixed 3.3 s -> 0.85 s, four archives 5.4 s -> 1.0 s. CPU time is about
  the same or slightly higher (SHA-256 and zstd work is spread over cores;
  raw fallbacks no longer re-read and re-hash). Upstream's extract+pack is
  2-25x slower than either and needs the extracted tree on disk (peak disk
  2x the archive). Upstream cannot read the Animal Crossing export at all
  (KaiDump's empty-file offsets).
- **Verification** runs at 5-6 GiB/s logical on the real archives instead
  of about 0.9 GB/s.
- **I/O**: the baseline wrote 9.9 GB for a 6.2 GB Animal Crossing output
  (frames written, truncated, rewritten raw); the new path writes 6.2 GB.
- **Memory** is the price: peak RSS 330-560 MB for the runtime profile on
  the real archives (baseline 43-138 MB), from up to 128 MiB of read-ahead,
  one encoder per worker and the 67,000-file table. Working memory is
  bounded by the `--memory` budget (automatic: 1/8 of RAM within
  256 MiB-2 GiB), not by file sizes; see the table above.
- **Profiles**: `archive` (zstd 19 + recompress, no large-file rule) is
  1.2-2.6% smaller than `runtime` on these inputs (Mario 2711 vs 2743 MiB,
  Animal Crossing 6079 vs 6178 MiB, large-raw 581 vs 718 MiB because it
  compresses the big pack) at 4-5x the time and 680-970 MB RSS. `fast` now uses
  `runtime`'s storage policy at level 1: on the real archives it is within
  0.2% of `runtime` in size and about as fast (mostly incompressible data:
  reading, hashing and writing dominate); on the compressible synthetic
  fixtures (after binary, scale 1) it is 17-20% faster (many-small 0.99 vs
  1.19 s, mixed 0.72 vs 0.89 s, multi 0.87 vs 1.07 s) and up to 2% larger
  (many-small 75.8 vs 74.2 MiB; mixed and multi come out 2.5% smaller). Higher compression and more concurrency are not free;
  `runtime` stays the default for nx2-flow.
- **Duplicates**: choosing the best existing copy makes the duplicates
  fixture 18.1 MiB instead of 18.6 MiB and 5x faster (no re-compression
  of a raw copy when a zstd-19 frame exists).
- **Archive concurrency**: on the four-archive folder, the automatic job
  count (4 here) is 1.0 s vs 1.4 s with 2 jobs and 2.3 s with 1.
- **Idempotence**: every second run is `skipped` and writes nothing.
  When the estimate is uncertain, Boxcar measures the rewrite with a
  counting writer instead of writing it: Mario 1.6 s and Animal Crossing
  1.6 s for runtime (were 4.1 s and 8.3 s with a full temporary write);
  the archive profile still re-encodes everything (28-31 s), because a
  zstd frame does not record its level.
