# 开发与贡献指南

[English](development.en.md) · [返回 README](../README.md)

所有命令均从仓库根目录执行。运行参数与容器部署见[部署指南](deployment.md)，用户可见行为见[使用指南](usage.md)，接口契约见 [API 参考](api.md)。

## 环境与构建

需要 Go 1.27 或更高版本，以及 `web/.nvmrc` 指定的 Node.js（当前为 22.23.2）和 npm。Go 通过 `web/embed.go` 嵌入 `web/dist`，clean checkout 必须先生成前端产物：

```sh
git clone https://github.com/yakumioto/torrentfs-go.git
cd torrentfs-go
./scripts/build-web.sh
go build -o ./torrentfs ./cmd/torrentfs
```

`./scripts/build-web.sh` 按 lockfile 安装依赖、构建 `web/dist` 并删除 `web/node_modules`，不会删除 `web/dist`。已有等价前端构建时，可以直接执行 Go 命令。

开发 Web UI 时，先在另一终端运行后端，然后启动 Vite：

```sh
npm ci --prefix web
npm run dev --prefix web
```

Vite 默认监听 `127.0.0.1:5173`，将 `/api` 代理到 `http://127.0.0.1:8080`。生产 UI 由 Go embed 提供，使用同源相对 `/api/v1` 请求。

## 架构与边界

```text
cmd/torrentfs                  CLI、配置、进程生命周期、优雅退出
        │
        ├── internal/session      torrent 生命周期、metainfo、网络、内存 cache
        ├── internal/filesystem   只读 FUSE 数据树
        ├── internal/api          HTTP 路由、认证、管理、status 快照
        └── web                   React UI，构建后嵌入二进制
```

- 挂载点只承载只读数据，没有管理用的 `metadata/` 或 `stats/` 目录；管理请求走 HTTP。
- torrent payload 不可变，piece 仅存在有界内存 cache 中。metainfo、registry、magnet intent、peer identity、分类、上传限速和字幕才是持久化管理状态。
- 字幕是独立 overlay，API 是受支持的写入口，FUSE/SMB 只读投影，不能覆盖 payload。
- cache 当前占用与运行时累计传输计数是不同契约；计数不持久化，重启清零。
- HTTP 默认 loopback，非 loopback 强制认证；服务不终止 TLS。
- API status 包含 piece、文件范围和 network/DHT 诊断；当前 UI 展示文件/piece 缓存视图，不提供 peer/DHT 面板。

完整磁盘布局与恢复规则见[使用指南](usage.md)。启动先校验参数和配置，再创建 logger、恢复 session、按需启动 HTTP/FUSE。根目录手工放入的任意 `.torrent` 不会创建任务或进入 FUSE，也不会阻止 API 删除。

## 一致性与存储保证

### 原子管理状态

canonical metainfo 按 info hash 校验和命名。磁力意图先写 pending，metadata 完成后发布最终 metainfo 并清理 pending。分类索引和任务 sidecar 通过临时文件、`fsync`、原子 rename 写入。

上传限速保存后在原地调整同一个 limiter，不重建 client/torrent。rename 是提交点；rename 前失败不改变规则，rename 后目录同步失败返回 `applied: true` 的持久性未确认错误，不能当作新规则未生效。机器响应见 [API 参考](api.md)。

### 字幕发布与读取

字幕采用同目录临时文件 → `fsync` → 原子 rename → 目录 `fsync`。rename 前失败清理临时文件，已有旧字幕保持完整；如果是新建，目标仍不存在。rename 成功后内容已经生效，后续目录同步或校验只记 warning，不再报告写入失败，避免磁盘、内存 index 与调用方对发布结果产生分歧。发布的 size/mtime 取自 rename 前 staging 文件，rename 保持同一 inode。

字幕 `stat` 每次读取当前 size/mtime，不沿用 inode 首次 lookup 的长度。一次 `open` 从同一份已打开文件同时取得内容与 size/mtime；direct I/O 保证新 open 读取完整新版本，已有 handle 继续读原快照，不会出现旧长度配新内容。创建、可写 Open、Rename、Unlink 等操作仍返回 `EROFS`。

字幕上传和添加 torrent 共享 root namespace 锁，涵盖整个上传和磁盘 I/O，避免在过期布局上发布或返回错误的 mount_path；该锁独立于 Session 全局 mutex，不阻塞状态查询。

新增 torrent 的 namespace 校验在 final metainfo 写入前进行；magnet metadata 解析完成时也校验，不能留下绕过校验、重启后又恢复的文件。任务被删除时，字幕目录不存在且父目录完成同步后，operation 才进入 `deleted`；失败任务不进入可见 index，修复后可幂等重试。

### 路径安全

`.metadata/subtitles` 的目录 handle 在启动时打开并复用到进程关闭。创建、替换、读取、扫描和删除都基于该 handle 的相对路径，每个路径组件用 `O_NOFOLLOW` 解析：

- 宿主移动 store 后在原路径放 symlink，不会重定向现有 handle；检测到替换后拒绝上传。
- 内部 symlink 不会被跟随，不能把 hash A 的 sidecar 导向 hash B 或其他位置。
- 删除使用 no-follow 目录 handle 和 `unlinkat` 递归，符号链接/特殊文件先被拒绝，不会跟随或误删。

启动恢复拒绝不能唯一对应视频的 sidecar 和不受管理的链接/设备/特殊文件，不会静默暴露不受信任内容。

### 关闭与静态服务

正常关闭顺序为 HTTP → session → FUSE unmount；session 先取消并排空未完成读取，unmount 有 30 秒 deadline。容器 SMB supervisor 先停 Samba 再停 torrentfs，核心进程异常退出或 mount 消失会联动停止，详见[部署指南](deployment.md)。

静态服务仅接受 `GET`/`HEAD`；根路径和无扩展名前端 deep link 回退到 `index.html`，带扩展名的未知 asset 返回 `404`。`index.html` 使用 `no-cache`，构建 assets 使用长期 immutable 缓存。

## 本地质量检查

先完成前端检查和构建，再执行 Go 命令：

```sh
npm ci --prefix web
npm run typecheck --prefix web
npm run lint --prefix web
npm test --prefix web -- --run
npm run build --prefix web
rm -rf -- web/node_modules

go build ./...
go vet ./...
go test ./...
go test -race ./...

go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
golangci-lint run ./...
```

## 集成与容器验证

### HTTP 与配置

```sh
./scripts/http-smoke.sh
./scripts/docker-config-smoke.sh
```

HTTP smoke 需要 Docker、`curl` 和 `python3`，不需要 FUSE；检查静态 root/deep link、缺失 asset、认证 `401`、`WWW-Authenticate`、登录、列表、登出，不向远端 registry 发布内容。

Docker config smoke 还需要 `awk`、`timeout`；验证内置 TOML、默认 CMD、环境覆盖、外部只读 TOML，以及 SMB 缺凭据、非法用户名或 UID 不匹配时在 listener 启动前失败。数据目录仍必须可写。

### FUSE-required

真实 FUSE 测试需要 `/dev/fuse`、`fusermount`/`fusermount3` 和挂载权限；普通测试在缺少条件时跳过。要将缺少前提视为失败：

```sh
TORRENTFS_FUSE_REQUIRED=1 go test -race -run 'TestFuse|TestSessionIncomplete' ./...
```

`TORRENTFS_FUSE_REQUIRED` 是测试门禁，不是 daemon 配置。

### 宿主可见 Docker FUSE

```sh
./scripts/docker-smoke.sh
```

需要 Linux rootful Docker、`/dev/fuse`、`SYS_ADMIN` 或等效权限、允许 FUSE 的 AppArmor 配置，以及 `findmnt`、`python3`、`sha256sum`、`timeout`。执行前确认宿主目标 mount 支持 `rshared`。

### 单容器 SMB

```sh
./scripts/docker-smb-smoke.sh
```

需要 Linux Docker daemon、`/dev/fuse`、`SYS_ADMIN`、`CAP_NET_BIND_SERVICE`、允许 FUSE 的安全策略，以及 `python3`、`sha256sum`、`dd`、`timeout`。脚本构建应用和独立 client 镜像，在隔离 Docker network 中检查认证、错误密码和 guest 拒绝、目录列举、读取哈希、只读拒写、`.metadata` 不可见、HTTP/SMB 共享凭据、日志不泄密、SIGTERM 顺序，以及核心进程异常退出的联动和退出码。client 操作用同一个有界 `CLIENT_TIMEOUT`，smbclient 本身也使用 `-t`；失败打印应用与 web seed 日志。

脚本还尝试 client 容器内 `mount.cifs`，以非零大偏移 `dd iflag=skip_bytes,count_bytes` 读取大于 cache 的区间并与源切片比较，验证随机 seek。宿主不具备 CIFS 能力时，明确打印 `host cannot mount CIFS` 并继续，由 required FUSE 测试（包括 `>4GiB` 虚拟文件大偏移）提供随机读取验证；挂载成功但数据错误则直接失败。显式跳过用 `TORRENTFS_SMB_SKIP_CIFS=1`，CI/nightly 不设置该变量，在有能力的 runner 上执行完整比较。

默认宿主/容器 UID 相同时容易掩盖 metadata 权限和清理问题；可指定不同运行身份：

```sh
TORRENTFS_SMOKE_UID=1500 TORRENTFS_SMOKE_GID=1500 ./scripts/docker-smb-smoke.sh
```

这两个变量只覆盖 smoke 的运行时环境，不是镜像 build args。

## 贡献与发布边界

欢迎提交问题和贡献。提交行为改动时，应更新对应详细指南和测试；README 只保留项目介绍、主要能力和完整首次使用入口。

CI nightly 的多平台 OCI 构建与本地 `scripts/nightly-build.sh` 归档脚本是不同入口。本文命令用于本地构建和验证，不将手工归档脚本写成 nightly 发布保证，也不假定存在浮动镜像 tag。
