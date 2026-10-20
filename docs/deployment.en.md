# Deployment guide

[简体中文](deployment.md) · [README](../README.en.md)

Commands in this guide run from the repository root. For a complete first-use workflow, use the [README quick start](../README.en.md#quick-start). Configuration fields are documented in the [configuration guide](configuration.en.md); management requests are in the [API reference](api.en.md).

## Local deployment

Build the binary using the [development guide](development.en.md). A FUSE mount additionally requires Linux FUSE3, `/dev/fuse`, and mount permissions; HTTP/API-only mode does not require a FUSE device.

Prepare existing, readable, writable, non-symlink directories:

```sh
mkdir -p "$PWD/torrents" "$PWD/mnt"
```

Without `-config`, the built-in defaults are HTTP `127.0.0.1:8080`, authentication disabled, and an automatically selected peer port.

### HTTP API and Web UI only

Omitting `-mountpoint` starts the default headless mode:

```sh
./torrentfs "$PWD/torrents"
```

Open `http://127.0.0.1:8080/` or check the service:

```sh
curl --fail http://127.0.0.1:8080/
curl --fail http://127.0.0.1:8080/api/v1/torrents
```

By default this provides management only. A non-empty `strm.base_url` additionally enables signed HTTP content reads, independently of whether a FUSE view is mounted.

### HTTP API, Web UI, and FUSE

```sh
./torrentfs -mountpoint "$PWD/mnt" "$PWD/torrents"
```

Add a magnet or upload a `.torrent` through the Web UI or API, then read its files under `mnt`. Do not copy `.torrent` files into the management directory: they are not auto-imported.

### FUSE only

```sh
TORRENTFS_HTTP_LISTEN_ADDR= \
  ./torrentfs -mountpoint "$PWD/mnt" "$PWD/torrents"
```

This displays existing managed data without HTTP or a subtitle-upload entry point. To add tasks or replace managed subtitles, stop this process and run an instance with HTTP enabled. Only one process may manage a `torrents` directory at a time.

To use the checked-in example configuration:

```sh
./torrentfs -config ./torrentfs.example.toml "$PWD/torrents"
```

`go run ./cmd/torrentfs ...` can replace the binary command, but `web/dist` must already have been built.

### Optional STRM mount

Native deployments use `-strm-mountpoint`, alone or together with `-mountpoint`. Set a non-empty `strm.base_url` and keep the HTTP listener enabled. Both mount directories must already exist; after path normalization and symlink resolution, they must not be identical or nested.

```sh
mkdir -p "$PWD/mnt-strm"
TORRENTFS_STRM_BASE_URL=https://media.example/torrentfs/ \
  ./torrentfs -mountpoint "$PWD/mnt" -strm-mountpoint "$PWD/mnt-strm" "$PWD/torrents"
```

`media.example` is a placeholder domain. Before use, configure a player-reachable HTTPS reverse proxy that strips `/torrentfs` and forwards `/stream/...` to the default local listener. Omit `-mountpoint "$PWD/mnt"` for a STRM-only view. Binding HTTP directly to a non-loopback address still requires complete authentication. The base URL is not inferred from Host; see the [configuration](configuration.en.md) and [usage](usage.en.md) guides for path and player boundaries.

## CLI and lifecycle

The command accepts these flags and one positional argument:

```text
torrentfs [-config <file>] [-mountpoint <dir>] [-strm-mountpoint <dir>] <torrents-dir>
```

| Argument | Meaning |
| --- | --- |
| `-mountpoint <dir>` | Original-file FUSE mount; may coexist with STRM and is optional when HTTP is enabled |
| `-strm-mountpoint <dir>` | Optional second read-only STRM mount, usable alone; requires non-empty `strm.base_url` and HTTP |
| `-config <file>` | Optional TOML configuration file |
| `<torrents-dir>` | Existing, writable, non-symlink management directory |
| `-h` / `--help` | Print help and exit |

A single `.torrent` file cannot be used as the positional argument. The daemon does not create the management directory. When the HTTP listener is non-empty, HTTP/API and Web UI start even if a mountpoint is supplied.

| Exit code | Meaning |
| --- | --- |
| `0` | Normal shutdown after `SIGINT` or `SIGTERM` |
| `1` | HTTP, session, or FUSE-unmount runtime error |
| `2` | Invalid flags, positional argument, directory, or configuration |

Normal shutdown first cancels HTTP stream reads and allows at most five seconds for HTTP drain, then forcibly closes connections if needed to unblock network writes. It closes the Session next, cancelling and draining reads waiting for pieces, and finally unmounts the original and STRM FUSE mounts concurrently. The entire unmount stage shares one **30-second** deadline, not 30 seconds per mount. Force-closing HTTP connections does not mean every handler has returned. A propagated mount copy held in another namespace produces a diagnostic and a non-zero exit rather than being treated as a successful lazy unmount.

## Docker deployment

The [Dockerfile](../Dockerfile) builds the Web UI with Node 22.23.2, compiles a `CGO_ENABLED=0` Go 1.27 binary with the UI embedded, and runs it in Debian bookworm-slim with FUSE3, Samba, `tini`, certificates, and `passwd`. The host does not need Node.js or Go.

The nightly workflow publishes tags containing a UTC date, commit SHA, and run id. It does not promise floating `latest` or `nightly` tags. Use a locally built image unless a specific published tag has been verified:

```bash
git clone https://github.com/yakumioto/torrentfs-go.git
cd torrentfs-go
docker build -t torrentfs .
```

### Runtime identity and persistent directory

The entrypoint must start as root and then drops torrentfs and smbd to `PUID:PGID`. Do not use `docker run --user`.

| Variable | Default | Constraint |
| --- | --- | --- |
| `PUID` | `1000` when unset | Unsigned decimal integer in `1..4294967294` |
| `PGID` | `1000` when unset | Unsigned decimal integer in `1..4294967294` |

Explicit empty, negative, zero, non-numeric, or out-of-range values fail before services start. Identity is selected at runtime, not through build arguments. A NAS identity such as `99:100` can be supplied without rebuilding the image.

Choose a non-root identity and prepare a new persistent directory:

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

sudo install -d -o "$PUID" -g "$PGID" -m 0755 /srv/torrents
```

For an existing directory, inspect its data and permissions before changing ownership. The entrypoint never chowns a bind mount. The selected numeric identity must be able to read, write, and traverse `/srv/torrents`; metainfo, registry state, subtitles, and locks are persisted there. Do not mount `/torrents` read-only. Piece payloads are not written there.

Without a bind mount, the image creates `/torrents` with mode `0777` only to make the default container start. `docker exec torrentfs id` normally reports the exec shell's identity, not the daemon's; inspect `/proc/<torrentfs-pid>/status` when checking the actual service identity.

### Recommended: container-local FUSE and SMB

Use Linux rootful Docker with `/dev/fuse`, `SYS_ADMIN`, and `NET_BIND_SERVICE`. Host security policy must permit FUSE. The example disables the AppArmor profile; adjust this to the host's security policy where possible.

HTTP and SMB share a password. Web login needs only the password; SMB also requires a username that resolves to the container's runtime Unix account. `torrentfs` is the stable account name even when its numeric UID changes. The following prompt uses Bash syntax:

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

Do not pass `-mountpoint` or `-strm-mountpoint` in SMB mode: the entrypoint owns `/share` and optional `/share-strm`, starting Samba only after all required FUSE mounts are ready. Do not cover these internal paths with bind mounts. No host `/mnt`, `rshared`, or cross-container mount propagation is needed. `/torrents` and `.metadata` are never shared.

Open `http://127.0.0.1:8080/`, log in with the configured password, and add a magnet or `.torrent`. For a scripted login/add/status workflow, use the [API reference](api.en.md).

The share name is `torrentfs`. File managers use `smb://127.0.0.1/torrentfs`; Windows uses `\\host\torrentfs`. A client can verify reads with `smbclient` and a temporary credential file:

```bash
(
  umask 077
  printf 'username=%s\npassword=%s\n' "$TORRENTFS_USERNAME" "$TORRENTFS_PASSWORD" \
    > ./torrentfs.smb-credentials
)
smbclient //127.0.0.1/torrentfs \
  -A ./torrentfs.smb-credentials -m SMB3 -c 'ls'
```

Replace `payload.bin` with a real path from the listing, then read it and remove the credential file:

```bash
smbclient //127.0.0.1/torrentfs \
  -A ./torrentfs.smb-credentials -m SMB3 \
  -c 'get payload.bin ./payload.bin'
rm -f -- ./torrentfs.smb-credentials
```

Single-file torrents appear directly at the share root, multi-file torrents retain their directories, and categories add one directory level. The share is read-only: writes, deletion, and rename fail. A `ready` task has metainfo, but reads may still wait for peers.

### Optional second STRM share

Only the original `torrentfs` share is exported by default. Enabling STRM preserves `/share` → `torrentfs` and automatically adds `/share-strm` → `torrentfs-strm`, using the same authentication, read-only permissions, and runtime identity. No host mount propagation is needed.

| Switch | Responsibility |
| --- | --- |
| `TORRENTFS_STRM_ENABLED` | Docker entrypoint only; defaults to `false`, accepts only `true`, `false`, `1`, or `0`, and requires `TORRENTFS_SMB_ENABLED=true` or `1` when enabled |
| `TORRENTFS_STRM_BASE_URL` / `[strm] base_url` | Daemon link base and HTTP-stream switch; must be non-empty with HTTP enabled, independently of the entrypoint switch |

Merge this fragment into the existing service in the [Compose template](../docker-compose.yaml). `192.0.2.10` is an example only; replace it with the player's reachable LAN address. Replace loopback port bindings while retaining devices, capabilities, data mounts, runtime identity, shared credentials, and required peer TCP/UDP port mappings:

```yaml
ports:
  - "192.0.2.10:8080:8080"
  - "192.0.2.10:445:445"
environment:
  TORRENTFS_HTTP_LISTEN_ADDR: "0.0.0.0:8080"
  TORRENTFS_HTTP_AUTH_ENABLED: "true"
  TORRENTFS_SMB_ENABLED: "true"
  TORRENTFS_STRM_ENABLED: "true"
  TORRENTFS_STRM_BASE_URL: "http://192.0.2.10:8080"
```

With `docker run`, add `--env TORRENTFS_STRM_ENABLED=true` and `--env TORRENTFS_STRM_BASE_URL=http://192.0.2.10:8080` before the image name in the original Docker + SMB command and adjust port bindings accordingly. Keep the existing username/password variables. The second share needs no extra credentials, and HTTP playback needs no management Bearer token.

Point the client library at `smb://192.0.2.10/torrentfs-strm`; the original remains `smb://192.0.2.10/torrentfs`. Do not use container-local `localhost` as the base URL. For an HTTPS domain or reverse-proxy prefix, ensure player reachability and strip the prefix before forwarding `/stream/...`. A non-empty `base_url` with the entrypoint switch off enables HTTP streams but adds no SMB share. Enabling the entrypoint switch without the base URL, HTTP, or SMB fails startup. HTTP-only mode can serve existing valid signed links, but generating the file view still requires a mount.

The entrypoint waits for **both** mounts before starting Samba and stops services if either mount disappears; it never falls back to sharing an ordinary directory. Naming, ISO/BDMV, subtitle matching, and client-cache boundaries are in the [usage guide](usage.en.md). Protect signed `.strm` contents and proxy logs; use HTTPS outside a trusted LAN.

### Network and credential boundaries

The example publishes HTTP and SMB only on loopback. For trusted LAN access, bind an explicit LAN address and configure a firewall. HTTP has no TLS; put it behind a trusted TLS reverse proxy rather than exposing it directly to an untrusted network.

The image fixes the peer port at `6881`. To accept inbound peers, add both `--publish 6881:6881/tcp` and `--publish 6881:6881/udp` to the same run. Publishing only HTTP or only one peer protocol is not sufficient. If the TOML changes the peer port to `0`, a static `6881` mapping no longer describes the selected port.

Docker environment variables are visible to users with inspect permissions. Treat them as plaintext deployment configuration, not as a secret store, and do not commit real passwords to shell history or image layers.

SMB listens only on TCP 445; ports 137/138/139 and `nmbd` are not used. The share has guest access disabled and maps clients to the same non-root identity as FUSE, so `allow_other` is not required. Enabling it intentionally widens local read access.

### Stop and restart

```bash
docker logs -f torrentfs
docker stop --time 60 torrentfs
docker start torrentfs
```

The entrypoint gives Samba up to 10 seconds and torrentfs up to 45 seconds for normal shutdown. Keep the same `/srv/torrents` bind mount to restore tasks, categories, favorites, metainfo, managed subtitles, upload-rate settings, and the STRM signing key. Tokens, transfer counters, and the piece cache are runtime state and reset after restart. After a base-URL or key change, players may retain cached `.strm` links: read/export again and rescan or reopen rather than relying on mtime, which retains the torrent's added time. After testing, stop the container before removing it with `docker rm torrentfs`, and unset credential variables in the shell.

If torrentfs or smbd exits unexpectedly, or the FUSE mount disappears, the entrypoint stops the other service and exits non-zero. Normal signals stop Samba first, then follow the daemon's HTTP → session → FUSE shutdown sequence. Forced termination is logged and reported as failure.

### External TOML configuration

The default image command is:

```text
/usr/local/bin/torrentfs -config /etc/torrentfs/torrentfs.toml /torrents
```

To override the bundled TOML while keeping that command:

```bash
docker run --detach --name torrentfs-config \
  --env PUID --env PGID \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src="$PWD/torrentfs.toml",dst=/etc/torrentfs/torrentfs.toml,readonly \
  torrentfs
```

If the file is mounted elsewhere, supply both the config flag and required positional directory:

```bash
docker run --detach --name torrentfs-config \
  --env PUID --env PGID \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src="$PWD/torrentfs.toml",dst=/config.toml,readonly \
  torrentfs -config /config.toml /torrents
```

These examples demonstrate config mounting only. Add the device, capabilities, credentials, and port bindings for the chosen mode. The external TOML must still satisfy non-loopback authentication validation.

The image's [bundled configuration](../docker/torrentfs.toml) uses container-local HTTP `127.0.0.1:8080`, peer port `6881`, IPv6 disabled, automatic port forwarding disabled, a `2GiB` cache, and `allow_other = false`. Publishing `8080` alone does not change a loopback listener. Native defaults use peer port `0` and enable both address families.

### HTTP-only container

Reuse the prepared `PUID`, `PGID`, directory, and shared credentials:

```bash
docker run --detach --name torrentfs-http \
  --publish 127.0.0.1:8080:8080 \
  --env PUID --env PGID \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_USERNAME --env TORRENTFS_PASSWORD \
  torrentfs
```

No FUSE device or capabilities are needed. This provides management only by default. Separately setting a non-empty `strm.base_url` can serve existing valid signed HTTP links without generating a mounted view.

### Host-visible Docker FUSE mount

This optional mode requires recursive shared propagation for `/srv/mnt` and permission for FUSE and `SYS_ADMIN` operations. Prepare a new mount directory for the selected identity and inspect propagation:

```bash
sudo install -d -o "$PUID" -g "$PGID" -m 0755 /srv/mnt
findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
```

Mount existing managed tasks without publishing HTTP:

```bash
docker run --detach --name torrentfs-fuse \
  --env PUID --env PGID \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  --env TORRENTFS_HTTP_LISTEN_ADDR= \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  torrentfs -config /etc/torrentfs/torrentfs.toml -mountpoint /mnt /torrents
```

Both bind sources must be accessible to the runtime identity. `/mnt` must be a writable bind target so the daemon can create a submount, but the mounted FUSE tree remains read-only. `rshared` and rootful `SYS_ADMIN` widen the mount boundary and should not be offered to untrusted callers.

For a fresh directory, retain an authenticated HTTP endpoint to add tasks; the FUSE-only example is not a complete first-use workflow. Managed subtitles must still be uploaded through API/UI, not `docker cp`. Changing `allow_other` permits more readers but does not make the mount writable.

### SMB-only access

Omitting the HTTP port binding leaves the image's HTTP listener accessible only inside the container; it does not disable HTTP. SMB still requires the shared username/password pair. To disable HTTP entirely, set `TORRENTFS_HTTP_LISTEN_ADDR=`; no HTTP management entry point remains, and STRM cannot be used (clear `base_url` and turn off the entrypoint STRM switch). The Go configuration layer ignores the shared pair when HTTP authentication is disabled.

The server's internal `/share`, the SMB endpoint `//SERVER/torrentfs`, and a client's local mountpoint are different namespaces. A client may choose `/mnt/torrentfs-client`:

```sh
mkdir -p /mnt/torrentfs-client
mount.cifs //SERVER/torrentfs /mnt/torrentfs-client \
  -o credentials=/path/to/credentials,vers=3.0,ro
ls -l /mnt/torrentfs-client/payload.bin
umount /mnt/torrentfs-client
```

Do not use `/` as the CIFS mountpoint or Samba share path. The server owns `/share`; do not also use it as a client mountpoint or a non-empty bind target.

## Deployment troubleshooting

| Symptom | Likely cause and action |
| --- | --- |
| Runtime identity error | Check non-zero `PUID`/`PGID` and read/write/traverse access to the bind-mounted `/torrents`. |
| HTTP unreachable after `-p 8080:8080` | Set a non-loopback container listener and enable authentication; port publishing does not change the daemon's bind address. |
| SMB refuses to start | Check `/dev/fuse`, `SYS_ADMIN`, `NET_BIND_SERVICE`, non-empty single-line credentials, and that the username resolves to the runtime UID. |
| External TOML causes a CLI error | A non-default mount path requires both `-config /config.toml` and `/torrents`; non-loopback authentication is still required. |
| Port 445, 8080, or 6881 already in use | Select an available host binding where the client supports it. Publish peer TCP and UDP together; SMB clients may require the standard port. |
| FUSE mount does not reach the host | Check the host propagation prerequisites; container-local SMB avoids this requirement. |

See the [usage guide](usage.en.md) for `adding`, delayed reads, and subtitle troubleshooting, and the [development guide](development.en.md) for HTTP, configuration, FUSE, and SMB smoke checks.
