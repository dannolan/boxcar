# NSP-Box → Go Port — Master Contract

Go port of the Python reference implementation at
`/Users/dan/Downloads/nsp-box-src/src/` (read those files; they are the spec).

Module: `github.com/smarkyie/nsp-box-go` at `/Users/dan/Downloads/nsp-box-go`.
Go 1.27. Standard library only unless your task says otherwise.

## Shared keystones (already implemented — use them, do NOT modify)

- `internal/binfmt` — `Require(cond, msg)` (panic-based, mirrors Python
  `require`), `Fail`, `Check(ctx)` (mirrors `checkpoint()`), `Recover(&err)`
  (defer at every exported op), `Align`, LE `U16At/U32At/U64At/PutU16/PutU32/
  PutU64/Append*`, `Progress(label string, done float64)`, `SHA256Sum`,
  `CopyWithHash`.
- `internal/stream` — port of Python `Blob`: `stream.New(parts...)` accepts
  `Part`, `[]Part`, `[]byte`, `*Stream`; `stream.Bytes/Padding/File(path,size)`;
  `Size()`, `Pad(a)`, `WriteTo(w, *WriteOpts)`, `ReadHead(n, *WriteOpts)`,
  `SHA256(*WriteOpts)`. `WriteOpts{Ctx, Progress, Hashes map[string]*[]byte,
  Block}` — `Hashes` captures SHA-256 of file parts during write (port of
  `capture_sources`). Streams are immutable and multi-pass: file parts are
  re-opened on every pass, so `ReadHead` then `WriteTo` works like Python.
- `internal/region` — port of Python `verify_format.Region` without mmap:
  bounded pread window over `*os.File`. `New(f)`, `Sub(off,size)`, `Slice()`,
  `ReadAt`, `Byte`, `U16/U32/U64`, `Digest(ctx)`, `BlockDigests(ctx, size,
  padded)`, `FileDigest(ctx, path)`.

Error style: dense validation logic ports 1:1 using `binfmt.Require(...)`
panics converted to errors via `defer binfmt.Recover(&err)` on exported
functions. Cancellation: `binfmt.Check(ctx)` at the same places Python calls
`checkpoint()`. Progress: call the provided `binfmt.Progress` with 0–100.

## Package ownership map

| Go package | Python source | Owner agent |
|---|---|---|
| `internal/pfs0` | `container_format.py` (`filesystem`, `package`) + `verify_format.partition` | W1-A |
| `internal/romfs` | `container_format.py` (`romfs`, `bucket_count`, `name_hash`) + `verify_format.romfs_files` | W1-B |
| `internal/nca` | `container_format.py` (`pfs_section`, `romfs_section`, `nca`) + `verify_format.nca_sections` | W1-C |
| `internal/metadata` | `engine.py` (`read_title_id`, `utf8_field`, `control_data`, clean_title) + `dlc.py` CNMT builders + `compatibility.py` | W1-D |
| `internal/patch` | `emulator_patch.py` + `emulator_profiles.py` + `emulator_patterns.py` + `emulator_payloads.py` | W1-E + W1-F |
| `internal/artwork` | `artwork.py` (reduced format set) | W1-G |
| `internal/snapshot` | `engine.py` (`find_snapshots`, `source_inventory`, stamps) | W2-A |
| `internal/nsp` | `engine.py` (`build`, `verify_image`) | W2-A |
| `internal/dlc` | `dlc.py` (`find_snapshots`, `inventory`, `build`, `verify`, `base_game_info`) | W2-B |
| `internal/nup` | `nup_format.py` (everything) | W2-C |
| `internal/nuptools` | `nup_tools.py` | W3-A |
| `cmd/nspbox` | `app.py` CLI args + `nup_cli.py` | W3-B |

## Contract signatures (pinned; other agents code against these)

```go
// internal/pfs0
type Entry struct{ Name string; Part stream.Part }        // file-backed or literal
func FileSystem(entries []Entry, hfs bool) (*stream.Stream, error) // container_format.filesystem
func Package(entries []Entry) (*stream.Stream, error)     // container_format.package ('nsp' only)
func Partition(ctx context.Context, r *region.Region) (map[string]*region.Region, error) // verify_format.partition (PFS0+HFS0)

// internal/romfs
type ManifestEntry struct{ Path string; Size int64; RomfsOffset int64 }
func Build(ctx context.Context, root string, manifest *[]ManifestEntry,
    notify func(count int), progress binfmt.Progress) (*stream.Stream, error) // container_format.romfs
func Files(ctx context.Context, r *region.Region) (map[string]*region.Region, error) // verify_format.romfs_files (bounded walker from nup_tools.romfs_files: depth<256, cycle detection)

// internal/nca
func PFSSection(ctx context.Context, body *stream.Stream) ([]byte, *stream.Stream, error) // (fsh, paddedBody)
func RomfsSection(ctx context.Context, body *stream.Stream, progress binfmt.Progress) ([]byte, *stream.Stream, error)
func NCA(sections [][2]any /*[]{fsh []byte, body *stream.Stream}*/, titleID uint64, contentType byte) (*stream.Stream, error) // distribution=0
func Sections(ctx context.Context, r *region.Region, verifyPayload bool) (titleID uint64, contentType byte, secs []NamedSection, err error) // verify_format.nca_sections
type NamedSection struct{ Kind string /*"pfs"|"romfs"*/; Data *region.Region }

// internal/metadata
func ReadTitleID(npdm []byte) (uint64, error)
func CleanTitle(name string) string
func Utf8Field(text string, size int) []byte
func ControlData(titleID uint64, version string, title string, existingNacp []byte, artwork []byte) (entries []KV, preserved bool, err error) // engine.control_data; KV{Name string, Data []byte}
func AppCNMT(titleID uint64, version uint32, records []CNMTRecord) []byte // engine.build inline
func AddOnCNMT(titleID uint64, version uint32, dataDigest []byte, dataSize int64) []byte // dlc.build inline
type CNMTRecord struct{ Digest [32]byte; ContentName string /*16-hex nibbles = first 16 bytes of digest*/; Size int64; ContentType byte }

// internal/artwork
type Selection struct{ Path string; Box *[4]float64; Fit bool }
func Bytes(sel *Selection) ([]byte, error) // 256x256 JPEG q92; supported: png/jpeg/gif/bmp/tiff/webp
func LoadImage(path string) (image.Image, error) // EXIF-transposed RGBA

// internal/patch — see internal/patch/CONTRACT.md
func Transform(exe []byte, profile Profile) ([]byte, error)
func Inspect(ctx context.Context, folder string) (View, error)
func Apply(ctx context.Context, folder string) (View, error)
func Remove(ctx context.Context, folder string) (View, error)

// internal/nup — see internal/nup/CONTRACT.md (W2-C)
// internal/nsp — see internal/nsp/CONTRACT.md (W2-A)
```

## Wave-2 contracts (pinned)

```go
// internal/snapshot (port engine.find_snapshots + source_inventory)
type Snapshot struct{ Root string; TitleID uint64; Version uint32; Label string; Title string; NacpPath string }
func Find(ctx context.Context, folder string) (found []Snapshot, skipped []string, err error) // same error text when none found
type Stamp struct{ Size int64; MtimeNS int64 }
func Inventory(ctx context.Context, root string, notify func(label string, found int)) (map[string]Stamp, error)
func ValidateSources(paths map[string]Stamp) error // re-stat; 'A source file changed during packaging...' on mismatch

// internal/nsp (port engine.build + verify_image; uses snapshot, metadata, pfs0, romfs, nca, artwork, diskfree)
type Report struct{ /* JSON fields mirroring engine.build's report dict exactly */ }
type Options struct {
  Output, Title, DisplayVersion string
  Artwork *artwork.Selection
  Verify, PackageFolder bool
  Progress binfmt.Progress
}
func Build(ctx context.Context, snap snapshot.Snapshot, opts Options) (Report, error)
func VerifyOutput(ctx context.Context, path string, snap snapshot.Snapshot, inv map[string]snapshot.Stamp,
  progress binfmt.Progress, captured map[string]*[]byte) ([]map[string]any, error)

// internal/dlc (port all of dlc.py; uses metadata.AddOnCNMT, pfs0, romfs, nca, region, diskfree)
type Snapshot struct{ Root string; TitleID uint64; Version uint32; Label string }
func BaseID(tid uint64) uint64 // & 0xffffffffffffe000
func ParseID(v string) (uint64, error)
func ParseVersion(v string) (uint32, error)
func Find(ctx context.Context, folder string, titleIDOverride string) ([]Snapshot, error)
type BaseInfo struct{ TitleID uint64; DLCBaseID uint64; Title string }
func BaseGameInfo(ctx context.Context, path string) (BaseInfo, error)
type Report struct{ /* JSON fields mirroring dlc.build's report dict */ }
type Options struct{ VerifyOutput bool; BaseGame string; Progress binfmt.Progress }
func Build(ctx context.Context, snap Snapshot, output string, opts Options) (Report, error)
func Verify(ctx context.Context, path string, snap Snapshot, sources map[string]snapshot.Stamp,
  progress binfmt.Progress, captured map[string]*[]byte) (int, error)

// internal/nup (port ALL of nup_format.py; zstd = github.com/klauspost/compress/zstd — only package allowed to add deps)
const ( Block = 1<<20; LargeFile = 256<<20; MaxWindow = 1<<30 )
var Presets = map[string]int{"Fast": 1, "Balanced": 3, "Smaller": 9}
func KindName(kind int) string // base/update/addon/addon_update/system
type TLV struct{ Kind uint16; Data []byte }
type Source struct{ /* port nup_format.Source: path-backed with stamps+Validate, or data-backed */ }
func SourcePath(path string) (*Source, error)   // refuses links; captures size+mtime stamps
func SourceData(data []byte) *Source
func (s *Source) Size() int64
func (s *Source) Open() (io.ReadCloser, error)  // re-openable reader
func (s *Source) ReadSmall(limit int64) ([]byte, error) // default 32 MiB cap
func (s *Source) Validate() error
type Package struct{ TitleID uint64; Version uint32; Kind int; Files map[string]*Source; Directories []string; Metadata []TLV; Flags int; Label string }
func ValidatePackages(packages []Package) error
func SafeName(name string, windows bool) error
func PathParts(path string) ([]string, error)
type PackOptions struct{ Level int; LargeRaw bool; Verify bool; Progress binfmt.Progress; SourceGuard func() error }
type Result struct{ Output string; Size, SourceSize, SavedBytes int64; Verified bool; Packages, DuplicateFilesSkipped int }
func Pack(ctx context.Context, packages []Package, output string, opts PackOptions) (Result, error)
type Stats map[string]any
type PackageInfo struct{ Index, Kind int; TitleID uint64; Version, Flags uint32; Files int; MetadataTypes []string; Title string }
type Archive struct{ /* unexported */ }
func Open(ctx context.Context, path string) (*Archive, error)
func (a *Archive) Close() error
func (a *Archive) Packages() []Package
func (a *Archive) Verify(ctx context.Context, progress binfmt.Progress) (Stats, error)
func (a *Archive) Inspect(ctx context.Context) (Stats, []PackageInfo, error)
func (a *Archive) FileChunks(ctx context.Context, index int, w io.Writer) error // hash-verifying decompressed stream
func Extract(ctx context.Context, path, output string, selection []int, progress binfmt.Progress) (Result, error)
func PackagePath(p Package) string
```

Rules pinned for every agent:

1. **Byte-exactness where deterministic**: NSP/NCA/PFS0/RomFS/IVFC/CNMT/NACP
   bytes must be identical to the Python output for identical inputs. NUP
   compressed extents may differ (different zstd backend) but the container
   must verify with the same rules. Patch `Transform()` output must be
   byte-identical to Python's for a given exe+profile.
2. **No mmap.** Use `region.Region` for output verification.
3. **Streaming only.** Never read whole game files or outputs into memory.
   `stream.Stream` composes; write via `WriteTo` to the temp file.
4. **No source mutation.** Refuse symlinks/reparse points exactly like the
   Python (`linked()` checks). On non-Windows symlink check only; on Windows
   use `golang.org/x/sys/windows` FILE_ATTRIBUTE_REPARSE_POINT (only
   `internal/patch`, `internal/snapshot`, `internal/nup` may import x/sys).
5. **No overwrite commits.** Outputs are written to
   `name.<10-hex-random>.partial` then committed: on Windows exclusive rename
   (Go `os.Rename` REPLACES — must use MoveFileEx without
   MOVEFILE_REPLACE_EXISTING via x/sys/windows, or check+rename), on POSIX
   `os.Link` + remove (Python `commit_file`). Cancellation removes the temp.
6. **Tests are mandatory**: table-driven `*_test.go` with synthetic data.
   Round-trip your own writer through your own reader. Compare fixed hex
   vectors for small headers. `go build ./... && go test ./internal/<pkg>/`
   must pass. Do not modify other packages; if a needed helper is missing,
   note it in your final report instead of editing shared files.
7. Report: files created, test results, any contract deviations.
