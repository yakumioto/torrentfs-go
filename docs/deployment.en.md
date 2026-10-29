# Deployment guide

[简体中文](deployment.md) · [README](../README.en.md)

Start with the [README Docker + SMB workflow](../README.en.md#quick-start). This guide covers selectable FUSE/SMB backends, container privileges, STRM, networking, and shutdown. Run repository commands from its root.

## Requirements

- Native builds need Go 1.27 or later and the Node.js/npm version in `web/.nvmrc`; a clean checkout must generate `web/dist` first.
- **Native SMB and HTTP-only need no FUSE device or mount privileges.** SMB accesses the virtual tree directly and does not start Samba.
- **Only FUSE mode** needs Linux FUSE3, `/dev/fuse`, and a host policy that permits mounting.
- Prefer an explicit non-root numeric Docker UID/GID. Publishing host port `445` is handled by Docker/host policy; the application listens on container port `1445` without `NET_BIND_SERVICE`. Rootless Docker's host low-port restrictions still apply.

## Local deployment

### Build and persistent directory

```sh
./scripts/build-web.sh
go build -o ./torrentfs ./cmd/torrentfs
mkdir -p "$PWD/torrents" "$PWD/mnt"
```

`torrents` must already exist, be readable/writable, and not be a symlink. It persists canonical metainfo, registry state, categories, subtitles, the signing key, and locks; pieces remain in memory only. Copying `.torrent` files there does not auto-import tasks. Only one process may manage a directory at a time.

### Select a backend

TOML selects one file output; HTTP management remains independently configurable:

```toml
[filesystem]
backend = "smb" # fuse / smb / none
read_timeout = "30s"

[fuse]
mountpoint = "/srv/mnt"
strm_mountpoint = ""
allow_other = false

[smb]
listen_addr = "0.0.0.0:1445"
username = "media-reader"
password_file = "/run/secrets/torrentfs-password"
```

- `fuse`: create the configured FUSE mounts, without SMB. At least one original-file or STRM mountpoint is required.
- `smb`: start built-in SMB without creating any FUSE mount. Unselected FUSE paths may remain in the same file.
- `none`: no file tree output, retaining HTTP API/Web UI. This is the default; disabling every output is rejected.

Configuration is read at startup; restart to switch backends. See the [configuration reference](configuration.en.md) for all fields and compatibility rules:

```sh
./torrentfs -config ./torrentfs.toml "$PWD/torrents"
```

Without config or mount arguments, defaults are HTTP `127.0.0.1:8080`, authentication disabled, and an automatically selected native peer port:

```sh
./torrentfs "$PWD/torrents"
curl --fail http://127.0.0.1:8080/api/v1/torrents
```

Legacy `-mountpoint` / `-strm-mountpoint` remain FUSE aliases and infer `fuse` when no backend is explicitly selected. Supplying them after explicitly choosing `smb` or `none` is a conflict, not a request to run both outputs.

### Optional STRM mount

FUSE may expose STRM alone or beside original files. HTTP must be enabled and `strm.base_url` non-empty. Mount directories must exist and must not be equal or nested after normalization and symlink resolution:

```sh
mkdir -p "$PWD/mnt-strm"
TORRENTFS_STRM_BASE_URL=https://media.example/torrentfs/ \
  ./torrentfs -mountpoint "$PWD/mnt" -strm-mountpoint "$PWD/mnt-strm" "$PWD/torrents"
```

Replace the placeholder domain with a player-reachable address. A prefixed reverse proxy must strip the prefix before forwarding `/stream/...`. SMB's STRM share needs no such mount directories.

### CLI and lifecycle

```text
torrentfs [-config <file>] [-mountpoint <dir>] [-strm-mountpoint <dir>] <torrents-dir>
```

`<torrents-dir>` is the one required positional argument; `-h` / `--help` prints help. An empty HTTP listener allows FUSE-only or original-file SMB-only operation, but removes management/subtitle-upload access and cannot be combined with STRM.

| Exit code | Meaning |
| --- | --- |
| `0` | Shutdown completed normally after `SIGINT`/`SIGTERM` |
| `1` | SMB, HTTP, session, FUSE-unmount runtime error, or incomplete shutdown |
| `2` | Invalid flags, directory, or configuration |

## Docker image and defaults

The [Dockerfile](../Dockerfile) builds a `CGO_ENABLED=0` Go binary with the UI embedded. Runtime retains optional FUSE3, `tini`, certificates, and identity-switching tools, but **no Samba, smbd, smbpasswd, or passdb**:

```sh
docker build -t torrentfs .
```

The image defaults to `USER 1000:1000` and this command:

```text
/usr/local/bin/torrentfs -config /etc/torrentfs/torrentfs.toml /torrents
```

Default mode is HTTP-only, still listening on container loopback `127.0.0.1:8080`. Publishing a port does not change its listener; exposed HTTP requires authentication. The image fixes the peer port at `6881`, disables IPv6/automatic port mapping, and limits the cache to `2GiB`.

Nightly builds run on `main` pushes or explicit `workflow_dispatch`, not every day without changes. Published tags include date, SHA, and run id; no floating `latest` / `nightly` tag is promised. Build locally unless a published tag has been verified.

## Runtime identity and storage

Use the README to prepare a non-zero identity and directory, then pass:

```sh
--user "$PUID:$PGID" --env PUID --env PGID
```

The numeric identity need not appear in `/etc/passwd`; the SMB login is independent of it. Omit `PUID/PGID` to retain a non-root process identity. If supplied, they must be decimal integers in `1..4294967294` and match `--user`; empty, zero, negative, invalid, or overflowing values are rejected.

The entrypoint never chowns bind mounts. The runtime identity must read, write, and traverse `/torrents`; do not make that mount read-only. The image's empty, unmounted `/torrents` is `0777` only as a default-startup fallback.

An explicit `--user 0:0` retains legacy root bootstrap, dropping to `PUID/PGID`. This is not the zero-capability path: it needs `SETUID`, `SETGID`, and `KILL` so root `tini` can forward signals to its differently owned child. Prefer starting non-root, which supports `--cap-drop ALL`.

`docker exec ... id` reports Docker's exec identity. Inspect `/proc/<torrentfs-pid>/status` for the daemon's Uid/Gid/CapEff/NoNewPrivs.

## Recommended: native read-only SMB

The [README](../README.en.md#quick-start) and [Compose template](../docker-compose.yaml) use:

```yaml
user: "1000:1000"
cap_drop: [ALL]
security_opt: ["no-new-privileges:true"]
ports:
  - "127.0.0.1:445:1445/tcp"
```

The application needs no `/dev/fuse`, `SYS_ADMIN`, `NET_BIND_SERVICE`, AppArmor unconfined, or mount propagation. Do not pass FUSE mount flags. `/share` is not a physical SMB backend: only the virtual tree is exported, never `/torrents/.metadata`.

| Setting | Responsibility |
| --- | --- |
| `filesystem.backend = "smb"` / `TORRENTFS_FILESYSTEM_BACKEND=smb` | Select native SMB |
| `smb.listen_addr` | Application listener, default `0.0.0.0:1445` |
| `smb.username`, `smb.password_file` | Independent SMB login and read-only mounted password file |
| `TORRENTFS_USERNAME`, `TORRENTFS_PASSWORD` | Compatible shared HTTP/SMB environment credentials, overriding SMB file credentials |

The original share is `torrentfs`: one NTLMv2 user, no guest/anonymous access or SMB1, minimum SMB2.1, required signing, and support for client-negotiated SMB3 encryption. Supporting encryption is not the same as requiring it by default. Use trusted networks, not direct Internet exposure. There is no Samba credential database; environment secrets remain visible to Docker administrators. Prefer a read-only secret file for SMB-only deployments. HTTP bcrypt cannot be reversed into an NTLM credential.

### Optional second STRM share

With HTTP enabled and a non-empty, player-reachable `strm.base_url`, SMB adds `torrentfs-strm` by default while retaining `torrentfs`. Set `smb.strm_enabled = false` / `TORRENTFS_SMB_STRM_ENABLED=false` to suppress the second share but keep signed HTTP streaming. Omitted means automatic selection from base_url. Legacy `TORRENTFS_STRM_ENABLED` is supported; explicit true requires SMB and a valid base URL. Conflicting old/new switches are rejected.

Merge the following into an existing Compose service while retaining storage, identity, credentials, and reduced privileges. Replace example `192.0.2.10` with a trusted, player-reachable LAN IP:

```yaml
ports:
  - "192.0.2.10:8080:8080"
  - "192.0.2.10:445:1445"
environment:
  TORRENTFS_FILESYSTEM_BACKEND: "smb"
  TORRENTFS_HTTP_LISTEN_ADDR: "0.0.0.0:8080"
  TORRENTFS_HTTP_AUTH_ENABLED: "true"
  TORRENTFS_STRM_BASE_URL: "http://192.0.2.10:8080"
```

Point the player's library at `smb://192.0.2.10/torrentfs-strm`. Do not use container-local `localhost`. HTTPS reverse proxies must be reachable by the player and forward `/stream/...` after stripping any prefix. Playback needs no management Bearer token. Protect `.strm` contents and access logs; use HTTPS outside trusted LANs.

### Client paths

- Endpoint: `smb://SERVER/torrentfs`, or `\\SERVER\torrentfs` on Windows.
- Share-relative file: e.g. `payload.bin`; no extra `share` directory is added.
- Client mountpoint: e.g. `/mnt/torrentfs-client`, not the server's virtual path.

```sh
smbclient //127.0.0.1/torrentfs -U torrentfs -m SMB3
```

Host `445:1445` publishing requires no client port change. Direct application-port access requires `smbclient -p 1445` or CIFS `port=1445`. Kernel CIFS uses its own private credential file:

```sh
mkdir -p /mnt/torrentfs-client
mount.cifs //SERVER/torrentfs /mnt/torrentfs-client \
  -o credentials=/path/to/credentials,vers=3.0,ro
ls -l /mnt/torrentfs-client/payload.bin
umount /mnt/torrentfs-client
```

Never mount a client share at `/`. SMB random reads enter the shared piece planner/cache directly, not Samba or FUSE.

## Other Docker modes

### HTTP-only

Select `filesystem.backend=none` (the default) and do not publish SMB. The same non-root `--user`, `--cap-drop ALL`, and no-new-privileges settings apply. Exposed HTTP requires authentication. A non-empty `strm.base_url` may serve existing signed links but creates no file share.

### Host-visible FUSE

Only this mode needs mounting privileges and a FUSE device. Prepare an empty directory accessible to the runtime identity and inspect host propagation:

```sh
sudo install -d -o "$PUID" -g "$PGID" -m 0755 /srv/mnt
findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
```

After confirming the host supports the required shared propagation:

```bash
docker run --detach --name torrentfs-fuse \
  --user "$PUID:$PGID" --env PUID --env PGID \
  --device /dev/fuse --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  --publish 127.0.0.1:8080:8080 \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  --env TORRENTFS_FILESYSTEM_BACKEND=fuse \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_USERNAME --env TORRENTFS_PASSWORD \
  torrentfs -config /etc/torrentfs/torrentfs.toml -mountpoint /mnt /torrents
```

Adjust AppArmor to the host policy. These privileges and propagation settings are not part of reduced-privilege SMB. Non-root `fusermount3` may require setuid execution, so do not blindly add SMB's no-new-privileges setting. The bind target must be writable to create a submount, while the exposed FUSE tree remains read-only. `fuse.allow_other` only permits additional local readers and also requires `user_allow_other` in `/etc/fuse.conf` for non-root mounts.

## External TOML

Mount a read-only config at the bundled path or supply both a complete `-config` flag and the positional directory:

```bash
docker run --detach --name torrentfs-config \
  --user "$PUID:$PGID" --env PUID --env PGID \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src="$PWD/torrentfs.toml",dst=/config.toml,readonly \
  torrentfs -config /config.toml /torrents
```

This only demonstrates config mounting; add credentials/ports for the selected mode. Environment overrides TOML, so remove old Compose backend overrides when selecting through TOML.

## Logs, stop, and recovery

```sh
docker logs -f torrentfs
docker stop --time 60 torrentfs
docker inspect torrentfs --format '{{.State.ExitCode}}'
docker start torrentfs
```

Native SMB stops accepting, cancels/drains reads and handles under a deadline, then follows the daemon's HTTP/session/FUSE cleanup constraints. There is no separate smbd process or shell supervisor. HTTP drain allows five seconds; the FUSE-unmount stage shares a 30-second deadline. Held propagated mounts or incomplete cleanup are reported as errors: a successful `docker stop` does not prove application exit code `0`.

The same `/torrents` restores tasks, categories, favorites, subtitles, limits, and the STRM signing key. Bearer tokens and pieces do not persist. Players may cache links after base-URL/key changes and need to reread/rescan. Keep the old image for rollback and inspect the running image, entrypoint, shares, and exit status after deployment; a successful image build is not a completed deployment.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| PUID/PGID conflicts with --user | Match non-zero numeric IDs and inspect data ownership; do not blindly recurse with chown |
| Published HTTP unreachable | Set a non-loopback listener and authentication; publishing does not change the listener |
| SMB cannot start | Check backend, internal 1445, credentials, and secret-file access; do not add FUSE privileges |
| Old/new backend or STRM settings conflict | Remove conflicting legacy switches and FUSE CLI arguments; explicitly select one backend |
| Host 445 occupied | Check the host SMB service or choose a port supported by the client; the app can keep its high port |
| Host cannot see FUSE | Only FUSE mode needs device, permissions, owned mount directory, and propagation |
| Player still shows old subtitles/links | New server enumeration refreshes snapshots, but clients may cache independently; reopen or rescan |

See the [usage guide](usage.en.md) for file/STRM/subtitle semantics and the [development guide](development.en.md) for validation commands.
