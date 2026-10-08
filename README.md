# torrentfs

[English](README.en.md)

`torrentfs` 是用 Go 编写的只读 BitTorrent 文件系统：按需获取内容，通过 FUSE 呈现为文件树，并用 Web UI/API 管理任务。

适合让播放器、媒体库、索引器或命令行工具像读取本地文件一样读取种子内容，也可通过 Docker 提供 SMB 只读共享。

## 核心特性

- **按需读取**：有界内存缓存，不将 torrent 内容完整下载到磁盘。
- **只读文件系统**：单文件直接呈现，多文件保留目录结构。
- **Web UI/API 管理**：添加磁力链接或上传 `.torrent`，查看任务与缓存状态。
- **任务与媒体管理**：分类、收藏、批量清理、字幕上传/替换和按时段上传限速。
- **Docker + SMB**：在同一容器内挂载 FUSE 并提供只读共享，无需宿主挂载传播。

## 快速开始

推荐路径：**Docker → Web UI 添加任务 → SMB 读取文件**。下面使用本地构建，不依赖未经核实的镜像 tag。

前提条件：

- Linux rootful Docker、可用的 `/dev/fuse`，以及允许 FUSE 的宿主安全策略。
- 可以授予容器 `SYS_ADMIN`、`NET_BIND_SERVICE`；本机 `8080` 和 `445` 端口可用。
- 使用 Bash，在同一个终端依次执行以下命令；宿主机不需要安装 Go 或 Node.js。

缓存默认上限为 `2GiB`，小内存部署请先按[配置参考](docs/configuration.md)调整。其他运行方式见[部署指南](docs/deployment.md)。

### 1. 构建镜像

```bash
git clone https://github.com/yakumioto/torrentfs-go.git
cd torrentfs-go
docker build -t torrentfs .
```

### 2. 准备目录和凭据

容器入口需要以 root 启动，实际服务使用非 root 的 `PUID:PGID`。不要传 `--user`。非 root 主机用户可复用自己的身份，root 用户需选择专用的非零身份：

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

以上用于新目录；已有数据请先检查属主和权限。入口不会自动 `chown`，目标身份必须能读、写和遍历该目录。

HTTP 和 SMB 共用一组凭据，SMB 用户名使用镜像内的运行账户 `torrentfs`：

```bash
export TORRENTFS_USERNAME=torrentfs
read -r -s -p 'HTTP/SMB password: ' TORRENTFS_PASSWORD
printf '\n'
export TORRENTFS_PASSWORD
```

密码通过环境变量传递，拥有 `docker inspect` 权限者可能看到它；不要将真实凭据写进命令、仓库或镜像层。

### 3. 启动服务

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

此示例仅向本机发布 HTTP/SMB，并放宽 AppArmor 以允许 FUSE；请只在可信主机使用。主机策略已允许 FUSE 时，可按[部署指南](docs/deployment.md)调整安全选项。

SMB 模式自动管理容器内 `/share`，不要追加 `-mountpoint`，无需把 `/share` 挂到宿主。需要 LAN 访问或入站 peer 时，按部署指南配置地址、防火墙和 TCP/UDP 端口。

### 4. 添加任务并读取

1. 打开 `http://127.0.0.1:8080/`，用 `torrentfs` 和刚才设置的密码登录。
2. 在 Web UI 上传自己的 `.torrent` 文件，或添加有可用来源的磁力链接。
3. 用支持 SMB 的文件管理器或播放器打开 `smb://127.0.0.1/torrentfs`，使用相同凭据访问文件；Windows 路径为 `\\127.0.0.1\torrentfs`。

也可用 `smbclient` 交互验证，密码会提示输入：

```sh
smbclient //127.0.0.1/torrentfs -U torrentfs -m SMB3
```

登录后执行 `ls` 列出文件，使用 `get <实际文件路径> <本地目标路径>` 验证读取。单文件 torrent 通常直接位于共享根，多文件保留目录树。

磁力链接可能先处于 `adding`；`ready` 只表示元信息可用，读取仍可能等待 peers。日志、停止与重启方法见[部署指南](docs/deployment.md)。

## 使用前须知

- **挂载点和 SMB 只读**：字幕必须通过 UI/API 上传，不能用 `docker cp` 写入。
- **内容缓存不持久化**：重启会清空 piece cache，但管理状态会恢复；metainfo 持久化在 `torrents-dir/<infohash>.torrent`，其他状态在 `torrents-dir/.metadata`。
- **任务只通过 UI/API 添加**：手工复制 `.torrent` 到目录不会自动导入。
- **HTTP 是管理接口**：不提供 torrent 内容下载或播放端点，也不是完整内容落盘的下载器。
- **注意网络边界**：非本机 HTTP listener 必须认证；对外部署使用 TLS 反向代理并限制访问。

## 文档导航

| 文档 | 内容 |
| --- | --- |
| [部署指南](docs/deployment.md) | 原生运行、Docker、FUSE/SMB、身份权限、网络与部署排查 |
| [配置参考](docs/configuration.md) | TOML、环境变量、默认值、单位和旧配置迁移 |
| [使用指南](docs/usage.md) | 文件布局、分类、收藏、字幕、上传限速、状态与常见问题 |
| [API 参考](docs/api.md) | 路由、认证、请求响应、curl 示例和错误码 |
| [开发与贡献](docs/development.md) | 源码构建、架构、测试及贡献流程 |

完整配置示例见 [torrentfs.example.toml](torrentfs.example.toml)。

## 贡献与许可证

欢迎提交问题和贡献，构建与验证流程见[开发指南](docs/development.md)。

Mozilla Public License 2.0，见 [LICENSE](LICENSE)。
