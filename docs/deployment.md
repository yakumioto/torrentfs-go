# 部署指南

[English](deployment.en.md) · [返回 README](../README.md)

首次使用推荐 [README 的 Docker + SMB 快速开始](../README.md#快速开始)。本文补充原生运行、其他容器模式、身份权限、网络和故障排查。所有仓库命令均从仓库根目录执行。

## 环境要求

- 原生构建需要 Go 1.27 或更高版本、`web/.nvmrc` 指定的 Node.js（当前为 22.23.2）和 npm。
- 使用 FUSE 需要 Linux FUSE3、`/dev/fuse` 和挂载权限；仅运行 HTTP API/Web UI 不需要 FUSE 设备。
- 容器内 FUSE/SMB 需要 Linux rootful Docker、`/dev/fuse`、`SYS_ADMIN`、`NET_BIND_SERVICE`，以及允许 FUSE 的宿主安全策略。
- 本文部分示例使用 Bash；API 命令见 [API 参考](api.md)，自动验证脚本及依赖见[开发指南](development.md)。

## 原生运行

### 构建与目录

Go 会嵌入 `web/dist`，clean checkout 必须先构建 Web UI，不能直接跳过前端构建执行 Go 命令：

```sh
./scripts/build-web.sh
go build -o ./torrentfs ./cmd/torrentfs
mkdir -p "$PWD/torrents" "$PWD/mnt"
```

构建脚本安装 lockfile 中的依赖、生成 `web/dist`，然后删除 `web/node_modules`，不会删除 `web/dist`。完整开发和测试流程见[开发指南](development.md)。

`torrents` 目录必须事先存在、可读写且不是符号链接。单个 `.torrent` 文件不能作为 positional 参数；程序不会替用户创建目录，也不会扫描目录中的任意 `.torrent` 文件来添加任务。

### 三种运行模式

不指定 `-config` 时，HTTP 默认监听 `127.0.0.1:8080`，认证关闭，peer 端口由客户端选择。

**HTTP API/Web UI（headless）**：省略 mountpoint，这是默认模式。

```sh
./torrentfs "$PWD/torrents"
```

打开 `http://127.0.0.1:8080/`，或检查服务：

```sh
curl --fail http://127.0.0.1:8080/
curl --fail http://127.0.0.1:8080/api/v1/torrents
```

**HTTP API/Web UI + FUSE**：提供 mountpoint 不会关闭 HTTP。

```sh
./torrentfs -mountpoint "$PWD/mnt" "$PWD/torrents"
```

**仅 FUSE**：清空 HTTP listener，必须提供 mountpoint。

```sh
TORRENTFS_HTTP_LISTEN_ADDR= \
  ./torrentfs -mountpoint "$PWD/mnt" "$PWD/torrents"
```

仅 FUSE 模式只读呈现已注册任务和已有 managed subtitles，没有添加任务或上传字幕入口。首次使用应启用 HTTP；同一时刻只允许一个进程管理同一个 `torrents` 目录。

使用 TOML：

```sh
./torrentfs -config ./torrentfs.example.toml "$PWD/torrents"
```

`go run ./cmd/torrentfs ...` 可以替代 `./torrentfs ...`，但同样必须先生成 `web/dist`。通过 UI/API 添加任务后，单文件 torrent 直接出现在挂载点下，多文件 torrent 保留目录树，见[使用指南](usage.md)。

### CLI 与退出

```text
torrentfs [-config <file>] [-mountpoint <dir>] <torrents-dir>
```

| 参数 | 说明 |
| --- | --- |
| `-mountpoint <dir>` | FUSE 挂载目录；HTTP listener 启用时可以省略 |
| `-config <file>` | 可选 TOML 配置文件；未提供时使用内置默认值和环境变量 |
| `<torrents-dir>` | 唯一 positional 参数；必须是已存在、可读写、非 symlink 的目录 |
| `-h` / `--help` | 输出帮助并退出 |

| 退出码 | 含义 |
| --- | --- |
| `0` | 收到 `SIGINT`/`SIGTERM` 后正常关闭 |
| `1` | HTTP、session 或 FUSE unmount 的运行时错误 |
| `2` | flag 用法、positional 参数、目录或配置校验错误 |

按 `Ctrl-C` 或发送 `SIGTERM` 可停止服务。正常关闭顺序为 HTTP → session → FUSE unmount；session 先取消并排空等待 piece 的读取。FUSE unmount 有 30 秒 deadline；如果其他 mount namespace 仍持有传播副本，进程会输出诊断并以 `1` 退出，不会把未完成的 lazy unmount 当作成功。

## Docker 镜像与默认值

Dockerfile 用 Node 22.23.2 构建 UI、Go 1.27 编译包含 UI 的 `CGO_ENABLED=0` 二进制，运行阶段是带 FUSE3、Samba、`tini`、CA certificates、`passwd` 和 `util-linux` 的 Debian bookworm-slim。

```sh
docker build -t torrentfs .
```

nightly workflow 的 GHCR tag 包含日期、提交和 run id，不保证浮动 `latest` 或 `nightly` tag 存在。未核实具体发布 tag 前，使用本地构建。

镜像默认命令等价于：

```text
/usr/local/bin/torrentfs -config /etc/torrentfs/torrentfs.toml /torrents
```

镜像默认 HTTP 仍绑定容器内 `127.0.0.1:8080`，只加 `-p 8080:8080` 不会改变监听地址。镜像 peer 端口固定为 `6881`、IPv6 默认禁用、自动端口映射禁用、cache 为 `2GiB`、`allow_other` 关闭。环境变量可以覆盖镜像 TOML，见[配置参考](configuration.md)。

## 运行身份与持久化目录

入口必须以 root 启动，用于账户和 runtime 初始化，之后以 `PUID:PGID` 运行 torrentfs 和 smbd。不要传 `docker run --user`，也不需要为每台主机重建镜像。

| 变量 | 默认值 | 约束 |
| --- | --- | --- |
| `PUID` | `1000`（未设置时） | 无符号十进制整数，范围 `1..4294967294` |
| `PGID` | `1000`（未设置时） | 无符号十进制整数，范围 `1..4294967294` |

显式空值、非数字、负数、`0` 和越界值都会在服务启动前被拒绝。`TORRENTFS_UID` / `TORRENTFS_GID` build args 不受支持；例如 NAS 的 `99:100` 身份应通过 `--env PUID=99 --env PGID=100` 设置。

入口不会自动 `chown` bind mount。目标身份必须能读、写和遍历 `/torrents`，因为 `.metadata`、canonical metainfo、registry、字幕和锁都需要持久化；piece payload 仍只在内存中。不要将 `/torrents` 挂成只读。无 bind mount 时镜像自带 `/torrents` 为 `0777`，仅保证默认容器可启动。

[README](../README.md#快速开始) 提供运行身份、目录和共享凭据的准备命令。本文其他 Docker 示例沿用其中的 `PUID`、`PGID`、`TORRENTFS_USERNAME`、`TORRENTFS_PASSWORD` 和 `/srv/torrents`。

可以通过 `/proc/<torrentfs-pid>/status` 检查实际服务身份；`docker exec torrentfs id` 默认看到 root shell，不代表 daemon 身份。

## 容器内 SMB 只读共享

推荐用同一个容器运行 HTTP 管理、FUSE 和 SMB，完整启动命令见 [README](../README.md#快速开始)。FUSE 与 Samba 留在同一个 mount namespace 内，不需要宿主 `/mnt`、`rshared` 或跨容器 mount propagation。

| 变量 | 说明 |
| --- | --- |
| `TORRENTFS_SMB_ENABLED` | 默认 `false`；支持 `true`、`false`、`1`、`0`，其他值会被拒绝 |
| `TORRENTFS_USERNAME` | SMB 必填；必须是镜像内解析到实际 runtime UID 的 Unix 账户，推荐固定名称 `torrentfs` |
| `TORRENTFS_PASSWORD` | SMB 必填；非空单行密码，不能包含 CR/LF |

SMB 模式独占内部 `/share` 挂载点，不要传 `-mountpoint`，也不要将非空 bind mount 挂到 `/share`。入口确认 fstype 为 `fuse.*` 后才启动 Samba，只监听 TCP 445，不启动 `nmbd`，不发布 137/138/139。

share 名固定为 `torrentfs`，配置始终只读、禁止 guest。`/torrents` 和 `.metadata` 不会被共享。Samba 将请求映射到 torrentfs 的相同运行身份，因此不依赖 `allow_other`；显式启用该选项会扩大访问面。

### 客户端路径

- 服务端后端：容器内部 `/share`。
- SMB endpoint：`smb://SERVER/torrentfs`；Linux 命令行使用 `//SERVER/torrentfs`，Windows 使用 `\\server\torrentfs`。
- share 内文件：例如 `payload.bin`，不会额外增加一层 `share` 或 `torrentfs`。
- 客户端 mountpoint：自行选择，例如 `/mnt/torrentfs-client`，挂载后文件是 `/mnt/torrentfs-client/payload.bin`。

可用 `smbclient` 登录后执行 `ls`、`get`，密码交互输入：

```sh
smbclient //127.0.0.1/torrentfs -U torrentfs -m SMB3
```

内核 CIFS 挂载需要客户端自己的凭据文件。将 `/path/to/credentials` 替换为仅自己可读的文件，内容为 `username=...` 和 `password=...`：

```sh
mkdir -p /mnt/torrentfs-client
mount.cifs //SERVER/torrentfs /mnt/torrentfs-client \
  -o credentials=/path/to/credentials,vers=3.0,ro
ls -l /mnt/torrentfs-client/payload.bin
umount /mnt/torrentfs-client
```

不要把客户端 mountpoint 设为 `/`，也不要将宿主或容器的 `/` 当作 Samba share path。客户端读取需要的 piece 会进入内存 cache；随机 seek 走同一条 Samba `pread` → FUSE `Read(off)` → piece planner/cache 链路。

### 仅 SMB 与退出行为

不发布 HTTP 端口只是让 HTTP 保持 container-local；并不等于关闭它。若显式把 `TORRENTFS_HTTP_LISTEN_ADDR` 置空，则没有任务添加和字幕上传入口，应先注册任务。HTTP auth 关闭时 Go 配置层忽略共享凭据，SMB 仍要求完整 pair。

缺少 FUSE 设备、必要 capability、凭据或有效运行身份时，入口会明确失败，不会退化为共享普通目录。torrentfs、smbd 任一核心进程异常退出或 FUSE 消失，容器会停止另一进程并以非零状态退出。

收到 `SIGTERM`/`SIGINT` 时，先有界停止 Samba，再通知 torrentfs 执行 HTTP → session → FUSE unmount；正常关闭返回 `0`，超时强制终止会记录日志并返回非零。

## 网络与安全

- README 示例仅向 `127.0.0.1` 发布 HTTP/SMB；可信 LAN 访问应绑定明确的 LAN 地址并配置防火墙。
- HTTP 服务不提供 TLS，应放在 TLS reverse proxy 后面，不要直接公开到不受信任的网络。
- 接收入站 peer 时，在 `docker run` 中同时添加 `--publish 6881:6881/tcp --publish 6881:6881/udp`。只发布 HTTP 或只发布一个 peer 协议都不够；peer 端口设为 `0` 时不能预先按 `6881` 映射。
- `SYS_ADMIN` 和 `apparmor=unconfined` 扩大容器权限边界。示例针对允许 FUSE 的可信 rootful 部署；部分主机不需要放宽 AppArmor，应按主机策略配置，而不是让不受信任的调用者控制该容器。
- 共享密码通过 stdin 初始化 Samba passdb，不写入命令行、Samba 配置或日志；但 Docker environment 和 metadata 对 inspect 权限者可见，不是 secret store。
- SMB 与 torrentfs 共用 cgroup，cache 规划需要给 Samba 和其他进程留出余量。端口冲突应选择客户端支持的绑定或处理现有服务，不能假设换 SMB 端口后客户端仍能透明使用标准共享。

## 其他 Docker 模式

### HTTP-only

不授予 FUSE 设备和 capability，仅提供管理 API/Web UI；它没有 torrent 内容下载/播放端点：

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

### 宿主可见 FUSE

此模式比容器内 SMB 有更多宿主要求：`/srv/mnt` 所在挂载必须支持递归双向传播。先准备一个对运行身份可读写、可遍历的空目录，再检查传播属性：

```sh
sudo install -d -o "$PUID" -g "$PGID" -m 0755 /srv/mnt
findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
```

确认宿主满足 shared propagation 前提后，保留认证 HTTP 入口，以便从空目录添加任务：

```bash
docker run --detach --name torrentfs-fuse \
  --publish 127.0.0.1:8080:8080 \
  --env PUID --env PGID \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_USERNAME --env TORRENTFS_PASSWORD \
  torrentfs -config /etc/torrentfs/torrentfs.toml -mountpoint /mnt /torrents
```

`/mnt` bind mount 必须可写才能创建 submount，但 FUSE 树本身仍然只读。`mount.allow_other` 只改变谁能读取，不增加写权限；非 root 挂载启用它还需要 `/etc/fuse.conf` 中的 `user_allow_other`。容器内 SMB 不需要此宿主挂载方案，不能把两种模式的传播说明混用。

字幕通过 API/UI 写入 `/torrents/.metadata/subtitles`，不需要 `docker cp` 或放宽挂载写权限。跨容器开放 API 时仍必须认证并采用可信网络/TLS 边界。

## 外置 TOML

保留默认 CMD 时，把配置挂到镜像默认路径：

```bash
docker run --detach --name torrentfs-config \
  --env PUID --env PGID \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src="$PWD/torrentfs.toml",dst=/etc/torrentfs/torrentfs.toml,readonly \
  torrentfs
```

若挂到其他路径，必须同时传完整的 `-config` 和 positional 目录；不能省略 `/torrents`：

```bash
docker run --detach --name torrentfs-config \
  --env PUID --env PGID \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src="$PWD/torrentfs.toml",dst=/config.toml,readonly \
  torrentfs -config /config.toml /torrents
```

这两个示例只展示配置挂载；公开 HTTP、启用 SMB 或 FUSE 时，还需要加入相应模式的参数。外置 TOML 仍须满足非 loopback listener 的认证校验，不能把配置路径或端口映射当作认证替代。

## 日志、停止与恢复

```sh
docker logs -f torrentfs
docker stop --time 60 torrentfs
docker start torrentfs
```

入口给 Samba 最多 10 秒、torrentfs 最多 45 秒的正常退出时间，60 秒 Docker stop timeout 覆盖这两个窗口。保留相同 `/srv/torrents` bind mount，重启后任务、metainfo、分类、收藏、字幕和上传限速会恢复；Bearer token 与 piece cache 不恢复。部署完成后可取消当前 shell 中的凭据变量：

```sh
unset TORRENTFS_USERNAME TORRENTFS_PASSWORD
```

## 部署故障排查

| 现象 | 原因与处理 |
| --- | --- |
| runtime identity 错误 | 检查 `PUID`/`PGID` 是否为空、零、非法或越界，以及目标数字身份能否读写遍历 bind mount |
| 发布 `8080` 后 HTTP 仍不可达 | daemon 仍监听容器 loopback；设置非 loopback listener 并启用完整认证 |
| SMB 无法启动 | 检查 `/dev/fuse`、`SYS_ADMIN`、`NET_BIND_SERVICE`、单行非空凭据，以及用户名是否解析到 runtime UID |
| 外置 TOML 报 CLI 错误 | 默认路径之外挂载时，需要同时传 `-config` 和 `/torrents` |
| 宿主看不到 FUSE | 检查挂载传播、安全策略和运行身份；需要其他 UID 读取时再评估 `allow_other` |
| `445`、`8080` 或 `6881` 被占用 | 检查现有服务并选择可用绑定；peer TCP/UDP 必须一起映射 |
| `docker cp` 写 `/mnt` 或 `/share` 失败 | 预期行为，FUSE/SMB 只读；字幕应通过 API/UI 上传 |

读取、字幕和状态问题见[使用指南](usage.md)；HTTP、Docker 配置、FUSE、SMB 的 smoke 验证见[开发指南](development.md)。
