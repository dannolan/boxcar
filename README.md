---
title: Boxcar
description: "Switch 2 game packager in Go: NSP builder, NUP archives, DLC, reversible emulator patch. Byte-exact Go port of NSP-Box."
version: 4.0.0-boxcar.1
repository: https://github.com/dannolan/boxcar
license: MIT
language: go
platforms: [macos, windows, linux]
agents:
  guide: AGENTS.md
  architecture: ARCHITECTURE.md
  golden-tests: docs/PYREF.md
topics: [nintendo-switch, nsp, nup, emulator, packager, cli, go]
---

<div align="center">

# 📦 Boxcar

**Packages, converts and inspects Switch 2 game archives — one static binary, no runtime, any OS.**

A Go port of [NSP-Box](https://github.com/smarkyie/NSP-Box) (v4.0.0).

[![CI](https://github.com/dannolan/boxcar/actions/workflows/ci.yml/badge.svg)](https://github.com/dannolan/boxcar/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Windows%20%7C%20Linux-lightgrey)](#install)

</div>

---

**Boxcar** rebuilds complete, already-extracted ExeFS/RomFS snapshots into plaintext NSPs, converts them to and from compressed NUP archives, packages RomFS-only DLC add-ons, and reversibly patches Windows x64 emulator builds to read embedded package metadata.

It is a faithful port of [smarkyie's NSP-Box](https://github.com/smarkyie/NSP-Box) from Python to Go, validated **byte-for-byte** against the original implementation.

> ⚠️ **Piracy is not supported.** Use only files and artwork you have lawfully obtained and are authorized to use. No games, game artwork, firmware or Nintendo keys are provided. See [DISCLAIMER.md](DISCLAIMER.md).

## Why

| | NSP-Box (Python) | Boxcar (Go) |
|---|---|---|
| Binary | 31 MB PyInstaller EXE | **~8 MB static binary** |
| Platforms | Windows 10/11 x64 only | **Windows, Linux, macOS** (amd64/arm64) |
| Startup | unpacks bundled runtime | instant |
| Concurrency | single-threaded | parallel hashing/compression-ready |
| GUI | Tkinter desktop app | CLI (GUI planned) |

The packaging core — PFS0/HFS0, RomFS/IVFC, NCA, CNMT/NACP/NPDM, NUP v1, PE patching — has **zero OS dependencies** in either implementation, so the port runs everywhere and can even patch Windows emulator executables *from* Linux or macOS.

**For AI coding agents**: start at [AGENTS.md](AGENTS.md) — commands, non-negotiables, and the package map. Architecture + contracts: [ARCHITECTURE.md](ARCHITECTURE.md).

## Install

### Prebuilt binaries (recommended)

Grab the latest release: **[github.com/dannolan/boxcar/releases/latest](https://github.com/dannolan/boxcar/releases/latest)**

| Download | Use on |
|---|---|
| 🍎 `boxcar-…-darwin-universal.tar.gz` | **macOS — Apple Silicon *and* Intel in one (recommended)** |
| `boxcar-…-darwin-arm64.tar.gz` | macOS Apple Silicon (M1/M2/M3/M4) |
| `boxcar-…-darwin-amd64.tar.gz` | macOS Intel |
| `boxcar-…-windows-amd64.zip` | Windows 10/11 x64 |
| `boxcar-…-windows-arm64.zip` | Windows on ARM |
| `boxcar-…-linux-amd64.tar.gz` | Linux x64 |
| `boxcar-…-linux-arm64.tar.gz` | Linux ARM64 |

macOS:

```bash
tar -xzf boxcar-*.tar.gz
cd boxcar-*/
xattr -d com.apple.quarantine boxcar   # binaries are unsigned; silence Gatekeeper once
./boxcar --help
```

Windows: extract the ZIP and run `boxcar.exe` (SmartScreen may ask — *More info → Run anyway*).

Verify a download:

```bash
shasum -a 256 -c SHA256SUMS.txt        # macOS / Linux
certutil -hashfile boxcar-4.0.0-windows-amd64.zip SHA256   # Windows
```

### From source

```bash
go install github.com/dannolan/boxcar/cmd/boxcar@latest
# or from a clone:
go build -o boxcar ./cmd/boxcar
```

Cross-compile for a friend:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o boxcar.exe ./cmd/boxcar
```

## Usage

### Build an NSP from an extracted game snapshot

```bash
boxcar build \
  --package "My Game Dump" \
  --output "My Game.nsp" \
  --title "My Game" \
  --display-version "1.2.3" \
  --artwork cover.png
```

Accepts the conventional dump layout (`exefs/` + `romfs/`, optionally under `base/` and `updates/<version>/`). The title ID is read from `exefs/main.npdm`. Artwork becomes a 256×256 embedded library icon (PNG/JPEG/GIF/BMP/TIFF/WEBP).

### Package DLC (RomFS-only add-ons)

```bash
boxcar dlc --package "dlc/0100abcdef0001001" --output "DLC.nsp" --base-game "My Game.nsp"
```

### NUP archives (compressed, multi-package)

```bash
boxcar nup --nup-operation pack       --input bundle/ --output bundle.nup --preset Balanced
boxcar nup --nup-operation nsp-to-nup --input game.nsp --output game.nup
boxcar nup --nup-operation extract    --input bundle.nup --output extracted/
boxcar nup --nup-operation nup-to-nsp --input bundle.nup --output restored/
boxcar nup --nup-operation inspect    --input bundle.nup
boxcar nup --nup-operation verify     --input bundle.nup
```

Presets: `Fast` (level 1), `Balanced` (3), `Smaller` (9). Identical files share one extent; files > 256 MiB stay raw unless you say otherwise.

### Emulator patch (reversible, offline)

```bash
boxcar patch inspect /path/to/emulator
boxcar patch apply   /path/to/emulator
boxcar patch remove  /path/to/emulator
```

Recognizes reviewed Selevet / Eglantine / PokeDinner builds by exact hash or normalized code fingerprint, backs up originals, and can restore them exactly. Targets are Windows x64 executables — but the patcher itself runs on any OS.

### Emulator key placeholders

```bash
boxcar keys inspect ~/.config/eden/keys
boxcar keys install ~/.config/eden/keys
```

Installs only locally-generated plaintext-repack placeholders to `prod.keys_autogenerated` — never your retail keys, never `prod.keys`.

## Fidelity

Port correctness is enforced by golden cross-validation against the Python original (tests shell out to a Python 3.11+ env with `Pillow`/`zstandard` when available; they skip gracefully otherwise):

- ✅ NSP output **byte-identical** to Python's for the same dump; Python's own verifier accepts Go output
- ✅ DLC NSP output **byte-identical** to Python's
- ✅ Emulator patch `transform()` **byte-identical** for all four profile kinds (+ legacy revision)
- ✅ NUP archives **cross-verify in both directions** (Go reads Python archives, Python reads Go archives)
- ✅ NSP → NUP → NSP round trip **byte-identical**

Known deviations: artwork input formats are PNG/JPEG/GIF/BMP/TIFF/WEBP (no AVIF/ICNS/JPEG2000 in pure Go); NUP zstd levels map to Go's klauspost speed tiers, so compressed sizes differ slightly (format stays cross-compatible); NSP→NUP conversion materializes region-backed files to temp files instead of mmap streaming. Full list: [ARCHITECTURE.md](ARCHITECTURE.md) § Deviations.

## Layout

```
cmd/boxcar/          CLI
internal/
  binfmt/ stream/ region/ diskfree/   shared keystones
  pfs0/ romfs/ nca/                   container writers + verifiers
  metadata/                           NPDM / NACP / CNMT
  snapshot/ nsp/ dlc/                 discovery + NSP engines
  nup/ nuptools/                      NUP v1 format + orchestration
  patch/                              PE surgery + embedded payloads
  artwork/ keys/                      icons + key placeholders
```

Module contracts and the port's design rules live in [ARCHITECTURE.md](ARCHITECTURE.md). Agent operating guide: [AGENTS.md](AGENTS.md).

## Testing

```bash
make check        # what CI runs: build + vet + gofmt + tests
make golden       # STRICT byte-exactness vs the Python reference (skips fail)
make race         # race detector on concurrent packages
make cross        # all release binaries into dist/
```

Golden tests compare Boxcar's output against the original Python
implementation and **skip silently** when the reference environment is
missing — set it up once with `make pyref` (see [docs/PYREF.md](docs/PYREF.md))
so a green suite actually proves byte-exactness. CI runs the strict mode on
every push against a pinned reference commit.

## License

Application code is MIT — see [LICENSE](LICENSE) (inherited from NSP-Box). Third-party notices in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) and [licenses/](licenses). This port was developed with AI assistance and independently validated against the reference implementation.

**Not affiliated with, sponsored by or endorsed by Nintendo.** Nintendo Switch and Nintendo Switch 2 are trademarks of Nintendo; game content belongs to its respective rights holders. Boxcar includes no Nintendo code, firmware, tickets, certificates or cryptographic keys.

## Credits

- [smarkyie/NSP-Box](https://github.com/smarkyie/NSP-Box) — the original application and this port's reference implementation
- [a55uka/nup](https://github.com/a55uka/nup) — the NUP format specification
- Franseven (PokeDinner) — native NSP support in the emulator
