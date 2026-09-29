# torrentfs

[简体中文](README.md)

`torrentfs` is a read-only BitTorrent filesystem written in Go. It fetches pieces on demand from peers, exposes torrent files as a FUSE filesystem, and provides an HTTP API with an embedded Web UI for task management.

It manages an existing, writable `torrents` directory. Torrents are added through the HTTP API or Web UI as magnet links or uploaded `.torrent` files; arbitrary `.torrent` files copied into the directory are not auto-imported. Single-file torrents appear directly as files, while multi-file torrents keep their directory tree. Pieces needed for reads are held in a bounded in-memory cache and are not written back as payload data.

Authenticated API/UI requests can upload managed subtitles. Subtitle sidecars are stored under `torrents-dir/.metadata/subtitles` and projected next to the corresponding video through the read-only filesystem. FUSE, SMB, and `docker cp` are not write paths.

Torrent tasks can also be assigned to categories. A category is a stable identifier and a single directory level in the FUSE tree; this version does not support renaming, deleting, nesting, or assigning multiple categories to one task.

**Good fit:** an existing player, indexer, or command-line tool that should read BitTorrent content like local files; a Docker deployment that needs a read-only SMB share; or an operator who wants Web UI/API control over torrents, categories, favorites, subtitles, and upload limits.

**Not a fit:** a downloader that must persist a complete payload to disk, a generic HTTP torrent download/streaming server, or an application that needs to write through the mounted tree. `ready` means that metainfo is available; it does not mean that every piece has already been downloaded.

## Contents

- [Positioning and storage model](#positioning-and-storage-model)
- [Quick start](#quick-start)
- [CLI and lifecycle](#cli-and-lifecycle)
- [FUSE layout](#fuse-layout)
- [HTTP API](#http-api)
- [Web UI](#web-ui)
- [Configuration](#configuration)
- [Docker](#docker)
- [Persistent state and limitations](#persistent-state-and-limitations)
- [Build, test, and contribute](#build-test-and-contribute)
- [Troubleshooting](#troubleshooting)
- [License](#license)

## Positioning and storage model

- **The mount contains data only.** The FUSE tree is read-only. Add, delete, subtitle upload, and status operations go through the HTTP API.
- **Subtitles are an overlay.** Managed subtitles are stored separately from immutable torrent payload files. The API is the only supported write path; FUSE and SMB only project the result.
- **Management state and cache are separate.** Canonical metainfo is stored as `torrents-dir/<infohash>.torrent`. Magnet intents, the registry, peer identity, categories, upload-rate settings, and managed subtitles live under `torrents-dir/.metadata`. Piece bytes exist only in memory.
- **Cache is not download progress.** `cached_bytes` is the amount currently resident in memory. Pieces can be evicted, and the cache starts empty after a restart.
- **Transfer counters are runtime counters.** `downloaded_bytes` and `uploaded_bytes` count useful torrent payload data and are not persisted across backend restarts.
- **HTTP has an explicit boundary.** The default HTTP listener is loopback-only. A non-loopback listener requires authentication. The service does not terminate TLS; put it behind a trusted TLS reverse proxy when needed.

A typical managed directory looks like this:

```text
<torrents-dir>/
├── <infohash>.torrent
└── .metadata/
    ├── categories.json
    ├── pending/<infohash>.magnet
    ├── state/<infohash>.json
    ├── subtitles/<infohash>/<torrent-relative path>
    ├── upload_rate.json
    ├── peer_id
    └── instance.lock
```

At startup, torrentfs restores tasks from `.metadata/state` and canonical metainfo files. It does not scan the root directory to guess tasks. One `torrents` directory can be managed by only one torrentfs process at a time.

## Quick start

### Requirements

- Go 1.27 or newer for a local build.
- The Node.js version in `web/.nvmrc` (currently 22.23.2) and npm for the Web UI build.
- Linux FUSE3, `/dev/fuse`, and mount permissions only when using FUSE. HTTP/API-only mode does not need a FUSE device.
- `curl` for API checks. Docker smoke scripts also require Docker and Python 3.

All commands below run from the repository root. The Go binary embeds `web/dist`, so a clean checkout must build the Web UI before running Go commands.

### Build locally

```sh
git clone https://github.com/yakumioto/torrentfs-go.git
cd torrentfs-go
./scripts/build-web.sh
go build -o ./torrentfs ./cmd/torrentfs
mkdir -p "$PWD/torrents" "$PWD/mnt"
```

`./scripts/build-web.sh` installs frontend dependencies from the lockfile, builds `web/dist`, and removes `web/node_modules`; it does not remove `web/dist`.

### Run modes

Without `-config`, the built-in defaults are HTTP `127.0.0.1:8080`, authentication disabled, and an automatically selected peer port. The `torrents` directory must already exist, be readable and writable, and not be a symlink.

Headless HTTP/API and Web UI mode:

```sh
./torrentfs "$PWD/torrents"
```

Open `http://127.0.0.1:8080/` or check the service:

```sh
curl --fail http://127.0.0.1:8080/
curl --fail http://127.0.0.1:8080/api/v1/torrents
```

HTTP/API plus FUSE:

```sh
./torrentfs -mountpoint "$PWD/mnt" "$PWD/torrents"
```

FUSE-only mode requires a mountpoint and disables HTTP. It can display existing managed data, but a new task must be added by an instance with HTTP enabled:

```sh
TORRENTFS_HTTP_LISTEN_ADDR= \
  ./torrentfs -mountpoint "$PWD/mnt" "$PWD/torrents"
```

To use the checked-in example configuration:

```sh
./torrentfs -config ./torrentfs.example.toml "$PWD/torrents"
```

Add a local `.torrent` through `POST /api/v1/torrents`; do not copy it into the managed directory. The service validates its info hash and stores it under the canonical `<infohash>.torrent` name. A magnet uses the same API and may remain in `adding` until metainfo arrives.

## CLI and lifecycle

The command accepts two flags and one positional argument:

```text
torrentfs [-config <file>] [-mountpoint <dir>] <torrents-dir>
```

| Argument | Meaning |
| --- | --- |
| `-mountpoint <dir>` | FUSE mount directory; optional when HTTP is enabled |
| `-config <file>` | Optional TOML configuration file |
| `<torrents-dir>` | Existing, writable, non-symlink management directory |
| `-h` / `--help` | Print help and exit |

When the HTTP listener is non-empty, HTTP/API and Web UI start even if a mountpoint is also supplied. A normal `SIGINT`/`SIGTERM` shutdown stops HTTP, then the session, then unmounts FUSE. Reads waiting for pieces are cancelled before unmounting.

## FUSE layout

```text
<mount>/
├── <single-name>       # unclassified single-file torrent
├── <multi-name>/       # unclassified multi-file torrent
│   └── <relative-file>
└── <category>/         # category directory, including an empty category
    ├── <single-name>
    └── <multi-name>/
        └── <relative-file>
```

- Every node is read-only. Create, write, delete, and rename operations fail.
- `.metadata` is never mounted, and there are no `metadata/` or `stats/` control directories.
- A single-file torrent is not wrapped in an extra directory, so an unclassified file can be opened as `<mount>/movie.mp4`.
- Category names are one path component and affect only the virtual path; they do not move payload or cache data.
- Display-name collisions are disambiguated with an info-hash prefix within the affected parent directory.

Managed subtitles are projected into the same tree. For a single-file video, a subtitle is a root sibling; for a multi-file torrent it is placed beside the video. Subtitle files are also read-only. Upload or replacement must use the authenticated API/UI.

## HTTP API

The HTTP listener serves both the embedded Web UI and `/api/v1`. The current management routes are:

| Method | Path | Purpose | Success |
| --- | --- | --- | --- |
| `POST` | `/api/v1/auth/login` | Exchange configured credentials for a Bearer token | `200` |
| `POST` | `/api/v1/auth/logout` | Revoke the current token | `204` |
| `POST` | `/api/v1/torrents` | Add a magnet JSON body or multipart `.torrent` upload | `201` |
| `GET` | `/api/v1/torrents` | List tasks | `200` |
| `GET` | `/api/v1/categories` | List categories | `200` |
| `POST` | `/api/v1/categories` | Create `{"name":"movies"}` | `201` |
| `PUT` | `/api/v1/torrents/{id}/category` | Set or clear a category | `200` |
| `GET` | `/api/v1/stats` | Read session cache and transfer statistics | `200` |
| `GET` | `/api/v1/settings/upload-rate` | Read upload-rate settings | `200` |
| `PUT` | `/api/v1/settings/upload-rate` | Update upload-rate settings | `200` |
| `GET` | `/api/v1/torrents/{id}` | Read a task summary | `200` |
| `GET` | `/api/v1/torrents/{id}/status` | Read pieces, files, and network diagnostics | `200` |
| `DELETE` | `/api/v1/torrents/{id}` | Start asynchronous deletion | `202` |
| `PUT` | `/api/v1/torrents/{id}/subtitles` | Upload or replace a managed subtitle | `201` / `200` |
| `PUT` | `/api/v1/torrents/{id}/favorite` | Set or clear the favorite flag | `200` |
| `POST` | `/api/v1/torrents/prune` | Delete old non-favorite tasks | `200` |
| `GET` | `/api/v1/operations/{id}` | Poll an asynchronous operation | `200` |

`{id}` is a 40-character lowercase hexadecimal info hash. Business errors use `{"error":"..."}` JSON, but unknown paths and methods can be handled by the standard `net/http` router.

### Authentication

With the local default listener and authentication disabled, a request can be made directly:

```sh
BASE_URL=http://127.0.0.1:8080
curl --fail "$BASE_URL/api/v1/torrents"
```

When authentication is enabled, only `POST /api/v1/auth/login` is unauthenticated. Other API requests require exactly one `Authorization: Bearer <token>` header. Static Web UI assets remain public so the browser can load the shell; this does not expose torrent data.

A TOML configuration uses one and only one bcrypt source:

```toml
[http]
listen_addr = "0.0.0.0:8080"

[http.auth]
enabled = true
username = "alice"
password_hash = "<bcrypt-hash>"
password_hash_file = ""
token_ttl = "30m"
```

`password_hash_file` must be a regular, non-symlink file readable only by its owner. A plaintext password is never valid TOML configuration. In the Docker image, the paired `TORRENTFS_USERNAME` and `TORRENTFS_PASSWORD` variables are a supported alternative: when HTTP authentication is enabled and both are present, the process generates a bcrypt hash at startup. The pair must be present together, non-empty, and the password must be at most 72 UTF-8 bytes. SMB uses the same pair but additionally requires the username to resolve to the container's runtime Unix account.

Tokens are opaque, stored only in daemon memory, renewed by valid requests, and invalidated by restart. The service does not use cookies, URL tokens, JWTs, or refresh tokens; `token_ttl` is a positive Go duration no longer than 24 hours.

### Basic curl flow

A login request must use JSON. `jq` is convenient for safely encoding credentials and extracting response fields:

```sh
BASE_URL=http://127.0.0.1:8080
LOGIN_BODY="$(jq -n \
  --arg username "$TORRENTFS_USERNAME" \
  --arg password "$TORRENTFS_PASSWORD" \
  '{username: $username, password: $password}')"
TOKEN="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/auth/login" \
  --header 'Content-Type: application/json' \
  --data "$LOGIN_BODY" | jq -r .token)"

MAGNET_URI='magnet:?xt=urn:btih:<info-hash>'
TORRENT_ID="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data "$(jq -n --arg magnet_uri "$MAGNET_URI" '{magnet_uri: $magnet_uri}')" \
  | jq -r .id)"

# Or upload a local .torrent. curl creates the multipart boundary.
TORRENT_ID="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN" \
  --form "file=@$PWD/example.torrent" \
  | jq -r .id)"

curl --fail --silent --show-error \
  --header "Authorization: Bearer $TOKEN" \
  "$BASE_URL/api/v1/torrents/$TORRENT_ID/status" | jq
```

Do not set `Content-Type: multipart/form-data` manually; curl must generate the boundary. A magnet can report `adding` with `metainfo_ready: false` until peers provide metadata. A `ready` task has a file view, but reads can still wait for peers.

### Subtitles and task management

Subtitle upload is multipart and accepts the target video display path plus the subtitle file. The server derives the final subtitle path; it does not permit arbitrary writes below the mount:

```sh
curl --fail --request PUT "$BASE_URL/api/v1/torrents/<torrent-id>/subtitles" \
  --header "Authorization: Bearer $TOKEN" \
  --form 'video_path=Season 1/E01.mkv' \
  --form 'file=@./E01.srt'
```

The video and subtitle basenames must match exactly. Supported subtitle extensions are lowercase `.srt`, `.ass`, and `.vtt`; the target must be a supported payload video and must not collide with an existing payload or ambiguous mount name. `docker cp` into `/mnt` or `/share` is not a supported alternative.

Categories use `POST /api/v1/categories` with `{"name":"movies"}` and `PUT /api/v1/torrents/{id}/category` with `{"category":"movies"}`; send an empty category to clear it. Favorites use `PUT /api/v1/torrents/{id}/favorite` with `{"favorite":true}`. Deletion and prune return asynchronous operation IDs; poll `/api/v1/operations/{id}` until the operation reaches its terminal state.

### Status and errors

The task lifecycle is not a completion percentage:

| State | Meaning |
| --- | --- |
| `adding` | Metainfo is not available yet, commonly for a magnet |
| `ready` | Metainfo is available and the current file view can be read |
| `error` | Metainfo could not be obtained or processed |
| `deleting` | Deletion is in progress |
| `delete_failed` | Deletion failed and can be retried |
| `deleted` | Terminal state of a deletion operation |

`cached_bytes` is current in-memory cache occupancy, not the number of bytes ever downloaded. `downloaded_bytes` and `uploaded_bytes` are useful/actual torrent payload counters for the current backend session and reset after restart.

Common statuses are `400` for invalid request data, `401` for missing/invalid/expired credentials, `404` for unknown tasks or operations, `409` for deletion or namespace conflicts, `413` for upload limits, `415` for unsupported media or subtitle formats, `500` for unclassified server errors, `503` for unavailable subtitle storage or unconfirmed upload-rate durability, and `507` for insufficient storage. Protected API `401` responses include `WWW-Authenticate: Bearer`.

## Web UI

When HTTP is enabled, the same listener serves the embedded Web UI and API. The dashboard can search and filter tasks, show cache and runtime transfer counters, and add magnets or one or more `.torrent` files. The detail view displays files, piece ranges, cache coverage, and managed subtitle targets.

The UI also supports categories, favorites, asynchronous deletion, managed subtitle upload/replacement, and upload-rate settings. Authentication tokens are held in the current browser tab's `sessionStorage`; restarting the service or expiring a token requires login again. The production UI is embedded into the Go binary. During development, Vite listens on `127.0.0.1:5173` and proxies `/api` to a separately running backend:

```sh
npm ci --prefix web
npm run dev --prefix web
```

## Configuration

Configuration is merged by field with this precedence:

```text
environment variables > TOML file > built-in defaults
```

The TOML decoder rejects unknown fields. There is no automatic environment variable for every TOML key; only documented bindings are read. An empty HTTP listener disables HTTP. A non-loopback HTTP listener requires complete authentication.

### Main TOML sections

| Section | Common keys | Notes |
| --- | --- | --- |
| `[http]` | `listen_addr`, `max_upload_bytes` | HTTP/UI listener; default `127.0.0.1:8080`, upload default 10 MiB |
| `[http.auth]` | `enabled`, `username`, `password_hash`, `password_hash_file`, `token_ttl` | Single-user bcrypt authentication and in-memory Bearer tokens |
| `[connections]` | `listen_host`, `listen_port`, `disable_ipv4`, `disable_ipv6`, `no_port_forwarding`, `bootstrap_nodes` | Peer listener, address families, NAT mapping, and DHT bootstrap |
| `[proxy]` | `socks5_url` | Optional `socks5://` or `socks5h://` proxy |
| `[cache]` | `capacity_bytes` | Positive in-memory piece-cache limit; default 2 GiB |
| `[identity]` | `tracker_user_agent`, `peer_id_prefix`, `extended_handshake_client_version` | Tracker and peer identity values |
| `[log]` | `level`, `format`, `add_source` | `debug`/`info`/`warn`/`error`, `text`/`json`, and source locations |
| `[mount]` | `allow_other` | Whether other local UIDs may read the FUSE mount |

The Docker image ships a different TOML default: peer port `6881`, IPv6 disabled, no automatic port forwarding, and a 2 GiB cache. The local Go default uses peer port `0` and enables both address families. Do not infer one set of defaults from the other.

### Supported environment variables

The ordinary field bindings are:

| Environment variable | TOML key |
| --- | --- |
| `TORRENTFS_CONNECTIONS_LISTEN_HOST` | `connections.listen_host` |
| `TORRENTFS_CONNECTIONS_LISTEN_PORT` | `connections.listen_port` |
| `TORRENTFS_CONNECTIONS_DISABLE_IPV4` | `connections.disable_ipv4` |
| `TORRENTFS_CONNECTIONS_DISABLE_IPV6` | `connections.disable_ipv6` |
| `TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING` | `connections.no_port_forwarding` |
| `TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES` | `connections.bootstrap_nodes` |
| `TORRENTFS_MOUNT_ALLOW_OTHER` | `mount.allow_other` |
| `TORRENTFS_PROXY_SOCKS5_URL` | `proxy.socks5_url` |
| `TORRENTFS_CACHE_CAPACITY_BYTES` | `cache.capacity_bytes` |
| `TORRENTFS_IDENTITY_TRACKER_USER_AGENT` | `identity.tracker_user_agent` |
| `TORRENTFS_IDENTITY_PEER_ID_PREFIX` | `identity.peer_id_prefix` |
| `TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION` | `identity.extended_handshake_client_version` |
| `TORRENTFS_HTTP_LISTEN_ADDR` | `http.listen_addr` |
| `TORRENTFS_HTTP_MAX_UPLOAD_BYTES` | `http.max_upload_bytes` |
| `TORRENTFS_HTTP_AUTH_ENABLED` | `http.auth.enabled` |
| `TORRENTFS_HTTP_AUTH_TOKEN_TTL` | `http.auth.token_ttl` |
| `TORRENTFS_LOG_LEVEL` | `log.level` |
| `TORRENTFS_LOG_FORMAT` | `log.format` |
| `TORRENTFS_LOG_ADD_SOURCE` | `log.add_source` |

`TORRENTFS_USERNAME` and `TORRENTFS_PASSWORD` are the special shared HTTP/SMB credential pair, not automatic bindings for arbitrary TOML keys. `TORRENTFS_SMB_ENABLED` is consumed by the Docker entrypoint. `PUID` and `PGID` select the container runtime identity and are not Go configuration variables.

`TORRENTFS_PATHS_DATA_DIR` is a removed historical variable and does not restore a disk payload path. The cache capacity must be positive; the two address families cannot both be disabled; peer ports must be in `0..65535`; and a non-loopback HTTP listener must have authentication enabled. Static TOML and ordinary environment variables are read at startup and are not hot-reloaded.

### Upload-rate settings

Upload limiting applies to aggregate BitTorrent payload uploads in a session and is measured in bytes per second. It does not limit HTTP request bodies, tracker traffic, or protocol overhead. It is managed through the API/UI rather than static TOML:

```text
GET /api/v1/settings/upload-rate
PUT /api/v1/settings/upload-rate
```

Example body:

```json
{
  "rate_limit_bytes_per_second": 1048576,
  "schedule": {"start": "08:00", "end": "22:00"}
}
```

A zero rate with no schedule means unlimited. A positive rate with no schedule applies all day; a schedule applies the limit during the half-open local-time window. The setting is stored as `torrents-dir/.metadata/upload_rate.json`, takes effect without rebuilding the client, and is loaded at startup. A directory-sync failure after the atomic rename can return a durability warning even though the new rule is already active.

## Docker

The Dockerfile builds the Web UI with Node 22.23.2, builds a `CGO_ENABLED=0` Go 1.27 binary with the UI embedded, and runs it in Debian bookworm-slim with FUSE3, Samba, `tini`, certificates, and `passwd`.

### Recommended: HTTP management plus container-local FUSE/SMB

This is the shortest first-use path on Linux with rootful Docker. FUSE and Samba remain in the container's mount namespace, so the host does not need to propagate a FUSE mount. Docker must be able to provide `/dev/fuse`, `SYS_ADMIN`, and `NET_BIND_SERVICE`; the host security policy must permit FUSE.

Build from the repository:

```bash
git clone https://github.com/yakumioto/torrentfs-go.git
cd torrentfs-go
docker build -t torrentfs .
```

The nightly workflow publishes tags containing a UTC date, commit SHA, and run id. It does not promise floating `latest` or `nightly` tags. Use a locally built image unless a specific published tag has been verified.

Prepare a non-zero runtime identity and a writable persistent directory. The entrypoint must start as root and then drops torrentfs and smbd to `PUID:PGID`; do not use `docker run --user`:

```bash
HOST_UID="$(id -u)"
HOST_GID="$(id -g)"
if [[ "$HOST_UID" == 0 || "$HOST_GID" == 0 ]]; then
  PUID=1500
  PGID=1500
else
  PUID="$HOST_UID"
  PGID="$HOST_GID"
fi
export PUID PGID

# For a new directory. Inspect existing data before changing its ownership.
sudo install -d -o "$PUID" -g "$PGID" -m 0755 /srv/torrents
```

The entrypoint never chowns a bind mount. The selected identity must be able to read, write, and traverse `/srv/torrents`; metadata, metainfo, registry state, subtitles, and locks are persistent there. Piece payloads are not written to this directory.

Use one shared credential pair for HTTP and SMB. In SMB mode, the username must resolve to the container's runtime Unix account; `torrentfs` is the stable account name. The `read` command below is Bash syntax:

```bash
export TORRENTFS_USERNAME=torrentfs
read -r -s -p 'Shared HTTP/SMB password: ' TORRENTFS_PASSWORD
printf '\n'
export TORRENTFS_PASSWORD

docker run --detach --name torrentfs \
  --publish 127.0.0.1:8080:8080 \
  --publish 127.0.0.1:445:445 \
  --env PUID --env PGID \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --cap-add NET_BIND_SERVICE \
  --security-opt apparmor=unconfined \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_SMB_ENABLED=true \
  --env TORRENTFS_USERNAME \
  --env TORRENTFS_PASSWORD \
  torrentfs
```

Do not pass `-mountpoint` in SMB mode: the entrypoint owns the fixed internal `/share` mount and starts Samba only after the FUSE mount is ready. The share is named `torrentfs`, is read-only, and never exposes `/torrents` or `.metadata`.

The example publishes HTTP and SMB only on loopback. For trusted LAN access, bind an appropriate LAN address and configure a firewall; put HTTP behind TLS because the service itself has no TLS. The Docker TOML fixes the peer port at `6881`; to accept inbound peers, publish both `6881/tcp` and `6881/udp` in the same run. Do not assume that publishing only HTTP makes peers reachable.

Docker environment variables can be visible to users with inspect permissions. Treat them as plaintext deployment configuration, not as a secret store.

### Login, add, and read

Open `http://127.0.0.1:8080/` and log in as `torrentfs`. The dashboard can upload a `.torrent` or add a magnet. Do not copy a `.torrent` into `/torrents`: the service does not scan that directory to create tasks.

For an API flow, use `curl` and `jq` to encode credentials safely:

```bash
BASE_URL=http://127.0.0.1:8080
LOGIN_BODY="$(jq -n \
  --arg username "$TORRENTFS_USERNAME" \
  --arg password "$TORRENTFS_PASSWORD" \
  '{username: $username, password: $password}')"
TOKEN="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/auth/login" \
  --header 'Content-Type: application/json' \
  --data "$LOGIN_BODY" | jq -r .token)"

MAGNET_URI='magnet:?xt=urn:btih:<info-hash>'
TORRENT_ID="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data "$(jq -n --arg magnet_uri "$MAGNET_URI" '{magnet_uri: $magnet_uri}')" \
  | jq -r .id)"

# Or upload a local .torrent. curl creates the multipart boundary.
TORRENT_ID="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN" \
  --form "file=@$PWD/example.torrent" \
  | jq -r .id)"

curl --fail --silent --show-error \
  --header "Authorization: Bearer $TOKEN" \
  "$BASE_URL/api/v1/torrents/$TORRENT_ID/status" | jq
```

A magnet may remain `adding` until peers provide metainfo. `ready` makes the file tree available but can still wait for pieces during reads. The SMB URI is `smb://127.0.0.1/torrentfs`; `smbclient` uses the equivalent `//127.0.0.1/torrentfs` form on Linux, while Windows uses `\\host\torrentfs`:

```bash
cat > ./torrentfs.smb-credentials <<EOF
username=$TORRENTFS_USERNAME
password=$TORRENTFS_PASSWORD
EOF
chmod 600 ./torrentfs.smb-credentials

smbclient //127.0.0.1/torrentfs \
  -A ./torrentfs.smb-credentials -m SMB3 -c 'ls'
# Replace payload.bin with a real path shown by ls.
smbclient //127.0.0.1/torrentfs \
  -A ./torrentfs.smb-credentials -m SMB3 \
  -c 'get payload.bin ./payload.bin'

rm -f ./torrentfs.smb-credentials
```

A single-file torrent normally appears at the share root; a multi-file torrent keeps its directories, and a category adds a category directory. The share is read-only. A restart preserves the same managed directory but invalidates tokens and clears the in-memory piece cache.

### Stop, restart, and external configuration

```bash
docker logs -f torrentfs
docker stop --time 60 torrentfs
docker start torrentfs
```

The entrypoint gives Samba up to 10 seconds and torrentfs up to 45 seconds for normal shutdown. Keep the same `/srv/torrents` bind mount when restarting. Remove the container after testing with `docker rm -f torrentfs`.

To override the image's bundled TOML while keeping the default command, mount over the same path:

```bash
docker run --detach --name torrentfs \
  --env PUID --env PGID \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src="$PWD/torrentfs.toml",dst=/etc/torrentfs/torrentfs.toml,readonly \
  torrentfs
```

If the file is mounted elsewhere, pass both the config path and the required positional directory. Omitting `/torrents` replaces the image's default command with an invalid CLI invocation:

```bash
docker run --detach --name torrentfs \
  --env PUID --env PGID \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src="$PWD/torrentfs.toml",dst=/config.toml,readonly \
  torrentfs -config /config.toml /torrents
```

### Other Docker modes

- **HTTP-only:** omit `/dev/fuse`, FUSE capabilities, and `TORRENTFS_SMB_ENABLED`. This provides management only; it does not provide a generic HTTP content download endpoint.
- **Host-visible FUSE:** use the FUSE example below with `/mnt`, `rshared`, and the host's mount-propagation prerequisites. Keep an authenticated HTTP endpoint if users need to add tasks to an empty directory; a FUSE-only example is not a complete first-use workflow.
- **Container-local SMB:** use `/share` as above. It does not need a host `/mnt`, `rshared`, or cross-container mount propagation.

### Docker defaults and identities

The image command is equivalent to:

```text
/usr/local/bin/torrentfs -config /etc/torrentfs/torrentfs.toml /torrents
```

The image creates `/torrents` with permissive initial mode only when no bind mount is supplied. Its TOML defaults HTTP to container-local `127.0.0.1:8080`, peer port `6881`, IPv6 disabled, no automatic port forwarding, a 2 GiB cache, and `allow_other = false`. Publishing `8080` alone does not change a loopback listener; set `TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080` and enable authentication for container access.

`PUID` and `PGID` must be unsigned decimal values in `1..4294967294`. The entrypoint rejects `docker run --user`, zero, invalid, and out-of-range values before starting services. SMB also requires `/dev/fuse`, `SYS_ADMIN`, `NET_BIND_SERVICE`, a valid non-empty username/password pair, and a runtime username that resolves to the selected UID.

### Host-visible Docker FUSE mount

This mode is optional and has more host-specific requirements than container-local SMB. `/srv/mnt` must support recursive shared propagation, and the host must allow the FUSE and `SYS_ADMIN` operations:

```bash
mkdir -p /srv/torrents /srv/mnt
# The entrypoint does not chown either bind source.
sudo chown "$PUID:$PGID" /srv/torrents /srv/mnt

docker run --detach --name torrentfs-fuse \
  --env PUID --env PGID \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  torrentfs -config /etc/torrentfs/torrentfs.toml -mountpoint /mnt /torrents
```

If HTTP is published for adding tasks, set a non-loopback listener and complete authentication; if HTTP is disabled, add tasks through another process before mounting the directory. The FUSE tree remains read-only even though `/mnt` itself must be a writable bind target for mount setup.

## Persistent state and limitations

- Canonical metainfo is `<infohash>.torrent`; pending magnets are under `.metadata/pending`.
- Registry state, category indices, upload-rate settings, peer identity, lock state, and managed subtitles are persisted below `.metadata`.
- Piece bytes, piece completion, cache-hit counts, transfer counters, and transient read priorities are in memory only. A restart does not rehash or restore the piece cache.
- The registry is the only task source of truth. Root-level `.torrent` files that are not canonical registry files are ignored.
- Managed subtitles remain separate from torrent payload and are cleaned up with the task. A read-only mount cannot be used to add or replace them.
- Static TOML and environment settings are read at startup. Upload-rate settings are the exception: the API/UI applies them immediately and persists them for the next startup.
- The HTTP API has no generic content download or playback route. Use the FUSE tree, container-local SMB, or a host-visible FUSE mount.

## Build, test, and contribute

The embedded Web UI should be built before Go quality checks:

```sh
npm ci --prefix web
npm run typecheck --prefix web
npm run lint --prefix web
npm test --prefix web -- --run
npm run build --prefix web
rm -rf -- web/node_modules

go build ./...
go vet ./...
go test ./...
```

The repository also provides focused Docker checks:

```sh
./scripts/http-smoke.sh
./scripts/docker-config-smoke.sh
./scripts/docker-smb-smoke.sh
./scripts/docker-smoke.sh
```

The HTTP script does not need FUSE. The SMB and host-FUSE checks require a Linux Docker daemon, `/dev/fuse`, the capabilities described above, and supporting tools such as `python3`, `sha256sum`, `findmnt`, and `timeout`. `docker-smb-smoke.sh` may skip host CIFS support only when explicitly configured to do so; that does not prove kernel CIFS mounting.

## Troubleshooting

| Symptom | Likely cause and action |
| --- | --- |
| Container exits with a runtime identity error | `PUID`/`PGID` is empty, zero, invalid, out of range, or the bind-mounted `/torrents` is not accessible to that numeric identity. |
| HTTP is unreachable after `-p 8080:8080` | The daemon is still listening on container loopback. Set `TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080` and enable authentication. |
| `401` from the API | Missing, malformed, expired, or revoked Bearer token; login again. |
| Magnet remains `adding` | Peers have not supplied metainfo. This is a network/availability condition, not an HTTP content download failure. |
| `ready` read waits | The task has metainfo but the required pieces are not cached; peers must provide them. |
| `docker cp` into `/mnt` or `/share` fails | Expected: the projected FUSE/SMB tree is read-only. Upload subtitles through the authenticated API/UI. |
| SMB refuses to start | Check `/dev/fuse`, `SYS_ADMIN`, `NET_BIND_SERVICE`, non-empty single-line credentials, and that `TORRENTFS_USERNAME` resolves to the runtime UID. |
| External TOML causes a CLI error | When mounting outside `/etc/torrentfs/torrentfs.toml`, pass both `-config /config.toml` and `/torrents`; the TOML must also satisfy the non-loopback authentication rule. |
| Port 445, 8080, or 6881 is already in use | Choose a free host binding where the client supports it, or stop the conflicting service. Peer TCP and UDP must be mapped together when inbound peers are required. |

## License

Mozilla Public License 2.0; see [LICENSE](LICENSE).
