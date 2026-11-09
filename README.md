# shardpilot-cli

> Go command-line interface for ShardPilot developers and operators. v0 ships **`shardpilot symbols upload`** — one static binary studios drop into CI to ship debug symbols (Breakpad `.sym`, zipped dSYM, PDB, ELF, R8/ProGuard mappings, IL2CPP line maps) to the ShardPilot crash platform — and **`shardpilot symbols missing`**, which lists the symbol files your crashes needed and did not find.

The wire contract this CLI is built against is the ShardPilot symbol-upload API (`POST /api/v1/symbols/upload`).

The public source snapshot contains the staged files outside `EXPORT-EXCLUDE`. Run `make export-dryrun` to verify the snapshot and its `make ci` plan. The public surface check compares complete repository names in links, API URLs and raw-content URLs with its public allowlist. It also flags explicit codename declarations, decision references and loopback port literals. Its IPv6 port check covers the canonical compressed and fully expanded loopback forms.

## Status

**v0 — `symbols upload` and `symbols missing` are implemented and tested.**

> **Both commands talk to `https://symbols.shardpilot.com` by default.** Set `--url`, or `SHARDPILOT_SYMBOLS_URL`, only to use a different host. Plain `http://` is refused unless the host is `localhost` or a loopback address, because every request carries your API key. See [Endpoint](#endpoint).

## Install

Releases are published on the [releases page](https://github.com/shardpilot/shardpilot-cli/releases):
one archive per platform plus `SHA256SUMS`, verified and installed as shown below.
To build from a checkout instead:

```sh
make build            # static host binary in bin/shardpilot
make build-all        # release matrix in dist/: linux, darwin, windows × amd64, arm64
```

After a version is published, pin that exact version for source installation:

```sh
go install github.com/shardpilot/shardpilot-cli/cmd/shardpilot@v0.4.0
```

`go install` compiles the package directly and never applies this repository's
linker flags, so the binary reads its version from Go's embedded build info
instead — `shardpilot version` reports the module version you asked for either
way.

For a published release, download the matching archive and `SHA256SUMS`.
Archives cover Linux, macOS and Windows on amd64 and arm64; each carries the
static binary, `LICENSE` and `NOTICE`. Unix archives preserve executable mode.
Select the published version (the examples use `v0.4.0`) and its platform archive:

```sh
set -eu
version=v0.4.0
asset=shardpilot_linux_amd64.tar.gz
gh release download "$version" --repo shardpilot/shardpilot-cli \
  --pattern "$asset" --pattern SHA256SUMS
awk -v asset="$asset" '$2 == asset {print}' SHA256SUMS > selected-checksum
test "$(wc -l < selected-checksum)" -eq 1
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum --check selected-checksum
else
  shasum -a 256 --check selected-checksum
fi
tar -xzf "$asset"
mkdir -p "$HOME/.local/bin"
install -m 755 shardpilot "$HOME/.local/bin/shardpilot"
```

Verify before extraction or execution. On macOS select a `darwin` archive;
on Windows select a `.zip` and compare `Get-FileHash -Algorithm SHA256` against
its exact entry in `SHA256SUMS` before extracting. Add the installation directory
to your `PATH` as needed. Release packaging verifies all six archive digests.

Every symbol upload and listing request, including retries and later pages,
sends `X-ShardPilot-SDK: cli/<version>`. The version comes from the same linker
stamp or embedded module version as `shardpilot version`; only this header
removes a leading `v` and SemVer build metadata. `User-Agent` keeps the original
resolved version. An invalid SemVer version or an identity longer than 64 bytes
stops either command before it sends a request.

## Quickstart

```sh
export SHARDPILOT_SYMBOLS_KEY="sp_live_..."       # symbols:write scope — env only, never a flag
export SHARDPILOT_SYMBOLS_CHANNEL=release         # build channel: develop or release (--channel overrides it)

# Breakpad text symbols — format auto-detected from content
shardpilot symbols upload --platform linux ./out/libgame.sym

# Zipped dSYM bundle
shardpilot symbols upload --platform ios ./MyApp.app.dSYM.zip

# Windows PDB — debug_id is required until the server parses PDB metadata
shardpilot symbols upload --platform windows --debug-id "$DEBUG_ID" ./Build/MyGame.pdb

# An Android .so — ELF is detected from the content
shardpilot symbols upload --platform android ./obj/arm64-v8a/libgame.so

# An R8/ProGuard mapping — never detected, so name it; --debug-id is the app build id
shardpilot symbols upload --platform android --format proguard --debug-id "$APP_BUILD_ID" ./mapping.txt

# Several files, machine-readable results (one JSON object per line)
shardpilot symbols upload --json --platform linux a.sym b.sym
```

**Files too large for one request.** A pdb or elf file over 63 MiB goes through
the server's direct multipart lane (`POST /api/v1/symbols/upload-sessions`),
up to 2 GiB. The CLI hashes the file, sends its 64 MiB parts straight to object
storage (`--timeout` bounds each part), then waits while the server verifies
it; the result line adds the bytes and the session id. A larger file in any
other format, or any file over 2 GiB, fails before a request with
`upload_too_large`, and a server that does not serve the lane exits `4`.

Files upload **serially, one result line per file**, and the process exits non-zero if any file failed. Uploads **stream** from disk (multipart over a pipe) — multi-GiB PDBs never need to fit in memory. The CLI declares an **exact `Content-Length`** and sends the `X-Shardpilot-Symbol-Format` admission hint on every upload. **A single request is bounded at 64 MiB** (63 MiB of file, after the multipart frame); a larger pdb or elf goes through the multipart lane above, and any other file over it fails with `upload_too_large`. Transient failures (5xx, network) are retried 4 times with backoff; 4xx rejections are never retried.

### Authentication

The API key is read **only** from `SHARDPILOT_SYMBOLS_KEY` and sent as `Authorization: Bearer <key>`. There is deliberately no `--api-key` flag: secrets passed as flags leak into `argv`, process listings (`ps`), and shell history. Use your CI system's secret store (see recipes below). The key needs the `symbols:write` scope.

### Endpoint

Both commands use the same base URL, chosen in this order:

1. `--url`;
2. `SHARDPILOT_SYMBOLS_URL`;
3. `https://symbols.shardpilot.com`, the public symbols host.

The CLI appends `/api/v1/symbols/upload` or `/api/v1/symbols/wanted`. The base URL must be `https://`. Plain `http://` is accepted only when the host is `localhost` or a loopback address (`127.0.0.0/8`, `::1`), for local testing; any other `http://` URL exits `1` before a request is made, so the key is never sent unencrypted. **Redirects are never followed**, so the key goes only to that URL: a redirect exits `4`, like a 404.

| Environment variable | Meaning |
|------|---------|
| `SHARDPILOT_SYMBOLS_KEY` | API key with the `symbols:write` scope (required; never a flag) |
| `SHARDPILOT_SYMBOLS_URL` | Endpoint base URL when `--url` is not given (default `https://symbols.shardpilot.com`) |
| `SHARDPILOT_SYMBOLS_CHANNEL` | Upload build channel when `--channel` is not given: `develop` or `release` |

### Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `--url` | `$SHARDPILOT_SYMBOLS_URL`, then `https://symbols.shardpilot.com` | Endpoint base URL |
| `--channel` | `$SHARDPILOT_SYMBOLS_CHANNEL` | *(required)* Build channel of the uploaded symbols: exactly `develop` or `release`. A given flag wins over the variable, even when empty. With neither set, or any other value (an empty one included), the command exits `1` before any request |
| `--platform` | *(required)* | One of `windows`, `macos`, `linux`, `android`, `ios`, `tvos`, `web`, `ps4`, `ps5`, `xbox`, `switch`, `other`. Case is ignored. `mac`, `osx`, `darwin` → `macos`; `win`, `win64` and `Windows*` target names → `windows`; `linuxarm64` → `linux`; `iphone`, `ipad`, `ipados` → `ios`. Any other value is refused before upload (exit `1`). A symbol is matched to a crash by platform, so a symbol stored under a guessed one would never match |
| `--format` | `auto` | `auto`, `breakpad`, `dsym`, `pdb`, `elf`, `proguard`, or `il2cpp_map` |
| `--debug-id` | | Debug identifier, single file only. **Required for `pdb` and `il2cpp_map`**; for `proguard` it is **the app build id** and required; optional for `elf` |
| `--timeout` | `5m` | Upload timeout: per file on the single-request door; per request and per part on the multipart lane, where the session (2 h) bounds the file |
| `--json` | off | One JSON result object per file on stdout |

Flags come before file paths: `shardpilot symbols upload [flags] <path>...`

### Exit codes

| Code | Meaning |
|------|---------|
| `0` | every file uploaded |
| `1` | usage or local error (bad flags, plain `http://` to a remote host, unreadable file, undetectable format) |
| `2` | the server rejected an upload (4xx other than 404: auth, validation, size cap) — never retried |
| `3` | server or network failure (5xx after retries, timeout) |
| `4` | this URL does not serve the endpoint (it answered 404, or a redirect, which is never followed) — check `--url` / `SHARDPILOT_SYMBOLS_URL`; nothing is wrong with your API key or your symbols |

With multiple files the exit code reflects the first failed file.

## Listing missing symbols

```sh
export SHARDPILOT_SYMBOLS_KEY=sp_live_...   # symbols:write scope
shardpilot symbols missing
```

`shardpilot symbols missing` reads `GET /api/v1/symbols/wanted` and prints one line per symbol identity your crashes needed and did not find: the platform, the format, and the debug id (for ProGuard, the app build id) an upload would carry, how often it was seen and when last. Every page is read. `--all` also lists identities a later upload has satisfied, marked `available since <time>`; `--json` prints one JSON object per identity. A count goes to stderr.

> **Not every ShardPilot host serves this endpoint yet.** A host that does not answers 404. The command then prints `<host> does not serve GET /api/v1/symbols/wanted (http 404); nothing is wrong with your API key or your symbols` and exits `4`. Nothing is retried, and nothing is wrong with your setup.

| Code | Meaning |
|------|---------|
| `0` | listed (the list may be empty) |
| `1` | usage or local error (bad flags, plain `http://` to a remote host) |
| `2` | the server rejected the request (4xx other than 404: auth, validation) — never retried |
| `3` | server or network failure (5xx after retries, timeout) |
| `4` | this URL does not serve the endpoint (it answered 404, or a redirect, which is never followed) |

## Symbol formats

Detection mirrors the server's rule: **content sniffing first** (magic bytes are authoritative — a Breakpad text file named `libgame.so` is still `breakpad`), file extension only as a tiebreak.

These are the six formats the endpoint accepts.

| Format | Detected by | `--debug-id` | Notes |
|--------|-------------|--------------|-------|
| `breakpad` | `MODULE ` header; `.sym` | no | Breakpad text symbols |
| `dsym` | zip / Mach-O magic; `.dsym`, `.zip` | no | upload the **zipped** dSYM bundle |
| `pdb` | `Microsoft C/C++ MSF` magic; `.pdb` | **required** | the endpoint cannot read PDB metadata yet |
| `elf` | `\x7fELF` magic; `.so` | optional | the server reads the build id from the object where it can, and answers 400 where it needs one passed |
| `proguard` | never detected (plain text) | **required**: the app build id | pass `--format proguard`; needs an API key scoped to one app |
| `il2cpp_map` | never detected (generic JSON) | **required**: the native module's debug id | pass `--format il2cpp_map` |

`proguard` and `il2cpp_map` files have no magic bytes, so the CLI never guesses them: a file in either format is an undetectable-format error until you name it with `--format`. A format that needs `--debug-id` is refused without one before anything is sent (exit `1`).

## CI recipes

### GitHub Actions

```yaml
- name: Upload symbols to ShardPilot
  env:
    SHARDPILOT_SYMBOLS_KEY: ${{ secrets.SHARDPILOT_SYMBOLS_KEY }}
  run: shardpilot symbols upload --channel release --platform linux out/symbols/*.sym
```

### Unreal Engine — post-package step

After packaging produces the `.pdb` next to your binary (Windows shells expand `%VAR%`):

```bat
shardpilot symbols upload --channel release --platform windows --debug-id %DEBUG_ID% "%ProjectDir%\Binaries\Win64\MyGame.pdb"
```

### Unity — post-build hook

```csharp
using UnityEditor.Callbacks;

public static class ShardPilotSymbols
{
    [PostProcessBuild]
    public static void Upload(UnityEditor.BuildTarget target, string buildPath)
    {
        // SHARDPILOT_SYMBOLS_KEY comes from the CI job environment — never
        // hardcode it in the project.
        var p = System.Diagnostics.Process.Start("shardpilot",
            $"symbols upload --channel release --platform android \"{buildPath}/symbols/libil2cpp.sym\"");
        p.WaitForExit();
        if (p.ExitCode != 0) throw new System.Exception($"symbol upload failed ({p.ExitCode})");
    }
}
```

### Gradle (Android)

```gradle
tasks.register("uploadShardpilotSymbols", Exec) {
    // SHARDPILOT_SYMBOLS_KEY comes from the CI environment.
    commandLine("shardpilot", "symbols", "upload",
        "--channel", "release", "--platform", "android",
        "$buildDir/symbols/libgame.sym")
}
```

## Development

Go `1.27.3`, stdlib only — no CLI framework, no third-party dependencies. `make fmt` and `make fmt-check` format with the gofmt beside the `go` they resolve, and refuse to run unless that installation reports exactly `go1.27.3`: a newer 1.27.x does not substitute. They derive the exact formatter release from the `go.mod` directive and pin `GOTOOLCHAIN=local`, so nothing here selects or downloads a toolchain: the `go` on your PATH must BE 1.27.3 — a 1.27.3 in the module cache behind an older `go` is refused — and with it installed the targets work offline. `gofmt` is the only formatter; there is no golangci-lint config, matching the org convention for SDK-adjacent repos.

```sh
make ci      # gofmt check + go vet + go test + build + analyzer interrupt controls
go test ./...
```

CI runs the same gates on every PR (`.github/workflows/ci.yml`). Public repositories select Ubuntu-hosted runners; CI checks the runner policy.
Checkouts with the static analyzer inputs also run blocking enum coverage and emit advisory NilAway JSON status. Both use the same project arguments.

## Related

- [`shardpilot-go`](https://github.com/shardpilot/shardpilot-go) — Go client SDK; shares the stdlib-only conventions.

## License

Apache-2.0. See [LICENSE](./LICENSE).
