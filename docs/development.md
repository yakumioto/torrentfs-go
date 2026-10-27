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
        ├── internal/filesystem   共享虚拟命名空间、只读 FUSE 树与 STRM 投影
        ├── internal/smb          只读 VFS 适配与显式 NTLMv2 服务配置
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

完整磁盘布局与恢复规则见[使用指南](usage.md)。启动先校验参数和配置，再创建 logger、恢复 session、按需启动 HTTP 与所选 FUSE/SMB。根目录手工放入的任意 `.torrent` 不会创建任务或进入 FUSE，也不会阻止 API 删除。

## 一致性与存储保证

### 原子管理状态

canonical metainfo 按 info hash 校验和命名。磁力意图先写 pending，metadata 完成后发布最终 metainfo 并清理 pending。分类索引和任务 sidecar 通过临时文件、`fsync`、原子 rename 写入。

上传限速保存后在原地调整同一个 limiter，不重建 client/torrent。rename 是提交点；rename 前失败不改变规则，rename 后目录同步失败返回 `applied: true` 的持久性未确认错误，不能当作新规则未生效。机器响应见 [API 参考](api.md)。

### 字幕发布与读取

字幕采用同目录临时文件 → `fsync` → 原子 rename → 目录 `fsync`。rename 前失败清理临时文件，已有旧字幕保持完整；如果是新建，目标仍不存在。rename 成功后内容已经生效，后续目录同步或校验只记 warning，不再报告写入失败，避免磁盘、内存 index 与调用方对发布结果产生分歧。发布的 size/mtime 取自 rename 前 staging 文件，rename 保持同一 inode。

字幕 `stat` 每次读取当前 size/mtime，不沿用 inode 首次 lookup 的长度。一次 `open` 从同一份已打开文件同时取得内容与 size/mtime；direct I/O 保证新 open 读取完整新版本，已有 handle 继续读原快照，不会出现旧长度配新内容。创建、可写 Open、Rename、Unlink 等操作仍返回 `EROFS`。

字幕上传和添加 torrent 共享 root namespace 锁，涵盖整个上传和磁盘 I/O，避免在过期布局上发布或返回错误的 mount_path；该锁独立于 Session 全局 mutex，不阻塞状态查询。

新增 torrent 的 namespace 校验在 final metainfo 写入前进行；magnet metadata 解析完成时也校验，不能留下绕过校验、重启后又恢复的文件。任务被删除时，字幕目录不存在且父目录完成同步后，operation 才进入 `deleted`；失败任务不进入可见 index，修复后可幂等重试。

### FUSE 视图与目录缓存

- 每个 Session `Torrent` 在 metadata 就绪后，一次遍历同时建立 display-path 索引和不可变 `[]FileView`，使用独立于 `Torrent.mu` 的初始化锁去重。metadata 未就绪不发布空 holder；同 hash 删除后重加是新 `Torrent` 和新身份。`Torrents()` 每次新建外层切片/结构体，实时组合 ready 状态、名称、分类、添加时间与字幕，只共享不可变 Files。内部持有 `Session.mu` 的调用使用 locked helper，不重入公开加锁方法；管理锁顺序仍为 namespace → per-hash → Session.mu。
- `FileListID` 是非零尺寸的不透明身份。非 nil `FilesID` 承诺 Files 切片永久只读；路径或大小变化必须发布新切片和新身份。nil 身份的 legacy Backend 通过精确内容比较与自有副本兼容，不能按切片地址判断是否变化。可选 `TorrentContentsBackend` 按 hash 返回一次当前内容快照；旧 Backend 每次目录操作最多扫描一次 `Torrents()`，不存在或关闭绝不回退旧 inode 的快照。
- 根目录、分类目录仍每次列出当前库并实时计算名称与冲突消歧，没有整库根名缓存。种子目录节点只保存 state/hash/prefix；Lookup、Readdir、Getattr 都按 hash 取当前内容，因此 go-fuse 复用旧 StableAttr/inode 后仍能看到上传的字幕。删除任务或移除最后一个成员的嵌套 prefix 后，旧目录查询返回 `ENOENT`。`EntryTimeout` 和 `AttrTimeout` 保持 0。
- 每个挂载私有缓存 `hash → 当前内容版本 → 已访问 prefix` 的排序子项与名称索引，直接复用 `childrenOfViews` 的优先级和纯字符串前缀规则。相同 FilesID 跳过 payload 比较，字幕逐字段精确比较；内容变化替换整个 hash 版本。Backend/StreamURL 在缓存锁外调用，首次构建按 hash/版本去重，迟到请求只能完成自己的快照，不能覆盖新版本或复活已删除缓存。
- 仅保留当前版本和实际存在的 prefix；列表同步或按 hash miss 清理已删除条目，不缓存缺失 hash/prefix。不回收稳定 inode 分配表 `inoByKey`，因此不能宣称整个挂载元数据内存严格有界。已打开的 DirStream 保持自己的成员列表和 offset；新 Lookup、新目录流观察最新内容。字幕没有单独删除 API，集合减少只由删除种子或单测 Backend 模拟。

### STRM 投影与签名读取

STRM Backend 包装现有 Backend，不另建一套 FUSE 节点，保留目录、非视频和原有 Category/Subtitle/SubtitleStat 可选能力的缺席语义。按 hash 的单种子投影不计算根名称，整库 `Torrents()` 才在自有外层切片上计算根命名。映射键包括源 FilesID 和 SingleFile：单文件根保留原 payload path，目录型才转换相对路径。受信任来源的完整投影通过只读切片和新的派生 FilesID 共享，热路径不逐项比较/复制整份文件列表；legacy 来源继续内容比较，并向外返回 Files/Subtitles 副本。

列目录只对长度未知的视频尝试一次签名。URL 失败保留 size=0 的条目，不发布不可变身份，后续操作继续重试；lookup/stat/open 明确返回实时错误，健康邻居不受影响，不永久缓存失败或在一次操作内循环重试。恢复或 live stat/open 发现 URL 文本长度变化时，发布新切片与派生身份，不原地改动旧快照；迟到回填核对当前 entry 与对应文件的尺寸版本，不能写进删除重建后的同 hash 条目，也不能因另一个文件的并发回填而丢弃有效结果。

payload live stat 与 `FileSnapshotBackend` 保持每次 URL 存活/ready 校验，同一次 open 的文本、reader 和长度绑定，不能改成先 stat 再 open。生成和读取 URL 文本不打开视频 payload。缓存的 passthrough 属性仍可在下一次列表/按 hash miss 清理前返回，真正打开始终交由 live Backend 决定。STRM mtime 沿用 torrent 添加时间；基址或密钥变化后需要客户端重新读取/扫描，不能假定热刷新。

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

正常关闭先取消 HTTP 流的 context，再给 HTTP Shutdown 最多 5 秒；超时以 HTTP Close 解除阻塞网络写。随后取消并排空 SMB 的连接和读取，再关闭 Session，最后卸载存在的 FUSE 挂载，整个 unmount 阶段共用 30 秒 deadline。强制 HTTP Close 不代表已等待所有 HTTP handler 返回，不能据此宣称所有 goroutine 都已结束。容器由 tini 直接管理单个 torrentfs，不再启动 Samba 或 shell supervisor，详见[部署指南](deployment.md)。

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

## 视图缓存回归与基准

- raw NodeFS/rawBridge 回归保留多级目录的旧 NodeId，先 Lookup/Readdir 再发布字幕；再次解析父路径须仍是同一 NodeId，直接通过旧 NodeId 的新 Lookup/Readdir 必须可见。原始和 STRM 两种 Backend 都覆盖，并检查已打开目录流的列表/offset 不改变。
- 真实 FUSE 回归在已访问目录及持有 dirfd 后上传字幕，覆盖深层目录、等长/变长替换、新查询与旧文件 descriptor 快照、删除后 `ENOENT`；普通测试缺少 FUSE 时会跳过，必须另跑下方 required 门禁。字幕集合减少在 Backend 单测模拟，不新增 API。
- Session/STRM/目录单测检查首次并发构建只发生一次、源/派生身份稳定、legacy 原地修改可见、可变 URL 长度与失败恢复、迟到结果不覆盖新版本，以及删除重加 churn 不保留旧索引；应使用 race 重复验证。

```sh
go test -race -count=3 -run 'TestPayloadHolder|TestFilesystemViews' ./internal/session
go test -race -count=3 ./internal/filesystem
go test -run '^$' -bench 'FilesystemView|TorrentLookup|STRMProjection' -benchmem -count=5 ./internal/session ./internal/filesystem
bash -n scripts/docker-smb-smoke.sh
```

基准合成 111 个种子、19792 个文件，最大种子 18920 个文件、216 个虚拟目录；Session 用真实 anacrolix metadata 走 `filesystemViewsLocked`，禁用网络发现/Tracker/WebSeed，不使用生产目录或凭据。分别报告冷/热 Session 视图、原始/STRM 根 Lookup、深层 Lookup/Readdir，以及冷/热 STRM 投影的 ns/op、B/op、allocs/op，并用 payload/index 构建、整库枚举和签名计数验证热路径。初始基线只依赖旧接口；接口落地后合成只读源显式提供 FilesID 和按 hash 内容，匹配 Session 的新契约。

性能比较必须使用相同库规模与调用方式，五次取中位数；合成基准不能代替隔离 FUSE/SMB 中相同 UID、目录、命令的完整路径/相对路径 stat、`ls -l`、`find`、`ls -lR` 对照，更不代表 Infuse 已验收。容器和 HTTP smoke 仅在获准的隔离环境运行；只做 `bash -n` 不算运行验证。

## 集成与容器验证

### HTTP 与配置

```sh
./scripts/http-smoke.sh
./scripts/docker-config-smoke.sh
```

HTTP smoke 需要 Docker、`curl` 和 `python3`，不需要 FUSE；检查静态 root/deep link、缺失 asset、认证 `401`、`WWW-Authenticate`、登录、列表、登出，不向远端 registry 发布内容。

Docker config smoke 还需要 `awk`、`timeout`；验证内置 TOML、默认 CMD、环境覆盖、外部只读 TOML、HTTP bcrypt 凭据覆盖、PUID/PGID 与目录权限。SMB 检查缺失/非法凭据、canonical 环境变量优先于 deprecated alias、环境优先于 TOML、非 Unix 登录名、STRM 布尔值与基址/HTTP 校验，以及 SMB 下拒绝任一挂载参数；断言无 FUSE/Samba、单 Go 进程和 SIGTERM 退出 `0`。

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

需要 Linux Docker daemon、`python3`、`sha256sum`、`dd` 和 `timeout`，不需要宿主 `/dev/fuse` 或 smbclient。应用容器没有 FUSE 设备、额外 capability 或放宽安全选项，并显式移除 `NET_BIND_SERVICE`；独立 client 镜像安装 smbclient 和 pinned go-smb2。必需验证包含自建 ranged webseed、`64MiB+123` 数据大于 `8MiB` cache 的全量哈希、大偏移 ReadAt/Seek/EOF、同连接 16 个缺来源读取并发、NTLMv2 domain/默认 domain、错误密码/guest 拒绝、目录列举、`.metadata` 隐藏、HTTP 认证和完整只读拒写（创建、writable open、append、truncate、write、mkdir、rename、delete、set-time）。这些协议/数据检查不可跳过。还断言无 smbd/无 FUSE mount、runtime UID/GID、tini 直接托管单 Go 进程、SIGTERM 退出 `0` 与 SIGKILL 退出 `137`。

STRM 场景保留原共享和第二只读共享、先访问多文件种子的顶层与深层目录再经认证 API 上传字幕，在两个共享重新列举、查询元数据、下载比对等长与变长替换；还检查 `.strm` 和 HTTP HEAD 不拉取视频、Range `206` 字节哈希、篡改签名 `403`、凭据不泄密与双共享关闭。它不代替 Infuse 实机验收。

内核 CIFS 仅是补充客户端：脚本尝试独立 client 容器 `mount.cifs` 后的大偏移与并发读取，应用权限不变。宿主无 CIFS/嵌套挂载能力时标记补充项未测；挂载成功但数据错误直接失败。`TORRENTFS_SMB_SKIP_CIFS=1` 仅跳过此补充项，不能跳过 smbclient/go-smb2 必需检查；CI/nightly 不设置它。`KEEP_SMOKE_ARTIFACTS=1` 保留失败日志与 fixture，所有运行容器、网络和镜像仍会清理。

默认宿主/容器 UID 相同时容易掩盖 metadata 权限和清理问题；可指定不同运行身份：

```sh
TORRENTFS_SMOKE_UID=1500 TORRENTFS_SMOKE_GID=1500 ./scripts/docker-smb-smoke.sh
```

这两个变量只覆盖 smoke 的运行时环境，不是镜像 build args。

## 贡献与发布边界

欢迎提交问题和贡献。提交行为改动时，应更新对应详细指南和测试；README 只保留项目介绍、主要能力和完整首次使用入口。

CI nightly 的多平台 OCI 构建与本地 `scripts/nightly-build.sh` 归档脚本是不同入口。本文命令用于本地构建和验证，不将手工归档脚本写成 nightly 发布保证，也不假定存在浮动镜像 tag。

`Nightly` 是开发镜像渠道名称，不保证每日发布。自动构建由 `main` 分支的 push 触发；没有 `main` 更新时不会每日自动重建。`workflow_dispatch` 保留为显式重建/重试入口，允许同一 SHA 手动重建。仅原仓库 `yakumioto/torrentfs-go` 的 `main` 可发布，现有质量门禁与唯一标签不变。
