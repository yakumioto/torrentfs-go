# torrentfs-go 下一轮迭代方向（v2：基于全量代码阅读）

## 0. 元信息与方法

- 仓库：`yakumioto/torrentfs-go`，基线 commit `28a19de`（分支 `fix/tracker-announce-liveness`）。
- 本版方法：5 个并行子代理分别**逐文件通读** `internal/session`、`internal/api|stream|auth|smb`、`internal/config|configctl|configcheck|configwrite|configmigrate|cmd`、`internal/vfs|filesystem|cache|piecestore|experiments|media`、`web/|CI|docs/`，再由本人对争议结论做二次抽查（读 `internal/session/file.go` 全文、`internal/cache/cache.go`、`internal/api/server.go`、`internal/smb/vfs.go`、`internal/stream/signer.go` 等）。
- 覆盖量：Go 非测试代码约 5.3 万行（session 2.9 万、config 1.1 万、api 6.1k、filesystem 4.6k、vfs 3.6k、smb 2.6k、cmd 3.1k），前端 88 个 `.ts/.tsx`。
- 统计口径：所有计数均排除 `.worktrees/work/`（那是 `feat/piece-download` 的 git worktree 副本，会把 test/benchmark 数量翻倍）。

> **v1 的教训**：上一版结论大量来自 `grep` 抽样与文件名推断，没有读完代码。本版第 3 节逐条列出被推翻或修正的结论；第 4 节起的建议已按更正后的事实重排。凡是无法从代码直接证实的，集中放在第 7 节。

## 1. 一页结论

1. 这个项目的工程质量显著高于"又一个 FUSE 挂载器"：配置 schema 单一真相源 + CI 逐字节校验、原子写回 + 基线冲突检测、SMB compound 读保序、短读防内核补零、连接配额按活动分档——这些是刻意设计并留下 ADR 的。
2. **真正的架构裂缝不在协议层，在"只读"这个自我描述上**：挂载面只读，但控制面写宿主磁盘（字幕内容、分类索引、收藏 sidecar、peer_id、锁、签名密钥）。v1 把项目当成纯只读系统，因此漏掉了整个控制面写路径与实验子系统。
3. **读路径有一处确定性的浪费**：每个 span 最多调 3 次 `cache.Get`，命中时复制两份整片（`internal/session/file.go:580` 与 `:620`），并把 `hits`/`misses` 计数放大 2–3 倍；而 `cache.Stats()` 从未被任何 API 消费。这是一次"改 20 行、去掉每读 1 次整片 memcpy"的机会。
4. **可观测性几乎为零，而且比"没有"更糟**：`/healthz`、`/readyz`、`/metrics` 会被 SPA 兜底返回 **200 + index.html**（`web/handler.go:31-44`），任何存活探针都会误判；版本号只在启动日志里恒为 `"dev"`；`http.Server` 只有 `ReadHeaderTimeout`。
5. **STRM 直链是最需要收口的授权面**：签名不绑任何主机/时间维度（HMAC 只覆盖常量域 + infohash + path，`internal/stream/signer.go:46-53`），链接永久有效且跨部署可复用；`/stream` 无并发上限；ETag 直接把签名回显给中间层日志（`internal/api/stream.go:82`）。
6. **实验子系统是产品的一部分，不是玩具**：它常开、默认 baseline、对 HTTP tracker 的上报 `uploaded` 做改写（inject/amplify 两种作弊模式），并有 JSONL ground-truth 记录。它对读路径零影响，但让"上传量"这个概念在 API 里同时存在"真实值"与"上报值"两种语义。
7. 前端不是"5 个页面很薄"：4 条路由 + 5 个页面组件，但 ConfigPage 1367 行（schema 驱动、热应用标注、409/503 语义、危险项二次确认、离开拦截），Dashboard 有统计/实验摘要/搜索/筛选/排序/批量清理，详情页有 pieces 缓存态图与字幕表。

## 2. 已实现能力（分层盘点）

### 2.1 挂载面：只读视图，但控制面可写

- `internal/vfs` 是协议无关的树模型，`Backend` 接口只有 `Torrents()` 与 `OpenFile()`/`OpenFileSnapshot()`，**没有任何写方法**（`internal/vfs/types.go:53-98`）。FUSE 与 SMB 都是适配器（`internal/filesystem/filesystem.go:15-32` 只做类型别名）。
- FUSE 权限：目录 `0555`、`Nlink=2`（`internal/filesystem/root.go:61-68`、`category_dir.go:40-44`、`torrent_dir.go:23-32`），文件 `0444`/`Nlink=1`（`torrent_file.go:27-46`）。写入类操作**显式返回 `EROFS`**（`root.go:144-179`、`category_dir.go:112-130`、`torrent_dir.go:99-117`）；`open(O_TRUNC)` 与非 `O_RDONLY` 被拒（`torrent_file.go:64-70`）。
- 未实现的是另一批：`Statfs`、`Setattr`、`Flush`、`Fsync`、xattr、`Link/Symlink/Mknod`、`OpenDir`、`Lseek`——它们落到 go-fuse 默认 `ENOSYS`。因此 `truncate(2)`/`chmod`/`utimens` 得到的是 `ENOSYS` 而不是 `EROFS`。
- 短读保护只在 FUSE 读句柄层（`torrent_file.go:159-167`），条件是"未到文件尾"；映射后的 errno 是 **`EIO`**（`errno.go:61-62` 默认分支），因为没有 `EUNEXPECTEDEOF` 这种东西。`vfs.Tree.Handle.ReadAt` 是纯透传，没有这层保护。
- 字幕文件用 `FOPEN_DIRECT_IO`（`torrent_file.go:96`），但只对字幕；STRM 投影出来的 `.strm` 是普通 payload，会进内核页缓存。
- 树的结构：根下是"分类目录 + 未分类种子"；**单文件种子直接暴露为根下文件而不是目录**（`internal/vfs/layout.go:127-132`）；字幕在种子目录内按相对路径出现，未分类单文件种子还会在根下多一份（`tree.go:386-398`）。收藏**不是文件系统概念**，只用于豁免按龄清理（`internal/session/registry.go:58-61`、`favorite.go:13-20`）。
- `vfs` 的 inode 表（`tree.go:63,75-84`）与 FUSE 的 `inoByKey`（`filesystem.go:53,75-84`）**只增不删**，删除种子后条目永不回收——长跑进程的单调增长点。

### 2.2 读路径：每片一个 flight，缓存整片，预取按播放窗口

- 生产用的 loader 是 `pieceFetcher`（`internal/session/torrents.go:325-330`）：每个缺失 piece 一个 flight，每个 flight 新建 `torrent.NewReader()` 且 `SetReadahead(0)`（`file.go:181-183`），多个等待者共享（`file.go:148-177`）；最后一个等待者离开才取消（`file.go:215-227`）。
- `pieceLoader`（整 torrent 单 reader + 容量 1 的 admission permit，`file.go:249-401`）**只在测试里被引用**，生产不可达。
- 短读重试 5 次 × 200 ms（`file.go:673-699`），重试单位是整片；外层每次 `ReadAtContext` 有一个整体 deadline，来自 `filesystem.read_timeout`（默认 30 s，`file.go:464-476`、`internal/config/filesystem.go:34`）。
- 缓存：全 session 单例、单把 `sync.Mutex` 的字节容量 LRU（`internal/cache/cache.go:80-106`），`highWater=7/8`、`lowWater=3/4`（`cache.go:36-41`），驱逐跳过 pinned 且永不驱逐刚插入项（`cache.go:435-461`），pin 预算 `capacity - LowWater`（`cache.go:74-76`）。`Get` 返回**副本**（`cache.go:142`），`Has` 是不改动 LRU、不计命中的探测（`cache.go:122-127`）。
- 预取：每种子一个 `prefetchCoordinator`（`torrents.go:371-373`），前向窗口 32 MiB（`prefetch.go:16`），低水位 `min(16 MiB, high/2)` 且会被 pin 预留压缩（`prefetch.go:923-933`）；seek 候选需**2 个不同 ticket、各自带来新增字节、起点在 ±8 MiB 内**（`prefetch.go:498-504`、`612-636`），anchor 漂移 >64 MiB 时清空候选（`prefetch.go:607-609`）。
- 预取并发上限是 **全 session 共享的 4 片**（`session.go:381` + `prefetch.go:23,262-265`），不是每种子 4 片。
- `raFile.readahead`（8 MiB，`file.go:51`）是**死配置**：唯一消费点在 `coordinator == nil` 分支（生产不可达），且 `pieceFetcher.ReadAtContext` 的第四个参数直接丢弃（`file.go:103`）。
- piecestore 不落盘：peer 写进 per-piece staging，哈希校验通过后 `cache.Put` promote（`internal/piecestore/store.go:1-8,416-483`）；staging 不计入缓存容量（`store.go:236`）；`pieceLength > capacity` 会让该种子**无法打开**（`store.go:146-151`），`> LowWater` 只告警（`store.go:152-157`）。

### 2.3 控制面：API 层的真实写路径

21 条路由（`internal/api/server.go:122-142`），其中 18 条被前端调用，3 条 piece-download 端点前端未接入。写操作全部落在宿主机 `<torrents-dir>` / `.metadata`：

| 写什么 | 落点 | 证据 |
| --- | --- | --- |
| 字幕内容 | `<metadata>/subtitles/<hex>/<video dir>/<stem>.srt\|ass\|vtt`，`chmod 0444`，rename 即提交点 | `internal/session/subtitle.go:477-684`、`subtitle_store.go:105-263` |
| 分类索引 | `.metadata/categories.json` | `internal/session/session.go:395`、`category.go:74-83` |
| 收藏/分类/错误/操作 sidecar | `.metadata/state/<hash>.json` | `internal/session/registry.go:88-115` |
| metainfo / pending 磁力 | `<torrents-dir>/<hash>.torrent`、`.metadata/pending/<hash>.magnet` | `internal/session/metadata.go:32-42` |
| peer_id / 实例锁 / 签名密钥 | `.metadata/peer_id`、`.metadata/instance.lock`、`.metadata/stream_key` | `identity.go:28-30,93-104`、`stream.go:19,27-35` |
| announce 日志 | 配置 `experiments.upload.announce_log` 指定的 JSONL | `internal/experiments/recorder.go:36-42` |

- 字幕**没有独立索引文件**：启动时扫描目录 + `stat` 重建（`subtitle.go:802-871`），`walkFiles` 顺手清理遗留 staging（`subtitle_store.go:289-318`）。
- 字幕只有上传/覆盖（PUT），**没有删除/重命名 API**（`/api/v1/torrents/{id}/subtitles` 仅 PUT），删除种子时才随之清理。
- 分类只有 `GET/POST`，没有改名/删除（`server.go:126-127`）。
- prune：只清理"未收藏且超期"的种子（`favorite.go:76-129`）；显式删除不因收藏而跳过。
- 删除事务：先同步 `AnnounceStopped`（有界 15 s，失败只记 operation.warning），再删 metainfo/magnet/字幕/sidecar（`manage.go:612-713`，ADR-0008）。

### 2.4 实验子系统（v1 完全遗漏）

- 它改写的不是真实流量，而是 **HTTP tracker announce 请求里的 `uploaded` 参数**（`internal/experiments/reporter.go:128-151`），真实计数器不动。
- 三种模式（`settings.go:17-31`）：`baseline`（控制组，不改写，只记录）、`inject`（按时间 × rate 伪造，与真实流量无关）、`amplify`（按真实增量的 multiplier 放大）。
- 常开：reporter 总是构造并挂到 `cc.HttpRequestDirector`（`session/session.go:254-256,344`），默认 `baseline`；只有配了 `announce_log` 才产生文件写入与 peer 采样。
- 只对 HTTP(S) tracker 生效；UDP tracker 不经这个 hook。
- 附带一个 ground-truth 记录器：`settings/announce/peers` 三类 JSONL（`recorder.go:12-17`），含 `real_uploaded` 与 `reported_uploaded`。
- 旧 `lab_settings.json` sidecar 已退役，只保留启动迁移读取（`internal/experiments/legacy.go:12-16`）。

### 2.5 配置与运维

- 单一 schema：`Config` 结构体 + 5 种 tag（`toml/enum/secret/component/hotapply`）是唯一真相源（`internal/config/config.go:108-313`），env 名机械派生为 `TORRENTFS_` + 大写点路径（`schema.go:275-277`），默认值只在 `Default()` 定义（`config.go:316-357`），生成物（example TOML、镜像 overlay、README 速查、`schema-documentation.json`）由 CI 逐字节比对（`scripts/config-docs-check.sh`）。
- **42 个叶键**、5 个分组、1 个 secret（`password`）、3 个 enum（`backend`、`smb.strm`、`experiments.upload.mode`）。
- 热应用 **14 个键**（schema 口径）；HTTP 关闭时实际只有 12 个，因为 `http.max_upload_size` 与 `http.auth.token_ttl` 的 applier 只在 HTTP 启用时注册（`cmd/torrentfs/main.go:268-291`、`configctl/controller.go:185-188`）。
- 写回是**稀疏**的：只渲染文件来源的键 + 本次改动键，env 来源的值永不落盘（`configwrite/writeback.go:92-105`）。
- 保存三阶段：整体校验 → 原子写回（临时文件 + fsync + rename + 目录 fsync）→ 热应用；rename 后目录 fsync 失败返回 `durability_unconfirmed`（`config/writefile.go:71-106`、`configctl/save.go:257-301`）。
- 409 不只代表冲突：`conflict`、`read_only`、`not_configured` 都是 409；503 对应 `persistence_unavailable` 与 `durability_unconfirmed`（`internal/api/config.go:158-167`）。
- 单实例锁是 advisory `flock`（`identity.go:89-115`），崩溃即释放；退出顺序 SMB(10 s 超时) → HTTP(无超时) → session → FUSE unmount（共享一个 30 s deadline，`cmd/torrentfs/main.go:439,471-565`）。
- 版本号：**只在启动日志里**，恒为 `"dev"`（`cmd/torrentfs/main.go:203`）；构建只传 `-s -w`，无 `-X`、无 `debug.ReadBuildInfo()`；`/api` 响应里没有版本；`User-Agent` 是配置项（默认伪装 `qBittorrent/4.4.0`）。

### 2.6 安全面

- 认证：token 32 字节随机、以 sha256 为 key 存在内存 map、滑动 TTL；**重启全部失效**（`internal/auth/service.go:66,159-204,242-244`）。bcrypt 成本下限 10 只在启动校验，配置侧生成固定 `DefaultCost`，不能调高（`service.go:21,88-94`、`config/credentials.go:19`）。
- 登录无失败计数/锁定/退避（`service.go:146-183`）。
- **认证关闭时 20 条管理路由全部匿名可达**（`internal/api/server.go:236-239` 直接放行），包括 `PUT /config` 与 `DELETE /torrents/{id}`。只有在这种部署下暴露非回环地址才被配置校验拦住（`config.go:37`）。
- HTTP 层除认证外**没有任何中间件**：无压缩、无 CORS、无安全响应头、无 CSRF token（但认证只用 `Authorization` 头，结构性规避了 CSRF）；`http.Server` 只设 `ReadHeaderTimeout: 10s`（`server.go:145-148`），没有读/写/空闲超时与 `MaxHeaderBytes`。
- SMB：2.1–3.0.2、`SigningRequired: true`、`EncryptionSupported: true`（**支持而非强制**）、`MaxConnections: 32`、`MaxReadSize 1 MiB`、写操作一律 `STATUS_ACCESS_DENIED`；协议日志被无条件丢弃防 NTLM blob 泄漏（`internal/smb/server.go:43-88,114-123`）。

### 2.7 前端

- 4 条路由 + 5 个页面组件：LoginPage 是 `auth.phase` 门禁而不是路由（`web/src/app/App.tsx:15-33`）。
- Dashboard：运行时统计 + 实验摘要 + 搜索 + 4 种筛选（含收藏）+ 4 键排序 + 批量清理 + 添加。
- 详情页：概览 / 文件·字幕 / 数据块（pieces 缓存态图）三个 Tab。
- ConfigPage 1367 行：schema 驱动、热应用 badge、环境覆盖标记、secret 标记、diff 预览、危险项二次确认、未保存离开拦截、409 冲突不覆盖、503 两种语义分开展示。
- 轮询：列表/status/stats 5 s、experiments 2 s、config 2.2 s、operation 1.5 s，页面隐藏即停（`web/src/queries/hooks.ts`、`utils/visibility.ts`）。
- token 存 `sessionStorage`（标签页级）。

### 2.8 测试与 CI

- 224 个 `_test.go`（`internal/session` 60 个），含进程内真实 peer swarm 与 BEP 27 正对照。
- CI：`go test ./...`、`go test -race ./...`、`golangci-lint`（源码构建 v2.12.2）、config-docs 逐字节、Docker 配置冒烟、Docker SMB 冒烟、真实 FUSE 挂载；另有一套 `.gitea/workflows/ci.yml`（4 个 job，无 Docker 冒烟）。
- `scripts/http-smoke.sh` 与 `scripts/docker-smoke.sh` **没有被任何 workflow 调用**。
- 0 个 fuzz；3 个 benchmark（2 个文件），**没有任何 CI job 运行 benchmark**。

## 3. v1 结论更正表

| # | v1 的说法 | 实际情况 | 证据 |
| --- | --- | --- | --- |
| 1 | "FUSE 只读节点（0444）" | 目录 `0555`、`Nlink=2`；文件才是 `0444` | `filesystem/root.go:61-68`、`torrent_file.go:27-46` |
| 2 | "短读转 `ErrUnexpectedEOF`" | 只在 FUSE 读句柄层、且仅"未到文件尾"时升级；映射成 **`EIO`**；vfs 层无此保护 | `torrent_file.go:159-167`、`errno.go:61-62`、`vfs/tree.go:565-587` |
| 3 | "预取低水位 16 MiB" | 是上限：`min(16 MiB, high/2)`，还会被 pin 预留压缩 | `prefetch.go:923-933` |
| 4 | "seek 需连续 2 次落在 ±8 MiB" | 是"2 个不同 ticket、各自带来新增字节"，不要求连续；候选在漂移 >64 MiB 时清空 | `prefetch.go:498-504,607-636` |
| 5 | "budget 4 片" | 是**全 session 共享** 4 片，且同时限制 active lease 数 | `session.go:381`、`prefetch.go:23,893` |
| 6 | 把 8 MiB readahead 当作预取机制 | 该字段是死配置（生产分支不可达、loader 参数被丢弃） | `file.go:51,103,561`、`torrents.go:371-373` |
| 7 | "探针 Get 多一次拷贝" | 命中时**两次**整片复制（`file.go:580` + `:620`），未命中**三次** `Get`；hits/misses 被放大 2–3 倍 | `file.go:579-585,620,639` |
| 8 | "分片下载默认 600 s、上限 3600 s（session）" | 这两个常量在 HTTP 层；session 接受任意 timeout | `internal/api/piece_download.go:45-47,110-118`、`session/piece_download.go:204-207` |
| 9 | "14 个可热应用键" | schema 口径 14，HTTP 关闭的部署实际 12 | `configctl/controller.go:185-188`、`main.go:268-291` |
| 10 | "配置页写回…冲突 409" | 409 还覆盖 `read_only` 与 `not_configured` | `api/config.go:158-163` |
| 11 | "版本出现在 /api 与 User-Agent" | `/api` 无版本；User-Agent 是配置项；版本只在启动日志且恒 `"dev"` | `main.go:203`、`session.go:294-295` |
| 12 | "Web UI 只有 5 个页面，功能很薄" | 5 个页面组件但只有 4 条路由；ConfigPage 1367 行，Dashboard 有统计/实验/筛选/排序/批量清理 | `web/src/app/App.tsx:15-33`、`pages/ConfigPage.tsx` |
| 13 | 未提及字幕/收藏/分类/实验的前端 | 四者都有界面：星标、分类对话框、字幕上传/表格、实验摘要卡（2 s 轮询） | `TorrentRow.tsx:41-100`、`CategoryManagementDialog.tsx`、`UploadSubtitleDialog.tsx`、`ExperimentSummaryCard.tsx` |
| 14 | 未提及实验子系统 | 常开的 announce 上报注入（baseline/inject/amplify）+ JSONL 记录 | `internal/experiments/*`、`session/session.go:254-256,344` |
| 15 | "只读文件系统"（整体判断） | 挂载面只读；控制面写宿主磁盘（字幕内容、分类、sidecar、密钥） | 见 §2.3 |
| 16 | 把 `pieceLoader` 当生产读路径 | 生产是 `pieceFetcher`（每片一个 reader）；`pieceLoader` 仅测试引用 | `torrents.go:325-330`、`file.go:181-183` |
| 17 | "无 /healthz"（隐含"探针会 404"） | 返回 **200 + index.html**（SPA 兜底），探针会误判为存活 | `web/handler.go:31-44` |
| 18 | "STRM 签名只绑 domain+infohash+path" | 不绑 domain/host/端口；只绑常量版本域 + infohash + path，链接跨部署可用 | `stream/signer.go:16,46-53` |

## 4. 真实缺口

### 4.1 读路径与缓存

- `cache.Get` 的复制语义让读路径固定付出 2 次整片分配；`Has` 已经存在却没被用在这两处（`file.go:580,620`）。
- `cache.Stats()`（Hits/Misses/Evictions/PutRejections/PinReservationRejections）从不被消费，且因为上面的重复 `Get` 本身就是脏数据。
- `Pin`（非 sized）对未驻留 key 预留 0（`cache.go:248-250`）；`remove` 不归还 sized 预留（`cache.go:500-502`），`PinnedBytes()` 会大于实际驻留 pinned 字节——语义上正确，但很容易被误读成泄漏。
- `vfs` inode 表与 FUSE `inoByKey` 只增不删，长期运行单调增长。
- 预取状态机复杂（1275 行），参数全为常量，且 `foregroundCancels` 是恒 0 的死计数器（`prefetch.go:214`）。

### 4.2 可观测性与运维

- 没有健康/就绪端点，且 SPA 兜底会让任何探针拿到 200。
- 没有版本注入，事故报告无法对应构建。
- `/api/v1/stats` 只有 5 个数字（startedAt、cache used/capacity、downloaded、uploaded，`session/runtime_stats.go:8-37`），没有命中率、没有预取、没有 peer 数、没有 per-torrent。
- `http.Server` 无读/写/空闲超时（`api/server.go:145-148`）。
- 上传量是进程内计数，重启归零；而实验又会改写"上报值"，两者在 API 里没有区分。

### 4.3 流式与授权

- STRM 签名无过期、无主机绑定；`/stream` 无并发/速率限制；ETag 回显签名（`api/stream.go:82`）。
- 认证关闭时管理面全匿名（`api/server.go:236-239`）。
- 登录无失败限流。

### 4.4 产品面

- 字幕能传不能删、不能改名，没有独立管理页；索引靠启动扫描重建。
- 分类只能新建与赋值，不能改名/删除。
- piece-download 三个端点前端完全没接（`web/src/api/client.ts` 无引用）。
- 实验只对 HTTP tracker 生效，UI 未标注这一限制；announce 日志格式没有 schema 文档。

### 4.5 协议与生态

- v2-only 磁力被 `missing info hash` 拒绝（见 §6.5）。
- 无 TLS；只有上传限速（会话级聚合、同日时间窗、不支持跨午夜）。
- webseed 由 metainfo `url-list` 隐式启用（`spec.Webseeds`，上游 `DisableWebseeds` 默认 false 且本项目从不设置），没有开关、没有流量区分。
- DHT/PEX/加密/ipfilter 没有显式开关；SOCKS5 模式下强制关闭 uTP 与 DHT（`session/proxy.go:77-79`）。

### 4.6 工程质量

- 0 fuzz；benchmark 不跑 CI；`http-smoke.sh`/`docker-smoke.sh` 未被任何流水线调用。
- 依赖两个 fork（`yakumioto/torrent`、`third_party/go-smb`），是长期维护负债。

## 5. 优先级表

成本：XS（≤2 h）/ S（≤1 人日）/ M（2–5 人日）/ L（>5 人日）。

| # | 方向 | 证据 | 收益 | 成本 | 风险 |
| --- | --- | --- | --- | --- | --- |
| 1 | 读路径缓存查询收敛：探针改 `Has`，`span` 内复用同一次 `Get` 结果，评估"返回内部切片 + 显式生命周期"是否可行 | ★ | 每次命中读去 1 次整片 memcpy，hits/misses 恢复真实 | S | 零拷贝要动 `Pin`/驱逐不变式，先做前两步 |
| 2 | 可观测性最小面：`/healthz` `/readyz`（必须排在 SPA 兜底之前）、`-X` 版本注入、`cache.Stats` + `prefetch.snapshot()` 进 `/api/v1/stats`、`http.Server` 超时 | ★ | 容器可编排、事故可定位 | S | 预取计数器当前有意不导出，需决策 |
| 3 | STRM 链接收口：签名加 `exp`、每链接并发上限、ETag 不回显签名 | ★ | 堵住永久免认证读与单链接打满 | M | 破坏性：已发出的 `.strm` 会失效，需要 TTL 默认值与迁移说明 |
| 4 | 字幕能力补齐：`DELETE /subtitles`、可选 `PUT` 改名、管理页；索引改 sidecar（现在是启动扫描） | ★ | 用户能治理自己上传的字幕 | M | 涉及命名空间冲突校验（`subtitle.go:733-773`） |
| 5 | v2-only：XS 先把 `missing info hash` 换成 `unsupported_v2_only` 并回显 `btmh`；M 再做真支持 | ★ | 消除误导；M 完成即可加 v2-only 种子 | XS / M | 截断 hash 已贯通 storage（`torrent.go:560`），无需迁移；风险在 swarm 规模 |
| 6 | 预取参数进 schema（窗口/低水位/字节预算），budget 从"4 片"改"字节"，顺手删掉死 readahead 字段 | ★ | 不同 pieceLength 行为一致；4K 高码率可调 | M | 参数过多会违背"少旋钮"的克制；`prefetch.go` 复杂度高 |
| 7 | 实验可观测性：UI 标注"仅 HTTP tracker"，announce 日志加 schema 文档与轮转 | ★ | 实验结论可复现、磁盘不失控 | S | 无 |
| 8 | piece-download 接入 UI（目前在 API 层完全不可达） | ★ | 已有能力不再闲置 | S | 无 |
| 9 | `/status` 的 pieces 裁剪（`?pieces=summary\|range`）+ 列表分页 | ★ | 轮询成本从 MB 降到 KB | S–M | 新参数默认关闭即兼容 |
| 10 | inode/reference 表回收（LRU 或按种子清理） | ★ | 长跑内存不再单调增长 | M | 要保证 FUSE 的 `Nlookup`/句柄语义 |
| 11 | 登录失败限流 + 开启无认证时的显式告警 | ★ | 堵暴力破解；避免"忘记开认证"的裸奔 | S | 无 |
| 12 | fuzz（磁力/STRM 路径/bencode）+ benchmark 进 CI | ★ | 回归防护；性能主张有基线 | S | 无 |
| 13 | 上传量持久化：**先区分真实值与上报值**再谈持久化 | ★ | 私有 tracker 分享率记账 | M | 实验改写让"上传量"有两个语义，不能只存一个数 |
| 14 | webseed 显式化：`DisableWebseeds` 开关 + 流量计数 + UI 标注 | ★/☆ | 冷门种子起播率；把隐式 HTTP 拉取变可控 | S–M | 需要一次真实 webseed 种子验证（§7） |

## 6. 下一步就该做的 5 个

### 6.1 读路径的两次 `Get`（S，收益确定）

`internal/session/file.go:579-585` 用 `cache.Get` 判断"这片在不在"，丢弃返回值；`file.go:620` 又 `Get` 一次拿数据。命中时复制两份整片，未命中时 `misses` 加三次。改法：探针换 `cache.Has`，把 `span` 的 `Get` 结果直接用于 `pieceMiss` 判断，命中路径就只剩一次复制。

### 6.2 可观测性最小面（S，但要注意 SPA 兜底）

`/healthz` 必须注册在 `dispatchAPIAndStatic` 的路由判定里、排在 `web.Handler()` 之前，否则会拿到 200 + HTML。版本用 `-X main.version` 注入，并在 `/api/v1/stats` 里回显。`cache.Stats()` 与 `prefetch.snapshot()` 直接进 stats 响应——但先做 6.1，否则命中率是脏数据。

### 6.3 STRM 收口（M，破坏性）

`signer.go` 的 HMAC 加上 `exp` 与主机绑定；`handleStream` 加一个全局并发信号量；ETag 改成不透明哈希而不是签名本身。

### 6.4 字幕管理（M）

补 `DELETE`，把启动扫描换成 sidecar 索引（或至少在扫描时落一份缓存）。没有删除接口这件事，在"传错字幕"的场景下很刺眼。

### 6.5 v2-only：先改报错，再决定是否支持（XS / M）

见 §3 与优先级表。关键事实：上游把 `canonicalShortInfohash()` 交给 storage（`torrent.go:560`），所以底层键空间天然兼容；真支持只需 `canonicalHash(spec)` + 6 处比较替换 + 允许 v1 为零 + 测试。**不支持的唯一理由是需求侧**（v2-only 种子少、对端支持 BEP 52 的少），不是技术障碍。

## 7. 未验证项

1. 真实播放器（Infuse/VLC/Plex）的读模式：是否 128 KiB 顺序读、seek 时是否重开文件——决定预取窗口与 seek 判据是否值得调。（验证：抓 FUSE 请求序列或加读探针）
2. webseed 种子在真实网络下的占比与起播收益。（验证：找 2–3 个带 `url-list` 的种子，对比开/关 `DisableWebseeds`）
3. 实验数据是否已有人消费（announce 日志有没有被外部分析流程读取）——决定它是"产品特性"还是"该收进构建标签的实验代码"。
4. SMB 3.1.1 与 Infuse 的断连是否由本项目响应引起（`server.go:52-54` 注释自述未定）。
5. 长跑（数周、数千种子）下 inode 表的实际增长速度。

## 8. 曾经列出、但站不住或需要降级的

- **"v2-only 加种成功但读不出来"**：错，入口就拒绝（`manage.go:85-91`）。已在 v1 修订中纠正。
- **"真支持 v2 是 L 级、要迁移"**：错，见 §6.5。
- **"Web UI 功能很薄"**：错，ConfigPage 与 Dashboard 的复杂度远超"薄"。
- **"只读文件系统，所以没有写路径"**：错，控制面写宿主磁盘。
- **"8 MiB readahead 是预取机制"**：错，死配置。
- **"缓存命中率可以从 /api/v1/stats 观察"**：错，Stats 没有被消费。
- **"上传量持久化"作为独立建议**：需要先解决"真实 vs 上报"两个语义，否则存下来的数是错的。

## 9. 已定的决策（本轮 grill 的结果）

用 `/grill-with-docs` 逐分支过了一遍设计树，结论如下；已立项的给出 issue 号，未立项的保留在此处以便下次接续。

**术语（已落进 `GLOSSARY.md`）**：新增「挂载树」「控制面」「缓存」「存在性检查」「累计上传量」；首句的"只读"限定到挂载树；「真实上传量」改为本次进程口径，「上报上传量」改为"真实 → 叠加累计基线 → 实验改写"。

| 决策 | 结论 | 状态 |
| --- | --- | --- |
| 本轮范围 | 只做纯增量项；破坏性项各自独立 | 已定 |
| 读路径深度 | 只换存在性检查 + 复用单次取片，不做零拷贝 | [#112](https://github.com/yakumioto/torrentfs-go/issues/112) |
| 可观测性范围 | 健康端点 + 版本注入 + 缓存/预取计数；不做 Prometheus | [#113](https://github.com/yakumioto/torrentfs-go/issues/113) |
| `/readyz` 判据 | HTTP 可服务且输出后端就绪；不含持久化软状态 | [#113](https://github.com/yakumioto/torrentfs-go/issues/113) |
| 服务端超时 | 补 Read/Idle，**不设全局 WriteTimeout**（会掐断 `/stream`） | [#113](https://github.com/yakumioto/torrentfs-go/issues/113) |
| ETag | `sha256(infohash + 路径)`，与签名无关 | [#114](https://github.com/yakumioto/torrentfs-go/issues/114) |
| v2-only | 先改报错文案，保持响应形状 | [#115](https://github.com/yakumioto/torrentfs-go/issues/115) |
| STRM 过期 | 硬切，不做双版本宽限；TTL 进 schema 默认 720h、重启生效 | [#116](https://github.com/yakumioto/torrentfs-go/issues/116) |
| STRM 并发 | 硬编码 8，503 + `Retry-After`，不开配置 | [#116](https://github.com/yakumioto/torrentfs-go/issues/116) |
| 字幕 | 只补删除（含 UI 入口），不改索引、不做改名 | [#117](https://github.com/yakumioto/torrentfs-go/issues/117) |
| 预取参数 | 暴露 `window_bytes` 与 `budget_bytes`，预算从片数改字节 | [#118](https://github.com/yakumioto/torrentfs-go/issues/118) |
| 登录限流 | 按来源 IP 指数退避，429 + `Retry-After` | 待立项 |
| 上传量跨重启 | 引入「累计上传量」，落 registry sidecar，announce 顺序为真实→累计→实验 | 待立项 |
| `/status` pieces | 默认不变，新增 `pieces=summary` 与 `pieces=range` | 待立项 |
| inode 回收 | 只回收 `vfs` 侧；FUSE 侧保持只增并写明上限 | 待立项 |

**被证据否掉的两个初始设想**：

1. "未启用认证时的显式告警"——配置校验已经拒绝"非回环地址 + 未启用认证"（`internal/config/config.go:39` `errExposedNoAuth`），所以这条只剩限流本身。
2. "字幕索引改 sidecar"——启动扫描是 O(字幕文件数) 且自愈，sidecar 反而要处理"用户手删文件"的不一致，不划算。
