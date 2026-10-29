# torrentfs

[English](README.en.md)

`torrentfs` 是用 Go 编写的只读 BitTorrent 文件系统：按需获取内容，通过可配置的 FUSE 或原生 SMB 呈现同一套虚拟文件树，并用 Web UI/API 管理任务。

适合让播放器、媒体库、索引器或命令行工具像读取本地文件一样读取种子内容，也可通过 Docker 提供 SMB 只读共享。

## 核心特性

- **按需读取**：有界内存缓存，不将 torrent 内容完整下载到磁盘。
- **只读文件系统**：单文件直接呈现，多文件保留目录结构。
- **Web UI/API 管理**：添加磁力链接或上传 `.torrent`，查看任务与缓存状态。
- **任务与媒体管理**：分类、收藏、批量清理、字幕上传/替换和按时段上传限速。
- **同级 FUSE / SMB 后端**：配置 `filesystem.backend` 选择输出；原生 SMB 不依赖 FUSE、Samba 或宿主挂载传播。
- **可选 STRM 视图**：保留原文件共享，另提供指向签名 HTTP 直链的 `.strm` 文件，见[部署指南](docs/deployment.md)。

## 快速开始

推荐路径：**Docker → Web UI 添加任务 → SMB 读取文件**。下面使用本地构建，不依赖未经核实的镜像 tag。

前提条件：

- 可用的 Linux Docker；本机 `8080` 和 `445` 端口可用，主机/Docker 策略允许发布它们。
- 推荐的 SMB 模式不需要 `/dev/fuse`、额外 capability 或放宽 AppArmor。
- 使用 Bash，在同一个终端依次执行以下命令；宿主机不需要安装 Go 或 Node.js。

缓存默认上限为 `2GiB`，小内存部署请先按[配置参考](docs/configuration.md)调整。其他运行方式见[部署指南](docs/deployment.md)。

也可下载 [Compose 模板](docker-compose.yaml)，按模板注释填写实际镜像 tag 和密码、准备属主及权限正确的数据目录，再在文件所在目录运行 `docker compose up -d`。

### 1. 构建镜像

```bash
git clone https://github.com/yakumioto/torrentfs-go.git
cd torrentfs-go
docker build -t torrentfs .
```

### 2. 准备目录和凭据

镜像默认以 `1000:1000` 运行。推荐用 `--user "$PUID:$PGID"` 明确选择非 root 身份；`PUID/PGID` 若设置，应与它一致。非 root 主机用户可复用自己的身份，root 用户需选择专用的非零身份：

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

下面通过环境变量为 HTTP 和 SMB 设置同一密码；Web 登录仅需密码，SMB 用户名示例为 `torrentfs`，与 Unix 运行账户无关：

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
  --publish 127.0.0.1:445:1445 \
  --user "$PUID:$PGID" \
  --env PUID --env PGID \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_FILESYSTEM_BACKEND=smb \
  --env TORRENTFS_USERNAME \
  --env TORRENTFS_PASSWORD \
  torrentfs
```

此示例只向本机发布 HTTP/SMB，应用以非 root、零 capability 和默认 AppArmor 运行。LAN 播放请按[部署指南](docs/deployment.md)绑定可信 LAN 地址并配置防火墙。

SMB 直接导出虚拟树，不创建 `/share` 挂载，也不要追加 FUSE 的 `-mountpoint`。也可在 TOML 中选择 `fuse`、`smb` 或 `none`；配置示例与 FUSE 专属权限见[配置参考](docs/configuration.md)和[部署指南](docs/deployment.md)。

### 4. 添加任务并读取

1. 打开 `http://127.0.0.1:8080/`，仅输入刚才设置的密码登录。
2. 在 Web UI 上传自己的 `.torrent` 文件，或添加有可用来源的磁力链接。
3. 用支持 SMB 的文件管理器或播放器打开 `smb://127.0.0.1/torrentfs`，使用用户名 `torrentfs` 和同一密码访问文件；Windows 路径为 `\\127.0.0.1\torrentfs`。

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
- **HTTP 默认用于管理**：显式启用 STRM 后才提供签名内容读取端点；不是将完整内容落盘的下载器。
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
