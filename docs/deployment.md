# 部署指南

[English](deployment.en.md) · [返回 README](../README.md)

首次使用推荐 [README 的 Docker + SMB 快速开始](../README.md#快速开始)。本文说明配置化的 FUSE/SMB 后端、容器权限、STRM、网络和退出行为。所有仓库命令从仓库根目录执行。

## 环境要求

- 原生构建需要 Go 1.27 或更高版本，以及 `web/.nvmrc` 指定的 Node.js 和 npm；clean checkout 必须先生成 `web/dist`。
- **原生 SMB 和 HTTP-only 不需要 FUSE 设备或挂载权限。** SMB 直接访问虚拟树，不启动 Samba。
- **只有 FUSE 模式**需要 Linux FUSE3、`/dev/fuse` 和允许挂载的主机策略。
- Docker 推荐以明确的非 root 数字 UID/GID 运行。发布宿主低端口 `445` 由 Docker/主机策略负责；应用在容器内监听 `1445`，不需要 `NET_BIND_SERVICE`。rootless Docker 对宿主低端口的限制仍需自行处理。

## 原生运行

### 构建与目录

```sh
./scripts/build-web.sh
go build -o ./torrentfs ./cmd/torrentfs
mkdir -p "$PWD/torrents" "$PWD/mnt"
```

`torrents` 必须事先存在、可读写且不是 symlink。它保存 canonical metainfo、registry、分类、字幕、签名密钥和锁；内容 piece 仍只在内存中。手工复制 `.torrent` 不会自动导入任务。一个目录同一时刻只允许一个 torrentfs 进程管理。

### 运行模式

通过 TOML 选择一个文件输出，HTTP 管理入口独立控制：

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

- `fuse`：创建配置的 FUSE 挂载，不启动 SMB；至少配置一个原文件或 STRM 挂载点。
- `smb`：启动内置 SMB，不创建任何 FUSE 挂载；未选中的 FUSE 路径可保留在同一配置中。
- `none`：不输出文件树，保留 HTTP API/Web UI。这是默认模式；不能同时关闭所有输出。

配置在启动时读取，切换后端需要重启。完整字段和兼容规则见[配置参考](configuration.md)。例如：

```sh
./torrentfs -config ./torrentfs.toml "$PWD/torrents"
```

不提供配置或挂载参数时，HTTP 默认监听 `127.0.0.1:8080`、认证关闭，原生 peer 端口自动选择：

```sh
./torrentfs "$PWD/torrents"
curl --fail http://127.0.0.1:8080/api/v1/torrents
```

旧 `-mountpoint` / `-strm-mountpoint` 仍作为 FUSE 配置别名；没有显式后端选择时可推导 `fuse`。显式选择 `smb` 或 `none` 后再传 FUSE CLI 参数会报冲突，不会悄悄同时启动两个输出。

### 可选 STRM 挂载

FUSE 可以单独挂 STRM，也可同时挂原文件。要求 HTTP 已启用、`strm.base_url` 非空，挂载目录事先存在，规范化及解析符号链接后不相同、不互相嵌套：

```sh
mkdir -p "$PWD/mnt-strm"
TORRENTFS_STRM_BASE_URL=https://media.example/torrentfs/ \
  ./torrentfs -mountpoint "$PWD/mnt" -strm-mountpoint "$PWD/mnt-strm" "$PWD/torrents"
```

占位域名需替换为播放器可达的地址；反代带前缀时须剥离前缀后转发 `/stream/...`。SMB 的 STRM 共享不需要这些挂载目录，见下文。

### CLI 与退出

```text
torrentfs [-config <file>] [-mountpoint <dir>] [-strm-mountpoint <dir>] <torrents-dir>
```

`<torrents-dir>` 是唯一必填 positional 参数；`-h` / `--help` 输出帮助。HTTP listener 置空可以只提供 FUSE 或原文件 SMB，但没有添加任务/上传字幕入口，也不能启用 STRM。

| 退出码 | 含义 |
| --- | --- |
| `0` | 收到 `SIGINT`/`SIGTERM` 后正常完成关闭 |
| `1` | SMB、HTTP、session、FUSE unmount 等运行时错误或未完成的关闭 |
| `2` | flag、目录或配置校验错误 |

## Docker 镜像与默认值

[Dockerfile](../Dockerfile) 构建内嵌 UI 的 `CGO_ENABLED=0` Go 二进制，运行阶段保留可选 FUSE3、`tini`、CA certificates 和身份切换工具，**不安装 Samba、smbd、smbpasswd 或 passdb**。

```sh
docker build -t torrentfs .
```

镜像默认 `USER 1000:1000`，命令为：

```text
/usr/local/bin/torrentfs -config /etc/torrentfs/torrentfs.toml /torrents
```

默认是 HTTP-only，HTTP 仍监听容器 loopback `127.0.0.1:8080`。只发布端口不会改变 listener；公开 HTTP 必须启用认证。镜像 peer 端口为 `6881`，IPv6/自动端口映射默认关闭，cache 上限 `2GiB`。

Nightly 由 `main` push 或显式 `workflow_dispatch` 构建，未更新 main 不会每日重复重建。发布标签含日期、提交和 run id；不保证浮动 `latest` / `nightly` 存在，未核实发布标签时请本地构建。

## 运行身份与持久化目录

推荐按 README 准备非零 `PUID/PGID` 和目录，然后使用：

```sh
--user "$PUID:$PGID" --env PUID --env PGID
```

数字身份不必在镜像 `/etc/passwd` 中存在，SMB 登录名也不必映射到 Unix 用户。`PUID/PGID` 可省略，此时沿用非 root 进程身份；若显式提供，必须是 `1..4294967294` 的十进制整数，并与 `--user` 一致。空、零、负数、非法或越界值会被拒绝。

入口不会自动 `chown` bind mount。运行身份必须能读、写、遍历 `/torrents`；不要把它挂成只读。无 bind mount 时镜像的空 `/torrents` 为 `0777`，仅为默认启动兜底。

旧部署可显式 `--user 0:0` 使用 root bootstrap，再由 `PUID/PGID` 降权；这条兼容路径不是零权限路径：至少需要 `SETUID`、`SETGID`，以及让 root `tini` 向不同 UID 子进程转发信号的 `KILL`。推荐路径从一开始便以非 root 启动，可使用 `--cap-drop ALL`。

`docker exec ... id` 使用 Docker 配置的 exec 身份；检查真实 daemon 时可查看 `/proc/<torrentfs-pid>/status` 的 Uid/Gid/CapEff/NoNewPrivs。

## 容器内 SMB 只读共享

推荐命令见 [README](../README.md#快速开始)，或使用 [Compose 模板](../docker-compose.yaml)。其关键约束为：

```yaml
user: "1000:1000"
cap_drop: [ALL]
security_opt: ["no-new-privileges:true"]
ports:
  - "127.0.0.1:445:1445/tcp"
```

应用不需要 `/dev/fuse`、`SYS_ADMIN`、`NET_BIND_SERVICE`、AppArmor unconfined 或挂载传播。不要同时传 FUSE `-mountpoint` 参数；`/share` 不是 SMB 的实体后端，SMB 只导出协议无关的虚拟树，`/torrents/.metadata` 不会被共享。

| 设置 | 职责 |
| --- | --- |
| `filesystem.backend = "smb"` / `TORRENTFS_FILESYSTEM_BACKEND=smb` | 选择原生 SMB |
| `smb.listen_addr` | 应用 listener，默认 `0.0.0.0:1445` |
| `smb.username`、`smb.password_file` | 独立 SMB 登录名和只读挂载的密码文件 |
| `TORRENTFS_USERNAME`、`TORRENTFS_PASSWORD` | 兼容的共享 HTTP/SMB 环境凭据，覆盖 SMB 文件凭据 |

原始 share 名为 `torrentfs`；单用户 NTLMv2，禁止 guest/匿名和 SMB1，最低 SMB2.1，要求签名，支持客户端协商 SMB3 加密。支持加密不等于默认强制加密；只在可信网络使用，不应直接公开到互联网。密码/NTLM 摘要不写入 Samba 数据库；环境变量仍对 Docker 管理员可见。SMB-only 推荐只读 secret 文件；HTTP bcrypt 不能用于反推出 SMB 的 NTLM 凭据。

### 可选 STRM 第二共享

设置播放器可达的非空 `strm.base_url` 并启用 HTTP 后，默认在同一 listener 上提供 `torrentfs-strm`，原始 `torrentfs` 保留。可用 `smb.strm_enabled = false` 或 `TORRENTFS_SMB_STRM_ENABLED=false` 关闭第二共享而保留 HTTP 直链；省略时按 base_url 自动决定。旧 `TORRENTFS_STRM_ENABLED` 仍兼容，显式 true 要求 SMB 和有效基址；不要设置互相矛盾的新旧开关。

在已有 Compose 服务中合并下面字段，保留数据目录、身份、凭据和减权设置；`192.0.2.10` 仅为示例，须替换为可信 LAN 地址：

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

播放器媒体库可指向 `smb://192.0.2.10/torrentfs-strm`。基址不要填容器内部 `localhost`；HTTPS 反代需播放器可达，并正确转发 `/stream/...`。不需要管理 Bearer token。保护 `.strm` 内容和反代访问日志，外网使用 HTTPS。

### 客户端路径

- SMB endpoint：`smb://SERVER/torrentfs`，Windows 为 `\\SERVER\torrentfs`。
- share 内路径例如 `payload.bin`；没有额外的 `share` 目录层级。
- 客户端挂载点自行选择，例如 `/mnt/torrentfs-client`，不是服务端虚拟路径。

```sh
smbclient //127.0.0.1/torrentfs -U torrentfs -m SMB3
```

标准宿主映射 `445:1445` 无需客户端改端口；直接连接应用的 `1445` 时，`smbclient` 需 `-p 1445`，CIFS 需 `port=1445`。内核 CIFS 示例使用独立的、仅自己可读的凭据文件：

```sh
mkdir -p /mnt/torrentfs-client
mount.cifs //SERVER/torrentfs /mnt/torrentfs-client \
  -o credentials=/path/to/credentials,vers=3.0,ro
ls -l /mnt/torrentfs-client/payload.bin
umount /mnt/torrentfs-client
```

客户端不要挂到 `/`。SMB 随机读取直接进入共享 piece planner/cache，不再经过 Samba 或 FUSE。

### 仅 SMB

不发布 HTTP 端口不等于关闭 listener。使用 `TORRENTFS_HTTP_LISTEN_ADDR=` 真正关闭 HTTP，并清空 `strm.base_url`；既有任务仍可通过原始共享读取，但没有任务管理/字幕上传入口。SMB 的凭据要求独立于 HTTP auth，仍须有效用户名和密码。

## 其他 Docker 模式

### HTTP-only

选择 `filesystem.backend=none`（默认），不映射 SMB 端口。可以使用同样的 `--user`、`--cap-drop ALL` 和 no-new-privileges；公开 HTTP 时必须配置认证。非空 `strm.base_url` 可服务已有有效签名链接，但不生成文件共享。

### 宿主可见 FUSE

只有此模式需要挂载权限和设备。先为运行身份准备可访问的空目录，并检查主机传播属性：

```sh
sudo install -d -o "$PUID" -g "$PGID" -m 0755 /srv/mnt
findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
```

主机确认支持所需的 shared propagation 后：

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

按主机策略调整 AppArmor；此模式扩大权限和挂载边界，不与 SMB 的零 capability 配置混用。非 root `fusermount3` 可能需要其 setuid 权限，不要机械加入 SMB 示例的 no-new-privileges。挂载目录必须可写以创建 submount，实际 FUSE 树仍只读；`fuse.allow_other` 仅允许其他本地 UID 读取，非 root 启用时还需 `/etc/fuse.conf` 的 `user_allow_other`。

## 外置 TOML

将配置只读挂到默认路径，或提供完整 `-config` 和 positional 目录：

```bash
docker run --detach --name torrentfs-config \
  --user "$PUID:$PGID" --env PUID --env PGID \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src="$PWD/torrentfs.toml",dst=/config.toml,readonly \
  torrentfs -config /config.toml /torrents
```

该示例只展示配置挂载；按选择的模式加入端口和凭据。环境变量优先于 TOML，若通过 TOML 切换后端，应删除旧 Compose 中的后端环境覆盖。

## 日志、停止与恢复

```sh
docker logs -f torrentfs
docker stop --time 60 torrentfs
docker inspect torrentfs --format '{{.State.ExitCode}}'
docker start torrentfs
```

原生 SMB 停止接入、取消并回收读取和句柄（有界等待），随后按 daemon 的 HTTP/session/FUSE 清理约束关闭；没有第二个 smbd 进程或 shell supervisor。HTTP drain 最多 5 秒，FUSE 卸载阶段共用 30 秒期限；仍持有传播挂载或无法完成的关闭会报告错误，不能把 `docker stop` 成功等同于应用退出码 `0`。

相同 `/torrents` 可恢复任务、分类、收藏、字幕、限速和 STRM 签名密钥；Bearer token 与 piece cache 不恢复。基址/密钥变更后播放器可能缓存旧链接，需要重新读取和重扫。部署变更应保留旧镜像以便回滚，并实际检查运行镜像、入口、共享及退出码；构建成功不等于部署完成。

## 部署故障排查

| 现象 | 原因与处理 |
| --- | --- |
| PUID/PGID 与 --user 冲突 | 数字身份须一致；检查非零范围和数据目录属主，不要盲目递归 chown |
| 发布 HTTP 后不可达 | 设置非 loopback listener 并启用认证；端口映射不会改变 listener |
| SMB 无法启动 | 检查 backend、1445 listener、有效凭据和 secret 文件权限；不需要补 FUSE/capability |
| 新旧后端/STRM 参数冲突 | 删除矛盾的旧环境开关和 FUSE CLI 参数，显式选择一个后端 |
| 宿主 445 被占用 | 检查宿主现有 SMB 服务，选择客户端支持的端口或解决冲突；应用仍可监听高位端口 |
| 宿主看不到 FUSE | 仅 FUSE 模式检查设备、权限、目录属主和传播；SMB 不使用该挂载 |
| 播放器仍显示旧字幕/链接 | 新目录枚举会刷新服务端快照，但客户端可能自行缓存；重新打开或重扫媒体库 |

读取、STRM 和字幕语义见[使用指南](usage.md)，测试入口见[开发指南](development.md)。
