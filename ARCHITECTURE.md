# Architecture

@meta {
  module: github.com/dannolan/boxcar
  go: ">= 1.27"
  reference: smarkyie/NSP-Box @ c3de7a120cb4b3c86cd5b5bcdbf5f33bbffe0fc0 (Python, the behavioral spec)
  audience: contributors and coding agents
  supersedes: PORTING.md (the original port-effort brief; wave terminology retired)
}

This document describes the module as it **is**, the contracts packages expose
to each other, the byte-exactness rules that make the port trustworthy, and
the documented deviations from the reference implementation.

## Layering

```
cmd/boxcar ──► nuptools ──► nsp ──► snapshot
     │             │  ▲               │
     │             │  └── dlc ────────┤
     │             ▼                  ▼
     │            nup ◄── (Source)  metadata ◄─ patch (PE, standalone)
     │                              ▲  ▲
     └──► patch, keys, artwork ─────┘  │
                                       │
   everything ▼                        │
   binfmt · stream · region · diskfree · testutil   (keystones)
```

Dependencies flow downward only. The keystones depend on nothing internal;
format packages build on keystones; engines build on format packages;
`nuptools` orchestrates; `cmd/boxcar` is the only entry point.

## Package map

| Package | Python counterpart | Role |
|---|---|---|
| `internal/binfmt` | — | `Require`/`Fail`/`Check`/`Recover` error style, LE packing, `Align`, `Progress` type |
| `internal/stream` | `container_format.Blob` | immutable multi-pass composite byte stream (bytes/padding/files); SHA-256 capture during write |
| `internal/region` | `verify_format.Region` | bounded pread window over `*os.File`; `Digest`, `BlockDigests`; **no mmap** |
| `internal/pfs0` | `container_format.filesystem/package`, `verify_format.partition` | PFS0/HFS0 writer + reader |
| `internal/romfs` | `container_format.romfs`, `verify_format.romfs_files` | RomFS/IVFC builder + bounded cycle-checked walker |
| `internal/nca` | `container_format.{pfs_section,romfs_section,nca}`, `verify_format.nca_sections` | NCA headers, integrity trees, verifier |
| `internal/metadata` | `engine.{read_title_id,clean_title,utf8_field,control_data}` + CNMT builders in `engine`/`dlc` | NPDM/NACP/CNMT builders |
| `internal/snapshot` | `engine.{find_snapshots,source_inventory}` | dump discovery, file stamps |
| `internal/nsp` | `engine.{build,verify_image}` | game NSP engine + independent readback verifier |
| `internal/dlc` | `dlc.py` | DLC packager (Data+Meta NCA, AddOnContent CNMT) |
| `internal/nup` | `nup_format.py` | NUP v1: pack, Archive (parse/verify/inspect), extract; optimize (repack, no Python counterpart) |
| `internal/nup/nuptest` | — | test-only writer for third-party NUP layouts (KaiDump) |
| `internal/nuptools` | `nup_tools.py` | discovery, merge/compact, NSP↔NUP conversion, batch optimize |
| `internal/patch` | `emulator_patch*.py` | PE parse/patch, embedded payloads (`go:embed data/`), Inspect/Apply/Remove |
| `internal/artwork` | `artwork.py` | decode (png/jpeg/gif/bmp/tiff/webp) → square-crop/fit → 256×256 JPEG q92 |
| `internal/keys` | `compatibility.py` | plaintext-repack placeholder key setup |
| `internal/diskfree` | `shutil.disk_usage` | free-space checks |
| `internal/testutil` | — | golden-test reference-env resolution (docs/PYREF.md) |
| `cmd/boxcar` | `app.py` CLI + `nup_cli.py` | CLI: `build`, `dlc`, `nup`, `optimize`, `patch`, `keys` |

## Contracts (pinned public APIs)

These signatures are load-bearing — cross-package tests and the CLI depend on
them. Changes require updating this file and downstream callers in the same
commit.

```go
// pfs0
type Entry struct{ Name string; Part stream.Part }
func FileSystem(entries []Entry, hfs bool) (*stream.Stream, error)
func Package(entries []Entry) (*stream.Stream, error)
func Partition(ctx context.Context, r *region.Region) (map[string]*region.Region, error)

// romfs
type ManifestEntry struct{ Path string; Size int64; RomfsOffset int64 }
func Build(ctx context.Context, root string, manifest *[]ManifestEntry,
    notify func(count int), progress binfmt.Progress) (*stream.Stream, error)
func Files(ctx context.Context, r *region.Region) (map[string]*region.Region, error)

// nca
type Section struct{ FSH []byte; Body *stream.Stream }
func PFSSection(ctx context.Context, body *stream.Stream) ([]byte, *stream.Stream, error)
func RomfsSection(ctx context.Context, body *stream.Stream, progress binfmt.Progress) ([]byte, *stream.Stream, error)
func NCA(sections []Section, titleID uint64, contentType byte) (*stream.Stream, error)
type NamedSection struct{ Kind string; Data *region.Region }
func Sections(ctx context.Context, r *region.Region, verifyPayload bool) (uint64, byte, []NamedSection, error)

// metadata
func ReadTitleID(npdm []byte) (uint64, error)
func CleanTitle(name string) string
func Utf8Field(text string, size int) []byte
type KV struct{ Name string; Data []byte }
func ControlData(titleID uint64, displayVersion, title string, existingNacp, artwork []byte) ([]KV, bool, error)
func AppCNMT(titleID uint64, version uint32, records []CNMTRecord) []byte
func AddOnCNMT(titleID uint64, version uint32, dataDigest []byte, dataSize int64) []byte

// snapshot
type Snapshot struct{ Root string; TitleID uint64; Version uint32; Label, Title, NacpPath string }
func Find(ctx context.Context, folder string) ([]Snapshot, []string, error)
type Stamp struct{ Size int64; MtimeNS int64 }
func Inventory(ctx context.Context, root string, notify func(label string, found int)) (map[string]Stamp, error)
func ValidateSources(paths map[string]Stamp) error

// nsp
type Report struct{ /* JSON mirror of engine.build's report dict */ }
type Options struct{ Output, Title, DisplayVersion string; Artwork *artwork.Selection; Verify, PackageFolder bool; Progress binfmt.Progress }
func Build(ctx context.Context, snap snapshot.Snapshot, opts Options) (Report, error)
func VerifyOutput(ctx context.Context, path string, snap snapshot.Snapshot, inv map[string]snapshot.Stamp,
    progress binfmt.Progress, captured map[string]*[]byte) ([]map[string]any, error)

// dlc
func BaseID(tid uint64) uint64
func ParseID(v string) (uint64, error)
func ParseVersion(v string) (uint32, error)
func Find(ctx context.Context, folder, titleIDOverride string) ([]Snapshot, error)
func BaseGameInfo(ctx context.Context, path string) (BaseInfo, error)
func Build(ctx context.Context, snap Snapshot, output string, opts Options) (Report, error)
func Verify(ctx context.Context, path string, snap Snapshot, sources map[string]snapshot.Stamp,
    progress binfmt.Progress, captured map[string]*[]byte) (int, error)

// nup
var Presets = map[string]int{"Fast": 1, "Balanced": 3, "Smaller": 9}
func KindName(kind int) string
type TLV struct{ Kind uint16; Data []byte }
func SourcePath(path string) (*Source, error)
func SourceData(data []byte) *Source
func ValidatePackages(packages []Package) error
func SafeName(name string, windows bool) error
func PathParts(path string) ([]string, error)
func Pack(ctx context.Context, packages []Package, output string, opts PackOptions) (Result, error)
func Open(ctx context.Context, path string) (*Archive, error)       // Archive: Packages/Verify/Inspect/FileChunks/Contents/Deviations/Parts; path may be a first part or a folder of parts
func FindParts(path string) (parts []string, name string, err error) // nx2-flow's nupparts.h rules; ErrLaterPart
func Extract(ctx context.Context, path, output string, selection []int, progress binfmt.Progress) (Result, error)
func PackagePath(p Package) string
type Content struct{ TitleID uint64; Version uint32; Kind, Flags int; Root string; ExeFS, RomFS bool
    Metadata []byte; Directories []string; Files []FileContent }
func LogicalDigest(contents []Content) string
func CompareContents(want, got []Content) error
type RepackOptions struct{ Level int; LargeRaw bool; LargeFileThreshold int64; MinSavings float64; Recompress bool; Progress binfmt.Progress }
var Profiles map[string]RepackOptions                               // "runtime" (DefaultProfile), "archive", "fast"; ProfileOptions(name)
type OptimizeOptions struct{ RepackOptions; Output string; InPlace, DryRun, Force bool; MinGain float64
    Measure bool; SplitSize int64; Transform func(context.Context, []Package) ([]Package, error) }
func Optimize(ctx context.Context, path string, opts OptimizeOptions) (OptimizeResult, error) // OptimizeResult{Status, Plan, Stats, LogicalSHA256, ...}

// nuptools (batch optimize; the rest of the package contract is in nuptools.go)
func Optimize(ctx context.Context, inputs []string, opts OptimizeOptions) (BatchResult, error)

// patch
func Transform(exe []byte, profile Profile) ([]byte, error)
func Inspect(ctx context.Context, folder string) (View, error)
func Apply(ctx context.Context, folder string) (View, error)
func Remove(ctx context.Context, folder string) (View, error)

// artwork
type Selection struct{ Path string; Box *[4]float64; Fit bool }
func Bytes(sel *Selection) ([]byte, error)
func LoadImage(path string) (image.Image, error)
func SupportedExtensions() []string

// keys
func PlaceholderText(names []string) string
func Inspect(directory string) (Status, error)
func Install(directory string) (Result, error)
```

## Byte-exactness rules

Deterministic outputs are the port's core guarantee. For identical inputs:

- **NSP and DLC NSP output must be byte-identical** to the Python reference
  (enforced by golden tests in `internal/nsp`, `internal/dlc`).
- **`patch.Transform` output must be byte-identical** to Python's for a given
  exe+profile (golden fixtures in `internal/patch/testdata`).
- **NUP containers must cross-verify both directions** (Go reads Python-built
  archives and vice versa). Compressed extent bytes may differ — the zstd
  backends differ — but layout, TOC, validation rules and raw extents may not.
- NSP construction is fully deterministic: no timestamps, UUIDs, or platform
  variance enter the byte stream. Preserve that property.
- **NUP optimizer** output uses Pack's TOC layout (`layout.go` is shared), so
  forcing a rewrite of a Pack archive reproduces it byte for byte. It must
  preserve `Archive.Contents` exactly (enforced at runtime by
  `CompareContents`), and the Python reference must verify and extract its
  output (`TestCrossOptimize`, part of `make golden`).

Enforcement: `make golden` (strict; skips become failures). CI runs it on
every push with a pinned reference checkout.

## Conventions (the "why" behind the odd-looking code)

- **Error style**: dense validation mirrors Python's `require()` via
  `binfmt.Require` panics converted by `defer binfmt.Recover(&err)`. Error
  strings frequently match Python exactly because tests assert them.
- **Cancellation**: `binfmt.Check(ctx)` at Python's `checkpoint()` positions.
- **Commits**: outputs are written to `name.<10-hex>.partial` and committed
  without overwriting: `os.Link`+unlink on POSIX; `MoveFileEx` without
  `REPLACE_EXISTING` on Windows (`commit_*.go`, `platform_*.go`). Never plain
  `os.Rename` on Windows for a commit — Go's version replaces. The one
  sanctioned replacement is `nup.Optimize` in in-place mode: only after the
  temp file is verified and the source's identity, size and mtime are
  re-checked, through `replaceFile` (POSIX `rename`; Windows `ReplaceFileW`,
  which keeps ACLs/attributes/streams, always with a backup name, after
  which the result is settled by file ID: success, original kept, original
  moved back, or `keepFilesError` naming the kept files, whose temp file
  `Optimize` must not delete), after `prepareReplacement` copied
  mode, owner and xattrs (POSIX; failure keeps the original), under a lock
  that must succeed (`flock` on POSIX, a no-share delete-on-close sidecar
  on Windows). New outputs (`nup.Optimize`, `nup.Pack`) use `commitNew`: a
  hard link, else (exFAT/FAT32) an exclusive rename (`renameat2`
  `RENAME_NOREPLACE` on Linux, `renamex_np` `RENAME_EXCL` on macOS), else a
  clean failure (`errNoExclusivePublish`) — never a placeholder file.
  Worst-case temp space is claimed through `diskfree.Reserve`, shared by
  concurrent jobs per volume.
- **Links refused**: symlinks everywhere; reparse points on Windows
  (`patch_windows.go`/`platform_windows.go`). Error text mirrors Python.
- **No mmap**: all random access goes through `internal/region`.
- **Streaming only**: never read game files or outputs fully into memory;
  compose with `stream` and write via `WriteTo`. The one bounded exception
  is the optimizer's parallel path (`internal/nup/parallel.go`): files up
  to 32 MiB are read ahead into 1 MiB chunks from the write's own job
  allowance, and CPU work holds one of GOMAXPROCS tokens; larger files
  stream. Output order and bytes do not depend on the worker count.
- **Working-memory budget** (`membudget.go`): one process-wide byte budget
  (`SetMemoryBudget`, automatic by default). Every phase that holds
  sizeable buffers reserves them first, while holding nothing else from
  the budget: a write reserves writer + workers (encoder per level from
  `encoderCost`, decoder per window from `decoderCost`) + job allowance in
  one `reserveFlexible` call that adapts the worker count; estimation,
  verification and `Pack` reserve their encoder/decoders. Jobs draw only on
  their write's allowance (at least the largest job), so archives never
  wait on each other in a cycle. No process-wide encoder or chunk pools.
- **Archives in parts** follow nx2-flow's `nupparts.h` exactly (`parts.go`);
  Boxcar writes only `<name>.NNN` parts of one size, publishes them last to
  first with `commitNew`, and never replaces parts in place.

## Deviations from the reference (all intentional)

1. Artwork decoders: PNG/JPEG/GIF/BMP/TIFF/WEBP only (no AVIF/ICNS/JPEG2000 —
   no pure-Go decoders); CatmullRom instead of Pillow's Lanczos; stdlib JPEG
   encoder differs byte-wise from Pillow's (icons are not golden-compared).
2. NUP compression uses klauspost zstd; levels 1/3/9 map to
   SpeedFastest/Default/BetterCompression. Container rules (window ≤ 1 GiB,
   single frame, ≤128 KiB blocks) are enforced identically.
3. NSP→NUP conversion materializes region-backed files to temp files instead
   of streaming over mmap windows.
4. `nca` distribution byte is fixed to 0 (plaintext) instead of parameterized.
5. Windows reparse-point checks exist only in `internal/patch` and `internal/nup`
   platform files; other packages check symlinks only.
6. ASCII-only decimal/hex classification vs Python's Unicode `isdecimal()`.
7. **NUP metadata placement**: the reference reader requires package metadata
   to live inside the blob region (`blobOff <= metaOff`). Real-world NUPs
   packed by other implementations (a55uka's reference packer, observed in a
   25.6 GB archive) place metadata outside the blob extents entirely. Boxcar
   accepts metadata anywhere in the file as long as it is in-bounds and does
   not overlap other extents (the overlap pass already enforces that).
   Regression test: `internal/nup/archive_reloc_test.go`.
8. **NUP empty-file offsets**: the reference reader (and SPEC §3.6) requires
   a zero-length file to have `extent_offset == 0`. KaiDump records the
   current blob cursor there instead, so its archives are rejected whenever
   the dump contains an empty file. Boxcar still requires `extent_size == 0`
   and `codec == 0` but ignores the offset, which is never read. Boxcar's
   writers (pack, optimize) always emit 0, so optimizing a KaiDump archive
   also makes it readable by strict readers.
   Regression test: `internal/nup/kaidump_test.go`.

`Archive.Deviations` reports archives that rely on 7 or 8; `nup inspect`/
`verify` add them as `notes`, and `optimize` rewrites such archives even
when there is nothing to save.

## Dependency policy

Runtime deps: `github.com/klauspost/compress` (nup only), `golang.org/x/image`
(artwork only), `golang.org/x/sys` (platform files only: Windows APIs, and
exclusive rename in `internal/nup/rename_{linux,darwin}.go`). Everything
else is stdlib. Adding a fourth requires an ARCHITECTURE.md update and a
strong justification — the point of the port is a small, auditable binary.

## Release process

1. Update `const version` in `cmd/boxcar/main.go` + CHANGELOG.md section.
2. Tag `v<version>` and push — `.github/workflows/release.yml` builds
   darwin universal/arm64/amd64, windows amd64/arm64, linux amd64/arm64,
   packages with docs, SHA256SUMS, and publishes the GitHub release.
3. CI (`.github/workflows/ci.yml`) runs `make check` on ubuntu/macos,
   build+vet on windows, and the strict golden job.
