# Third-party notices

The app's MIT license applies to its original Python source. Bundled dependencies retain their own licenses. Preserve this file, `LICENSE.txt`, and `licenses/` when redistributing this release.

| Component | Version in release | Notice / license copy |
|---|---|---|
| CPython | 3.11.9 x64 | `licenses/CPython-LICENSE.txt` (PSF and included component notices) |
| Tcl/Tk | 8.6, as supplied with CPython | `licenses/Tcl-license.terms`, `licenses/Tk-license.terms` |
| Pillow | 12.2.0 | `licenses/Pillow-LICENSE.txt` (MIT-CMU and included library notices) |
| python-zstandard | 0.25.0 | `licenses/python-zstandard-LICENSE.txt` (BSD 3-Clause) |
| Zstandard | 1.5.7 | `licenses/Zstandard-LICENSE.txt` (BSD license option) |
| PyInstaller bootloader | 6.19.0 | `licenses/PyInstaller-COPYING.txt` (GPL with bootloader exception) |
| OpenSSL | Runtime version recorded in BUILD_INFO.json | `licenses/OpenSSL-LICENSE.txt` (Apache License 2.0) |
| Microsoft Visual C++ runtime | Redistributable supplied with CPython | Microsoft runtime component; ownership and terms remain with Microsoft. |

Pillow's full bundled notice file contains the notices for its image/font/compression libraries. Portions of this software use the FreeType Project (https://www.freetype.org); see the FreeType license text in that file. Not every optional Pillow codec is necessarily loaded by this app.

PyInstaller's bootloader exception permits distribution of bundled applications under their own license; the exact exception is reproduced in the included COPYING file. The app does not include a modified PyInstaller bootloader. Upstream bootloader source corresponding to the build tool release is available at https://github.com/pyinstaller/pyinstaller/tree/v6.19.0/bootloader .

No Nintendo game files, artwork, firmware, tickets, certificates, cryptographic keys or SDK implementation are included. Placeholder values under `compatibility/` are generated locally from public fixed labels and cannot decrypt retail content. No Eglantine binaries are bundled. Product names identify compatibility targets and do not imply affiliation or endorsement.

The optional emulator patch includes an original MIT-licensed native probe (`native/container_probe.c`), compiled with Zig 0.16.0. It has no linked runtime, imports or bundled compiler. Eglantine and PokeDinner source retain their upstream licenses; only compatibility fingerprints and generic ABI anchors are included, not their executable functions. See `EMULATOR_PATCH.md` for source references and rebuild instructions. Zig, pefile and Capstone are developer tools, not runtime dependencies shipped in the app.

Nintendo Switch, Nintendo Switch 2 and related marks belong to Nintendo. Game content and other protected materials belong to their respective rights holders. This independent project is not affiliated with or endorsed by Nintendo and does not support piracy. See [DISCLAIMER.md](DISCLAIMER.md) for ownership and lawful-use notices.

Upstream projects:

- Python: https://www.python.org/
- Tcl/Tk: https://www.tcl.tk/
- Pillow: https://python-pillow.github.io/
- PyInstaller: https://pyinstaller.org/
- OpenSSL: https://openssl.org/
