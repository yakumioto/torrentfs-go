# Development guide

[简体中文](development.md) · [README](../README.en.md)

All commands run from the repository root. Deployment modes and runtime permissions are in the [deployment guide](deployment.en.md).

## Requirements and local build

- Go 1.27 or newer.
- The Node.js version specified by [web/.nvmrc](../web/.nvmrc), currently 22.23.2, and npm.
- Linux FUSE3, `/dev/fuse`, and mount permissions when running real FUSE tests.
- Docker and supporting tools for container smoke checks.

The Go binary embeds `web/dist`. A clean checkout must build the UI before Go build, run, or test commands:

```sh
git clone https://github.com/yakumioto/torrentfs-go.git
cd torrentfs-go
./scripts/build-web.sh
go build -o ./torrentfs ./cmd/torrentfs
mkdir -p "$PWD/torrents" "$PWD/mnt"
```

[build-web.sh](../scripts/build-web.sh) installs frontend dependencies from the lockfile, builds `web/dist`, and removes `web/node_modules`; it does not remove `web/dist`.

## Architecture and storage model

```text
cmd/torrentfs          CLI, configuration, process lifecycle, shutdown
        │
        ├── internal/session      torrent lifecycle, metainfo, network, cache
        ├── internal/filesystem   read-only FUSE data tree
        ├── internal/api          HTTP routes, authentication, management, status
        └── web                   React UI embedded into the Go binary
```

The mount contains data only, with no control directories. Task management and subtitle publication go through the HTTP API. Managed subtitles are an overlay, stored separately from immutable payload and projected read-only by FUSE/SMB.

Canonical metainfo is stored as `torrents-dir/<infohash>.torrent`; the registry, pending magnets, categories, peer identity, upload-rate settings, and managed subtitles live under `.metadata`. The registry is the source of truth rather than an arbitrary directory scan. Only one process manages a directory at a time.

Piece bytes exist only in a bounded shared memory cache. Cache occupancy is not download progress, and transfer counters are runtime payload counters rather than persistent management state. Restart neither rehashes nor restores pieces from disk. See the [usage guide](usage.en.md) for the persistent layout and user-facing semantics.

The HTTP listener defaults to loopback. A non-loopback listener requires authentication; TLS termination belongs to a trusted reverse proxy rather than the daemon.

## Frontend development

Run the backend separately and start Vite:

```sh
npm ci --prefix web
npm run dev --prefix web
```

Vite listens on `127.0.0.1:5173` and proxies `/api` to `http://127.0.0.1:8080`. Production builds use same-origin relative `/api/v1` requests and are embedded into the Go binary.

Production static serving accepts `GET` and `HEAD`. Root and extensionless frontend deep links fall back to `index.html`; unknown assets with extensions return `404`. `index.html` uses `no-cache`, and built assets use long-lived immutable caching.

## Quality checks

Build the embedded UI before Go checks:

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
go test -race ./...

go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
golangci-lint run ./...
```

Use `./scripts/build-web.sh` when only the embedded UI build is needed. It does not replace typechecking, linting, or frontend tests.

## Integration and container checks

### HTTP-only and configuration smoke checks

```sh
./scripts/http-smoke.sh
./scripts/docker-config-smoke.sh
```

[http-smoke.sh](../scripts/http-smoke.sh) requires Docker, `curl`, and Python 3, but not a FUSE device. It builds a local image and checks static root/deep links, missing assets, authentication, `WWW-Authenticate`, login, protected list, and logout. It does not publish to a remote registry.

[docker-config-smoke.sh](../scripts/docker-config-smoke.sh) additionally verifies bundled TOML, the default command, environment overrides, external config mounts, and invalid SMB credentials/runtime accounts. It also requires `awk` and `timeout`.

### Required FUSE tests

Ordinary tests can skip FUSE cases when the device or permissions are missing. To turn missing prerequisites into a failure:

```sh
TORRENTFS_FUSE_REQUIRED=1 go test -race -run 'TestFuse|TestSessionIncomplete' ./...
```

`TORRENTFS_FUSE_REQUIRED` is a test gate, not daemon configuration. Real tests require `/dev/fuse`, `fusermount`/`fusermount3`, and mount permissions.

### Host-visible Docker FUSE smoke check

```sh
./scripts/docker-smoke.sh
```

[docker-smoke.sh](../scripts/docker-smoke.sh) requires a Linux Docker daemon, `/dev/fuse`, `SYS_ADMIN` or equivalent, a host security policy permitting FUSE, `findmnt`, Python 3, `sha256sum`, and `timeout`. The host mount source must meet the recursive shared-propagation prerequisites from the deployment guide.

### SMB smoke check

```sh
./scripts/docker-smb-smoke.sh
```

[docker-smb-smoke.sh](../scripts/docker-smb-smoke.sh) requires Linux Docker, `/dev/fuse`, `SYS_ADMIN`, `NET_BIND_SERVICE`, permission for FUSE, Python 3, `sha256sum`, `dd`, and `timeout`. It verifies authentication and guest rejection, directory listings, read hashes, read-only behavior, hidden metadata, shared credentials, runtime identities, normal shutdown, and failure coupling between smbd and torrentfs.

The script attempts a kernel CIFS mount and a large-offset read as well. If the host lacks CIFS/nested-mount capability, it reports that limitation and continues; a successful mount with wrong data is a failure. `TORRENTFS_SMB_SKIP_CIFS=1` explicitly skips that check. Neither kind of skip proves kernel CIFS mounting, and the required FUSE tests retain the separate random-read evidence. CI and nightly do not set the explicit skip variable.

To test a runtime identity different from the host's:

```sh
TORRENTFS_SMOKE_UID=1500 TORRENTFS_SMOKE_GID=1500 ./scripts/docker-smb-smoke.sh
```

These variables override smoke-test runtime settings, not image build arguments. Client operations share a bounded timeout; failures print service and source logs for diagnosis.

The CI nightly multi-platform OCI build and [nightly-build.sh](../scripts/nightly-build.sh) local archive entry point are different workflows. Local validation commands do not imply a floating release tag or publication guarantee.

## Contributing

Run checks appropriate to the changed layer and report any skipped FUSE, CIFS, or container prerequisites rather than presenting a skip as successful runtime verification. Keep source examples and API/configuration contracts consistent with the [configuration guide](configuration.en.md) and [API reference](api.en.md).
