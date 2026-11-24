# survival-drive

Build, verify and clone a self-contained offline library and AI assistant for Windows.

`survival-drive` is one Go binary (standard library only, no third-party modules) that:

- **resolves** a curated catalogue of public archives into a deterministic lock file
  (a size and checksum for every file: sha256, or md5 where the upstream publishes only that),
- **fetches** those files from public mirrors with parallel ranged downloads and resume,
- **builds** a drive-ready image: archives, models, servers, a one-click launcher,
  licence texts and a checksum manifest,
- **burns** the image to exFAT media, resuming and repairing as it goes,
- **verifies** every byte against the manifest.

The result works with no internet, no installer and no account. This repository contains the
tool and the catalogue, **not the content**: the content is downloaded from public archives and
stays under the licences of its authors.

---

## What a build contains

| Item | Purpose |
|---|---|
| `.zim` archives | Kiwix libraries: Wikipedia, Wikibooks, Wikisource, Wikivoyage, Wiktionary, Stack Exchange, Project Gutenberg, MedlinePlus, Appropedia, energypedia, Wikivoyage and more |
| `.gguf` models | local chat models served by `llama-server` |
| `bin\kiwix\kiwix-serve.exe` | serves the archives on `127.0.0.1` |
| `bin\llama-*\llama-server.exe` | Vulkan and CPU builds of llama.cpp |
| `START_SURVIVAL_AI.exe` | one-click launcher and dashboard |
| `README.txt` | buyer-facing guide, generated from the lock |
| `LICENSES\` | `NOTICE.md`, licence texts, and the GPL source offer |
| `_MANIFEST.sha256` | a checksum for every file |
| `_MANIFEST.sizes` | expected size per file, so the quick check catches truncation without reading every byte |
| `tools\` | this builder, the lock it used, and its source |

Two reference SKUs are in `profiles/`:

| Profile | Nominal media | Resolved size (sell) |
|---|---|---|
| `sku1` | 128 GB flash | ~90 GiB |
| `sku2` | 512 GB NVMe | ~410 GiB |
| `sku2` (full) | 512 GB NVMe | ~430 GiB |

Actual sizes come from the lock, not from this table. `sell` excludes items marked
`commercial_ok: false`; `full` is the personal-use variant that includes them.

---

## Requirements

**To build**

- Windows 10/11 x64 (the shipped binaries target Windows; the Go code also builds elsewhere)
- Go 1.24 or newer
- Free disk: roughly twice the content size (cache + image), plus the target media
- Internet access for `resolve` and `fetch`

**To run a built drive**

- Windows 10 x64 (1809+) or Windows 11
- 2 GB free RAM for the library; about 7 GB more to load the assistant model
- Any current browser, with JavaScript allowed for `127.0.0.1`
- Optional: a Vulkan-capable GPU; otherwise the assistant runs on the CPU
- No administrator rights, no internet

---

## Build

```bat
git clone https://github.com/threex0/survival-drive.git
cd survival-drive
go build -o bin\survival-drive.exe .\cmd\survival-drive
go build -o bin\START_SURVIVAL_AI.exe .\cmd\start-survival-ai
```

There are no dependencies to download: `go.mod` has no requirements.

---

## Quick start

```bat
REM 1. pin the catalogue (no download). ~2 minutes, ~1.2 MB lock file.
bin\survival-drive.exe resolve -profile profiles\sku1.json -variant sell ^
    -out cache\.ledger\sku1-sell.lock.json

REM 2. download the pinned files to a cache on a large drive.
bin\survival-drive.exe fetch -lock cache\.ledger\sku1-sell.lock.json -cache D:\cache

REM 3. assemble the image (README, tools, runtime DLLs, manifest).
bin\survival-drive.exe build -lock cache\.ledger\sku1-sell.lock.json -out D:\image\sku1-sell

REM 4. format the target once, then copy. THIS ERASES THE TARGET.
FORMAT_DRIVE.cmd G:
bin\survival-drive.exe burn -image D:\image\sku1-sell -drive G:

REM 5. prove the result.
bin\survival-drive.exe verify -drive G:
```

`RESOLVE.cmd` and `FETCH_SKU1.cmd` wrap steps 1 and 2. They use `SURVIVAL_CACHE` when set
(otherwise `.\cache`), so a 90 GiB cache can live on another drive:

```bat
set SURVIVAL_CACHE=F:\survival-cache
FETCH_SKU1.cmd
```

`fetch` is safe to interrupt and re-run: completed files are reused, pieces are verified before
they are kept, and a failed check triggers a repair instead of a silent corrupt image.

### Useful flags

| Command | Flag | Meaning |
|---|---|---|
| `resolve` | `-variant sell\|full` | commercial or personal variant |
| `resolve` | `-strict` | fail if anything is unpinned or only estimated |
| `fetch` | `-conns N` | parallel connections per file (default 12) |
| `fetch` | `-only ids` | fetch a subset, for smoke tests |
| `build` | `-allow-missing` | build a partial image instead of failing |
| `build` | `-rehash` | re-read every file to rebuild the manifest |
| `build` | `-runtime-dlls=false` | do not copy the MSVC runtime next to the servers |
| `burn` | `-resume` | continue an interrupted copy |
| `burn` | `-allow-fs` | permit a non-exFAT target |
| `verify` | `-quick` | presence and size only (fast; misses corruption) |
| `verify` | `-lock <file>` | audit a cache instead of a drive |

Run `survival-drive.exe help` for the complete list.

---

## Verifying a drive you were given

Every build ships the tool, the lock and the source under `tools\`, so a buyer does not have to
trust the seller:

```bat
REM full check: reads every byte, ~30-60 minutes at USB speeds
tools\survival-drive.exe verify -drive G:

REM quick check: presence and size only (the dashboard button does this)
tools\survival-drive.exe verify -drive G: -quick
```

To rebuild an identical image from public sources:

```bat
tools\survival-drive.exe fetch -lock tools\sku1-sell.lock.json -cache D:\cache
tools\survival-drive.exe build -lock tools\sku1-sell.lock.json -out D:\image
```

The lock pins every file by size and checksum. If an upstream file changes, the tool refuses it and
asks you to resolve again rather than silently producing a different drive.

---

## Repository layout

```
cmd\survival-drive\       resolve, fetch, build, burn, verify
cmd\start-survival-ai\    the launcher and dashboard
internal\resolve\         catalogue resolvers (Kiwix, Hugging Face, GitHub, static)
internal\fetch\           parallel ranged download, resume, piece repair
internal\image\           image tree, exFAT lint, licences, README, manifest
internal\burn\            copy, resume, verify while writing
internal\launcher\        service supervision, memory budget, HTTP API, UI
profiles\                 catalogue and per-SKU profiles
assets\                   licence texts and the drive README template
```

The catalogue is data, not code: add an item to `profiles/catalogue.json`, give it a resolver,
and it participates in budgeting, downloading, attribution and verification like everything else.

---

## Tests

```bat
go test ./...
node internal\launcher\ui\reload_test.js
gofmt -l .
```

The UI test is a dependency-free Node script that asserts the dashboard opens real
`target="_blank"` links (not iframes or popups), keeps polling inert, and keeps the library
independent of the assistant.

---

## Licence

This repository is **GNU General Public License v3.0 or later (GPL-3.0-or-later)**. The full text
is in [`LICENSE`](LICENSE); [`NOTICE`](NOTICE) records third-party components and the GPL source
offer.

The content the tool downloads is **not** part of this repository and remains under the licences
of its authors (mostly CC BY-SA 4.0, with public-domain and model-specific terms). See `NOTICE`.

This project is not affiliated with or endorsed by Wikimedia, Kiwix, Google DeepMind, Hugging Face,
Stack Exchange or any content provider. It contains Wikipedia content; it is not "official
Wikipedia".

Security reports: see [`SECURITY.md`](SECURITY.md).
