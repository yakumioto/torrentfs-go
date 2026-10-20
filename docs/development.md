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
        ├── internal/session      torrent 生命周期、metainfo、网络、cache、签名读取
        ├── internal/filesystem   原始只读 FUSE 树与 STRM Backend 投影
        ├── internal/media        共享视频/ISO 判定与显式 MIME
        ├── internal/stream       HMAC 签名、URL/路径规则、可取消 Reader 契约
        ├── internal/api          管理认证、status 与独立签名 HTTP 流
        └── web                   React UI，构建后嵌入二进制
```

- 挂载点只承载只读数据，没有管理用的 `metadata/` 或 `stats/` 目录；管理请求走 HTTP。
- torrent payload 不可变，piece 仅存在有界内存 cache 中。metainfo、registry、magnet intent、peer identity、分类、上传限速、字幕和可选的 STRM 签名密钥是持久化管理状态。
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

### STRM 投影与签名读取

STRM Backend 包装现有 Backend，不另建一套 FUSE 节点。它投影复制的快照，保留目录和非视频条目，消歧但不丢文件，并将可见名称映射回不可变的原始 payload 路径；分类和字幕能力继续转发。节点通过可选的实时文件 stat 与打开快照接口读取投影元数据，将一次 open 的 reader 和长度绑定，避免旧目录快照或复用 inode 保留过期的零长度。每次 FUSE lookup 都会列出整库，因此投影按 info hash 缓存命名映射和已生成的链接长度：列目录只为长度未知的视频签名并清理已移除种子；文件 lookup/stat 不再获取整库快照，生成文件只做一次 O(1) 签名校验。Session 也按 display path 建立一次性文件索引。生成和读取 URL 文本不打开视频 payload。某文件 URL 持续失败时，快照保留 size 为 0 的条目，但 lookup/stat/open 明确报错，不能阻塞健康邻居、返回成功空文本或无限重试；恢复后无需卸载即可读取。STRM mtime 沿用 torrent 添加时间；基址或密钥变化后需要客户端重新读取/扫描，不能假定热刷新。

`internal/media` 统一原有 14 种视频、独立 ISO 判断及显式 MIME。`internal/stream` 复制调用方密钥，用完整 HMAC-SHA256 绑定版本域、info hash 和原始 torrent 相对路径，逐组件转义且只解码一次；拒绝编码斜线和歧义组件，`%252F` 一次解码后仍是字面 `%2F`。

Session 仅在启用流时、获得实例锁后且创建 client 等资源前初始化 `.metadata/stream_key`。新密钥恰好 32 字节，以 `0600` 原子保存；长度损坏、权限过宽、符号链接或非普通文件使启动失败，不静默换钥。`StreamURL` 只检查 ready payload 元数据，不拉取 piece；`OpenStream` 先验签，再打开复用的可取消 reader，不从宿主或 FUSE 路径打开内容。

独立 `/stream/...` handler 不放宽 `/api/` 的 Bearer 认证。它用请求 context 绑定的 `ReadAtContext` 适配 `io.NewSectionReader` 与 `http.ServeContent`，显式设置 MIME 避免嗅探，不新增整文件或整块 HTTP cache。HEAD/无 body 条件响应不读取 payload；完成、取消、错误都会取消并关闭 reader。记录响应发出后的读取失败并中止传输，不追加 JSON 或伪装完整成功。

### 路径安全

`.metadata/subtitles` 的目录 handle 在启动时打开并复用到进程关闭。创建、替换、读取、扫描和删除都基于该 handle 的相对路径，每个路径组件用 `O_NOFOLLOW` 解析：

- 宿主移动 store 后在原路径放 symlink，不会重定向现有 handle；检测到替换后拒绝上传。
- 内部 symlink 不会被跟随，不能把 hash A 的 sidecar 导向 hash B 或其他位置。
- 删除使用 no-follow 目录 handle 和 `unlinkat` 递归，符号链接/特殊文件先被拒绝，不会跟随或误删。

启动恢复拒绝不能唯一对应视频的 sidecar 和不受管理的链接/设备/特殊文件，不会静默暴露不受信任内容。

### 关闭与静态服务

正常关闭先取消 HTTP 流的 context，再给 HTTP Shutdown 最多 5 秒；超时以 HTTP Close 解除阻塞网络写。随后关闭 Session，取消并排空未完成读取，最后并行卸载两个 FUSE 挂载，整个 unmount 阶段共用 30 秒 deadline。强制 Close 不代表已等待所有 handler 返回，不能据此宣称所有 goroutine 都已结束。容器 SMB supervisor 先停 Samba 再停 torrentfs，核心进程异常退出或任一 mount 消失会联动停止，详见[部署指南](deployment.md)。

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

## STRM 测试覆盖

- 配置/媒体/签名包：默认关闭、TOML/env 优先级、严格基址校验与凭据脱敏；14 种视频与 ISO 分离、显式 MIME；中文和 `%/#/?/+` 往返、路径前缀、一次解码、编码斜线拒绝、签名篡改和跨文件/hash 拒绝。
- Session：仅签名已注册 ready payload、重启复用私有密钥、损坏/权限/符号链接拒绝、删除后不可读，以及无 peers 的读取超时、取消和关闭。
- STRM Backend：不修改原快照、保留非视频/已有 STRM/字幕、确定性消歧、分类和动态增删、字幕快照、并发 inode/path 映射；持续 URL 错误保留 0 字节条目且不阻塞其他文件。真实 FUSE 集成比较 STRM 文本、HTTP Range 和原文件字节，生成 STRM 不读取 payload。
- HTTP/CLI：GET/HEAD、单/多 Range、条件请求/If-Range、416/403/404/405/503、大于 4GiB 偏移、HEAD 不拉取 payload、真实连接截断、客户端取消释放，以及阻塞读取/写入下有界关闭；双挂载参数和共用卸载 deadline。

这些覆盖通过 Go 质量检查及下列 FUSE-required、容器检查运行，不等于 Infuse 设备已验收。实际客户端仍需检查扫描、播放、seek、字幕自动匹配及基址/密钥变更后的缓存刷新。

## 集成与容器验证

### HTTP 与配置

```sh
./scripts/http-smoke.sh
./scripts/docker-config-smoke.sh
```

HTTP smoke 需要 Docker、`curl` 和 `python3`，不需要 FUSE；检查静态 root/deep link、缺失 asset、认证 `401`、`WWW-Authenticate`、登录、列表、登出，不向远端 registry 发布内容。

Docker config smoke 还需要 `awk`、`timeout`；验证内置 TOML、默认 CMD、环境覆盖、外部只读 TOML，以及 SMB 缺凭据、非法用户名或 UID 不匹配时在 listener 启动前失败。还检查 STRM 布尔值、缺少 SMB/基址/HTTP 的非法组合，以及 SMB 模式下自定义任一挂载参数被提前拒绝。数据目录仍必须可写。

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

STRM 场景还验证默认只导出原共享、启用后双只读共享/双挂载、读取 `.strm` 不拉取视频、HTTP HEAD 不读 payload、Range `206` 字节哈希、篡改签名 `403`、双卸载和任一挂载丢失的联动。就绪检查使用完整日志行区分 `/share` 与 `/share-strm`；这是自动化协议验证，不代替 Infuse 实机验收。

脚本还尝试 client 容器内 `mount.cifs`，以非零大偏移 `dd iflag=skip_bytes,count_bytes` 读取大于 cache 的区间并与源切片比较，验证随机 seek。宿主不具备 CIFS 能力时，明确打印 `host cannot mount CIFS` 并继续，由 required FUSE 测试（包括 `>4GiB` 虚拟文件大偏移）提供随机读取验证；挂载成功但数据错误则直接失败。显式跳过用 `TORRENTFS_SMB_SKIP_CIFS=1`，CI/nightly 不设置该变量，在有能力的 runner 上执行完整比较。

默认宿主/容器 UID 相同时容易掩盖 metadata 权限和清理问题；可指定不同运行身份：

```sh
TORRENTFS_SMOKE_UID=1500 TORRENTFS_SMOKE_GID=1500 ./scripts/docker-smb-smoke.sh
```

这两个变量只覆盖 smoke 的运行时环境，不是镜像 build args。

## 贡献与发布边界

欢迎提交问题和贡献。提交行为改动时，应更新对应详细指南和测试；README 只保留项目介绍、主要能力和完整首次使用入口。

CI nightly 的多平台 OCI 构建与本地 `scripts/nightly-build.sh` 归档脚本是不同入口。本文命令用于本地构建和验证，不将手工归档脚本写成 nightly 发布保证，也不假定存在浮动镜像 tag。
