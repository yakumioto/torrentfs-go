# torrentfs

Mount BitTorrent downloads as a FUSE filesystem.

> **Status: M4 configuration and restart recovery.** `torrentfs` loads one or
> more `.torrent` files, mounts a torrent tree, serves file content on demand,
> and exposes a writable `metadata/` control directory for adding and removing
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
go vet ./...
golangci-lint run ./...
```

## Usage

```sh
go run ./cmd/torrentfs -mountpoint <dir> [-config <file>] [-data-dir <dir>] [torrent-file]...
```

`-mountpoint` is required. Without `-config`, defaults are used; a TOML file
loads the four sections shown in `torrentfs.example.toml`, and an explicit
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
```

`capacity_bytes` is a byte limit. An empty `socks5_url` disables the proxy;
otherwise use `socks5://` or `socks5h://`, optionally with username/password.
The proxy applies to TCP peer connections and HTTP(S) tracker, metainfo, and
webseed requests. UTP, DHT, and UDP tracker traffic are disabled or rejected in
proxy mode, so there is no direct UDP fallback. Incoming TCP listening remains
controlled by `[connections]` and is not routed through the SOCKS5 proxy.

## License

Mozilla Public License 2.0 — see [LICENSE](LICENSE).
