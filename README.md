# torrentfs

[![CI](https://github.com/yakumioto/torrentfs-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/yakumioto/torrentfs-go/actions/workflows/ci.yml)
[![License: MPL 2.0](https://img.shields.io/badge/license-MPL--2.0-blue.svg)](LICENSE)

`torrentfs` 是一个用 Go 编写的**只读 BitTorrent 文件系统**：按需获取种子内容并缓存在内存中，把种子虚拟成一棵只读文件树，通过 **FUSE 挂载**、**内置只读 SMB 共享**或**签名 HTTP 直链（STRM）**对外提供；任务由内嵌的 Web UI / REST API 管理。

适合播放器、媒体库、索引器或命令行工具像读本地文件一样读取种子内容，也适合通过 Docker 在 NAS 或普通 Linux 主机上提供只读 SMB 共享。

> 它不是下载器：不会把完整内容落盘，内存缓存重启即清空；任务、分类、字幕等管理状态会持久化。

## 核心特性

- **按需读取**：读取到哪个 piece 才拉取哪个 piece，缓存是有硬上限的内存 LRU（默认 `2GiB`）。
- **只读文件树**：单文件种子直接呈现为文件，多文件种子保留原目录结构。
- **三种输出**：FUSE 挂载、原生 SMB 共享，或只保留 HTTP（`none`）。
- **原生 SMB**：不依赖 FUSE、Samba、`/dev/fuse` 或宿主挂载传播。
- **Web UI / REST API**：添加磁力或 `.torrent`，管理任务、分类、收藏、批量清理、字幕和上传限速。
- **可选 STRM**：在原文件共享之外生成 `.strm` 文件，内容是一行签名 HTTP 直链，支持 Range / HEAD。
- **单二进制**：Web UI 内嵌；Docker 镜像以非 root（`1000:1000`）运行。

## 快速开始

推荐路径：**Docker → Web UI 添加任务 → SMB 读取**。SMB 模式不需要 `/dev/fuse`、额外 capability 或放宽 AppArmor。

### 1. 构建镜像

```bash
git clone https://github.com/yakumioto/torrentfs-go.git
cd torrentfs-go
docker build -t torrentfs .
```

### 2. 准备目录和凭据

镜像默认以 `1000:1000` 运行。推荐显式指定非 root 的 `--user "$PUID:$PGID"`（`PUID/PGID` 若提供必须与它一致），并准备一个运行身份可读写的 torrents 目录：

```bash
HOST_UID="$(id -u)"; HOST_GID="$(id -g)"
if [[ "$HOST_UID" == 0 || "$HOST_GID" == 0 ]]; then PUID=1500; PGID=1500; else PUID="$HOST_UID"; PGID="$HOST_GID"; fi
export PUID PGID
sudo install -d -o "$PUID" -g "$PGID" -m 0755 /srv/torrents

export TORRENTFS_USERNAME=torrentfs
read -r -s -p 'HTTP/SMB password: ' TORRENTFS_PASSWORD; printf '\n'
export TORRENTFS_PASSWORD
```

### 3. 启动

```bash
docker run --detach --name torrentfs \
  --publish 127.0.0.1:8080:8080 \
  --publish 127.0.0.1:445:1445 \
  --publish 6881:6881/tcp \
  --publish 6881:6881/udp \
  --user "$PUID:$PGID" \
  --env PUID --env PGID \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_BACKEND=smb \
  --env TORRENTFS_USERNAME \
  --env TORRENTFS_PASSWORD \
  torrentfs
```

HTTP/SMB 只向本机发布，peer 端口发布 TCP/UDP 以接受入站连接。LAN 播放请改为绑定可信 LAN 地址并配置防火墙。也可使用仓库里的 [Compose 模板](docker-compose.yaml)，按注释填写镜像 tag 和密码后运行 `docker compose up -d`。

镜像内 `/etc/torrentfs/torrentfs.toml` 及其目录归运行用户（`1000:1000`）所有，因此配置页面可以把改动直接写回该文件；写回会在同目录创建临时文件并保留一份 `.bak`。Compose 模板用命名卷 `torrentfs-config` 持久化这个目录，容器重建后改动仍在，执行 `docker compose down -v` 可删除卷、回到镜像默认配置。若以自定义 TOML 启动，请把运行用户可写的文件或目录挂载到 `/etc/torrentfs`，否则配置页面会降级为只读并显示原因。

### 4. 添加任务并读取

1. 打开 `http://127.0.0.1:8080/`，输入密码登录。
2. 在 Web UI 上传 `.torrent`，或添加有可用来源的磁力链接。
3. 用支持 SMB 的文件管理器或播放器打开 `smb://127.0.0.1/torrentfs`，用户名 `torrentfs`、密码同上；Windows 路径为 `\\127.0.0.1\torrentfs`。

```sh
smbclient //127.0.0.1/torrentfs -U torrentfs -m SMB3
```

磁力链接可能先处于 `adding`；`ready` 只表示元信息可用、能建立文件视图，读取仍可能等待 peers。

### 原生运行

需要 Go 1.27+ 与 `web/.nvmrc` 指定的 Node.js：

```bash
./scripts/build-web.sh
go build -o ./torrentfs ./cmd/torrentfs
mkdir -p ./torrents
./torrentfs ./torrents
```

`<torrents-dir>` 是唯一必填的位置参数，必须已存在且不是符号链接；省略配置时使用默认值（HTTP `127.0.0.1:8080`、认证关闭、`backend=none`）。需要自定义时，复制 `torrentfs.example.toml` 为 `torrentfs.toml` 后用 `-config` 指定。

## 访问方式

| `backend` | 行为 | 依赖 |
| --- | --- | --- |
| `none`（默认） | 不输出文件树，只保留 Web UI / HTTP API | 无 |
| `smb` | 内置只读 SMB，导出 `torrentfs` 共享 | 无 FUSE / Samba |
| `fuse` | 创建只读 FUSE 挂载 | Linux FUSE3、`/dev/fuse`、允许挂载的主机策略 |

```toml
backend = "smb"   # fuse / smb / none

# 一套凭据服务 SMB 与 HTTP
username = "torrentfs"
password = ""      # 优先用 TORRENTFS_PASSWORD 传入

# 选择 fuse 时配置挂载点
[fuse]
# mountpoint = "/srv/mnt"

[smb]
listen_addr = "0.0.0.0:1445"
```

不写 `backend` 就是 `none`：配置了挂载点也不会自动切换到 FUSE，要用 FUSE 必须显式写 `backend = "fuse"`。

**STRM（可选）**：设置播放器可达的 `strm.base_url` 后，SMB 会额外提供 `torrentfs-strm` 共享（FUSE 用 `fuse.strm_mountpoint`），视频会生成一行签名 URL 的 `.strm` 文件：

```toml
[strm]
base_url = "http://192.168.1.10:8080"
```

`.strm` 链接是只读、文件级签名凭据，不需要 Bearer token；常见视频扩展名会转为同名 `.strm`，ISO 转为 `.iso.strm`；基址必须从播放器可达，不要填容器内部的 `localhost`。

SMB 的 STRM 共享由 `smb.strm` 三态控制，默认 `auto`（配了 `strm.base_url` 就提供）；`on` 强制提供（必须有 `strm.base_url`），`off` 强制不提供：

```toml
[smb]
strm = "off"
```

## 配置

- 配置优先级：**环境变量 > TOML 文件 > 内置默认值**；启动时读取，修改后需重启。
- 完整配置项与注释见 [torrentfs.example.toml](torrentfs.example.toml)；容器默认配置见 [docker/torrentfs.toml](docker/torrentfs.toml)。
- 关键默认值：HTTP `127.0.0.1:8080`（认证默认关闭）、`backend=none`、SMB `0.0.0.0:1445`、cache `2GiB`、单次读取超时 `30s`。
- 每个配置项都有对应的环境变量，由配置项点路径机械派生：点路径转大写、`.` 换成 `_`、加 `TORRENTFS_` 前缀。例如 `http.auth.token_ttl` → `TORRENTFS_HTTP_AUTH_TOKEN_TTL`，`cache.capacity` → `TORRENTFS_CACHE_CAPACITY`。没有例外。
- **配置文件入口**：`TORRENTFS_CONFIG` 指定配置文件路径，`-config` 参数优先于它。**不做隐式搜索路径**：不指定就是不带配置文件启动，不会去猜某个路径。
- **配置页面写回需要可写配置文件**：进程必须对配置文件及其所在目录都有写权限，原子写回会在同目录创建临时文件并保留 `.bak`。镜像默认把 `/etc/torrentfs` 交给运行用户，Compose 模板用命名卷持久化；只读挂载、只读根文件系统，或卷属主与自定义 `PUID/PGID` 不一致时，页面降级为只读并显示原因（`file_not_writable`），保存不会写入，进程继续使用当前配置。环境变量覆盖项与口令 `password` 同样始终只读。
- **未知变量会报错**：`TORRENTFS_` 前缀下拼错的名字在启动时会被点名，不会被静默忽略后按默认值启动。`TF_` 前缀是给测试开关、CI 冒烟与 Compose 插值用的，不属于配置命名空间。
- **凭据**：顶层 `username` / `password` 一对凭据，SMB 登录与 HTTP 登录共用同一个口令。`username` 仅 SMB 登录使用，**Web 登录只校验密码**，不需要用户名。`password` 在 `backend = "smb"` 或 `http.auth.enabled = true` 时必填，`username` 仅在 `backend = "smb"` 时必填。优先用 `TORRENTFS_USERNAME` / `TORRENTFS_PASSWORD` 传入；在 TOML 里写明文口令会在启动时打一条不含口令值的告警。
- 绑定非 loopback 地址时必须启用认证；Bearer token 只存在内存中，重启失效。
- 上传限速在 Web UI 中设置并立即生效，不通过 TOML 或环境变量配置。

### 配置项速查

下表由配置 schema 生成，与 [torrentfs.example.toml](torrentfs.example.toml) 同源；各配置项的完整注释见该文件。

<!-- BEGIN generated:readme-reference -->
| 配置项 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `backend` | `TORRENTFS_BACKEND` | `none` | Backend selects the output backend: none, fuse, or smb. |
| `username` | `TORRENTFS_USERNAME` | `` | Username is the login name the SMB output accepts. The HTTP login does not check a user name, so the key is only required for the SMB output. |
| `password` | `TORRENTFS_PASSWORD` | `` | Password is the plaintext password shared by the SMB output and the HTTP login. It is required for the SMB output and for enabled HTTP authentication, and it has no default. |
| `connections.listen_host` | `TORRENTFS_CONNECTIONS_LISTEN_HOST` | `` | ListenHost is the address the session listens on for peer connections. Empty lets the client pick its default. |
| `connections.listen_port` | `TORRENTFS_CONNECTIONS_LISTEN_PORT` | `0` | ListenPort is the TCP/UDP port for incoming peer connections. 0 selects a random free port. |
| `connections.reading_conns` | `TORRENTFS_CONNECTIONS_READING_CONNS` | `50` | ReadingConns is the peer connection allowance for a torrent that is being read: a file handle is open, or one was open within ReadGrace. A torrent whose whole content is already in the piece cache does not use it, because playback is served from the cache. It must be at least 1. |
| `connections.uploading_conns` | `TORRENTFS_CONNECTIONS_UPLOADING_CONNS` | `10` | UploadingConns is the peer connection allowance while a torrent is uploading but nobody is reading it. There is no session-wide budget; every torrent's allowance is decided independently. A value of 0 disables the uploading allowance, leaving a cached torrent its probe connection. |
| `connections.read_grace` | `TORRENTFS_CONNECTIONS_READ_GRACE` | `2m0s` | ReadGrace is how long a torrent keeps its reading allowance after its last file handle closes. It covers a seek, a pause or a player reconnect without dropping the peer connections. It must be positive. |
| `connections.disable_ipv4` | `TORRENTFS_CONNECTIONS_DISABLE_IPV4` | `false` | DisableIPv4 turns off every IPv4 listener, dialer, and DHT server. |
| `connections.disable_ipv6` | `TORRENTFS_CONNECTIONS_DISABLE_IPV6` | `false` | DisableIPv6 turns off every IPv6 listener, dialer, and DHT server. Only one family may be disabled: disabling both leaves no transport. |
| `connections.no_port_forwarding` | `TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING` | `true` | NoPortForwarding disables UPnP/NAT-PMP port mapping. It defaults to true so a deployment with no UPnP device behaves the same as one that has a device but no mapping rule. |
| `connections.bootstrap_nodes` | `TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES` | `[]` | BootstrapNodes overrides the default DHT bootstrap hosts with explicit host:port entries. Entries are resolved per address family and filtered to the family of the socket that asks for them. Empty uses the client's built-in bootstrap list. |
| `proxy.socks5_url` | `TORRENTFS_PROXY_SOCKS5_URL` | `` | Socks5URL is an optional SOCKS5 or SOCKS5H endpoint. |
| `cache.capacity` | `TORRENTFS_CACHE_CAPACITY` | `2GiB` | CapacityBytes is the maximum number of bytes retained in memory. |
| `identity.tracker_user_agent` | `TORRENTFS_IDENTITY_TRACKER_USER_AGENT` | `qBittorrent/4.4.0` | TrackerUserAgent is sent as the User-Agent header on HTTP tracker announces. |
| `identity.peer_id_prefix` | `TORRENTFS_IDENTITY_PEER_ID_PREFIX` | `-qB4400-` | PeerIDPrefix is the prefix of the generated 20-byte BitTorrent peer ID. |
| `identity.extended_handshake_client_version` | `TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION` | `qBittorrent/4.4.0` | ExtendedHandshakeClientVersion is sent as the BEP 10 extended handshake v field. |
| `http.listen_addr` | `TORRENTFS_HTTP_LISTEN_ADDR` | `127.0.0.1:8080` | ListenAddr is the address the HTTP service binds. Empty disables the service. The default binds loopback only. |
| `http.auth.enabled` | `TORRENTFS_HTTP_AUTH_ENABLED` | `false` | Enabled turns on the HTTP login. It is required for a listener that is not bound to the loopback address. |
| `http.auth.token_ttl` | `TORRENTFS_HTTP_AUTH_TOKEN_TTL` | `30m0s` | TokenTTL is the inactivity window of a login token. A valid request slides the window; the tokens live in memory and die with the process. |
| `http.max_upload_size` | `TORRENTFS_HTTP_MAX_UPLOAD_SIZE` | `10MiB` | MaxUploadBytes caps any uploaded request body: a .torrent upload or a managed subtitle file. |
| `strm.base_url` | `TORRENTFS_STRM_BASE_URL` | `` | BaseURL is the player-reachable HTTP(S) URL, optionally with a reverse proxy path prefix. Empty disables signed streaming links. |
| `log.level` | `TORRENTFS_LOG_LEVEL` | `info` | Level is the minimum level of a log record: debug, info, warn, or error. Debug is disabled by default. |
| `log.format` | `TORRENTFS_LOG_FORMAT` | `text` | Format is the shape of a log record: text for humans or json for log collectors. |
| `log.add_source` | `TORRENTFS_LOG_ADD_SOURCE` | `false` | AddSource adds the source file and line to every log record. |
| `filesystem.read_timeout` | `TORRENTFS_FILESYSTEM_READ_TIMEOUT` | `30s` | ReadTimeout bounds a single read of a piece from the torrent. |
| `fuse.mountpoint` | `TORRENTFS_FUSE_MOUNTPOINT` | `` | Mountpoint is the directory the file tree is mounted on. |
| `fuse.strm_mountpoint` | `TORRENTFS_FUSE_STRM_MOUNTPOINT` | `` | STRMMountpoint is the directory the signed-streaming view is mounted on. |
| `fuse.allow_other` | `TORRENTFS_FUSE_ALLOW_OTHER` | `false` | AllowOther passes allow_other to the kernel, lifting FUSE's default restriction that only the mounting user may access the mount. |
| `smb.listen_addr` | `TORRENTFS_SMB_LISTEN_ADDR` | `0.0.0.0:1445` | ListenAddr is the address the SMB service binds. |
| `smb.strm` | `TORRENTFS_SMB_STRM` | `auto` | STRM switches the STRM share of the SMB output: auto, on, or off. |
| `experiments.upload.mode` | `TORRENTFS_EXPERIMENTS_UPLOAD_MODE` | `baseline` | Mode selects the reporting model: baseline, inject, or amplify. It defaults to baseline, the control arm that reports the real counters. There is no inference: a rate or multiplier needs its mode or the experiment refuses to start. |
| `experiments.upload.rate` | `TORRENTFS_EXPERIMENTS_UPLOAD_RATE` | `` | Rate is the average synthetic growth of the reported upload in inject mode. |
| `experiments.upload.multiplier` | `TORRENTFS_EXPERIMENTS_UPLOAD_MULTIPLIER` | `0` | Multiplier grows the reported upload by this factor of the real payload bytes in amplify mode. |
| `experiments.upload.jitter` | `TORRENTFS_EXPERIMENTS_UPLOAD_JITTER` | `20` | Jitter is the per-announce deviation from the nominal increment, in percent. It defaults to 20 percent; zero disables the deviation. |
| `experiments.upload.cap` | `TORRENTFS_EXPERIMENTS_UPLOAD_CAP` | `0B` | Cap is the per-torrent ceiling on the cumulative reported upload. |
| `experiments.upload.seed` | `TORRENTFS_EXPERIMENTS_UPLOAD_SEED` | `0` | Seed fixes the random jitter sequence for reproducible runs. Zero lets the process choose one. |
| `experiments.upload.announce_log` | `TORRENTFS_EXPERIMENTS_UPLOAD_ANNOUNCE_LOG` | `` | AnnounceLog is the JSON Lines file that receives one record per tracker announce. Empty disables it. |
| `experiments.upload.peer_sample_interval` | `TORRENTFS_EXPERIMENTS_UPLOAD_PEER_SAMPLE_INTERVAL` | `0s` | PeerSampleInterval is how often peer activity is sampled into the announce log. |
<!-- END generated:readme-reference -->

### 检查与查看配置

两个子命令只处理配置，不需要 torrents 目录：

```sh
# 部署前校验：配置正确时静默退出 0，有问题时打印全部错误并退出 2
torrentfs config check --config ./torrentfs.toml

# 查看生效配置；口令一律显示为 <redacted>
torrentfs config print --config ./torrentfs.toml
torrentfs config print --format json --sources   # 附带每个值的配置来源
```

`--sources` 会标出每个值来自**内置默认值**、**配置文件**还是**环境变量**，用来排查"为什么没生效"。打印结果是**报告**，不是配置文件——脱敏后的占位符无法回灌。

`config print` 把报告写到标准输出，`config check` 的诊断写到标准错误，所以 `torrentfs config print --format json | jq` 可以直接管道处理。

正常启动时日志里也会记录一份脱敏的生效配置摘要（`effective configuration`）。


### 迁移对照表

已删除的名称在启动时会报错并给出替代写法，不会被静默忽略。

本表由程序内的删除表生成（`go run ./internal/config/cmd/config-doc migration-table`），测试会逐行比对两者是否一致。

| 已删除 | 替代写法 |
| --- | --- |
| `mount` | `filesystem.read_timeout`、`fuse.allow_other` |
| `mount.read_timeout` | `filesystem.read_timeout` |
| `mount.allow_other` | `fuse.allow_other` |
| `filesystem.backend` | `backend` |
| `smb.strm_enabled` | `smb.strm` |
| `http.auth.username` | `username` |
| `http.auth.password_hash` | `password` |
| `http.auth.password_hash_file` | `password` |
| `smb.username` | `username` |
| `smb.password_file` | `password` |
| `lab` | `experiments.upload` |
| `lab.upload_mode` | `experiments.upload.mode` |
| `lab.upload_rate` | `experiments.upload.rate` |
| `lab.upload_multiplier` | `experiments.upload.multiplier` |
| `lab.upload_jitter` | `experiments.upload.jitter` |
| `lab.upload_cap` | `experiments.upload.cap` |
| `lab.upload_seed` | `experiments.upload.seed` |
| `lab.announce_log` | `experiments.upload.announce_log` |
| `lab.peer_sample_interval` | `experiments.upload.peer_sample_interval` |
| `-mountpoint` | `fuse.mountpoint` |
| `-strm-mountpoint` | `fuse.strm_mountpoint` |
| `TORRENTFS_MOUNT_READ_TIMEOUT` | `TORRENTFS_FILESYSTEM_READ_TIMEOUT` |
| `TORRENTFS_MOUNT_ALLOW_OTHER` | `TORRENTFS_FUSE_ALLOW_OTHER` |
| `TORRENTFS_SMB_ENABLED` | `TORRENTFS_BACKEND` |
| `TORRENTFS_STRM_ENABLED` | `TORRENTFS_SMB_STRM` |
| `TORRENTFS_FILESYSTEM_BACKEND` | `TORRENTFS_BACKEND` |
| `TORRENTFS_SMB_STRM_ENABLED` | `TORRENTFS_SMB_STRM` |
| `TORRENTFS_SMB_USERNAME` | `TORRENTFS_USERNAME` |
| `TORRENTFS_SMB_PASSWORD_FILE` | `TORRENTFS_PASSWORD` |
| `TORRENTFS_LAB_UPLOAD_MODE` | `TORRENTFS_EXPERIMENTS_UPLOAD_MODE` |
| `TORRENTFS_LAB_UPLOAD_RATE` | `TORRENTFS_EXPERIMENTS_UPLOAD_RATE` |
| `TORRENTFS_LAB_UPLOAD_MULTIPLIER` | `TORRENTFS_EXPERIMENTS_UPLOAD_MULTIPLIER` |
| `TORRENTFS_LAB_UPLOAD_JITTER` | `TORRENTFS_EXPERIMENTS_UPLOAD_JITTER` |
| `TORRENTFS_LAB_UPLOAD_CAP` | `TORRENTFS_EXPERIMENTS_UPLOAD_CAP` |
| `TORRENTFS_LAB_UPLOAD_SEED` | `TORRENTFS_EXPERIMENTS_UPLOAD_SEED` |
| `TORRENTFS_LAB_ANNOUNCE_LOG` | `TORRENTFS_EXPERIMENTS_UPLOAD_ANNOUNCE_LOG` |
| `TORRENTFS_LAB_PEER_SAMPLE_INTERVAL` | `TORRENTFS_EXPERIMENTS_UPLOAD_PEER_SAMPLE_INTERVAL` |

## 数据与状态

- `<torrents-dir>/<infohash>.torrent` 保存 canonical metainfo，`<torrents-dir>/.metadata/` 保存任务、分类、字幕、限速和 STRM 密钥；`.metadata` 不会出现在 FUSE/SMB 中。
- 任务只能通过 Web UI/API 添加，手工复制 `.torrent` 到目录不会自动导入。
- 内容缓存（piece 数据、完成状态、传输计数）只在内存中，重启清空；管理状态会恢复。
- 同一个 `torrents-dir` 同一时刻只允许一个进程管理。

## 安全提示

- HTTP 默认仅监听 loopback；公开监听必须启用认证，并放在 TLS 反向代理之后。
- 文件输出完全只读：字幕请通过 Web UI/API 上传，不要用 `docker cp` 写入挂载点。
- SMB 只读、要求签名，不应直接暴露到互联网。
- SMB / HTTP-only 模式可 `--cap-drop ALL`；只有 FUSE 模式需要 `/dev/fuse`、`SYS_ADMIN` 和宿主挂载传播。
- 备份数据目录时请一并保护签名密钥 `stream_key`。

## 开发与测试

```bash
# 前端
npm ci --prefix web
npm run typecheck --prefix web
npm run lint --prefix web
npm test --prefix web -- --run
npm run build --prefix web

# 后端
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

CI 会执行上述检查，并额外运行 golangci-lint、Docker 配置/SMB 冒烟和真实 FUSE 挂载测试。

## 许可证

本项目使用 [Mozilla Public License 2.0](LICENSE)。
