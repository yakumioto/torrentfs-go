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
        ├── internal/session      torrent lifecycle, metainfo, network, cache, signed reads
        ├── internal/vfs          protocol-independent model, directory indexes, open snapshots, STRM
        ├── internal/filesystem   FUSE Adapter: inodes, POSIX attributes, mounts
        ├── internal/smb          SMB Adapter: read-only shares, handles, cancellation
        ├── internal/media        shared video/ISO detection and explicit MIME types
        ├── internal/stream       HMAC signatures, URL/path rules, cancellable Reader contract
        ├── internal/api          management authentication, status, separate signed HTTP streams
        └── web                   React UI embedded into the Go binary
```

The mount contains data only, with no control directories. Task management and subtitle publication go through the HTTP API. Managed subtitles are an overlay, stored separately from immutable payload and projected read-only by FUSE/SMB.

Canonical metainfo is stored as `torrents-dir/<infohash>.torrent`; the registry, pending magnets, categories, peer identity, upload-rate settings, managed subtitles, and optional STRM signing key live under `.metadata`. The registry is the source of truth rather than an arbitrary directory scan. Only one process manages a directory at a time.

Piece bytes exist only in a bounded shared memory cache. Cache occupancy is not download progress, and transfer counters are runtime payload counters rather than persistent management state. Restart neither rehashes nor restores pieces from disk. See the [usage guide](usage.en.md) for the persistent layout and user-facing semantics.

The HTTP listener defaults to loopback. A non-loopback listener requires authentication; TLS termination belongs to a trusted reverse proxy rather than the daemon.

## Filesystem view and directory cache contracts

- Each Session `Torrent` lazily builds the display-path index and immutable `[]FileView` in one traversal after metadata arrives. A dedicated initialization lock deduplicates work without reentering `Torrent.mu`; unresolved metadata is not memoized. Removing/readding a hash creates a new `Torrent`, holder, and identity. Every `Torrents()` call owns its outer slice/structs and refreshes ready state, names, categories, added times, and subtitles; only immutable Files are shared. Internal callers holding `Session.mu` use a locked helper rather than a public locking method. Management lock order remains namespace → per-hash → Session.mu.
- `FileListID` is a non-zero-sized opaque identity. Non-nil `FilesID` promises that Files remain permanently read-only; any path/size change needs a new slice and identity. Legacy nil-identity backends use exact comparisons and owned copies, never slice-address equality. Optional `TorrentContentsBackend` returns one current per-hash snapshot; a legacy directory operation scans `Torrents()` at most once. Missing/closed results never fall back to inode snapshots.
- Root/category operations still list the current library and recompute names and collision handling on every call; there is no whole-library root-name cache. Torrent directory nodes retain only state/hash/prefix. Lookup, Readdir, and Getattr fetch current per-hash contents, so subtitles remain visible even when go-fuse reuses an old StableAttr/inode. Deleted torrents and nested prefixes without remaining members return `ENOENT` through retained directories. `EntryTimeout` and `AttrTimeout` stay zero.
- Each mount privately caches `hash → current contents version → visited prefix`, retaining the sorted `childrenOfViews` output and a name index without changing layout priorities or textual prefix matching. Equal FilesID skips payload comparison; subtitles are compared field by field. Membership changes replace the hash's entire version. Backend/StreamURL calls happen outside cache locks; first builds are deduplicated per hash/version, and late requests may finish their own snapshot but cannot overwrite a newer version or resurrect removed entries.
- Only current versions and existing prefixes are retained. Listings and per-hash misses prune removed hashes; missing hashes/prefixes are not cached. Stable `inoByKey` allocations are not reclaimed, so total mount metadata is not claimed to be strictly bounded. Open DirStreams retain their own lists and offsets; new lookups/streams observe current members. There is no separate subtitle-deletion API; reduced subtitle sets are exercised through torrent deletion or test backends.

## STRM projection and HTTP lifecycle

The protocol-independent STRM Backend in `internal/vfs` wraps the shared Backend, retaining directories, non-video files, and the original presence/absence of Category/Subtitle/SubtitleStat capabilities. Per-hash projection does not compute root names; only whole-library `Torrents()` performs root naming on its own outer slice. Mapping keys include source FilesID and SingleFile shape: a single-file root keeps its original payload path, while directory torrents transform relative paths. Complete projections from trusted sources share read-only slices under a new derived FilesID, avoiding full-list comparison/copying on hot paths. Legacy sources keep content comparisons and return independent Files/Subtitles copies.

Listings attempt each unknown video length once. URL failures retain size-zero entries without publishing an immutable identity; later operations retry. Live lookup/stat/open return the actual error without blocking healthy neighbors, freezing failures, or retrying in a loop within one operation. Recovery and live stat/open observations of changed URL lengths publish new slices and derived identities, never mutate an older snapshot. Late size backfills verify both the entry still owning the hash and the corresponding file's size revision, preventing writes into a removed/recreated entry without discarding valid concurrent backfills for other files.

Payload live stat and `FileSnapshotBackend` still validate URL registration/readiness on every operation. Each open binds the exact generated text, reader, and length rather than performing a separate stat before open. Generating/reading URL text never opens video payload. Cached passthrough attributes can still succeed until a subsequent listing/per-hash miss prunes them, but opening always delegates to the live Backend. STRM mtime retains the torrent's added time; changed base URLs or keys require client rereads/rescans rather than an assumed hot refresh.

`internal/media` centralizes the original 14 video extensions, separate ISO detection, and explicit MIME types. `internal/stream` copies the signing key, binds a versioned domain, info hash, and raw torrent-relative path using full HMAC-SHA256, and escapes/decodes path segments exactly once. Encoded slashes and ambiguous components are rejected; `%252F` remains a literal `%2F` after one decode.

The Session creates/reuses `.metadata/stream_key` only when streaming is enabled, after obtaining the instance lock and before opening client resources. The key is exactly 32 bytes, atomically created with mode `0600`; invalid lengths, broad permissions, symlinks, and non-regular files fail startup rather than silently rotating credentials. `StreamURL` validates ready payload metadata without fetching pieces. `OpenStream` verifies before opening the shared context-aware reader, never a host or FUSE path.

The separate `/stream/...` handler does not weaken `/api/` Bearer authentication. It adapts `ReadAtContext` to `io.NewSectionReader` and `http.ServeContent`, uses explicit MIME without sniffing, and adds no whole-file or whole-block HTTP cache. HEAD/bodyless conditions avoid payload reads. Readers are cancelled/closed on completion, cancellation, and failure; recorded read failures after headers abort the transport rather than append JSON or fabricate a complete response.

Shutdown cancels stream contexts before a maximum five-second HTTP drain, forcing HTTP Close when necessary to unblock network writes. Session close follows, then both FUSE mounts are unmounted concurrently under one shared 30-second deadline. Force-closing connections is not proof that every handler or goroutine has returned. Native SMB stops accepting and cancels/drains its reads before session close. There is no Samba or two-process shell supervisor; static configuration selects SMB or FUSE. See the [deployment guide](deployment.en.md).

## Native SMB interfaces and dependency boundary

`internal/vfs.Tree` supplies Lookup/Stat, immutable directory observations, and open handles. `Handle.Metadata` describes the bytes of that same open; `ReadAtContext` must keep request cancellation rather than wrap only `io.ReaderAt`. Both adapters share layout, categories, subtitles, STRM, and the Session reader. Session production code does not depend on go-fuse types. SMB resolves virtual identities only, never a fallback host path.

An enumeration retains its snapshot across pages and refreshes on restart/reopen. Case-insensitive lookup, Windows-invalid names, and collisions use stable mappings rather than losing or opening the wrong file. Content/metadata versions invalidate indexes; hot paths must not rescan the whole library per file. Exact default-stream size/allocation queries do not enable ADS writes.

A slow READ on one connection must not block healthy READ/ECHO. CANCEL belongs to its request; handle/tree/session teardown, disconnect, and shutdown reclaim owned tasks. Shared piece flights survive until their last waiter leaves. Moving a prefetch anchor only withdraws stale background leases, not independent active foreground reads. Connections, frames, and pending work remain bounded.

`third_party/go-smb` pins an upstream baseline plus reviewable local patches, retaining MIT notices and patch documentation. It is not a complete Samba replacement, and relay/credential-capture hooks are not enabled. Runtime images distribute both the root and third-party licenses, not the whole source tree. Authentication, signing, encryption, compound, and lifecycle changes need real protocol and race regressions, not only a client from the same library.

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

## STRM test coverage

- Configuration/media/signing: disabled defaults, TOML/environment precedence, strict base URL validation and credential redaction; 14 video extensions separate from ISO and explicit MIME; Chinese and `%/#/?/+` round trips, prefixes, one-pass decoding, encoded-slash rejection, tampering, and cross-file/hash rejection.
- Session: registered ready payload checks, private key reuse after restart, damaged/permission/symlink rejection, deletion revocation, and peerless read timeouts, cancellation, and close.
- STRM Backend: unchanged original snapshots, retained non-video/existing STRM/subtitle entries, deterministic collision handling, categories and dynamic changes, subtitle snapshots, concurrent inode/path mappings, and zero-size entries for persistent URL failures without blocking healthy files. Real FUSE integration compares STRM text, HTTP Range, and original-file bytes without fetching payload to generate STRM.
- HTTP/CLI: GET/HEAD, single/multipart Range, conditions/If-Range, 416/403/404/405/503, offsets beyond 4 GiB, no-payload HEAD, real-connection truncation, cancellation cleanup, bounded shutdown with blocked reads/writes, dual-mount arguments, and a shared unmount deadline.

These are exercised by the Go quality checks and the required-FUSE/container checks below, not an Infuse device acceptance claim. Actual clients still need scanning, playback, seek, subtitle-matching, and base-URL/key-change cache-refresh checks.

## View-cache regressions and benchmarks

- Raw NodeFS/rawBridge regressions retain old multi-level directory NodeIds, perform Lookup/Readdir before subtitle publication, and assert that resolving the parents still reuses those NodeIds. New Lookup/Readdir calls through the old nodes must see subtitles in both original and STRM views, while existing streams retain their lists/offsets.
- Real FUSE regressions upload after visiting directories and retaining a dirfd, covering deep paths, equal/changed-length replacement, current queries versus old file-descriptor snapshots, and `ENOENT` after deletion. Ordinary tests can skip without FUSE, so run the required gate below. Reduced subtitle sets are simulated in Backend unit tests without adding an API.
- Session/STRM/directory tests count concurrent first builds, check stable source/derived identities, legacy in-place mutations, variable URL lengths and failure recovery, late-result fencing, and cache retention under delete/readd churn. Repeat these with the race detector.

```sh
go test -race -count=3 -run 'TestPayloadHolder|TestFilesystemViews' ./internal/session
go test -race -count=3 ./internal/filesystem ./internal/vfs ./internal/smb
go test -run '^$' -bench 'FilesystemView|TorrentLookup|STRMProjection' -benchmem -count=5 ./internal/session ./internal/filesystem ./internal/vfs
bash -n scripts/docker-smb-smoke.sh
```

The synthetic library has 111 torrents and 19,792 files; the largest torrent has 18,920 files in 216 virtual directories. Session benchmarks use real anacrolix metadata through `filesystemViewsLocked`, with discovery/trackers/webseeds disabled and no production data or credentials. Report ns/op, B/op, and allocs/op separately for cold/hot Session views, original/STRM root Lookup and deep Lookup/Readdir, and cold/hot STRM projection. Payload/index construction, library-list, and signature counters verify the hot paths. The initial baseline depends only on the old interfaces; after implementation the synthetic read-only source explicitly provides FilesID and per-hash contents, matching the Session contract.

Use the same scale and call patterns and take the median of five runs. Synthetic results do not replace isolated FUSE/SMB comparisons of full/relative-path stat, `ls -l`, `find`, and `ls -lR` under the same UID and directory, nor do they prove Infuse acceptance. Run container/HTTP smoke checks only in an authorized isolated environment; `bash -n` alone is syntax validation, not runtime verification.

## Integration and container checks

### HTTP-only and configuration smoke checks

```sh
./scripts/http-smoke.sh
./scripts/docker-config-smoke.sh
```

[http-smoke.sh](../scripts/http-smoke.sh) requires Docker, `curl`, and Python 3, but not a FUSE device. It builds a local image and checks static root/deep links, missing assets, authentication, `WWW-Authenticate`, login, protected list, and logout. It does not publish to a remote registry.

[docker-config-smoke.sh](../scripts/docker-config-smoke.sh) additionally needs `timeout` and Python 3. It checks the non-root default, numeric `--user`, zero capabilities/no-new-privileges, explicit root bootstrap, TOML backend choice, secret files, shared HTTP credentials, and conflicting legacy inputs. SMB-only really disables HTTP and needs no `/dev/fuse` or Samba. Identity/configuration failures precede session initialization; inaccessible storage must also fail.

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

The application runs with an explicit non-root UID/GID, `--cap-drop ALL`, no-new-privileges, and default AppArmor, without FUSE. Prerequisites are Linux Docker, Python 3, `sha256sum`, `dd`, and `timeout`; a separate client image provides smbclient/cifs-utils/curl. Checks cover authentication, wrong-password/guest denial, share enumeration, full-read hashes, denied writes, hidden metadata, shared HTTP credentials, real SMB-only, redacted logs, SIGTERM exit status, and abrupt single-process death.

STRM checks cover original/optional STRM read-only shares, visiting directories before subtitle publication/replacement, current enumeration/metadata/exact bytes through both shares, no-payload `.strm`/HTTP HEAD, Range `206`, and tampered-signature `403`. Physical `/share` mounts or Samba PID/passdb files are no longer acceptance criteria.

Only the disposable kernel-CIFS **client** may need privileged mounting; the application keeps zero capabilities. When CIFS mounts, a cold range larger than cache must match the source. With 16 unavailable reads pending on that same connection, a healthy cold range must finish correctly within 30 seconds. Server/client slow-read bounds are 45/60 seconds so early cancellation cannot fake success.

Unavailable CIFS/modules/nested mounting is reported as not run; `TORRENTFS_SMB_SKIP_CIFS=1` explicitly skips it. Passing FUSE reads does not prove SMB/CIFS compatibility. `internal/smb` still exercises authenticated TCP, an independent Go client, same-connection 16-read/CANCEL behavior, multi-page listings, and shutdown. Infuse remains a device acceptance check.

Use `TORRENTFS_SMOKE_IMAGE=<local-tag>` to reuse a built image without deleting it. Rebuild after source changes: old-image success is not new-source success. Default scripts own and clean their temporary images, networks, and data and publish nothing remotely.

To test a runtime identity different from the host's:

```sh
TORRENTFS_SMOKE_UID=1500 TORRENTFS_SMOKE_GID=1500 ./scripts/docker-smb-smoke.sh
```

These variables override smoke-test runtime settings, not image build arguments. Client operations share a bounded timeout; failures print service and source logs for diagnosis.

The CI nightly multi-platform OCI build and [nightly-build.sh](../scripts/nightly-build.sh) local archive entry point are different workflows. Local validation commands do not imply a floating release tag or publication guarantee.

`Nightly` names the development image channel, not a daily publication guarantee. Automatic builds run on pushes to `main`; without `main` updates there is no daily automatic rebuild. `workflow_dispatch` remains an explicit rebuild/retry entry point and allows rebuilding the same SHA manually. Only `main` in the original `yakumioto/torrentfs-go` repository can publish; existing quality gates and unique tags are unchanged.

## Contributing

Run checks appropriate to the changed layer and report any skipped FUSE, CIFS, or container prerequisites rather than presenting a skip as successful runtime verification. Keep source examples and API/configuration contracts consistent with the [configuration guide](configuration.en.md) and [API reference](api.en.md).
