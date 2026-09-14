# torrentfs

Mount BitTorrent downloads as a FUSE filesystem.

> **Status: M5 — robustness and release.** `torrentfs` takes one writable
> `torrents` directory, continuously reconciles its direct `.torrent` files,
> mounts a torrent tree, serves file content on demand, and exposes a writable
> `metadata/` control directory for adding and removing torrents. A single-file
> torrent is exposed directly as a regular file, e.g. `<mount>/movie.mp4`, so a
> player can open and seek it without a wrapper directory. A read-only `stats/`
> control tree mirrors the data tree and reports piece state; repeated reads use
> an in-memory piece cache. `metadata` and `stats` are reserved root names, so
> a top-level torrent with either name is disambiguated.
>
> A status leaf lists the pieces of the file it mirrors in index order,
> separated by single spaces and ending with a newline: `[x]` is a verified
> complete piece, `[X n]` is a partial piece with `n` available bytes, `[N]` is
> an incomplete piece that is not wanted, and `[]` represents other incomplete
> states. For a single-file torrent the whole-torrent state is one leaf at
> `<mount>/stats/<name>`.

## Build

Requires Go 1.27+.

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
golangci-lint run ./...
```

## Usage

```sh
go run ./cmd/torrentfs -mountpoint <dir> [-config <file>] [-data-dir <dir>] <torrents-dir>
```

`-mountpoint` is required, and `<torrents-dir>` is exactly one existing,
readable and writable directory. A file path such as
`/data/torrentfs/input.torrent` is rejected: single-file positional input is
not supported. Without `-config`, defaults are used; a TOML file loads the five
sections shown in `torrentfs.example.toml`, and an explicit `-data-dir`
overrides `[paths].data_dir`. `-data-dir` stores downloaded torrent data only;
it is distinct from `<torrents-dir>`.

At startup, torrentfs restores metadata and scans only direct regular,
non-symlink files in `<torrents-dir>` whose names end in lower-case `.torrent`.
It does not recurse into subdirectories. The directory is reconciled about
once per 100 ms while the process runs: adding a stable valid `.torrent` loads
it without restart, and removing a source releases its torrent when no other
directory source or metadata file refers to the same info hash. Duplicate files
for one hash share one torrent. Configuration is read at every startup;
changing the file takes effect after a restart, not through SIGHUP.

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
├── <multi-name>/       # multi-file torrent: the usual directory tree
│   └── <relative-file>
├── metadata/           # writable control directory (maps to <torrents-dir>/.metadata)
└── stats/              # read-only status mirror of the data tree
    ├── <single-name>       # one status leaf for the single-file torrent
    └── <multi-name>/<relative-file>
```

A torrent whose metainfo has no directory structure (a single-file torrent) is
exposed as the regular file `<mount>/<name>`; a player can open
`<mount>/movie.mp4` and seek it directly. A multi-file torrent keeps its
directory tree, and a torrent that holds one file but has directory structure
stays a directory. The `stats/` tree mirrors the data tree node for node — a
data file becomes a status leaf, a data directory a status directory — and is
read-only: writes, creates, and renames inside it fail with `EROFS`. The
former per-torrent `<mount>/<name>/.stats` file is gone, and there is no alias
for the old nested single-file path `<mount>/<name>/<name>`; both are
development-stage breaking changes.

`metadata` and `stats` are reserved mount-root names. A torrent whose display
name collides with one, or with another torrent's name, is disambiguated by
appending a hash prefix, so the data tree and the `stats/` mirror always use the
same name. The status leaf for a multi-file torrent covers only the pieces that
back that file; a boundary piece shared with a neighbouring file is reported by
both leaves.

Data already present under the data directory (default: `<user cache
dir>/torrentfs`) is served without contacting the network; missing pieces are
fetched from peers while the mount is live. Write a complete `.torrent` file to
`<mountpoint>/metadata/` to add a persistent metadata source, and unlink it to
remove that source. On disk, metadata is always `<torrents-dir>/.metadata`,
sibling to the `<torrents-dir>/.stats` anchor of the `stats/` control
namespace; neither directory is scanned as an ordinary source. Only
`.metadata` holds files — the mounted `stats/` tree renders piece state live
and writes nothing. This is a development-stage breaking layout change: the
former sibling `filepath.Clean(data_dir)+".metadata"` is not read, migrated,
reported, or written.

Metadata restores the torrent set and references, while no SQLite or other
progress database is used. Downloaded data is rechecked by anacrolix after a
restart, but the in-memory piece cache, cache hit count, and transient read
priorities are not retained.

## Configuration

The supported TOML keys are:

```toml
[paths]
data_dir = "./torrentfs-data"

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
  than hang. The session and mount stay healthy: the `stats/` tree reports the
  incomplete state instead of the process crashing.
- **Partial pieces.** A piece only some of whose bytes are present renders as
  `[X n]` in a `stats/` leaf, with `n` the available byte count; `[N]` marks an
  incomplete piece that is not wanted and `[]` any other incomplete state.
- **Missing paths.** A torrent or file that does not exist maps to `ENOENT`; a
  path below a single-file torrent root maps to `ENOTDIR`; the torrent and
  `stats/` trees are read-only, so writes into them return `EROFS`.
- **Underlying errnos are preserved.** A wrapped `syscall.Errno` such as
  `ENODATA` reaches the caller unchanged; only unclassified failures flatten to
  `EIO`. A missing peer swarm is a health warning surfaced through the `stats/`
  tree and read errors, not a crash.

## Testing

The suite has three layers:

1. **Unit and offline integration** (`go test ./...`) — config, cache,
   filesystem layout (including single-file roots and the `stats/` mirror),
   session lifecycle, per-file piece-state projection, and metadata handling.
   These run anywhere, without network access.
2. **Concurrency and error paths** — concurrent reads, lookups, directory
   listings, `stats/` reads, and metadata churn; session close races; and
   incomplete or missing data. Run under the race detector with
   `go test -race ./...`.
3. **Real FUSE mounts** — a smoke mount, a single-file direct read and seek, a
   multi-file tree with its `stats/` mirror, concurrent reads through the
   mount, and a self-hosted swarm (a loopback HTTP tracker plus a seeder and a
   leecher session) that transfers real content into a FUSE mount. These
   require `/dev/fuse`, `fusermount`/`fusermount3`, and mount permission.

Real-mount tests skip themselves, with a printed reason, where FUSE is
unavailable — they are never counted as a passing FUSE run. To make that
absence a failure instead, set `TORRENTFS_FUSE_REQUIRED=1`:

```sh
TORRENTFS_FUSE_REQUIRED=1 go test -race -run 'TestFuse|TestSessionIncomplete' ./...
```

The swarm test binds loopback only, uses an in-process tracker, and disables
DHT and UTP, so it never contacts the public network.

## Docker (rootful)

The repository includes an offline fixture and an end-to-end Docker/FUSE check.
From the repository root, run:

```sh
./scripts/docker-smoke.sh
```

The script builds the image, bind mounts a writable temporary `torrents`
directory at `/torrents`, preloads the matching payload under `/data`, reads it
through the FUSE mount, and verifies rejected single-file and missing-directory
CLI inputs. It requires a working Docker daemon, `/dev/fuse`, `SYS_ADMIN` mount
permission, and (on AppArmor hosts) permission to use
`--security-opt apparmor=unconfined`. The fixture is mounted at runtime; it is
not copied into the production image.

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
`/torrents/.metadata` and the `/torrents/.stats` anchor; do not mount it
read-only. Add and remove direct regular
lower-case `*.torrent` files in that directory while the container is running.
Use a temporary filename followed by an atomic rename for writers. `/srv/torrentfs-data`
stores downloaded payload data and `/srv/mnt` must be a writable mountpoint;
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
