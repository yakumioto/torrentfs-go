# torrentfs

Mount a BitTorrent download as a FUSE filesystem.

> **Status: M0 skeleton.** This repository is an early scaffold: a compilable
> Go module with linter setup and a minimal CLI. Nothing is mounted yet —
> `torrentfs` currently only answers `-h`/`--help` and exits cleanly.

## Build

Requires Go 1.27+.

```sh
go build ./...
go vet ./...
golangci-lint run ./...
```

## Usage

```sh
go run ./cmd/torrentfs -h
```

No filesystem operation is implemented yet; the configuration schema and the
mount path land in later milestones (see the MIO-4 issue).

## License

Mozilla Public License 2.0 — see [LICENSE](LICENSE).
