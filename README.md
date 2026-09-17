# torrentfs

Mount BitTorrent downloads as a FUSE filesystem.

> **Status: M5 — robustness and release.** `torrentfs` takes one writable
> `torrents` directory, continuously reconciles its direct `.torrent` files,
> and mounts a torrent data tree. The mount is **data only and read-only**: a
> single-file torrent is exposed directly as a regular file, e.g.
> `<mount>/movie.mp4`, so a player can open and seek it without a wrapper
> directory; a multi-file torrent keeps its directory tree. Repeated reads use
> an in-memory piece cache.
>
> Torrent management — add by upload or magnet, list, delete, and per-torrent
> piece status — lives in the HTTP API, not in the mount. Durable managed
> metainfo and pending magnet intents are stored under
> `<torrents-dir>/.metadata`, an implementation detail that is never mounted.
> Piece state is not persisted anywhere: it is read from the running torrent
> client on demand, and completion after a restart comes from verifying the
> downloaded payload.

## Build

Requires Go 1.27+ and the Node version in `web/.nvmrc`.
The Web UI is built before Go packages so `web/dist` is available to the Go
`embed` package; generated `web/dist` and `web/node_modules` are not committed.

```sh
npm ci --prefix web
npm run typecheck --prefix web
npm run lint --prefix web
npm test --prefix web
npm run build --prefix web
rm -rf -- web/node_modules
go build ./...
go test ./...
go test -race ./...
go vet ./...
golangci-lint run ./...
```

For a build-only path such as the nightly package, use `./scripts/build-web.sh`;
it runs the lockfile install, builds `web/dist`, and performs the same cleanup.

`./scripts/build-web.sh` uses the lockfile, produces `web/dist`, and removes
`web/node_modules` after the build so Go's recursive package commands do not
inspect example source shipped inside JavaScript dependencies. CI performs the
frontend checks and the same cleanup before running Go quality checks.

## Nightly builds

The `Nightly` GitHub Actions workflow runs every day at **16:17 UTC** (**00:17
Beijing time the following day**) against the exact `main` commit that triggered
it. It can also be started from the Actions page with `workflow_dispatch`; select
`main` in the branch selector. Pull requests, forks, and other refs are not
published.

A successful run publishes a Linux/amd64, `CGO_ENABLED=0` tarball and its
`.sha256` file as an immutable prerelease. The nightly tag's UTC date comes from
the triggering commit so rerunning the same workflow run keeps the same tag. The
archive includes `BUILD_INFO`,
which records the full commit SHA, UTC date, nightly tag, Go version, target,
and workflow run. Actions artifacts are retained for 14 days; nightly
prereleases are retained for 30 days before the workflow removes only matching
`nightly-*` prereleases and tags. Nightly builds are for validation and testing,
not stable releases, and running the binary requires the Linux FUSE facilities
described below.

After downloading both files, verify and unpack them from the same directory:

```sh
sha256sum --check torrentfs-nightly-<date>-<short-sha>-<run-id>-linux-amd64.tar.gz.sha256
tar -xzf torrentfs-nightly-<date>-<short-sha>-<run-id>-linux-amd64.tar.gz
```

## Usage

```sh
go run ./cmd/torrentfs -mountpoint <dir> [-config <file>] [-data-dir <dir>] <torrents-dir>
```

`-mountpoint` is required unless the HTTP API is enabled
(`http.listen_addr` is set); without it, torrentfs runs headless and is
managed entirely over HTTP. `<torrents-dir>` is exactly one existing, readable
and writable directory. A file path such as
`/data/torrentfs/input.torrent` is rejected: single-file positional input is
not supported. Without `-config`, defaults are used; a TOML file loads the
sections shown in `torrentfs.example.toml`, and an explicit `-data-dir`
overrides `[paths].data_dir`. `-data-dir` stores downloaded torrent data only;
it is distinct from `<torrents-dir>`.

At startup, torrentfs restores managed metadata and scans only direct regular,
non-symlink files in `<torrents-dir>` whose names end in lower-case `.torrent`.
It does not recurse into subdirectories. The directory is reconciled about
once per 100 ms while the process runs: adding a stable valid `.torrent` loads
it without restart, and removing a source releases its torrent when no other
directory source or managed metadata source refers to the same info hash.
Duplicate files for one hash share one torrent. Configuration is read at every
startup; changing the file takes effect after a restart, not through SIGHUP.

Write sources through a temporary filename such as `input.torrent.part`, then
atomically rename it to `input.torrent`. Torrentfs checks file identity, size,
and modification time before and after parsing, so it does not load a file that
changes while being read. A malformed runtime replacement leaves an already
loaded source active and is retried after the file changes; a stable malformed
file present at startup fails startup with its path. Symlinks, temporary files,
other extensions, and torrent-named directories are ignored.

### Mounted layout

```text
<mount>/
├── <single-name>       # single-file torrent: one regular file, playable directly
└── <multi-name>/       # multi-file torrent: the usual directory tree
    └── <relative-file>
```

The mount contains torrent data only. A torrent whose metainfo has no directory
structure (a single-file torrent) is exposed as the regular file
`<mount>/<name>`; a player can open `<mount>/movie.mp4` and seek it directly.
A multi-file torrent keeps its directory tree, and a torrent that holds one
file but has directory structure stays a directory. Every node is read-only:
creates, writes, unlinks, and renames anywhere in the mount fail with `EROFS`.

There is no `metadata/` or `stats/` control directory, and `metadata` and
`stats` are no longer reserved root names: a torrent with either display name
appears as an ordinary data node. Torrents whose display names collide are
still disambiguated by appending a hash prefix. This is a breaking change for
scripts that wrote to `<mount>/metadata/` or read `<mount>/stats/`; both are
now served by the HTTP API instead.

### Managing torrents and reading piece state

All management happens over the authenticated HTTP API (`/api/v1`):

| Request | Purpose |
| --- | --- |
| `POST /api/v1/auth/login` | Exchange the configured username and password for an in-memory Bearer token |
| `POST /api/v1/auth/logout` | Revoke the presented in-memory Bearer token |
| `POST /api/v1/torrents` | Add a torrent from an uploaded `.torrent` (multipart) or a magnet URI (JSON) |
| `GET /api/v1/torrents` | List every task |
| `GET /api/v1/torrents/{id}` | One task's aggregate state |
| `GET /api/v1/torrents/{id}/status` | Per-piece and per-file status snapshot |
| `DELETE /api/v1/torrents/{id}?purge_data=<bool>` | Delete a task, optionally purging its payload |
| `GET /api/v1/operations/{id}` | Poll a deletion operation |

`GET /api/v1/torrents/{id}/status` returns one fresh, consistent snapshot:

```json
{
  "torrent": {"id": "40-lowercase-hex", "info_hash": "40-lowercase-hex", "state": "downloading"},
  "metainfo_ready": true,
  "piece_length": 262144,
  "pieces": [{"index": 0, "known": true, "complete": true, "partial": false, "wanted": true, "checking": false}],
  "files": [{"path": "sub/file.bin", "size": 1234, "piece_start": 0, "piece_end": 1}]
}
```

`pieces` is the whole torrent in absolute, zero-based, ascending piece order.
Each file reports a half-open `[piece_start, piece_end)` range into that same
array, so a piece spanning a file boundary is referenced by both files instead
of being duplicated. Only partial pieces carry `available_bytes`. A task whose
metainfo has not arrived yet (an unresolved magnet) returns `200` with
`metainfo_ready: false` and empty arrays; an unknown id returns `404`.
When authentication is enabled, the login route is public and every other
`/api/` route requires an explicit `Authorization: Bearer <token>` header. The
embedded Web UI's static `GET`/`HEAD` shell and assets are public so a browser
can load the document before it has a token; they contain no torrent data. When
authentication is disabled, the API retains its anonymous loopback behavior.
There is no anonymous non-loopback API exception.

### Web UI

When the HTTP service is enabled, the same listener serves the embedded Web UI
and `/api/v1`. Open the listener address in a browser; the shell supports the
Dashboard, torrent detail, Files, Pieces, magnet/file add, and asynchronous
delete flows. Files and pieces are read from the existing API snapshot, so the
UI does not expose local data paths, add frontend-specific endpoints, or claim
byte-level precision that the API cannot provide. Peer counts are intentionally
shown as unavailable because they are not part of the public torrent DTO.

During development, run Vite from `web`:

```sh
npm ci --prefix web
npm run dev --prefix web
```

Vite serves on `:5173` and proxies `/api` to `http://127.0.0.1:8080`. A
production build is same-origin and uses relative `/api/v1` requests. The UI
keeps Bearer tokens in memory only: it does not use cookies, URL parameters,
`localStorage`, `sessionStorage`, JWT decoding, or a refresh endpoint. A page
refresh or daemon restart therefore requires a fresh connection probe and,
when enabled, a new login. Valid API requests slide the server-side inactivity
window; the browser treats a `401` as the authoritative signal to show the
login gate.

## On-disk state

```text
<torrents-dir>/
  <name>.torrent                     # user-owned source files (scanned, watched)
  .metadata/<info-hash>.torrent      # managed metainfo published by the API
  .metadata/<info-hash>.magnet       # durable intent for an unresolved magnet
  .metadata/<legacy-name>.torrent    # legacy managed source, restored but never rewritten
  .stats/                            # legacy empty directory: ignored, never created, never removed

<data-dir>/
  payload/<info-hash>/...            # or paths.payload_dir; the real download data
  state/<info-hash>.json             # interrupted-deletion sidecars only, no piece state
```

`.metadata` is an implementation detail of the torrents directory: it is
persisted, restored on startup, and never mounted. A legacy `.stats` directory
from an older version is ignored — the current release neither creates it nor
deletes it, and it holds no piece state. Piece completion is not stored
anywhere: it is derived from the torrent client and from verifying the
downloaded payload. Downloaded data is rechecked after a restart, but the
in-memory piece cache, cache hit count, and transient read priorities are not
retained.

A magnet URI is accepted immediately: its intent is published as
`.metadata/<info-hash>.magnet` before registration, so a restart before the
metainfo arrives retries the fetch. Once the metainfo resolves it is published
as the canonical `.torrent` and the pending `.magnet` is removed; if both ever
exist, the `.torrent` wins. `DELETE /api/v1/torrents/{id}` removes every
internal metadata source for that hash — canonical and legacy names, plus a
pending magnet — so a deleted task cannot reappear after a restart. A torrent
still referenced by a user-owned top-level `.torrent` file is refused with
`409`, and its payload is never purged.

## Configuration

The supported TOML keys are:

```toml
[paths]
data_dir = "./torrentfs-data"
payload_dir = ""

[http]
listen_addr = "127.0.0.1:8080"
max_upload_bytes = 10485760

[http.auth]
enabled = false
username = ""
# Use either password_hash or password_hash_file, never both.
password_hash = ""
password_hash_file = ""
token_ttl = "30m"

[connections]
listen_host = ""
listen_port = 0

[proxy]
socks5_url = ""

[cache]
capacity_bytes = 67108864

[identity]
tracker_user_agent = "qBittorrent/4.4.0"
peer_id_prefix = "-qB4400-"
extended_handshake_client_version = "qBittorrent/4.4.0"
```

`payload_dir` is the managed root for per-torrent payload directories; each
torrent owns `<payload_dir>/<info_hash>`, and that is the only directory ever
purged by `purge_data=true`. An empty value uses `<data_dir>/payload`.

An empty `[http].listen_addr` disables the API. The default binds loopback
only. Binding a non-loopback address requires a complete enabled
`[http.auth]` configuration; this service does not provide TLS, so put
non-loopback deployments behind a TLS reverse proxy.

When `http.auth.enabled` is true, `username` and exactly one bcrypt
`password_hash` or owner-readable-only `password_hash_file` are required.
`password_hash` is a bcrypt hash, never a plaintext password.
`password_hash_file` must point to a regular, non-symlink file readable only by
its owner. The default `token_ttl` is 30 minutes and may not exceed 24 hours. Log in with
`POST /api/v1/auth/login` using `{"username":"...","password":"..."}`;
the response contains an opaque Bearer token. Send it explicitly as
`Authorization: Bearer <token>` on later requests. Tokens are held only in
memory, expire after a period without a valid request, and slide their expiry
by `token_ttl` after every valid authenticated request, so continuous use can
slide indefinitely and there is no absolute session lifetime. They are all
invalidated when the process restarts. `POST /api/v1/auth/logout` revokes the presented
token. There is no JWT, refresh endpoint, refresh token, Cookie authentication,
or automatic Cookie renewal; because authentication uses an explicit header,
this configuration does not add Cookie-based CSRF behavior.

When authentication is disabled, loopback HTTP retains the anonymous development
behavior. Authentication is not optional for non-loopback listeners.
`max_upload_bytes` caps an uploaded `.torrent` body.

`capacity_bytes` is a byte limit. An empty `socks5_url` disables the proxy;
otherwise use `socks5://` or `socks5h://`, optionally with username/password.
The proxy applies to TCP peer connections and HTTP(S) tracker, metainfo, and
webseed requests. UTP, DHT, and UDP tracker traffic are disabled or rejected in
proxy mode, so there is no direct UDP fallback. Incoming TCP listening remains
controlled by `[connections]` and is not routed through the SOCKS5 proxy.

The default identity is qBittorrent 4.4.0: `tracker_user_agent` and the BEP 10
extended-handshake `v` value are `qBittorrent/4.4.0`, and `peer_id_prefix` is
`-qB4400-`. `[identity].tracker_user_agent` changes only the `User-Agent`
header on HTTP tracker announce requests; it does not change metainfo, webseed,
or scrape requests. `peer_id_prefix` is a prefix, not a complete peer ID: it is
limited to 20 bytes, and any remaining bytes are generated randomly for each
session. A 20-byte prefix leaves no random suffix. The generated peer ID is
used for BitTorrent handshakes and announces. Explicit TOML values override
the defaults; explicitly setting an identity value to an empty string delegates
that field to the anacrolix default. `v` is sent only when the peer supports the
extended handshake.

## Error behavior

Reads never fabricate data. When the pieces behind a range are unavailable, a
read either blocks until they arrive or returns an error; it does not return
zero-filled or partial-success content.

- **No peers / no seeder.** A torrent whose data is not local and whose swarm
  has no peers stays incomplete. Reads of missing ranges wait for pieces that
  never arrive until the session is closed, at which point they fail rather
  than hang. The session and mount stay healthy: the status API reports the
  incomplete state instead of the process crashing.
- **Partial pieces.** A partial piece reports `partial: true` with an
  `available_bytes` count in `GET /api/v1/torrents/{id}/status`.
- **Missing paths.** A torrent or file that does not exist maps to `ENOENT`; a
  path below a single-file torrent root maps to `ENOTDIR`; the whole mount is
  read-only, so writes into it return `EROFS`.
- **Underlying errnos are preserved.** A wrapped `syscall.Errno` such as
  `ENODATA` reaches the caller unchanged; only unclassified failures flatten to
  `EIO`. A missing peer swarm is a health warning surfaced through the status
  API and read errors, not a crash.

## Testing

The suite has three layers:

1. **Unit and offline integration** (`go test ./...`) — config, cache,
   filesystem layout (single-file roots, read-only data nodes, former control
   names as plain data), session lifecycle, per-file piece-range projection in
   the status snapshot, and managed metadata handling. These run anywhere,
   without network access.
2. **Concurrency and error paths** — concurrent reads, lookups, directory
   listings, and data-tree namespace churn; session close races; upload
   rollback; magnet intent recovery; and incomplete or missing data. Run under
   the race detector with `go test -race ./...`.
3. **Real FUSE mounts** — a smoke mount, a single-file direct read and seek, a
   read-only multi-file data tree, concurrent reads through the mount, and a
   self-hosted swarm (a loopback HTTP tracker plus a seeder and a leecher
   session) that transfers real content into a FUSE mount. These require
   `/dev/fuse`, `fusermount`/`fusermount3`, and mount permission.

Real-mount tests skip themselves, with a printed reason, where FUSE is
unavailable — they are never counted as a passing FUSE run. To make that
absence a failure instead, set `TORRENTFS_FUSE_REQUIRED=1`:

```sh
TORRENTFS_FUSE_REQUIRED=1 go test -race -run 'TestFuse|TestSessionIncomplete' ./...
```

The swarm test binds loopback only, uses an in-process tracker, and disables
DHT and UTP, so it never contacts the public network.

## Docker HTTP smoke

The image contains only the Go binary and runtime libraries; Node and `web/dist`
are build-stage inputs embedded in that binary. Run the HTTP-only check without
FUSE:

```sh
./scripts/http-smoke.sh
```

For a headless container, bind a listener explicitly and enable authentication:

```sh
docker build -t torrentfs .
docker run --rm \
  -p 8080:8080 \
  -v /srv/torrentfs-data:/data \
  -v /srv/torrents:/torrents \
  -v /srv/torrentfs.toml:/config.toml:ro \
  torrentfs -config /config.toml /torrents
```

Use `[http].listen_addr = "0.0.0.0:8080"` and a complete `[http.auth]` section
in that config. `-p` publishes a port but does not change what the daemon
listens on. The default remains loopback-only; put an exposed deployment behind
TLS at the edge. The HTTP smoke checks the public root and deep link, asset
MIME/cache behavior, API `401` plus `WWW-Authenticate`, login `no-store`,
authenticated list, logout, and the rule that unknown API paths never become
SPA HTML.

## Docker (rootful)

The repository includes an offline fixture and an end-to-end Docker/FUSE check.
From the repository root, run:

```sh
./scripts/docker-smoke.sh
```

The script builds the image, bind mounts a writable temporary `torrents`
directory at `/torrents`, preloads the matching payload under
`/data/payload/<info-hash>/` (the real storage layout), reads it through the
FUSE mount, asserts the mount exposes no `metadata/` or `stats/` control path,
asserts a pre-existing legacy `/torrents/.stats` is left untouched, and
verifies rejected single-file and missing-directory CLI inputs. It requires a
working Docker daemon, `/dev/fuse`, `SYS_ADMIN` mount permission, and (on
AppArmor hosts) permission to use `--security-opt apparmor=unconfined`. The
fixture is mounted at runtime; it is not copied into the production image.

Mount a host directory at `/torrents` and pass that directory as the sole
positional argument:

```sh
mkdir -p /srv/torrentfs-data /srv/torrents /srv/mnt
docker build -t torrentfs .
docker run --rm \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  -v /srv/torrentfs-data:/data \
  -v /srv/torrents:/torrents \
  -v /srv/mnt:/mnt \
  torrentfs -mountpoint /mnt -data-dir /data /torrents
```

`/srv/torrents` must be writable because torrentfs creates and updates
`/torrents/.metadata`; do not mount it read-only. Add and remove direct regular
lower-case `*.torrent` files in that directory while the container is running,
or manage torrents through the HTTP API. Use a temporary filename followed by
an atomic rename for file-based writers. `/srv/torrentfs-data` stores
downloaded payload data and `/srv/mnt` only needs to be a mountpoint;
`-data-dir` does not change the torrent source directory.

Mounting FUSE needs the host to grant the container the FUSE device and the
mount capability. The image installs `fuse3` and mount helpers and runs
rootful, but it cannot grant itself either of those: `--device /dev/fuse` and a
capability such as `SYS_ADMIN` (or an equivalent privileged configuration) are
required, and some hosts also need `--security-opt apparmor=unconfined`. A
container started without them fails to mount; it does not silently fall back.
Treat the container as privileged: it can mount a filesystem on behalf of
whoever runs it, so do not expose it to untrusted callers.

The CI FUSE job (`TORRENTFS_FUSE_REQUIRED=1`) needs the same capability on its
runner; a runner without it fails the job by design.

## License

Mozilla Public License 2.0 — see [LICENSE](LICENSE).
