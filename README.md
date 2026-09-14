# torrentfs

Mount BitTorrent downloads as a FUSE filesystem.

> **Status: M5 — robustness and release.** `torrentfs` loads one or more
> `.torrent` files, mounts a torrent tree, serves file content on demand, and
> exposes a writable `metadata/` control directory for adding and removing
> torrents. Each torrent root exposes a read-only `.stats` file with one status
> token per piece; repeated reads use an in-memory piece cache. `.stats` is a
> reserved virtual name, so a top-level torrent file with that name is hidden.
>
> `.stats` lists pieces in index order, separated by single spaces and ending
> with a newline: `[x]` is a verified complete piece, `[X n]` is a partial
> piece with `n` available bytes, `[N]` is an incomplete piece that is not
> wanted, and `[]` represents other incomplete states.

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
go run ./cmd/torrentfs -mountpoint <dir> [-config <file>] [-data-dir <dir>] [torrent-file]...
```

`-mountpoint` is required. Without `-config`, defaults are used; a TOML file
loads the five sections shown in `torrentfs.example.toml`, and an explicit
`-data-dir` overrides `[paths].data_dir`. Positional torrent files are optional:
the session restores metadata first, then adds those files idempotently.
Configuration is read at every startup; changing the file takes effect after a
restart, not through SIGHUP.

Data already present under the data directory (default: `<user cache
dir>/torrentfs`) is served without contacting the network; missing pieces are
fetched from peers while the mount is live. Write a complete `.torrent` file to
`<mountpoint>/metadata/` to add a torrent, and unlink it to remove that torrent.
On disk, the control directory is the sidecar path
`filepath.Clean(data_dir)+".metadata"`; startup scans its regular, non-symlink
`*.torrent` files in filename order. Temporary files, symlinks, and other
entries are ignored. A malformed `.torrent` file makes startup fail instead of
silently dropping a torrent.

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
tracker_user_agent = ""
peer_id_prefix = ""
extended_handshake_client_version = ""
```

`capacity_bytes` is a byte limit. An empty `socks5_url` disables the proxy;
otherwise use `socks5://` or `socks5h://`, optionally with username/password.
The proxy applies to TCP peer connections and HTTP(S) tracker, metainfo, and
webseed requests. UTP, DHT, and UDP tracker traffic are disabled or rejected in
proxy mode, so there is no direct UDP fallback. Incoming TCP listening remains
controlled by `[connections]` and is not routed through the SOCKS5 proxy.

`[identity].tracker_user_agent` changes only the `User-Agent` header on HTTP
tracker announce requests; it does not change metainfo, webseed, or scrape
requests. `peer_id_prefix` is a prefix, not a complete peer ID: it is limited
to 20 bytes, and any remaining bytes are generated randomly for each session.
An empty prefix inherits the dependency's default prefix and random suffix; a
20-byte prefix leaves no random suffix. The generated peer ID is used for
BitTorrent handshakes and announces. `extended_handshake_client_version` is
the BEP 10 extended-handshake `v` value. Empty identity values inherit the
anacrolix defaults, and `v` is sent only when the peer supports the extended
handshake.

## Error behavior

Reads never fabricate data. When the pieces behind a range are unavailable, a
read either blocks until they arrive or returns an error; it does not return
zero-filled or partial-success content.

- **No peers / no seeder.** A torrent whose data is not local and whose swarm
  has no peers stays incomplete. Reads of missing ranges wait for pieces that
  never arrive until the session is closed, at which point they fail rather
  than hang. The session and mount stay healthy: `.stats` reports the
  incomplete state instead of the process crashing.
- **Partial pieces.** A piece only some of whose bytes are present renders as
  `[X n]` in `.stats`, with `n` the available byte count; `[N]` marks an
  incomplete piece that is not wanted and `[]` any other incomplete state.
- **Missing paths.** A torrent or file that does not exist maps to `ENOENT`;
  the torrent tree is read-only, so writes into it return `EROFS`.
- **Underlying errnos are preserved.** A wrapped `syscall.Errno` such as
  `ENODATA` reaches the caller unchanged; only unclassified failures flatten to
  `EIO`. A missing peer swarm is a health warning surfaced through `.stats` and
  read errors, not a crash.

## Testing

The suite has three layers:

1. **Unit and offline integration** (`go test ./...`) — config, cache,
   filesystem layout, session lifecycle, and metadata handling. These run
   anywhere, without network access.
2. **Concurrency and error paths** — concurrent reads, lookups, directory
   listings, and metadata churn; session close races; and incomplete or missing
   data. Run under the race detector with `go test -race ./...`.
3. **Real FUSE mounts** — a smoke mount, concurrent reads through the mount,
   and a self-hosted swarm (a loopback HTTP tracker plus a seeder and a leecher
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

## Docker (rootful)

The repository includes an offline fixture and an end-to-end Docker/FUSE check.
From the repository root, run:

```sh
./scripts/docker-smoke.sh
```

The script builds the image, bind mounts `examples/docker/example.torrent` as a
single file at `/torrents/example.torrent`, preloads its matching payload under
`/data`, reads that payload through the FUSE mount, and verifies the missing-file
error path. It requires a working Docker daemon, `/dev/fuse`, `SYS_ADMIN` mount
permission, and (on AppArmor hosts) permission to use
`--security-opt apparmor=unconfined`. The fixture is mounted at runtime; it is
not copied into the production image.

To run the image with a real torrent, set `TORRENT_FILE` to that existing
absolute `.torrent` file. The image does not contain user torrent files, and a
host directory bind mount does not create `/torrents/example.torrent` for you:

```sh
TORRENT_FILE=/srv/torrents/real-file.torrent
test -f "$TORRENT_FILE" || {
  printf 'torrent file not found: %s\n' "$TORRENT_FILE" >&2
  exit 1
}
docker build -t torrentfs .
docker run --rm \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  -v /srv/torrentfs-data:/data \
  --mount "type=bind,src=$TORRENT_FILE,dst=/torrents/input.torrent,readonly" \
  -v /srv/mnt:/mnt \
  torrentfs -mountpoint /mnt -data-dir /data /torrents/input.torrent
```

`TORRENT_FILE` must be a readable regular file, `/srv/torrentfs-data` must be
writable for torrent data, and `/srv/mnt` must be a writable mountpoint. The
single-file `--mount` destination and the positional argument must match.
`-data-dir` controls torrent data storage; it does not change the torrent input
path.

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
