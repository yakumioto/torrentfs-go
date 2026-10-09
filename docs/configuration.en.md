# Configuration guide

[简体中文](configuration.md) · [README](../README.en.md)

Commands run from the repository root. Start with [torrentfs.example.toml](../torrentfs.example.toml); the Docker image uses [docker/torrentfs.toml](../docker/torrentfs.toml).

## Loading and precedence

```sh
cp torrentfs.example.toml torrentfs.local.toml
./torrentfs -config ./torrentfs.local.toml "$PWD/torrents"
```

Configuration is merged by field:

```text
environment variables > TOML file > built-in defaults
```

The TOML decoder rejects unknown fields. There is no automatic environment variable for every TOML key; only the bindings listed below are read, and unlisted variables are ignored. Static TOML and environment variables are read at startup and are not hot-reloaded. [Upload-rate settings](usage.en.md) are managed separately through API/UI and take effect immediately.

## Byte quantities and durations

`cache.capacity` and `http.max_upload_size` require an integer with an explicit byte unit in TOML and their corresponding environment variables.

| Units | Multiplier |
| --- | --- |
| `B`, `KB`, `MB`, `GB`, `TB` | Decimal powers of 1000 |
| `KiB`, `MiB`, `GiB`, `TiB` | Binary powers of 1024 |

Examples: `32MB = 32000000` bytes, `8GB = 8000000000` bytes, and `2GiB = 2147483648` bytes. Units are case-sensitive. Bare numbers, fractions, bit units, scientific notation, and values that cannot be represented exactly by the runtime integer are rejected.

`http.auth.token_ttl` and `mount.read_timeout` use a positive Go duration such as `30m`, `1h30m`, or `250ms`; `token_ttl` is additionally capped at 24 hours. Valid authenticated requests slide the token inactivity window.

## Main TOML sections

| Section | Common keys | Notes |
| --- | --- | --- |
| `[http]` | `listen_addr`, `max_upload_size` | HTTP/UI listener; default `127.0.0.1:8080`, upload default `10MiB` |
| `[http.auth]` | `enabled`, `username`, `password_hash`, `password_hash_file`, `token_ttl` | Single-user bcrypt authentication and in-memory Bearer tokens |
| `[connections]` | `listen_host`, `listen_port`, `disable_ipv4`, `disable_ipv6`, `no_port_forwarding`, `bootstrap_nodes` | Peer listener, address families, NAT mapping, and DHT bootstrap |
| `[proxy]` | `socks5_url` | Optional `socks5://` or `socks5h://` proxy |
| `[cache]` | `capacity` | In-memory piece-cache limit; default `2GiB` |
| `[identity]` | `tracker_user_agent`, `peer_id_prefix`, `extended_handshake_client_version` | Tracker and peer identity values |
| `[log]` | `level`, `format`, `add_source` | `debug`/`info`/`warn`/`error`, `text`/`json`, and source locations |
| `[mount]` | `allow_other`, `read_timeout` | Whether other local UIDs may read the FUSE mount; upper bound on one foreground content read, default `30s` |

Native defaults use peer port `0`, both address families enabled, and automatic port forwarding disabled (`no_port_forwarding = true`). The Docker TOML instead fixes the peer port at `6881` and disables IPv6. Do not infer one set of defaults from the other.

## HTTP authentication and network boundary

An empty `http.listen_addr` disables HTTP and the Web UI. A non-empty listener must be a valid `host:port`; a non-loopback address requires complete authentication. HTTP has no TLS and should be placed behind a trusted TLS reverse proxy when exposed beyond the local machine.

A TOML configuration uses exactly one bcrypt password source, never a plaintext password:

```toml
[http]
listen_addr = "0.0.0.0:8080"

[http.auth]
enabled = true
username = "alice"
password_hash = "<bcrypt-hash>"
password_hash_file = ""
token_ttl = "30m"
```

`password_hash_file` must be a non-empty regular, non-symlink file readable only by its owner, no larger than 1024 bytes. The service validates the bcrypt hash and cost. When authentication is disabled, TOML username and both password-source fields must be empty; disabling auth does not automatically clear credentials already present in the file.

The special `TORRENTFS_USERNAME` / `TORRENTFS_PASSWORD` pair is an alternative when HTTP authentication is enabled:

- Both variables must be present together and non-empty.
- The password must be at most 72 UTF-8 bytes; it is not truncated.
- Startup generates a cost-10 bcrypt hash and atomically overrides the TOML username and hash source; the resulting configuration does not retain the plaintext password.
- When HTTP authentication is disabled, the Go configuration layer ignores the pair so SMB-only deployments can use it.
- SMB adds a runtime Unix-account requirement; see the [deployment guide](deployment.en.md).

Environment credentials can appear in process environments and Docker metadata. They are plaintext deployment configuration, not a secret store. Token and header semantics are documented in the [API reference](api.en.md).

## Supported environment variables

Ordinary field bindings are explicit:

| Environment variable | TOML key | Format |
| --- | --- | --- |
| `TORRENTFS_CONNECTIONS_LISTEN_HOST` | `connections.listen_host` | String |
| `TORRENTFS_CONNECTIONS_LISTEN_PORT` | `connections.listen_port` | Decimal integer |
| `TORRENTFS_CONNECTIONS_DISABLE_IPV4` | `connections.disable_ipv4` | Go boolean |
| `TORRENTFS_CONNECTIONS_DISABLE_IPV6` | `connections.disable_ipv6` | Go boolean |
| `TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING` | `connections.no_port_forwarding` | Go boolean |
| `TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES` | `connections.bootstrap_nodes` | Comma-separated `host:port` list |
| `TORRENTFS_MOUNT_ALLOW_OTHER` | `mount.allow_other` | Go boolean |
| `TORRENTFS_MOUNT_READ_TIMEOUT` | `mount.read_timeout` | Go duration, e.g. `30s` |
| `TORRENTFS_PROXY_SOCKS5_URL` | `proxy.socks5_url` | String |
| `TORRENTFS_CACHE_CAPACITY` | `cache.capacity` | Explicit byte unit, e.g. `1GiB` or `32MB` |
| `TORRENTFS_IDENTITY_TRACKER_USER_AGENT` | `identity.tracker_user_agent` | String |
| `TORRENTFS_IDENTITY_PEER_ID_PREFIX` | `identity.peer_id_prefix` | String |
| `TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION` | `identity.extended_handshake_client_version` | String |
| `TORRENTFS_HTTP_LISTEN_ADDR` | `http.listen_addr` | String; empty disables HTTP |
| `TORRENTFS_HTTP_MAX_UPLOAD_SIZE` | `http.max_upload_size` | Explicit byte unit, e.g. `10MiB` |
| `TORRENTFS_HTTP_AUTH_ENABLED` | `http.auth.enabled` | Go boolean |
| `TORRENTFS_HTTP_AUTH_TOKEN_TTL` | `http.auth.token_ttl` | Go duration |
| `TORRENTFS_LOG_LEVEL` | `log.level` | `debug`, `info`, `warn`, or `error` |
| `TORRENTFS_LOG_FORMAT` | `log.format` | `text` or `json` |
| `TORRENTFS_LOG_ADD_SOURCE` | `log.add_source` | Go boolean |

`TORRENTFS_USERNAME` and `TORRENTFS_PASSWORD` are the special shared credential pair, not automatic bindings for arbitrary TOML keys. `TORRENTFS_SMB_ENABLED`, `PUID`, and `PGID` are handled by the Docker entrypoint, not the Go configuration loader.

An existing ordinary string variable may be empty to clear a field. Empty numeric, boolean, or duration variables produce an error. For example:

```sh
TORRENTFS_HTTP_LISTEN_ADDR=127.0.0.1:8080 \
TORRENTFS_HTTP_MAX_UPLOAD_SIZE=10MiB \
TORRENTFS_CACHE_CAPACITY=1GiB \
./torrentfs "$PWD/torrents"
```

## Validation and sizing

- Peer ports must be in `0..65535`; `0` selects an available port. Incoming peers require a fixed port published for both TCP and UDP.
- IPv4 and IPv6 cannot both be disabled. Bootstrap entries need a host and a port in `1..65535`.
- Cache capacity must be positive and within the watermark arithmetic safety limit. It is a cap, not a reservation. A torrent with a piece larger than the cache capacity is rejected.
- Leave memory for protocol buffers, staging, the rest of the daemon, and Samba when sizing a container; cache capacity is not the process RSS limit.
- A proxy may be empty or use `socks5://` / `socks5h://` with a valid host; any explicit port must be in `1..65535`. Validation errors redact proxy credentials.
- `identity.peer_id_prefix` is at most 20 bytes, and the tracker User-Agent cannot contain CR/LF.
- `http.max_upload_size` must be positive and caps both `.torrent` and subtitle uploads.
- `mount.allow_other` defaults to false. Enabling it permits other local UIDs to read the mount; non-root mounts also require `user_allow_other` in `/etc/fuse.conf`. It never makes the mount writable.
- `mount.read_timeout` must be a positive Go duration. It bounds a **single** foreground content read, not a whole playback session or file handle: when no verified piece arrives in time, only that read ends with a read error after the deadline (the client may report a timeout or an I/O error), without deleting the torrent or cancelling other reads. Raise it for slow swarms or large pieces.

## Migrating old configuration

Replace `cache.capacity_bytes` / `http.max_upload_bytes` with `cache.capacity` / `http.max_upload_size`, and `TORRENTFS_CACHE_CAPACITY_BYTES` / `TORRENTFS_HTTP_MAX_UPLOAD_BYTES` with `TORRENTFS_CACHE_CAPACITY` / `TORRENTFS_HTTP_MAX_UPLOAD_SIZE`. Old TOML keys are rejected and old environment variables are no longer read; SI/IEC value syntax is unchanged. Old bare-number quantities must be rewritten with units before startup.

The old `[paths]` section is unsupported. `TORRENTFS_PATHS_DATA_DIR` is ignored and does not restore a disk payload path. `TORRENTFS_FUSE_REQUIRED` is a test gate, not daemon configuration.
