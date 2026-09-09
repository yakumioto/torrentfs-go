# torrentfs

Mount BitTorrent downloads as a FUSE filesystem.

> **Status: M2 lifecycle and write semantics.** `torrentfs` loads one or more
> `.torrent` files, mounts a torrent tree, serves file content on demand, and
> exposes a writable `metadata/` control directory for adding and removing
> torrents. Piece status, `.stats`, and caching land in later milestones.

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
fetched from peers while the mount is live. Write a complete `.torrent` file to
`<mountpoint>/metadata/` to add a torrent, and unlink it to remove that torrent.
Send `SIGINT` or `SIGTERM` to unmount and exit.

A TOML configuration file is not read yet; the schema and loading land in M4.

## License

Mozilla Public License 2.0 — see [LICENSE](LICENSE).
