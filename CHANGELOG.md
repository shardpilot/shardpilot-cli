# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.0] - 2026-11-09

### Added

- `shardpilot symbols upload` sends a pdb or elf file too large for one request
  (over 63 MiB, up to 2 GiB) through the server's direct multipart lane: parts
  go straight to object storage, and the CLI waits for the server's verdict.
  Other formats over the single-request size, and any file over 2 GiB, fail
  locally with `upload_too_large`; a server without the lane exits `4`.

### Changed

- `--timeout` is per file on the single-request door, and per request and per
  part on the lane, whose session (2 h) bounds the file. It must be positive:
  zero or a negative value now exits `1` before any request.

## [0.3.0] - 2026-10-30

### Breaking changes

- `shardpilot symbols upload` now requires a build channel: pass
  `--channel develop` or `--channel release`, or set
  `SHARDPILOT_SYMBOLS_CHANNEL`. The flag wins over the variable. Without a
  channel, or with any other value (an empty one included), the command exits
  `1` before sending any request. Add a channel to existing upload jobs before
  upgrading. The channel is sent with each upload as the `channel` field.

## [0.2.0] - 2026-10-29

### Release archives

- Select hosted Ubuntu runners for public workflows and pin actions to full commit SHAs.
- Release archives carry `SHA256SUMS`; packaging verifies all six archive
  digests before publication. The install instructions verify the selected archive.

### Migration to 0.2.0-dev

The development version is now `0.2.0-dev`. Both symbol commands read only
`SHARDPILOT_SYMBOLS_KEY`; the former name is no longer accepted, including
when the new variable is empty. Update CI secret mappings and local exports.

| Old name | New name |
|---|---|
| `SHARDPILOT_API_KEY` | `SHARDPILOT_SYMBOLS_KEY` |

`SHARDPILOT_SYMBOLS_URL` and `--url` are unchanged.

### Changed

- **`symbols upload --platform` takes only the endpoint's platforms:** `windows`,
  `macos`, `linux`, `android`, `ios`, `tvos`, `web`, `ps4`, `ps5`, `xbox`,
  `switch`, `other`.
  - Case is ignored.
  - Engine and SDK spellings are folded first, as the endpoint folds them:
    `mac`, `osx`, `darwin` → `macos`; `win`, `win64` and `Windows*` target
    names → `windows`; `linuxarm64` → `linux`; `iphone`, `ipad`, `ipados` →
    `ios`.
  - A fold is noted on stderr, and the result line shows the platform sent.
  - Any other value exits `1` before any request and lists the accepted
    values. Before, any non-empty string was sent as typed. The endpoint would
    store a symbol under a platform no crash carries, or refuse it.

### Added

- Both symbol commands send `X-ShardPilot-SDK: cli/<version>` on every request,
  retry and listing page. The resolved binary version supplies the identity,
  with its leading `v` and build metadata removed; `User-Agent` is unchanged.
  Invalid SemVer versions or identities over 64 bytes fail before any request.

- **`shardpilot symbols missing`** lists the debug symbol files your crashes
  needed and did not find (`GET /api/v1/symbols/wanted`): every page,
  still-missing only unless `--all`, JSON under `--json`. A host that does not
  serve the endpoint (404) gets a specific message and the new exit code `4`.
- **`NOTICE` and `SECURITY.md`.** Apache-2.0 §4(d) obliges a redistributor to
  carry a NOTICE and this repository had none. SECURITY.md also says the thing
  a CLI needs to say about credentials: the key is read from
  `SHARDPILOT_SYMBOLS_KEY` and there is deliberately NO flag for it, because a
  credential on a command line is visible in the process table, lands in shell
  history, and in CI usually lands in a build log too.
- **`make release-dryrun`**, run on EVERY pull request: cross-compiles all six
  targets, packages each into a `.tar.gz` (Unix) or `.zip` (Windows) so the
  executable bit survives an HTTP download, writes `dist/SHA256SUMS` over
  the ARCHIVES, and asserts one non-empty binary per platform and one checksum
  line per platform. The release tag is stamped into the binary through
  `-ldflags -X`, so `shardpilot version` and the User-Agent report the release
  rather than `0.1.0-dev`. It is everything
  a tag build does except the two irreversible steps.
- **`.github/workflows/release.yml`**, triggered on a `v*` tag. It refuses a
  tag that is not an ancestor of `main`, refuses a dirty checkout (Go 1.24+
  stamps `+dirty` into the embedded version), refuses a CHANGELOG whose top
  section is still `[Unreleased]`, then uploads the six binaries and their
  checksums to a GitHub Release — marked pre-release for `-alpha`/`-beta`/`-rc`
  tags.

### Changed

- **The endpoint is `SHARDPILOT_SYMBOLS_URL`, defaulting to
  `https://symbols.shardpilot.com`.** Breaking: `SHARDPILOT_INGEST_URL` is no
  longer read. That variable is the analytics ingest origin in the ShardPilot
  SDKs, and the symbols routes are not served there, so a CLI pointed at it
  failed with a 404 that read like a rejected key. Both commands now resolve
  the base URL from `--url`, then `SHARDPILOT_SYMBOLS_URL`, then the public
  symbols host, so the common case needs no URL at all. Migration: rename the
  variable, or delete it if it pointed at the public host.
- **Plain `http://` is refused unless the host is `localhost` or a loopback
  address.** Every request carries the API key, and over plain http anyone on
  the path can read it. The CLI used to accept `http://` to any host. The
  check runs before any request, exits `1`, and names the host. Local testing
  against `http://localhost:<port>` or `http://127.0.0.1:<port>` still works.
- **Redirects are never followed.** Go's HTTP client keeps the
  `Authorization` header on a redirect to the same hostname, whatever the
  port or scheme, so a redirect could carry the key to a listener nobody
  named, or from `https://` to plain `http://`. The key now goes only to the
  URL that was set. The symbols routes never redirect, so a 3xx exits `4`
  with a message naming the redirect, and is not retried.
- **A 404 from `symbols upload` exits `4`, not `2`.** The upload route never
  answers 404 itself, so a 404 means the host does not serve it. The message
  now says so and that nothing is wrong with the key or the file, instead of
  sending you to debug your credentials. `symbols missing` already did this.
- **`symbols upload` sends every format the endpoint accepts: `elf`,
  `proguard` and `il2cpp_map` join `breakpad`, `dsym` and `pdb`.** The CLI
  used to detect ELF and refuse it, and refused `proguard` and `il2cpp_map`
  even when named, though the endpoint accepts all three, so those symbols
  could not be uploaded at all. ELF is detected from its magic; `proguard`
  and `il2cpp_map` have none and must be named with `--format`. `--debug-id`
  is required for `pdb`, `proguard` (the app build id) and `il2cpp_map` (the
  native module's debug id), and refused without it before anything is sent;
  it is optional for `elf`.
- **Internal ShardPilot references are gone from everything a user or a
  package index sees.** The package doc comment — which publishes verbatim to
  pkg.go.dev the moment this repository is public — cited an internal decision
  record and named an internal service. So did two `symbols upload` ERROR
  MESSAGES, printed to a customer's terminal. An internal server-side feature
  flag and its rollout state were named in a source comment. All replaced with
  statements about what the CLI and the endpoint do.
- **"the door" is now "the endpoint" or "the server" throughout**, in help
  text, error messages, README and comments. It is ShardPilot's internal word
  for a server route and means nothing to a reader outside it.
- **The README no longer promises an install that cannot work.** It said
  `go install …@latest` and that "GitHub Releases binaries will accompany the
  first tagged release". This repository has **no tags at all**, so `@latest`
  resolves to a pseudo-version at best and to nothing while the repository is
  private. The section now says there is no released version yet, shows the
  build-from-checkout path that does work, and shows what the released path
  will look like.

### Added

- Symbol uploads now declare an exact `Content-Length` (multipart frame +
  file size, computed without buffering — the body still streams) and send
  the `X-Shardpilot-Symbol-Format` admission hint header. Neither changes how
  an upload behaves today. Files whose size cannot be stat'd fall back to
  chunked encoding exactly as before.

- Go module bootstrap (`github.com/shardpilot/shardpilot-cli`, Go 1.26.5,
  stdlib only) with `cmd/shardpilot` entry point, Makefile (`build`,
  `build-all` release matrix), and CI (gofmt check, vet, test, build).
- `shardpilot symbols upload` v0: streaming multipart upload to
  `POST /api/v1/symbols/upload` with bearer auth from `SHARDPILOT_SYMBOLS_KEY`
  (environment-only by design), the endpoint's six formats (breakpad, dsym,
  pdb and elf detected content-first; proguard and il2cpp_map only via an
  explicit `--format`; `--debug-id` required for pdb, proguard and
  il2cpp_map), 4× retry with backoff on 5xx/network (never on 4xx), typed exit
  codes, serial multi-file uploads with per-file result lines, and a `--json`
  machine-readable output mode.
