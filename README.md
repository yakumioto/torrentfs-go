# torrentfs

Mount a BitTorrent download as a read-only FUSE filesystem.

> **Status: M1 read-only MVP.** `torrentfs` loads one or more `.torrent`
> files, mounts a read-only tree with one directory per torrent, and serves
> each file's content on demand from a torrent session. Write semantics,
> `.stats`, caching and seeding land in later milestones.

## Build

Requires Go 1.27+.

```sh
go build ./...
go vet ./...
golangci-lint run ./...
```

## Usage

```sh
go run ./cmd/torrentfs -mountpoint <dir> [-data-dir <dir>] <torrent-file>...
```

Data already present under the data directory (default: `<user cache
dir>/torrentfs`) is served without contacting the network; missing pieces are
fetched from peers while the mount is live. Send `SIGINT` or `SIGTERM` to
unmount and exit.

A TOML configuration file is not read yet; the schema and loading land in M4.

## License

Mozilla Public License 2.0 — see [LICENSE](LICENSE).
