# torrentfs

[简体中文](README.md)

`torrentfs` is a read-only BitTorrent filesystem written in Go. It fetches content on demand, exposes a FUSE file tree, and provides a Web UI/API for task management.

Use it to let players, media libraries, indexers, or command-line tools read torrents like local files, or to provide a read-only SMB share through Docker.

## Features

- **On-demand reads:** a bounded in-memory cache, without downloading complete torrent payloads to disk.
- **Read-only filesystem:** single files appear directly; multi-file torrents retain their directory structure.
- **Web UI/API management:** add magnets or upload `.torrent` files and inspect tasks and cache state.
- **Task and media management:** categories, favorites, age-based cleanup, subtitle upload/replacement, and scheduled upload limits.
- **Docker + SMB:** FUSE and a read-only share in one container, without host mount propagation.

## Quick start

Recommended path: **Docker → add a task in the Web UI → read over SMB**. Build locally below rather than relying on an unverified image tag.

Requirements:

- Linux with rootful Docker, an available `/dev/fuse`, and a host security policy that permits FUSE.
- Permission to grant `SYS_ADMIN` and `NET_BIND_SERVICE`; host ports `8080` and `445` must be available.
- Bash; run the commands in order in the same terminal. Go and Node.js are not required on the host.

The default cache limit is `2GiB`; adjust it for smaller hosts using the [configuration reference](docs/configuration.en.md). Other run modes are in the [deployment guide](docs/deployment.en.md).

### 1. Build the image

```bash
git clone https://github.com/yakumioto/torrentfs-go.git
cd torrentfs-go
docker build -t torrentfs .
```

### 2. Prepare the directory and credentials

The entrypoint must start as root, but the services run as a non-root `PUID:PGID`. Do not pass `--user`. A non-root host user can reuse their identity; a root user needs a dedicated non-zero identity:

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

This is for a new directory; inspect ownership and permissions before using existing data. The entrypoint does not chown bind mounts, and the selected identity must be able to read, write, and traverse the directory.

HTTP and SMB share a password. Web login needs only that password; SMB also uses the container's runtime account name `torrentfs`:

```bash
export TORRENTFS_USERNAME=torrentfs
read -r -s -p 'HTTP/SMB password: ' TORRENTFS_PASSWORD
printf '\n'
export TORRENTFS_PASSWORD
```

The password is passed through the environment and may be visible to users with `docker inspect` access. Do not put real credentials in command arguments, the repository, or image layers.

### 3. Start the service

```bash
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

This example publishes HTTP/SMB only on loopback and relaxes AppArmor to permit FUSE; use it only on a trusted host. If the host policy already permits FUSE, adjust the security options as described in the [deployment guide](docs/deployment.en.md).

SMB mode owns the container-local `/share` mount: do not add `-mountpoint`, and do not bind `/share` to the host. For LAN access or inbound peers, follow the deployment guide's address, firewall, and TCP/UDP settings.

### 4. Add a task and read

1. Open `http://127.0.0.1:8080/` and log in using only the password you just set.
2. Upload your `.torrent` file in the Web UI or add a magnet with available sources.
3. Open `smb://127.0.0.1/torrentfs` in an SMB-capable file manager or player, using username `torrentfs` and the same password. The Windows path is `\\127.0.0.1\torrentfs`.

Alternatively, use interactive `smbclient`; it will prompt for the password:

```sh
smbclient //127.0.0.1/torrentfs -U torrentfs -m SMB3
```

Run `ls` after logging in, then `get <actual-file-path> <local-destination>` to verify a read. Single-file torrents normally appear at the share root; multi-file torrents retain their directory tree.

A magnet may start in `adding`. `ready` means metainfo is available, but reads may still wait for peers. See the [deployment guide](docs/deployment.en.md) for logs, stopping, and restarting.

## Before you use it

- **FUSE and SMB are read-only:** upload subtitles through the UI/API, not with `docker cp`.
- **Content cache is not persistent:** restarting clears the piece cache, but management state is restored. Metainfo is persisted at `torrents-dir/<infohash>.torrent`; other state lives under `torrents-dir/.metadata`.
- **Add tasks through the UI/API:** copying `.torrent` files into the directory does not import them.
- **HTTP is a management interface:** it has no torrent content download/playback endpoint, and this is not a downloader that persists complete payloads.
- **Respect the network boundary:** non-loopback HTTP listeners require authentication; use a TLS reverse proxy and restrict access for exposed deployments.

## Documentation

| Guide | Contents |
| --- | --- |
| [Deployment](docs/deployment.en.md) | Native run modes, Docker, FUSE/SMB, identity, networking, and deployment troubleshooting |
| [Configuration](docs/configuration.en.md) | TOML, environment variables, defaults, units, and migration |
| [Usage](docs/usage.en.md) | File layout, categories, favorites, subtitles, upload limits, state, and common questions |
| [API reference](docs/api.en.md) | Routes, authentication, requests/responses, curl examples, and error codes |
| [Development and contribution](docs/development.en.md) | Source builds, architecture, tests, and contribution workflow |

See [torrentfs.example.toml](torrentfs.example.toml) for a complete configuration example.

## Contributing and license

Issues and contributions are welcome. See the [development guide](docs/development.en.md) for building and validation.

Mozilla Public License 2.0; see [LICENSE](LICENSE).
