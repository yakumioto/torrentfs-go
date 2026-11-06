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
- 完整字段与注释见 [torrentfs.example.toml](torrentfs.example.toml)；容器默认配置见 [docker/torrentfs.toml](docker/torrentfs.toml)。
- 关键默认值：HTTP `127.0.0.1:8080`（认证默认关闭）、`backend=none`、SMB `0.0.0.0:1445`、cache `2GiB`、单次读取超时 `30s`。
- 每个配置项都有对应的环境变量，由配置项点路径机械派生：点路径转大写、`.` 换成 `_`、加 `TORRENTFS_` 前缀。例如 `http.auth.token_ttl` → `TORRENTFS_HTTP_AUTH_TOKEN_TTL`，`cache.capacity` → `TORRENTFS_CACHE_CAPACITY`。没有例外。
- **配置文件入口**：`TORRENTFS_CONFIG` 指定配置文件路径，`-config` 参数优先于它。**不做隐式搜索路径**：不指定就是不带配置文件启动，不会去猜某个路径。
- **未知变量会报错**：`TORRENTFS_` 前缀下拼错的名字在启动时会被点名，不会被静默忽略后按默认值启动。`TF_` 前缀是给测试开关、CI 冒烟与 Compose 插值用的，不属于配置命名空间。
- **凭据**：顶层 `username` / `password` 一对凭据，SMB 登录与 HTTP 登录共用同一个口令。`username` 仅 SMB 登录使用，**Web 登录只校验密码**，不需要用户名。`password` 在 `backend = "smb"` 或 `http.auth.enabled = true` 时必填，`username` 仅在 `backend = "smb"` 时必填。优先用 `TORRENTFS_USERNAME` / `TORRENTFS_PASSWORD` 传入；在 TOML 里写明文口令会在启动时打一条不含口令值的告警。
- 绑定非 loopback 地址时必须启用认证；Bearer token 只存在内存中，重启失效。
- 上传限速在 Web UI 中设置并立即生效，不通过 TOML 或环境变量配置。

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
