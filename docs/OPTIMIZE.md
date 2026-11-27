# Optimizing NUP archives

`boxcar optimize` rewrites existing NUP archives without extracting them. It
reuses compressed data that is already good, compresses what was stored raw
with Boxcar's presets, stores identical files once, applies the large-file
RAW rule, and verifies the result before anything replaces an original. It
works on single archives and on whole game libraries.

```bash
boxcar optimize game.nup --dry-run                  # preview; writes nothing
boxcar optimize game.nup --output game.small.nup    # new file; original untouched
boxcar optimize ./games --recursive --dry-run       # preview a library
boxcar optimize ./games --recursive --in-place      # replace originals after verification
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
other file with that content points at the same extent.

| Stored in the source as | Storage policy says | Result |
|---|---|---|
| zstd | compress | frame reused verbatim (`--recompress`: re-encoded with `--preset`, smaller frame kept) |
| zstd | raw: larger than `--large-file-threshold`, or saves less than `--min-savings` | decompressed and stored raw |
| raw | compress | compressed with `--preset`; kept raw when zstd does not save enough |
| raw | raw: larger than `--large-file-threshold` | copied verbatim |

The policy is the one `pack` uses: files above `--large-file-threshold`
(default `256MiB`) stay raw unless you pass `--no-large-files-raw`, and a
compressed file is kept only if it is smaller than the raw file (or, with
`--min-savings N`, at least N percent smaller).

An archive counts as **already optimized** when the estimated savings are
below `--min-gain` (default 1% of the archive), nothing breaks the storage
policy and it follows the NUP spec. Such archives are skipped unless you
pass `--force`. Archives whose compressed files break the policy (for
example a 1 GiB file compressed by another tool while the threshold is
256 MiB) are always rewritten, even if the result is larger, because that
is what the policy asks for.

### Archives that deviate from the spec

Boxcar reads two kinds of archives that the NUP spec or the NSP-Box reader
does not allow: KaiDump's empty files with a nonzero extent offset, and
package metadata stored before the blob region. `boxcar optimize` (and
`nup --nup-operation verify`/`inspect`) prints a `note:` line for them, and
optimizing rewrites them even when there is nothing to save, because the
rewrite is compliant (`normalized:` in the output). The JSON lists them
under `plan.deviations` (`notes` for verify/inspect).

## Safety

- The source archive is never written to. The new archive is written to
  `<destination>.<random>.partial` next to its destination, after checking
  there is room for the worst case.
- When writing is done, Boxcar reopens the new archive with the full parser,
  decompresses every file and checks it against its SHA-256, and compares
  the logical content with the source's (both logical SHA-256 values are
  reported).
- The source is checked as it is read, including duplicate copies that
  deduplication drops. A damaged source fails the run instead of being
  silently repaired.
- `--output` never overwrites an existing file: the verified file is
  published with a hard link, or on filesystems without hard links (exFAT
  and FAT32 SD cards) with an exclusive rename. Only where neither exists
  is the name reserved with an empty file for the instant before the
  rename.
- `--in-place` replaces the original only after verification, and only if
  the path still holds the very file that was read (same file identity,
  size and modification time), with a single atomic rename (POSIX
  `rename`, Windows `MoveFileEx`). On macOS and Linux an advisory lock
  stops a second optimization of the same archive, and the partial copy is
  created with the original's permission bits, which the replacement
  keeps. On Windows the replacement gets the folder's default permissions;
  explicit access lists on the original are not copied. If the rewrite is
  not smaller, the original is kept unless a policy fix, a spec deviation,
  `--latest-snapshot` or `--force` requires the rewrite.
- These checks guard against other Boxcar runs and against a file that
  changed or was replaced while it was being optimized. They are not
  atomic against another program writing to the same path at the same
  instant: the empty-file reservation above, and the identity check right
  before an in-place rename, each leave a brief window. The advisory lock
  is skipped on filesystems that refuse it (some network mounts). Do not
  point two different tools at the same archive at once.
- Errors and Ctrl-C remove the partial file and leave the original as it
  was. A second Ctrl-C exits immediately. After a hard kill or power loss the
  original is still intact; a stray `*.partial` file may remain and can be
  deleted.
- In a batch, a failing archive is reported and the others continue. The
  exit status is 1 if any archive failed.

`--in-place` needs free space for one complete optimized copy on the same
drive while it works. Memory use does not depend on how large the files
are, since every file streams through fixed buffers; it grows with how many
files the archive holds, because their table is kept in memory. In tests a
4.1 GB archive of 22,000 files peaked at about 75 MB and a 67,000-file
archive at about 130 MB, per concurrent job.

## Options

| Flag | Default | Meaning |
|---|---|---|
| `--output PATH` | | New archive: a `.nup` path for one archive, otherwise a folder that receives the archives under their relative paths (that folder is never searched for inputs) |
| `--in-place` | off | Replace each archive after its rewrite is verified |
| `--dry-run` | off | Plan and estimate only; nothing is written |
| `--recursive` | off | Also search subfolders of folder inputs |
| `--jobs N` | 1 | Archives optimized at the same time |
| `--preset` | `Balanced` | zstd level for files that get compressed: `Fast` (1), `Balanced` (3), `Smaller` (9) |
| `--recompress` | off | Re-encode already-compressed files with `--preset`, keeping the smaller frame |
| `--large-files-raw` / `--no-large-files-raw` | on | Keep files above the threshold uncompressed |
| `--large-file-threshold SIZE` | `256MiB` | The large-file threshold (`64MiB`, `1.5GiB`, bytes...; KB/MB/GB are decimal) |
| `--min-savings PCT` | 0 | Store a file raw unless zstd saves at least PCT percent of it |
| `--min-gain PCT` | 1 | Skip archives whose estimated savings are below PCT percent |
| `--force` | off | Rewrite archives even when they look already optimized |
| `--latest-snapshot` | off | Merge base and updates into the latest snapshot (changes content) |
| `--result-json FILE` | | Also write the JSON result to a new file |

`--large-file-threshold` and `--min-savings` also work with
`boxcar nup --nup-operation pack` and `nsp-to-nup`.

Progress and one line per archive go to stderr; the JSON result goes to
stdout. A dry run estimates the output size by compressing every file that
would be compressed or, when that is more than 64 MiB, evenly spread
samples of them; sampled estimates are approximate and lean conservative
(`"exact": false` in the plan), and a real run always reports the actual
sizes.

Optimized copies kept inside a folder you search later (for example
`games/optimized/` under `games/`) are archives like any other: a later
`--recursive` run lists them and skips them as already optimized. The
folder given as `--output` in the same run is never searched.

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
NUP files on the SD card. Its loader reads raw files directly from the card,
but, in its own words, "a file kept packed in the NUP file is unpacked whole
the first time it is read". As of loader 0.1.6 it keeps those unpacked files
in a memory cache sized at the larger of 192 MiB and the largest compressed
file plus 64 MiB, taken from the memory the game could otherwise use. Size
is therefore not the only goal: large compressed files cost memory and
loading time on the console.

A setting that keeps every compressed file at or under 128 MiB (so the
cache stays at its 192 MiB minimum) and leaves barely-compressible files
raw:

```bash
boxcar optimize Game.nup --output Game.nx2.nup --large-file-threshold 128MiB --min-savings 5
boxcar optimize /path/to/games --recursive --in-place --large-file-threshold 128MiB --min-savings 5
```

If a game runs out of memory or stalls while loading big assets, lower the
threshold further (for example `32MiB`). Re-running with a lower threshold
rewrites the compressed files above it as raw, so the archive may grow;
that is expected.

Copy the optimized `.nup` into the `games` folder that nx2-flow uses
(`/nx2-flow/games/` by default), or write it there directly with
`--output` (exFAT and FAT32 cards work; the temporary file needs room on
the card). Boxcar works on single-file archives; if
your card is FAT32 and the archive is 4 GiB or more, optimize the single
file first and then split it the way nx2-flow's
[docs/layout.md](https://github.com/Olmectron/nx2-flow/blob/main/docs/layout.md)
describes (`Game.nup.001`, `Game.nup.002`, ...). Join split parts back into
one file before optimizing them again.

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
independently. For nx2-flow, add `--large-file-threshold 128MiB
--min-savings 5` to step 4 and load `Game.optimized.nup` on the console.
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

`status` is `optimized`, `skipped` (see `reason`), `planned` (dry run;
`action` says `optimize` or `skip`) or `failed` (see `error`). Sizes are
bytes. For dry runs `size_after` is the estimate. `plan` describes the
decisions per distinct content (`reuse_frames`, `compress`, `reencode`,
`decode`, `keep_raw`, `duplicate_files`, `redundant_bytes`,
`policy_fixes`); `stats` reports what the run actually did.

## Limitations

- Archives split into parts (a folder named `*.nup`, or `Game.nup.001`...)
  must be joined into one file first.
- Archives with a superblock extension Boxcar does not know (header size
  other than 0x80 or header version other than 1) are refused rather than
  rewritten.
- Compression inside one archive uses one core; `--jobs` runs several
  archives in parallel.
- With `--recompress`, the estimate assumes no gain; the real run keeps
  whichever frame is smaller.
- `--in-place` writes a new file and renames it over the old one, so other
  hard links to the original keep the old bytes.
