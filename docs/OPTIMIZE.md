# Optimizing NUP archives

`boxcar optimize` rewrites existing NUP archives without extracting them. It
reuses compressed data that is already good, compresses what was stored raw,
stores identical files once (from the best of their copies), applies the
storage policy of a profile, and verifies the result before anything
replaces an original. It works on single archives, archives kept in parts
for FAT32 cards, and whole game libraries.

```bash
boxcar optimize game.nup --dry-run                  # preview; writes nothing
boxcar optimize game.nup --output game.small.nup    # new file; original untouched
boxcar optimize game.nup --output small.nup --profile archive   # smallest, for storage
boxcar optimize ./games --recursive --dry-run       # preview a library
boxcar optimize ./games --recursive --in-place      # replace originals after verification
boxcar optimize Game.nup.001 --output /sd/nx2-flow/games/Game.nup --split fat32
```

`boxcar optimize PATH...` is shorthand for the NUP tool syntax
`boxcar nup --nup-operation optimize --input PATH` (repeat `--input` for
several paths). Flags and paths can come in any order. Every run needs one
of `--output`, `--in-place` or `--dry-run`.

## What changes and what does not

Optimization changes how the archive is **stored**, never what it
**contains**. These are preserved exactly:

- the package list and its order: title IDs, versions, kinds (base, update,
  DLC) and flags;
- each package's metadata block, byte for byte, including TLV types Boxcar
  does not know (vendor types, other tools' numbering);
- every directory (including empty ones), every file path and every file's
  bytes;
- the package records' `exefs`/`romfs` directory pointers and root names.

Together these form the archive's **logical content**. Boxcar summarizes it
as a *logical SHA-256*: two archives with the same logical SHA-256 that both
pass `boxcar nup --nup-operation verify` hold the same games, byte for byte,
however they are stored.

What may change is physical: which files are compressed, deduplication,
extent order and alignment, where metadata sits, and the TOC layout (always
the same canonical layout `boxcar nup --nup-operation pack` writes).
Zero-length files get the extent offset 0 that the NUP spec requires.

Content-altering operations are opt-in. Today there is one:
`--latest-snapshot` merges a base game and its updates into the latest
runnable snapshot (the same merge `pack` performs by default), keeping DLC.

### How each file is stored

Each distinct file content (same SHA-256 and size) is stored once; every
other file with that content points at the same extent. When the source
holds one content several times in different forms (raw in the base, a
fast frame in an update, a strong frame in a DLC), the output is built from
the best copy the policy allows: the smallest compliant frame, or a raw copy
when the policy wants the file raw; it is never re-compressed from a worse
copy. Every dropped copy is still read and checked against the SHA-256.

| Stored in the source as | Storage policy says | Result |
|---|---|---|
| zstd | compress | frame reused verbatim (`--recompress`: re-encoded with `--preset`, smaller frame kept) |
| zstd | raw: larger than `--large-file-threshold`, or saves less than `--min-savings` | decompressed and stored raw |
| raw | compress | compressed with `--preset`; kept raw when zstd does not save enough |
| raw | raw: larger than `--large-file-threshold` | copied verbatim |

The policy is the one `pack` uses, with the profile's settings (see
[Profiles](#profiles)): files above `--large-file-threshold` stay raw when
the large-file rule is on, and a compressed file is kept only if it is at
least `--min-savings` percent smaller than the raw file (any saving at 0).

An archive counts as **already optimized** when the estimated savings are
below the minimum gain, nothing breaks the storage policy and it follows
the NUP spec. Such archives are skipped unless you pass `--force`. The
minimum gain is 1% of the archive for archives found by searching a folder
and any saving at all for archives you name explicitly (`--min-gain`
overrides both). For an explicitly named archive whose estimate is not
exact (sampled, or `--recompress`, whose gain cannot be estimated), a
"skip" verdict is not trusted: Boxcar measures the real rewrite, keeps it
if it saves the minimum gain (at least one byte) and otherwise reports
`skipped` with the measured result. A dry run says so with action
`measure` ("would measure"). Such likely-to-be-discarded rewrites, and any
sampled estimate under 1% of the archive, are first run against a writer
that only counts bytes, so a skip costs the encoding time but writes
nothing; a rewrite that does reach the minimum gain is then written and
verified (one extra pass). Any other rewrite is written to a temporary file
first, and if its measured size misses the minimum gain it is discarded: a
skipped rewrite never replaces or publishes anything, but it may have
written a temporary file. A second run over the result of `--recompress`
or the archive profile spends the encoding time again, because a zstd
frame does not record the level it was made with.

Archives whose compressed files break the policy (for example a 1 GiB file
compressed by another tool while the threshold is 128 MiB) are always
rewritten, even if the result is larger, because that is what the policy
asks for.

### Archives that deviate from the spec

Boxcar reads two kinds of archives that the NUP spec or the NSP-Box reader
does not allow: KaiDump's empty files with a nonzero extent offset, and
package metadata stored before the blob region. `boxcar optimize` (and
`nup --nup-operation verify`/`inspect`) prints a `note:` line for them, and
optimizing rewrites them even when there is nothing to save, because the
rewrite is compliant (`normalized:` in the output). The JSON lists them
under `plan.deviations` (`notes` for verify/inspect).

## Profiles

A profile is a named starting point for the storage policy; any explicit
flag (`--preset`, `--large-file-threshold`, `--min-savings`,
`--recompress`, `--no-large-files-raw`) overrides its one setting. There is
only one policy implementation; profiles are presets of it.

| Profile | Level | Large files raw | Min savings per file | Existing frames | For |
|---|---|---|---|---|---|
| `runtime` (default) | Balanced (3) | above 128 MiB | 5% | reused | archives nx2-flow loads from the card |
| `archive` | Best (zstd 19) | no | any | re-encoded, smaller kept | storage and transfer only |
| `fast` | Fast (1) | above 128 MiB | 5% | reused | `runtime`'s policy with less compression effort |

Why these values:

- **128 MiB** (`runtime`): nx2-flow's loader (checked against 0.1.7,
  `loader/runtime/game.c`) unpacks a compressed file whole, on first read,
  into a cache of max(192 MiB, largest compressed file + 64 MiB), capped at
  the total of the compressed files, taken from the memory the game could
  use (games with 1 GB or more of "apart" memory use max(largest + 16 MiB,
  48 MiB) instead). Keeping every compressed file at or under 128 MiB keeps
  that cache at its 192 MiB floor. What matters is the file's unpacked size;
  the loader streams the frame in 1 MiB pieces, so the frame size does not.
  Its decoder keeps zstd's default window limit (2^27 = 128 MiB); Boxcar's
  encoder uses an 8 MiB window at every level, so all frames load.
- **5%** (`runtime`): a heuristic, not an nx2-flow requirement (the
  loader reads raw and compressed files alike, whatever they save). A file that zstd
  shrinks by less than 5% saves almost nothing on the card but costs a full
  unpack into the cache and decompression time on the console; storing it
  raw lets the loader read it in place. Measured on Mario Party Superstars
  it keeps 0.4% of the archive raw that 0% would compress.
- **`fast` = `runtime`'s policy at level 1.** The storage policy decides
  how nx2-flow loads the archive (which files it unpacks into its cache),
  so a profile picked for speed keeps it and only spends less compression
  effort. Measured (8 CPUs, warm cache): with the former `fast` policy
  (256 MiB, any saving) Mario Party Superstars came out 0.33% smaller but
  with 509 frames instead of 230, including a 50 MiB file that saves under
  5%, so nx2-flow's cache would be 192 MiB instead of 114 MiB; Animal
  Crossing 0.17% smaller with 2,831 frames instead of 2,234. Run times
  differed by less than run-to-run noise (Mario 5.1 vs 4.6 s, Animal
  Crossing 8.6 vs 8.7 s): these games are mostly incompressible, so reading,
  hashing and writing dominate, and `fast` is not reliably faster than
  `runtime` on them. It pays off on archives with much compressible data
  (see [BENCHMARKS.md](BENCHMARKS.md)).
- **Best** (`archive`): klauspost's `SpeedBestCompression`, offered for
  `optimize` only (`pack` keeps the reference levels 1, 3 and 9). On the
  synthetic benchmarks it is 2-3% smaller than Balanced on compressible
  data and 3-10x slower, with about 350-650 MB peak memory (its encoders
  are large); see [Benchmarks](#benchmarks).

`--latest-snapshot` (consolidating base and updates) stays opt-in in every
profile, because it changes content.

## Archives in parts

A FAT32 card takes no file of 4 GiB or more, so NUP archives for nx2-flow
are often cut into parts. Boxcar follows nx2-flow's own convention
([docs/layout.md](https://github.com/Olmectron/nx2-flow/blob/main/docs/layout.md),
"A NUP file in parts"; `loader/runtime/nupparts.h` is its definition) rather
than inventing one:

- **Reading**: `optimize`, `nup --nup-operation verify`/`inspect`/`extract`
  accept the first part (`Game.nup.001`, `Game.nup.00`, `Game.nup.part1`,
  `Game.nup.aa`, `Game.nu0`, `Game.part1.nup`, `Game.001.nup`) or a folder
  holding nameless parts `00`, `01`, ... (or one game's named parts). Parts
  must be numbered from 0 or 1 with none missing and be at least two (a
  folder of nameless parts may hold one); at most 64 parts. A name that only
  looks like a part (`Game.nup.001` with no `.002`) is a file of its own; a
  later part is refused with a pointer to the first. Folder searches list
  each archive in parts once.
- **Writing**: `--split SIZE` writes the output as `<name>.001`,
  `<name>.002`, ... of SIZE bytes each (the last takes the rest), exactly as
  nx2-flow's file receiver names them; `--split fat32` uses the receiver's
  size, 4 GiB - 64 KiB. The default, `--split auto`, does that only when the
  output folder is on a FAT volume (detected on Linux, macOS and Windows;
  not through FUSE), where a whole file of 4 GiB would fail. An output that
  fits in one part is written as one plain file. `--split none` never
  splits.
- **Publication**: parts are written under temporary names, verified as one
  archive, and then published one by one with the same exclusive
  publication as single files, from the last part to the first. nx2-flow
  takes a game only from its first part on, so the game appears on the card
  only once it is complete. If a publication fails, the parts published so
  far are removed. An existing output, or a stray `<name>.NNN` beside it
  that the loader would read as one of its parts, is refused before
  anything is written.
- An archive in parts cannot be replaced `--in-place` (several files cannot
  be swapped atomically); optimize it to `--output` instead.

## Safety

- The source archive is never written to. The new archive is written to
  `<destination>.<random>.partial` (parts: `<destination>.NNN.<random>.partial`)
  next to its destination, after checking there is room for the worst case.
- When writing is done, Boxcar reopens the new archive with the full parser,
  decompresses every file and checks it against its SHA-256, and compares
  the logical content with the source's (both logical SHA-256 values are
  reported).
- The source is checked as it is read, including duplicate copies that
  deduplication drops. A damaged source fails the run instead of being
  silently repaired.
- `--output` never overwrites an existing file and never shows an empty
  or partial archive under the final name: the complete, flushed file is
  published with a hard link, or on filesystems without hard links (exFAT
  and FAT32 SD cards) with an exclusive rename (`renameat2
  RENAME_NOREPLACE` on Linux, `renamex_np RENAME_EXCL` on macOS,
  `MoveFileEx` without replace on Windows). Where neither exists (some
  FUSE drivers, for example) the run fails with nothing published; write
  to another drive and copy the file over.
- `--in-place` replaces the original only after verification, and only if
  the path still holds the very file that was read (same file identity,
  size and modification time, checked again right before the swap). The
  swap is one atomic rename on macOS and Linux and `ReplaceFileW` on
  Windows. `ReplaceFileW` is not atomic and can fail half way
  (`ERROR_UNABLE_TO_MOVE_REPLACEMENT_2` leaves nothing at the name), so
  Boxcar always has it move the original to a backup name beside it
  (`<name>.nup.<random>.boxcar-backup`) and then checks, by file ID, what
  is where before it reports anything: the new file in place is success
  (the backup is then deleted); the original still in place is a failure
  with the original kept; the original only at the backup name is moved
  back and reported as a failure. If moving it back fails, nothing is
  deleted and the error names the backup and the verified optimized copy
  so either can be renamed into place. The only recoverable copy is never
  deleted. These paths run in CI on Windows with the failures simulated. Concurrent Boxcar runs on the same archive are excluded with a
  lock: `flock` on the archive (macOS/Linux) or a hidden
  `<name>.nup.boxcar-lock` file opened without sharing and deleted on close
  (Windows). If the lock cannot be taken because the filesystem does not
  support it, the in-place run is refused instead of running unlocked.
- Security metadata is kept: on macOS and Linux the replacement gets the
  original's permission bits (including setuid/setgid/sticky), owner and
  group, and extended attributes (POSIX ACLs, SELinux labels, macOS
  metadata); the partial copy is created with the original's permission
  bits so it is never readable by more users meanwhile. On Windows
  `ReplaceFileW` keeps the original's ACLs, attributes, creation time and
  alternate streams. Anything that cannot be carried over fails the run and
  keeps the original, unless the replacement already has it (exFAT and
  FAT32 have one owner and mode per volume). If the rewrite is not smaller,
  the original is kept unless a policy fix, a spec deviation,
  `--latest-snapshot` or `--force` requires the rewrite.
- Free space is checked for the worst-case output size, and concurrent
  jobs (`--jobs`) on one drive share that check: each job's reservation
  counts only what its partial file may still grow by.
- Limits: the lock coordinates Boxcar runs, not other programs. A program
  that ignores it and rewrites or replaces the archive in the instant
  between Boxcar's final identity check and the swap is not detected; do
  not point two different tools at the same archive at once. Durability
  after a power loss depends on the filesystem honouring `fsync` (the new
  archive is flushed before it is published, and the folder after);
  removable media with write caches may still lose the last change, but
  never expose a partial archive under the final name on a journaling or
  copy-on-write filesystem. On FAT32/exFAT, which have no journal, a power
  loss during the rename itself can leave the folder needing a disk check;
  the original's data is not overwritten in place either way.
- Errors and Ctrl-C remove the partial file and leave the original as it
  was. A second Ctrl-C exits immediately. After a hard kill or power loss the
  original is still intact; a stray `*.partial` file may remain and can be
  deleted. On Windows a hard kill in the middle of `ReplaceFileW` can leave
  the original under its `*.boxcar-backup` name; rename it back.
- In a batch, a failing archive is reported and the others continue. The
  exit status is 1 if any archive failed.

`--in-place` needs free space for one complete optimized copy on the same
drive while it works. Memory is governed by one working-memory budget (see
[Concurrency](#concurrency-and-resources)); it does not depend on how large
the files are. Measured peak RSS is in [Benchmarks](#benchmarks).

## Concurrency and resources

- **Inside one archive**, files up to 32 MiB that are compressed or
  re-encoded are read, checked and encoded by up to one worker per CPU
  while a single writer writes them in layout order, so the output is byte
  for byte the same for any number of workers. Larger files stream on the
  writer. A raw fallback is written from memory without reading the source
  again. Verification of the new archive also checks its distinct extents
  on several CPUs.
- **CPU**: one token per CPU, held while encoding or verifying, shared by
  all archives of a run.
- **Working memory**: one budget shared by all archives of a run,
  automatic by default (an eighth of physical memory, between 256 MiB and
  2 GiB) or set with `--memory SIZE`. Everything sizeable is reserved from
  it before it is allocated: each encoder (measured for klauspost's
  encoders with Boxcar's 8 MiB window: 10 MiB at Fast, 20 MiB at Balanced,
  23 MiB at Smaller and 56 MiB at Best, the level-19 `archive` profile),
  each decoder (its frame window plus 2 MiB), the read buffers, the bytes
  workers read and encode ahead of the writer (up to 128 MiB per archive,
  never less than its largest job), the estimator's encoder and
  verification's decoders. A write reserves its writer, its workers and
  its read-ahead in one step while it holds nothing else, and takes fewer
  workers when the budget is short, so archives never wait on each other in
  a cycle; a reservation larger than the whole budget is granted when
  nothing else holds memory, so a small budget slows work down rather than
  failing it. Cancelling or failing returns every reservation. There are no
  process-wide pools left between archives.
- **Working memory is not RSS.** The budget bounds what Boxcar's
  optimizer holds; the process also has the file table and the plan (they grow
  with the number of files), the Go
  runtime's own overhead, and freed memory the runtime has not returned to
  the OS yet. Expect peak RSS around the budget in use plus that. Boxcar
  does not set `GOMEMLIMIT`; if you set it, it is a target for the garbage
  collector, not a hard cap.
- **Across archives**, `--jobs 0` (default) runs min(archives, 4, CPUs/2)
  at once, because each archive's planning, writer and commit are serial.
  Measured on 8 CPUs: four 125 MB archives 2.3 s with one job, 1.4 s with
  two and 1.0 s with four, with no more peak memory; Mario Party plus
  Animal Crossing 14.8 s with one job and 13.3 s with two, for about 23%
  more peak memory. `--jobs N` fixes the number.
- Free space for concurrent jobs is reserved per drive (see Safety).

## Options

| Flag | Default | Meaning |
|---|---|---|
| `--output PATH` | | New archive: a `.nup` path for one archive, otherwise a folder that receives the archives under their relative paths (that folder is never searched for inputs) |
| `--in-place` | off | Replace each archive after its rewrite is verified |
| `--dry-run` | off | Plan and estimate only; nothing is written (action `measure` = a real run would decide by measuring) |
| `--recursive` | off | Also search subfolders of folder inputs |
| `--jobs N` | 0 (auto) | Archives optimized at the same time; 0 = min(archives, 4, CPUs/2) |
| `--memory SIZE` | `auto` | Working-memory budget shared by all archives (`auto` = 1/8 of RAM within 256 MiB-2 GiB); workers adapt to it |
| `--profile` | `runtime` | Storage policy preset: `runtime`, `archive` or `fast` (see [Profiles](#profiles)); the flags below override single settings |
| `--preset` | profile's | zstd level for files that get compressed: `Fast` (1), `Balanced` (3), `Smaller` (9), `Best` (19) |
| `--recompress` | profile's | Re-encode already-compressed files with `--preset`, keeping the smaller frame |
| `--large-files-raw` / `--no-large-files-raw` | profile's | Keep files above the threshold uncompressed |
| `--large-file-threshold SIZE` | profile's | The large-file threshold (`64MiB`, `1.5GiB`, bytes...; KB/MB/GB are decimal); implies `--large-files-raw` |
| `--min-savings PCT` | profile's | Store a file raw unless zstd saves at least PCT percent of it |
| `--min-gain PCT` | auto | Skip archives whose estimated savings are below PCT percent (auto: 1 for archives found in folders, any saving for archives named explicitly) |
| `--split SIZE` | `auto` | Write outputs as parts of SIZE (`fat32` = 4 GiB - 64 KiB; `auto` = fat32 on FAT volumes only; `none`) |
| `--force` | off | Rewrite archives even when they look already optimized |
| `--latest-snapshot` | off | Merge base and updates into the latest snapshot (changes content) |
| `--result-json FILE` | | Also write the JSON result to a new file |

`--large-file-threshold` and `--min-savings` also work with
`boxcar nup --nup-operation pack` and `nsp-to-nup`.

Progress and one line per archive go to stderr; the JSON result goes to
stdout. A dry run estimates the output size by compressing every file that
would be compressed or, when that is more than 64 MiB, evenly spread
samples of them. The plan says how far to trust the number:
`savings_basis` is `exact` (every candidate was compressed),
`estimated` (sampled; on Mario Party the estimate was 1.1% for an actual
1.8%) or `lower_bound` (`--recompress`: re-encoding is assumed to gain
nothing); `exact_savings` is the part known exactly (deduplication, frames
reused, policy decisions that need no compression). A real run always
reports the actual sizes, and see above for how an uncertain skip of an
explicitly named archive is measured instead of trusted.

Optimized copies kept inside a folder you search later (for example
`games/optimized/` under `games/`) are archives like any other: a later
`--recursive` run lists them and skips them as already optimized. The
folder given as `--output` in the same run is never searched.

## Benchmarks

`go run ./scripts/bench` compares boxcar binaries on deterministic
synthetic fixtures (large raw files, incompressible and pre-compressed
assets, 40,000 small files, duplicates in three representations, mixed
base/update/DLC, a multi-archive folder) and optionally on your own
archives, including a second run of each profile. Results and method:
[BENCHMARKS.md](BENCHMARKS.md). In short, against the PR baseline on 8
CPUs: Mario Party Superstars 8.9 s -> 4.0 s, Animal Crossing 30.8 s ->
8.7 s, verification about 6x faster, for 330-540 MB peak memory instead of
43-138 MB.

## Workflow: KaiDump backups

[KaiDump](https://github.com/BlafKing/KaiDump) can export a game, its
update and its DLC from a Switch 2 as one NUP. Its NUP export (1.0.x) stores
every file uncompressed and writes every copy of identical files separately,
so these archives usually have a lot to gain. The export also gives
zero-length files a nonzero extent offset, which strict readers (including
NSP-Box's) reject; Boxcar reads such archives, and the optimized archive is
spec-compliant.

1. Export with KaiDump as **NUP** to an exFAT SD card, over FTP (binary
   mode) or over USB. KaiDump cannot write a NUP larger than 4 GiB to a
   FAT32 card.
2. Copy the `.nup` to your computer and check it:
   `boxcar nup --nup-operation verify --input "Game [0100...].nup"`.
   A `note:` about empty files means the dump contains zero-length files
   that strict readers reject; optimizing fixes that.
3. Preview: `boxcar optimize "Game [0100...].nup" --dry-run`
4. Optimize into a new file, or in place once you trust the process:

   ```bash
   boxcar optimize "Game [0100...].nup" --output "Game [0100...].optimized.nup"
   boxcar optimize ~/Backups/KaiDump --recursive --in-place --jobs 2
   ```

Files that were already compressed by the game (most video, much audio and
many textures) stay raw; the savings come from everything else and from
duplicates.

## Workflow: nx2-flow

[nx2-flow](https://github.com/Olmectron/nx2-flow) runs games straight from
NUP files on the SD card. Its loader reads raw files directly from the card
and unpacks a compressed file whole the first time it is read, into a cache
taken from the game's memory (details under [Profiles](#profiles)). Size is
therefore not the only goal: large compressed files cost memory and loading
time on the console. The default `runtime` profile is made for this:

```bash
boxcar optimize Game.nup --output Game.nx2.nup
boxcar optimize /path/to/games --recursive --in-place
```

If a game runs out of memory or stalls while loading big assets, lower the
threshold (for example `--large-file-threshold 32MiB`). Re-running with a
lower threshold rewrites the compressed files above it as raw, so the
archive may grow; that is expected.

Copy the optimized `.nup` into the `games` folder that nx2-flow uses
(`/nx2-flow/games/` by default), or write it there directly with
`--output` (the temporary file needs room on the card). On a FAT32 card,
an output of 4 GiB or more is written as `Game.nup.001`, `Game.nup.002`,
... automatically (`--split auto`); pass `--split fat32` when the card is
mounted in a way Boxcar cannot recognise as FAT (for example through
FUSE). Archives already in parts can be optimized directly from their
first part.

Not yet validated on a console: the profile values follow the loader's
source, and the parts follow its naming code, but neither has been loaded
on hardware by this change. Please report results.

## Testing on a real archive

Build the CLI (Go 1.27 or newer) and run it against a copy of a real
archive. Steps 2, 3 and 5 only read; step 4 writes a new file next to the
original.

```bash
# 1. Build
git clone https://github.com/thedavidweng/boxcar && cd boxcar
git checkout cursor/nup-optimize-28d8
go build -o boxcar ./cmd/boxcar            # Windows: go build -o boxcar.exe ./cmd/boxcar

# 2. The original parses and every file matches its SHA-256 (read-only)
./boxcar nup --nup-operation verify --input "/path/Game.nup"

# 3. What optimizing would do, and the logical SHA-256 (read-only)
./boxcar optimize "/path/Game.nup" --dry-run

# 4. Write an optimized copy; the original is not modified
./boxcar optimize "/path/Game.nup" --output "/path/Game.optimized.nup" --result-json optimize-result.json

# 5. Independent re-check of the copy, then both archives side by side:
#    the "logical SHA-256" values printed for the two archives must match.
./boxcar nup --nup-operation verify --input "/path/Game.optimized.nup"
./boxcar optimize "/path/Game.nup" "/path/Game.optimized.nup" --dry-run
```

Step 4 already verifies the copy and compares its logical content with the
original before committing it; steps 2 and 5 repeat those checks
independently. For nx2-flow the default `runtime` profile applies; load
`Game.optimized.nup` on the console.
Once satisfied, `--in-place` does the same for originals.

On Windows use `.\boxcar.exe` in PowerShell. `jq -r '.archives[].logical_sha256'`
extracts the full digests from the JSON output.

## Result JSON

Output of optimizing a small all-raw test archive (abridged):

```json
{
  "dry_run": false,
  "in_place": false,
  "archives": [
    {
      "input": "/path/Game.nup",
      "output": "/path/Game.optimized.nup",
      "status": "optimized",
      "reason": "estimated savings 65.3% reach the 1% minimum gain",
      "size_before": 14005436,
      "size_after": 4863788,
      "saved_bytes": 9141648,
      "logical_sha256": "b47bff8b22d2fe52ab4d973574cafabb06610968f0f638fa539746be99616f24",
      "output_logical_sha256": "b47bff8b22d2fe52ab4d973574cafabb06610968f0f638fa539746be99616f24",
      "content_changed": false,
      "verified": true,
      "plan": { "files": 6, "unique_contents": 4, "duplicate_files": 1, "compress": 4, "estimated_size": 4863788, "exact": true },
      "stats": { "unique_contents": 4, "duplicate_files": 1, "compressed": 3, "incompressible": 1, "reused_frames": 0 }
    }
  ],
  "summary": { "archives": 1, "optimized": 1, "skipped": 0, "planned": 0, "failed": 0,
               "size_before": 14005436, "size_after": 4863788, "saved_bytes": 9141648, "saved_percent": 65.27 }
}
```

`status` is one of:

| Status | Meaning |
|---|---|
| `optimized` | written, verified and committed, and smaller |
| `normalized` | written, verified and committed, not smaller, but required: a policy fix, a spec deviation or `--latest-snapshot` |
| `skipped` | left as it is; `reason` says why (already optimized, measured rewrite too small, in place not smaller). Nothing was replaced or published; a measured skip may have written and removed a temporary file |
| `planned` | dry run; `action` says `optimize`, `skip` or `measure` |
| `failed` | see `error`; nothing was written or replaced |

Every written archive is verified, so `verified` is true for `optimized`
and `normalized`. Sizes are bytes. For dry runs `size_after` is the
estimate. `parts` lists the output's parts when it was written in parts
(`output` is the first). `policy` echoes the effective settings (profile,
level, threshold, min savings, recompress, jobs, `split_size`,
`memory_budget`). `plan`
describes the decisions per distinct content (`reuse_frames`, `compress`,
`reencode`, `decode`, `keep_raw`, `duplicate_files`, `redundant_bytes`,
`better_extents`, `policy_fixes`) and the estimate's `savings_basis`;
`stats` reports what the run actually did.

The exit status is 0 when every archive was optimized, normalized, skipped
or planned, and 1 when any failed or the arguments were unusable.

## Limitations

- Archives in parts cannot be replaced in place; write them to `--output`.
- Publishing parts is not atomic as a whole: parts are published last to
  first and removed again if one fails, but a crash or power loss in the
  middle can leave the later parts under their final names without the
  first one. nx2-flow ignores them (it needs the first part); delete them
  or rerun. Stray `*.partial` temporary parts can be deleted.
- Validation on a real Switch 2 with nx2-flow is still pending: the
  `runtime` profile follows nx2-flow's loader source (0.1.7) and its split
  convention, and the split output is checked against that convention's
  reader, but it has not been loaded on a console yet.
- Archives with a superblock extension Boxcar does not know (header size
  other than 0x80 or header version other than 1) are refused rather than
  rewritten.
- Files above 32 MiB are compressed on one core each (streamed); smaller
  files use every core.
- With `--recompress` (and the `archive` profile) the estimate assumes no
  gain; the real run keeps whichever frame is smaller and discards the
  rewrite if it saves nothing, after spending the encoding time.
- FAT volumes are recognised on Linux (vfat), macOS and Windows; through
  FUSE, or on other systems, pass `--split fat32` yourself.
- `--in-place` writes a new file and renames it over the old one, so other
  hard links to the original keep the old bytes.
