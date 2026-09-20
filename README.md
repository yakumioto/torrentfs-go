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
> Pieces live only in memory: they are never written to disk and a restart
> starts from an empty cache. The cache is bounded and evicts least-recently-used
> pieces, so the API reports how much of a torrent is *cached* rather than how
> much was ever downloaded.

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

A successful run publishes a multi-platform OCI image for Linux/amd64 and
Linux/arm64 at `ghcr.io/yakumioto/torrentfs-go`. The workflow builds and starts
both platform images on the same GitHub Actions runner before logging in to GHCR
and pushing the final image. Existing test, lint, and required FUSE checks must
also pass; a failed check or image validation never reaches the push step.

The only published tag is the immutable
`nightly-<date>-<short-sha>-<run-id>-<run-attempt>`. Its UTC date and short SHA
come from the triggering commit, while the workflow run ID and attempt
distinguish the initial run from reruns. The image also records the full commit
in `org.opencontainers.image.revision`, along with its source, commit timestamp,
and nightly tag. No `latest`, stable, or other alias is published. Nightly runs
do not create GitHub Releases, release assets, Actions artifacts, or
`.dockerbuild` build records: the workflow sets `DOCKER_BUILD_RECORD_UPLOAD=false`
and `DOCKER_BUILD_SUMMARY=false`. This workflow does not delete registry tags or
clean up historical GitHub nightly releases.

Pull a specific nightly image by its immutable tag:

```sh
docker pull ghcr.io/yakumioto/torrentfs-go:nightly-<date>-<short-sha>-<run-id>-<run-attempt>
docker image inspect ghcr.io/yakumioto/torrentfs-go:nightly-<date>-<short-sha>-<run-id>-<run-attempt> \
  --format '{{index .Config.Labels "org.opencontainers.image.revision"}}'
```

## Usage

```sh
go run ./cmd/torrentfs -mountpoint <dir> [-config <file>] <torrents-dir>
```

`-mountpoint` is required unless the HTTP API is enabled
(`http.listen_addr` is set); without it, torrentfs runs headless and is
managed entirely over HTTP. `<torrents-dir>` is exactly one existing, readable
and writable directory. A file path such as
`/data/torrentfs/input.torrent` is rejected: single-file positional input is
not supported. Configuration is merged as environment variables > TOML file >
built-in defaults. Without `-config`, the loader uses the built-in defaults and
environment variables; a TOML file loads the sections shown in
`torrentfs.example.toml`. All persistent state is stored under
`<torrents-dir>/.metadata`; piece data is memory-only.

At startup, torrentfs restores managed metadata and scans only direct regular,
non-symlink files in `<torrents-dir>` whose names end in lower-case `.torrent`.
It does not recurse into subdirectories. The directory is reconciled about
once per 100 ms while the process runs: adding a stable valid `.torrent` loads
it without restart, and removing a source releases its torrent when no other
directory source or managed metadata source refers to the same info hash.
Duplicate files for one hash share one torrent. Configuration is read at every
startup; changing the file takes effect after a restart, not through SIGHUP.
Without `-config`, the built-in defaults are
combined with the supported environment variables; an explicit TOML file
replaces only the values it contains.

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
| `DELETE /api/v1/torrents/{id}` | Delete a task; its cached pieces are dropped with it |
| `GET /api/v1/operations/{id}` | Poll a deletion operation |

`GET /api/v1/torrents/{id}/status` returns one fresh, consistent snapshot:

```json
{
  "torrent": {"id": "40-lowercase-hex", "info_hash": "40-lowercase-hex", "state": "ready", "total_bytes": 1234, "cached_bytes": 262144},
  "metainfo_ready": true,
  "piece_length": 262144,
  "pieces": [{"index": 0, "cached": true, "cached_bytes": 262144, "pinned": false}],
  "files": [{"path": "sub/file.bin", "size": 1234, "piece_start": 0, "piece_end": 1}]
}
```

`pieces` is the whole torrent in absolute, zero-based, ascending piece order.
Each file reports a half-open `[piece_start, piece_end)` range into that same
array, so a piece spanning a file boundary is referenced by both files instead
of being duplicated. Each piece reports whether it is resident in the cache
right now (`cached`), how many of its bytes are resident (`cached_bytes`), and
whether it is currently protected from eviction by an active read (`pinned`).
A task whose metainfo has not arrived yet (an unresolved magnet) returns `200`
with `metainfo_ready: false` and empty arrays; an unknown id returns `404`.

### External state model

`state` is a lifecycle stage, never a completion percentage:

| Value | Meaning |
| --- | --- |
| `adding` | Metainfo not available yet (a magnet still resolving) |
| `ready` | Metainfo available; the torrent is readable and seeds whatever the cache currently holds |
| `error` | Metainfo could not be obtained |
| `deleting` | A deletion is in progress |
| `delete_failed` | A deletion failed and can be retried |
| `deleted` | Terminal state of a deletion operation (never written to disk) |

`cached_bytes` on the torrent and on each piece is the only cache metric:
it is how many bytes of the torrent are resident in memory *right now*. It
falls when pieces are evicted, so a cache-usage bar can move backwards. That is
intended — it describes the cache, not how much was ever downloaded.

The API deliberately does not expose download or seeding progress. anacrolix's
completion view lags behind a memory-only cache (an evicted piece still counts
as complete until it is read again), so reporting it would show a number that is
wrong by design. Every `ready` task seeds from its current cache contents, which
is exactly the acceptance rule that seeding never changes what may be evicted.
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
production build is same-origin and uses relative `/api/v1` requests.

When authentication is enabled, the UI keeps the opaque Bearer token in
tab-scoped `sessionStorage`. It does not use cookies, URL parameters,
`localStorage`, JWT decoding, or a refresh endpoint. A refresh inside the same
tab restores that token and validates it with the existing `/api/v1/torrents`
probe, so a still-valid token does not ask you to sign in again. Closing the tab
ends the browser page session and drops the stored token, and a browser that
denies `sessionStorage` access falls back to an in-memory session for that page
load. Logout attempts to revoke the token on the server and always clears
`sessionStorage`, the in-memory token, and the query cache, even when the revoke
request fails.

Server-side tokens live in daemon memory and expire on inactivity, so a `401`
stays the authoritative signal to show the login gate: when the token has
expired, was revoked, or was lost to a daemon restart, the UI drops the local
session and asks for a new sign-in. Valid API requests slide the server-side
inactivity window.

## On-disk state

```text
<torrents-dir>/
  <name>.torrent                     # user-owned source files (scanned, watched)
  .metadata/<info-hash>.torrent      # managed metainfo published by the API
  .metadata/<info-hash>.magnet       # durable intent for an unresolved magnet
  .metadata/<legacy-name>.torrent    # legacy managed source, restored but never rewritten
  .metadata/peer_id                   # durable 20-byte peer ID for this torrents directory
  .metadata/state/<info-hash>.json    # interrupted-deletion sidecars only
  .metadata/instance.lock            # single-instance lock, held while the process runs
  .stats/                            # legacy empty directory: ignored, never created, never removed
```

`.metadata` is an implementation detail of the torrents directory: it is
persisted, restored on startup, and never mounted. It contains the managed
metainfo, pending magnet intents, peer identity, deletion sidecars, and instance
lock. A legacy `.stats` directory from an older version is ignored — the current
release neither creates it nor deletes it, and it holds no piece state. **No
piece data and no piece completion is stored anywhere**: the cache exists only
in memory, a restart starts empty, and neither the cache contents, the cache hit
count, nor transient read priorities survive a restart. There is no initial
rehash and no attempt to recover a piece from disk.

A magnet URI is accepted immediately: its intent is published as
`.metadata/<info-hash>.magnet` before registration, so a restart before the
metainfo arrives retries the fetch. Once the metainfo resolves it is published
as the canonical `.torrent` and the pending `.magnet` is removed; if both ever
exist, the `.torrent` wins. `DELETE /api/v1/torrents/{id}` removes every
internal metadata source for that hash — canonical and legacy names, plus a
pending magnet — so a deleted task cannot reappear after a restart. Deleting a
torrent also drops its cached pieces. A torrent still referenced by a
user-owned top-level `.torrent` file is refused with `409`.

## Configuration

The supported TOML keys are:

```toml
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
# Turn off one peer address family. Disabling both is rejected at startup.
disable_ipv4 = false
disable_ipv6 = false
# UPnP/NAT-PMP port mapping. Off by default: a deployment that has no UPnP
# device then behaves the same as one that has a device but no mapping rule.
no_port_forwarding = true
# Replaces the built-in DHT bootstrap hosts. Each entry is a host:port that is
# resolved per address family; empty uses the built-in list.
bootstrap_nodes = []

[proxy]
socks5_url = ""

[cache]
capacity_bytes = 2147483648

[identity]
tracker_user_agent = "qBittorrent/4.4.0"
peer_id_prefix = "-qB4400-"
extended_handshake_client_version = "qBittorrent/4.4.0"

[log]
level = "info"
format = "text"
add_source = false

[mount]
# Only the mounting UID/GID can access the FUSE data by default.
allow_other = false
```

Each leaf TOML key has a matching environment variable. Only the exact names
below are read; unrelated `TORRENTFS_*` variables are ignored.

| TOML key | Environment variable | Value format |
| --- | --- | --- |
| `connections.listen_host` | `TORRENTFS_CONNECTIONS_LISTEN_HOST` | string |
| `connections.listen_port` | `TORRENTFS_CONNECTIONS_LISTEN_PORT` | decimal integer |
| `connections.disable_ipv4` | `TORRENTFS_CONNECTIONS_DISABLE_IPV4` | Go boolean |
| `connections.disable_ipv6` | `TORRENTFS_CONNECTIONS_DISABLE_IPV6` | Go boolean |
| `connections.no_port_forwarding` | `TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING` | Go boolean |
| `connections.bootstrap_nodes` | `TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES` | comma-separated `host:port` list |
| `proxy.socks5_url` | `TORRENTFS_PROXY_SOCKS5_URL` | string |
| `cache.capacity_bytes` | `TORRENTFS_CACHE_CAPACITY_BYTES` | decimal integer |
| `identity.tracker_user_agent` | `TORRENTFS_IDENTITY_TRACKER_USER_AGENT` | string |
| `identity.peer_id_prefix` | `TORRENTFS_IDENTITY_PEER_ID_PREFIX` | string |
| `identity.extended_handshake_client_version` | `TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION` | string |
| `http.listen_addr` | `TORRENTFS_HTTP_LISTEN_ADDR` | string |
| `http.max_upload_bytes` | `TORRENTFS_HTTP_MAX_UPLOAD_BYTES` | decimal integer |
| `http.auth.enabled` | `TORRENTFS_HTTP_AUTH_ENABLED` | Go boolean |
| `http.auth.username` | `TORRENTFS_HTTP_AUTH_USERNAME` | string |
| `http.auth.password_hash` | `TORRENTFS_HTTP_AUTH_PASSWORD_HASH` | bcrypt hash string |
| `http.auth.password_hash_file` | `TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE` | file path |
| `http.auth.token_ttl` | `TORRENTFS_HTTP_AUTH_TOKEN_TTL` | Go duration, such as `30m` |
| `log.level` | `TORRENTFS_LOG_LEVEL` | `debug`, `info`, `warn`, or `error` |
| `log.format` | `TORRENTFS_LOG_FORMAT` | `text` or `json` |
| `log.add_source` | `TORRENTFS_LOG_ADD_SOURCE` | Go boolean |
| `mount.allow_other` | `TORRENTFS_MOUNT_ALLOW_OTHER` | Go boolean |

Values are merged per field with this precedence: environment variable > TOML
file > built-in default. An environment variable that is present but empty
clears a string field; empty numeric, boolean, and duration values are invalid.
Environment overrides are applied before cross-field validation, so they can
replace a lower-priority value that would otherwise fail validation. Explicitly clear
`TORRENTFS_HTTP_AUTH_PASSWORD_HASH` when switching to
`TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE`; authentication still requires exactly
one password source.

`log.level` accepts `debug`, `info`, `warn`, or `error`; the default is `info`,
so debug records are disabled unless explicitly enabled. `log.format` accepts
`text` or `json`, and `add_source` includes the source file and line in each
record. Logs never include authentication tokens, password hashes, or proxy
credentials.

`cache.capacity_bytes` is the hard upper bound on the in-memory piece cache. The
default, 2147483648 (2 GiB), targets a host with about 4 GB of RAM; size it at
roughly 50-60% of a container's memory limit, because the cap applies to the
process and exceeding the container limit gets it OOM-killed. The default is a
cap, not a reservation: the cache fills lazily, so a small workload's resident
memory stays small. A value of zero or less is rejected at startup. Eviction
starts at 7/8 of this value and reclaims down to 3/4, so a read window always
has headroom without waiting for the cache to fill completely; the high- and
low-water marks are derived, not configurable, and each is clamped to at least
one byte so that a very small capacity still retains the one piece that fits
it. A torrent whose piece length exceeds the capacity cannot be added: it could
never be read, so it is refused at add time instead of looping between download
and eviction.

An empty `[http].listen_addr` disables the API. The default binds loopback
only. Binding a non-loopback address requires a complete enabled
`[http.auth]` configuration; this service does not provide TLS, so put
non-loopback deployments behind a TLS reverse proxy.

When `http.auth.enabled` is true, `username` and exactly one bcrypt
`password_hash` or owner-readable-only `password_hash_file` are required.
`password_hash` is a bcrypt hash, never a plaintext password.
`password_hash_file` must point to a regular, non-symlink file readable only by
its owner. For an exposed deployment configured by environment variables, set
all of the required authentication fields together, preferably using a secret
file:

```sh
docker run --rm -p 8080:8080 \
  --mount type=bind,src=/srv/secrets/torrentfs-password-hash,dst=/run/secrets/password-hash,readonly \
  -e TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  -e TORRENTFS_HTTP_AUTH_ENABLED=true \
  -e TORRENTFS_HTTP_AUTH_USERNAME=alice \
  -e TORRENTFS_HTTP_AUTH_PASSWORD_HASH= \
  -e TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE=/run/secrets/password-hash \
  torrentfs
```

The default `token_ttl` is 30 minutes and may not exceed 24 hours. Log in with
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

An empty `socks5_url` disables the proxy;
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
limited to 20 bytes, and the remaining bytes are generated randomly. A 20-byte
prefix leaves no random suffix. The complete 20-byte peer ID is written once to
`<torrents-dir>/.metadata/peer_id` on first start and reused on every later
start, so a private tracker sees one stable peer instead of a new one after each
restart. Each torrents directory has its own identity. Deleting that file — or
changing `peer_id_prefix` so the stored ID no longer matches, which logs a
warning — generates and stores a new identity. A `peer_id` file whose length is
not exactly 20 bytes fails startup instead of being silently regenerated. The
peer ID is used for BitTorrent handshakes and announces. Explicit TOML values override the defaults; explicitly setting an
identity value to an empty string delegates that field to the anacrolix
default. `v` is sent only when the peer supports the extended handshake.

## Peer ports, NAT, and single-instance operation

Inbound peer connections need a reachable port. With `listen_port = 0` the
client picks a free port at startup, and that port is what the tracker announce
carries. The `session ready` record reports it as `effective_listen_port`,
alongside the configured `listen_port` (which keeps its original meaning, the
raw configuration value) and the `listen_addrs` actually bound. Note that with a
dynamic port each listener may end up on a different port; `effective_listen_port`
is the port the client reports to peers. To accept inbound peers, set a fixed
`listen_port` and publish the same port for TCP and UDP — with Docker,
`-p 6881:6881/tcp -p 6881:6881/udp`. A dynamic port cannot be published in
advance, and a bridge-network container is not reachable from the swarm without
that mapping.

`no_port_forwarding` defaults to `true`, so torrentfs never asks UPnP or
NAT-PMP for a mapping: a host with no UPnP device behaves exactly like one that
has a device but no mapping rule. Outbound connections work either way; only
inbound reachability depends on the mapping.

`disable_ipv4` and `disable_ipv6` turn off an address family for listeners,
dialers, and DHT server sockets together. Only one family may be disabled;
disabling both leaves no transport and is rejected at startup. Disabling the
family whose DHT bootstrap hosts carry no usable address is the usual way to
stop the repeated bootstrap attempts described below.

`bootstrap_nodes` replaces the built-in DHT bootstrap hosts with explicit
`host:port` entries, resolved per address family on each attempt. Use it when
the default resolver's answers carry no address of a family a socket needs, or
when only a specific bootstrap host is reachable.

Diagnostics: `GET /api/v1/torrents/{id}/status` reports a `network` object with
`effective_listen_port`, the torrent's `total_peers`, `pending_peers`,
`active_peers`, `connected_seeders`, and `piece_complete`, plus one DHT entry
per address family (`nodes`, `good_nodes`, `resolved`, `kept`, `ready`). Every
field is additive: `metainfo_ready` still means only that the metainfo is
loaded, never that peers exist. An empty tracker peer list is a successful
announce and is honoured for the tracker's interval, so no immediate retry is
issued. A DHT family that cannot be bootstrapped is logged once per state
change as `dht starting nodes unavailable` with a status name such as
`dht_udp6_unavailable`, instead of once per routing-table refresh. The upstream
record that repeats every refresh (`error bootstrapping during bucket refresh`)
is demoted to `debug`, so it is not emitted at the default level; set
`level = "debug"` to see it.

`listen_host` is applied to every listener and dialer, not per address family.
A literal IPv4 address such as `127.0.0.1` therefore fails the IPv6 listener
(`listen tcp6: no suitable address found`) on a dual-stack build, which aborts
startup. Leave `listen_host` empty to bind both families, or disable the family
you are not binding with `disable_ipv4` / `disable_ipv6`.

One torrents directory is managed by one process at a time: the session holds
an exclusive lock on `<torrents-dir>/.metadata/instance.lock`, and a second
instance pointed at the same directory fails to start with an actionable error.
The lock is released when the process exits; it does not cover two machines
sharing one tracker account with different torrents directories.

## Private torrents (BEP 27)

A torrent whose metainfo carries `private=1` is isolated from peer discovery, as
[BEP 27](https://www.bittorrent.org/beps/bep_0027.html) requires. For such a
torrent torrentfs does not announce to or query the DHT, does not exchange peers
over PEX (`ut_pex`), and does not use Local Peer Discovery. Only the trackers
declared inside the torrent are used. Public torrents are unaffected: their DHT
bootstrap, announcements, and PEX behave exactly as before.

This is a property of the torrent, not a setting. There is no per-torrent or
global switch to turn it on, and nothing to configure.

How a torrent is added decides whether the isolation covers the whole session:

- **`.torrent` file, embedded bytes, or an already known info dict** — the
  metainfo is present at add time, so the torrent is isolated from the first
  byte. This is the recommended way to add a private torrent.
- **magnet link** — a magnet carries no info dict, so the private flag is unknown
  until metadata is fetched. Until then the torrent is treated as public and may
  announce to the DHT; the moment the metadata lands, the DHT announcer stops on
  its next pass. The exposure window is exactly the metadata fetch. A private
  magnet still resolves its metadata through its tracker, so downloads work — but
  if your tracker forbids any DHT contact, add the torrent by file instead of by
  magnet.

Two further notes:

- A `private=1` torrent with no trackers cannot find peers at all. That is the
  correct BEP 27 outcome, not a fault.
- `private=1` is honoured whether it is `true` only; a missing flag, `false`, or a
  v2-only metainfo all mean "public", matching the upstream definition.

Support comes from a pinned fork of `github.com/anacrolix/torrent`: release
v1.61.0 has the `private` field but never reads it at runtime, so `go.mod`
replaces the module with `github.com/yakumioto/torrent v1.61.0-bep27.2`. That
release is v1.61.0 plus the three upstream hunks from commit `76452a2c8a2f`,
and one hunk of our own: `internal/mytimer.Timer.When()` takes a read lock
around its `when` field, which upstream still reads unsynchronized. Without it
`Client.WriteStatus` races the announce-timer goroutine and cannot be used as a
status observation point. The replace directive is temporary: drop it once
anacrolix/torrent publishes a release containing both.

## Known upstream issues

`github.com/anacrolix/torrent` v1.61.0 — and therefore the pinned
`v1.61.0-bep27.1` fork below — can panic inside `PeerConn.servePeerRequest`:

```
peerconn.go:766: panic: assertion failed: MapContains(c.unreadPeerRequests, r)
```

`peerRequestDataReadFailed` returns early when the torrent is already closed,
before removing the request from `unreadPeerRequests` and before `useBestReject`
runs, so the deferred invariant check in `servePeerRequest` fires. The window is
a peer request whose data read fails while its torrent is being closed or
dropped. This is a code-level root cause read from the upstream source, not a
reproduced failure.

torrentfs does not work around this. The `replace` directive above pins the fork
to an exact release plus three named hunks, so adding a hunk for an unrelated
upstream defect would change what that pin means. The fix belongs in
`anacrolix/torrent`.

## Error behavior

Reads never fabricate data. When the pieces behind a range are unavailable, a
read either blocks until they arrive or returns an error; it does not return
zero-filled or partial-success content.

Overlapping reads are serialized over one shared torrent reader: a new read
waits for the read in flight instead of cancelling it, so ordinary player
concurrency (readahead, seeks, probes) cannot turn into a cancellation storm. A
read is cancelled only by its own request going away, by the session closing,
or by the torrent being removed.

- **No peers / no seeder.** A torrent whose data is not local and whose swarm
  has no peers stays incomplete. Reads of missing ranges wait for pieces that
  never arrive until the session is closed, at which point they fail rather
  than hang. The session and mount stay healthy: the status API reports the
  incomplete state instead of the process crashing.
- **Partially cached pieces.** A piece that is only partly resident reports
  `cached: false` with the resident byte count in `cached_bytes` in
  `GET /api/v1/torrents/{id}/status`. The read path still fetches the piece as
  a whole.
- **Missing paths.** A torrent or file that does not exist maps to `ENOENT`; a
  path below a single-file torrent root maps to `ENOTDIR`; the whole mount is
  read-only, so writes into it return `EROFS`.
- **Underlying errnos are preserved.** A wrapped `syscall.Errno` such as
  `ENODATA` reaches the caller unchanged; only unclassified failures flatten to
  `EIO`. A missing peer swarm is a health warning surfaced through the status
  API and read errors, not a crash.

## Breaking changes

Upgrading from a release that persisted pieces on disk:

| Change | Detail |
| --- | --- |
| `paths.data_dir` / `-data-dir` / `TORRENTFS_PATHS_DATA_DIR` removed | The `[paths]` TOML section is rejected as an unknown field; the removed environment variable is ignored. State now lives under each `<torrents-dir>/.metadata`. |
| Peer identity moved | `<data-dir>/peer_id` is not migrated. Copy it manually to `<torrents-dir>/.metadata/peer_id` if preserving a private-tracker identity matters; otherwise a new 20-byte identity is generated. |
| Deletion sidecars moved | `<data-dir>/state/` is not migrated. Copy unfinished sidecars manually to `<torrents-dir>/.metadata/state/` if recovery is required. |
| Peer IDs are per torrents directory | The old shared data directory could make multiple torrents directories share one identity. Each torrents directory now has its own `.metadata/peer_id`. |
| Legacy payload warning removed | `<data-dir>/payload/` is no longer inspected; pieces remain memory-only and old files are left untouched. |
| `cache.capacity_bytes` default 64 MiB → 2 GiB | The default now targets a ~4 GB host. Set it explicitly for smaller containers. |
| `cache.capacity_bytes = 0` is now invalid | `0` meant "cache nothing", which would make every read fail. Startup rejects it. |
| `purge_data` removed | `DELETE /api/v1/torrents/{id}?purge_data=...` loses the parameter and the operation response loses the `purge_data` field. Deleting a task now always drops its cached pieces. An old client that still sends the parameter gets a normal `202`: Go's HTTP server ignores unknown query parameters. |
| `completed_bytes` and `progress` removed | There is no download-progress concept. Read `cached_bytes` instead. |
| Per-piece `known`/`complete`/`partial`/`checking`/`wanted`/`available_bytes` removed | Replaced by `cached` / `cached_bytes` / `pinned`, which describe cache residency. |
| `state` values `downloading` and `seeding` removed; `ready` added | `state` is now a lifecycle stage. A `ready` task serves whatever the cache holds. |
| `seeding` no longer exists | Every `ready` task seeds from its current cache contents; seeding never keeps a piece from being evicted. |

No compatibility layer is provided: the memory-only cache and the external
state model ship together, and field aliases would contradict the removal of
the old persistence path.

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

When adding or changing an **observation point** — a status or state API the
tests read to assert behavior — first confirm that API is concurrency-safe for
the way the test calls it. Then run the package under the race detector as a
whole (`go test -race ./...`), not just the new test: a race in the observed
API usually needs the rest of the package running to fire, so a single
`-run` narrows the run until it is clean and hides the defect. No data race
involving the observation point may remain in that output.

## Docker HTTP smoke

The image contains the Go binary, runtime libraries, and the built-in
`/etc/torrentfs/torrentfs.toml`; Node and `web/dist` are build-stage inputs
embedded in that binary. The image also creates `/torrents` so its command can
start without any external configuration. Run the HTTP-only check without FUSE:

```sh
./scripts/http-smoke.sh
```

The configuration smoke checks the built-in file, the image default command,
environment-only overrides, and an external TOML file:

```sh
./scripts/docker-config-smoke.sh
```

With no arguments, the image starts a headless HTTP service on loopback using
its built-in configuration and the container-local `/torrents` directory. The
directory is temporary container storage unless it is bind mounted. To expose
the API, set a non-loopback listener and complete authentication configuration
together; environment variables override the built-in file without an
additional `-config` argument:

```sh
docker build -t torrentfs .
docker run --rm \
  -p 8080:8080 \
  -v /srv/torrents:/torrents \
  -e TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  -e TORRENTFS_HTTP_AUTH_ENABLED=true \
  -e TORRENTFS_HTTP_AUTH_USERNAME=alice \
  -e TORRENTFS_HTTP_AUTH_PASSWORD_HASH='$2a$10$N9qo8uLOickgx2ZMRZoMye8fOsiTWZqYtkxvXkKm8BMzjT7t/vIdq' \
  torrentfs
```

The image default listener remains loopback-only, and `-p` does not change what
the daemon listens on. Put an exposed deployment behind TLS at the edge.

The image sets a fixed peer port (`listen_port = 6881`). Publish it for TCP and
UDP to accept inbound peers; without the mapping the container still downloads
from outbound connections but cannot be dialed:

```sh
docker run --rm \
  -p 8080:8080 \
  -p 6881:6881/tcp \
  -p 6881:6881/udp \
  -v /srv/torrents:/torrents \
  torrentfs
```

An external TOML file remains supported when a deployment wants file-based
configuration:

```sh
docker run --rm \
  -p 8080:8080 \
  -v /srv/torrents:/torrents \
  -v /srv/torrentfs.toml:/config.toml:ro \
  torrentfs -config /config.toml /torrents
```

Use `listen_addr = "0.0.0.0:8080"` and a complete authentication section in
that file. The HTTP smoke checks the public root and deep link, asset MIME/cache
behavior, API `401` plus `WWW-Authenticate`, login `no-store`, authenticated
list, logout, and the rule that unknown API paths never become SPA HTML.

## Docker (rootful)

The repository includes an offline fixture and an end-to-end Docker/FUSE check.
From the repository root, run:

```sh
./scripts/docker-smoke.sh
```

The script builds the image, bind mounts a writable temporary `torrents`
directory at `/torrents`, and mounts `/mnt` with recursive shared propagation.
Pieces are never read from disk now, so instead of preloading a payload the
script serves the fixture over plain HTTP and hands the data scenarios a
web-seeded (BEP 19) copy of the fixture torrent; anacrolix fetches the piece
over HTTP and writes it into the in-memory cache through the same storage path
a BitTorrent peer would. It reads the payload through FUSE and checks that
both the container mount and the host source directory expose the same file and
hash, that the mount exposes no `metadata/` or `stats/` control path, that host
writes are rejected, and that a pre-existing legacy `/torrents/.stats` is left
untouched. It also verifies rejected single-file and missing-directory CLI
inputs and confirms a normal stop removes the propagated host mount. A second,
offline scenario starts an incomplete torrent with no peer, leaves two readers
outstanding on the missing piece, and checks that SIGTERM still stops the
process with exit code 0, without a daemon-owned unmount failure and without
the anacrolix reader cancellation errors during the running phase. On that
fixture the file is a single page, so the kernel collapses the two same-page
reads into one in-flight request; the scenario therefore covers shutdown and
unmount behaviour, and the cancellation regression itself is covered by the
FUSE/swarm test in the Go suite. A third scenario reproduces the case where a
second mount namespace holds a propagated copy of the FUSE mount: the peer is
left running while the daemon gets SIGTERM, and the script checks that the
daemon stops within its unmount deadline, exits 1, prints the timeout
diagnostic, reports no daemon-owned unmount failure, and releases the
propagated host mount once the peer is gone. Every Docker
call in the script, including the teardown waits, is bounded; a timeout
collects diagnostics and fails instead of hanging. It
requires a working Docker daemon, `/dev/fuse`, `findmnt`, `python3` (for the
web seed file server), `SYS_ADMIN` mount permission, and (on AppArmor hosts)
permission to use `--security-opt apparmor=unconfined`. The fixture is mounted at runtime; it is
not copied into the production image.

Mount a host directory at `/torrents` and pass that directory as the sole
positional argument:

```sh
mkdir -p /srv/torrents /srv/mnt
docker build -t torrentfs .
docker run --rm \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  -v /srv/torrents:/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  torrentfs -mountpoint /mnt /torrents
```

Docker's default bind propagation is `rprivate`, which keeps a FUSE
submount created inside the container from appearing in the host source
directory. `rslave` only propagates host mounts into the container and is not
sufficient here; `/mnt` must use recursive bidirectional `rshared` propagation.
On Linux, the host mount containing `/srv/mnt` must already be shared. Check
that prerequisite with:

```sh
findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
```

Docker Desktop and other environments without Linux bind-mount propagation do
not support this setup. Do not add `readonly` or `:ro` to the `/mnt` bind: the
FUSE daemon needs the writable bind target to create its submount, while the
FUSE filesystem itself remains read-only and rejects writes with `EROFS`.
`rshared` is recursive and bidirectional, so container mount and unmount changes
under this source subtree can affect the host, and host changes can enter the
container. Together with a rootful container and `SYS_ADMIN`, this expands the
mount authority boundary; use it only for trusted containers.

The FUSE mount keeps the default owner-only access (`mount.allow_other = false`), so
other host users, including root, may receive `EACCES` rather than see the mounted
data. The setting is an explicit opt-in: `mount.allow_other = true` (or
`TORRENTFS_MOUNT_ALLOW_OTHER=true`) lets every local UID read the mounted data,
including root, without changing the filesystem's read-only behavior. A non-root
mount also requires `user_allow_other` in `/etc/fuse.conf`; the image leaves that
line disabled by default.

### Shutdown, outstanding reads, and `Device or resource busy`

On SIGINT/SIGTERM the process stops the HTTP API, closes the session, and only
then unmounts the FUSE filesystem. That order is required: closing the session
cancels and drains reads that are still waiting for pieces, so the unmount no
longer has to wait on them. Unmounting first makes `fusermount3` fail with
`failed to unmount /mnt: Device or resource busy` whenever a request is still
outstanding, and the mount can then only be released lazily.

A `Device or resource busy` on unmount has three distinct causes, and they need
different answers:

- **daemon-owned outstanding request.** A read issued through the mount is
  still waiting on a missing piece. With no host process holding the mount,
  this reproduces from the container alone. It is what the session-close-first
  order fixes; after the fix the same scenario unmounts cleanly.
- **a host holder.** A player, media scanner, open file descriptor, or a
  process whose working directory is inside the propagated mount keeps the
  mount busy. Confirm it with the owning PID and command:

  ```sh
  findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
  fuser -vm /srv/mnt     # when fuser is installed and permitted
  lsof /srv/mnt          # when lsof is installed and permitted
  ```

  Release the holder (stop the player/scan) and retry the unmount. When
  `fuser`/`lsof` are missing or lack permission, the holder diagnostics are
  simply incomplete — that is not evidence that no holder exists.
- **another mount namespace holding a propagated copy.** Where `/mnt` is
  propagated (`rshared`) into another namespace — another container on the same
  host, for example — that namespace keeps a copy of the FUSE mount. The copy
  keeps the FUSE connection alive, so it never reaches `ENODEV` and
  `Server.Unmount` is left parked in its event-loop `Wait`. Distinguish this
  cause from the other two by `findmnt -T <mount> -o PROPAGATION,OPTIONS` plus
  the absence of both an outstanding read and a local fd/cwd holder. It reports
  **no** `Device or resource busy` line: unmounting the daemon's own copy
  succeeds, and only the connection outlives it.

  That wait is bounded. The unmount stage gets `defaultUnmountTimeout` (30
  seconds, `cmd/torrentfs/main.go`) and then gives up instead of hanging until
  the peer goes away. On expiry torrentfs writes
  `torrentfs: unmount <mountpoint>: unmount did not return within 30s: another
  mount namespace may still hold a propagated copy of the FUSE mount; ...` to
  stderr and **exits 1**. The non-zero code is deliberate: exiting 0 would
  claim the mount was released when a peer still holds a copy, while a failure
  tells a supervisor or orchestrator to look. The abandoned unmount cannot be
  cancelled — only process exit ends it — so a propagating peer that outlives
  the daemon keeps its copy as an `ENOTCONN` residual mount until its own
  namespace ends; reclaiming it is the responsibility of whoever started that
  container. Prefer stopping such containers before, or concurrently with, the
  daemon: an unmount that completes on its own still exits 0.

  Measured on the smoke fixture: with a peer container started with `sleep 300`
  still running, the daemon stopped after the full 30 s deadline with exit code
  1 and the diagnostic above, without a `Device or resource busy` line, and
  reclaiming the peer released the propagated host mount. This blocking
  predates the session-close-first order and is not caused by it — it was
  reproduced on the commit before that change.

`rshared` propagation on the `/mnt` bind is required for a FUSE submount to be
visible in the host source directory; it is not the cause of a busy unmount and
should not be relaxed to `rprivate`/`rslave`. Likewise, `fusermount3 -uz` is a
lazy detach for cleaning up a mount that already failed to release; it must
never be used as, or mistaken for, a successful graceful unmount.

`/srv/torrents` must be writable because torrentfs creates and updates
`/torrents/.metadata`; do not mount it read-only. Add and remove direct regular
lower-case `*.torrent` files in that directory while the container is running,
or manage torrents through the HTTP API. Use a temporary filename followed by
an atomic rename for file-based writers. Peer identity and deletion sidecars are
also stored under `/srv/torrents/.metadata`, and `/srv/mnt` must be an empty
mountpoint on that shared host mount.

Mounting FUSE needs the host to grant the container the FUSE device and the
mount capability. The image installs `fuse3` and mount helpers and does not
force a process identity: the default entrypoint runs as root, while the
`--user` procedure above runs as the matching host user. The image cannot grant
itself the host permissions: `--device /dev/fuse` and a capability such as
`SYS_ADMIN` (or an equivalent privileged configuration) are required, and some
hosts also need `--security-opt apparmor=unconfined`. A container started
without them fails to mount; it does not silently fall back. Treat the
container as privileged: it can mount a filesystem on behalf of whoever runs
it, so do not expose it to untrusted callers.

The CI FUSE job (`TORRENTFS_FUSE_REQUIRED=1`) needs the same capability on its
runner; a runner without it fails the job by design.

### Running the FUSE mount as a host user

The image intentionally has no `USER` instruction, so its default remains
root-compatible. To run the daemon as a host user, build the image with a
matching passwd/group entry and pass the same numeric IDs at runtime:

```sh
HOST_UID="$(id -u)"
HOST_GID="$(id -g)"
docker build \
  --build-arg TORRENTFS_UID="$HOST_UID" \
  --build-arg TORRENTFS_GID="$HOST_GID" \
  -t torrentfs .

mkdir -p /srv/torrents /srv/mnt
# Run this as an administrator if the directories are not already yours.
chown "$HOST_UID:$HOST_GID" /srv/torrents /srv/mnt

docker run --detach --name torrentfs \
  --user "$HOST_UID:$HOST_GID" \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  --env TORRENTFS_HTTP_LISTEN_ADDR= \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  torrentfs -mountpoint /mnt /torrents
```

The build arguments matter: `fusermount3` must resolve the runtime UID inside
`/etc/passwd`, and a custom UID/GID requires rebuilding with matching arguments.
Both bind sources must be writable by that user: `/torrents` stores `.metadata`,
and `/mnt` must be writable so FUSE can create its submount. Keep the host mount
shared as shown above. The host must provide `/dev/fuse`, `SYS_ADMIN`, and, on
AppArmor hosts, permission for `apparmor=unconfined`; if `/dev/fuse` is
`0660 root:fuse`, also add the host fuse group with `--group-add`.

Verify the runtime identity and propagated ownership from the host:

```sh
docker exec torrentfs id
findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
ls -ln /srv/mnt
stat -c '%u:%g %a %n' /srv/mnt/<file>  # HOST_UID:HOST_GID 444 /srv/mnt/<file>
cat /srv/mnt/<file>                    # direct read as HOST_UID, no sudo
docker stop torrentfs
findmnt -T /srv/mnt                    # no FUSE mount should remain
```

## License

Mozilla Public License 2.0 — see [LICENSE](LICENSE).
