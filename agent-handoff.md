# Agent Handoff

## Task

**MIO-43**（issue `01a0bc74-bd99-77b2-88c3-816f26efd320`）：排查 torrentfs-go 无法正常获取 torrent 数据。

本轮（Implementer）任务目标：按已获人工批准的 Orchestrator 执行计划，完成 **Phase 1–3 的实现与离线测试**，完成后交 Reviewer。

计划中实现类 AC（逐条在 `## AC Evidence` 取证）：

- P1.1 `session ready` 追加 `effective_listen_port` 与 listener 地址，不改既有 `listen_port` 语义
- P1.2 DHT 每族结构化状态；每族「无 starting nodes」由每分钟 ERROR 降级为一条有界 WARN + 状态名
- P1.3 脱敏补 `credential|passkey|authkey`、URL 查询串、`req` 结构体
- P1.4 status DTO 追加 peer/conn 计数与每族 DHT 状态，不改 `ready` 语义
- P2.1 `connections.disable_ipv4` / `disable_ipv6`（禁止同时关闭）
- P2.2 `connections.no_port_forwarding`（默认 true）
- P2.3 `connections.bootstrap_nodes`（按族过滤）
- P2.4 Docker 固定并同时发布 TCP+UDP peer 端口 + 文档
- P3.1 peer ID 原子持久化（损坏即报错退出）
- P3.2 `<torrents-dir>/.metadata/.lock` 进程级 flock 单实例

计划明确要求提供的逐条测试证据见 `## Verification`。

## Plan

依据：Orchestrator 附件 `agent-handoff.md`（`Approval: REQUIRED`，已由 yakumioto 于 2026-09-20T02:19:13Z 批准，Squad Leader 于 02:19:24Z 交接）。

基线：`origin/main @ 171804b`（MIO-42），依赖锁定 `anacrolix/torrent v1.61.0` / `anacrolix/dht/v2 v2.23.0`，Go 1.27。

关键约束（均已遵守）：

1. `configureDhtStartingNodes` 必须继续先调用既有 `ConfigureAnacrolixDhtServer`，否则会覆盖测试注入。
2. 新增 TOML 键必须同时进入 `Config` 结构体、`torrentfs.example.toml`、`docker/torrentfs.toml`（严格解码）。
3. 新环境变量必须显式注册在 `environmentBindings`，否则 `TORRENTFS_*` 静默无效。
4. 测试 seam 固定为 `internal/session/export_test.go` 的 `NewWithClientConfig` / `TorrentClientConfig`；普通 session 测试会真实启动 DHT/UPnP，新测试必须显式关闭。
5. 质量门与 CI 一致：`go build` / `go vet` / `go test` / `go test -race` / `golangci-lint`（v2.12.2）+ 前端四件套（涉及前端时）。
6. 非目标：不做空-peers 强制重试、不改 `ready` 语义、不删除 MIO-42 地址族过滤、不做 per-torrent DHT/PEX 开关、不把「首次读才 announce」当修复项。
7. Phase 0 运维动作（凭据轮换、停多余实例）不由本 Agent 代为执行；`private=1` 与 DHT 冲突仅记录。

## Changes

### Phase 1 — 诊断与脱敏

- `internal/session/session.go:310`：`session ready` **追加** `effective_listen_port`（`Session.EffectiveListenPort()` → `Client.LocalPort()`）与 `listen_addrs`（`Client.ListenAddrs()`）。既有 `listen_port` 仍记录配置值，未改名、未删除。
- `internal/session/dht.go`：新增 `dhtFamilyState` / `dhtRecorder`（`:37-116`）。`configureDhtStartingNodes(cc, recorder, logger)`（`:118`）在 wrapper 内记录每族 `family` / `local_addr` / `resolved` / `kept` / `err`；`observe` 只在**状态变化**时输出：健康→（从不可用恢复时）一条 INFO、不可用→一条 WARN 且带 `reason`（无 err 时为 `dht_<family>_unavailable`），重复同状态保持静默。`snapshot()` 供 status API 使用。既有先调用 `existing(server)` 的顺序保持不变。
- `internal/logging/logging.go`：`credentialPattern` 补 `credential|passkey|authkey`；`sensitiveKey` 统一判定；`redactURL` 除 userinfo 外清洗敏感查询参数；新增 `redactRequest` 处理 `*http.Request` / `http.Request`（输出 `METHOD url`，保留 host）。另新增 `demotingHandler`（`:74-107`）：把 upstream 每分钟重复的 `error bootstrapping during bucket refresh` 记录降级到 `debug`，并在此级别低于 handler 阈值时直接丢弃，使默认级别下**只保留一条有界 WARN**；debug 级别仍可见。
- `internal/session/backend.go`：新增 `NetworkStatus`（`:50-68`）并作为 `TorrentStatusView.Network`（`:48`）由 `TorrentStatusFor` 填充（`:144` → `networkStatus` `:185`）；`dhtStatus`（`:205`）把 recorder 快照与 `Client.DhtServers()` + `Server.Stats()` 的实时节点数合并，可区分「该族无 server」与「有 server 但路由表为空」。`ready` / `metainfo_ready` 计算路径未改。
- `internal/api/handlers.go:47-77`：`torrentStatusResponse` **追加** `network` 对象（`effective_listen_port`、`total_peers`、`pending_peers`、`active_peers`、`connected_seeders`、`piece_complete`、`dht[]`），映射函数 `newNetworkStatusResponse`（`:120`）。

### Phase 2 — 环境适配

- `internal/config/config.go:154-179`：`Connections` 新增 `disable_ipv4` / `disable_ipv6` / `no_port_forwarding` / `bootstrap_nodes`；`Default()` 中 `NoPortForwarding: true`；`Validate()` 拒绝两族同时关闭（`connections.disable_ipv4`）与非法 bootstrap 条目（`connections.bootstrap_nodes`，新增 `validateBootstrapNode`，要求 `host:port` 且 1≤port≤65535）。
- `internal/config/env.go`：新增 4 条 binding，`TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES` 按逗号分隔（空项忽略）。
- `internal/session/session.go:184-193`：映射到 `cc.DisableIPv4` / `cc.DisableIPv6` / `cc.NoDefaultPortForwarding`；`bootstrap_nodes` 非空时把 `cc.DhtStartingNodes` 换为 `staticStartingNodes`（`internal/session/dht.go:192`，每次调用解析每条目的所有地址，再由既有 `filterDhtStartingNodes` 按 socket 族过滤）。
- `docker/torrentfs.toml`：`listen_port = 6881`、`disable_ipv6 = true`、`no_port_forwarding = true`、`bootstrap_nodes = []`；`Dockerfile`：`EXPOSE 8080 6881/tcp 6881/udp` 并注明 EXPOSE 本身不发布；`torrentfs.example.toml` 与 `README.md` 同步新增键与说明。

### Phase 3 — 身份与单实例

- 新增 `internal/session/identity.go`：`resolvePeerID`（`:37`）在 `customize` 之后、`torrent.NewClient` 之前解析身份——显式 `cc.PeerID` 优先且不落盘；否则读 `<data_dir>/peer_id`，缺失时用 `cc.Bep20` 前缀 + `crypto/rand` 生成 20 字节并原子写入（复用 `writeFileAtomic`）；文件长度非 20 字节**报错退出**；前缀不匹配时 WARN 并重生成。
- `lockInstance` / `releaseInstanceLock`（`:92` / `:122`）：对 `<torrents-dir>/.metadata/instance.lock` 取 `LOCK_EX|LOCK_NB`，`EWOULDBLOCK` 时返回可操作错误；在 `newWithClientConfig` 早期获取（`session.go:166`），`Close` 中释放（`session.go:402`），失败路径显式释放避免 fd 泄漏。
- `go.mod`：`golang.org/x/sys` 由 indirect 提升为 direct（使用 `x/sys/unix`，原已在依赖图中，`go.sum` 无变化）。

### 测试

- 新增：`internal/session/network_identity_test.go`、`internal/config/connections_test.go`。
- 扩展：`internal/logging/logging_test.go`（凭据脱敏两种 handler + upstream 降级）、`internal/session/dht_test.go`（v4-only 过滤、static bootstrap、recorder 有界告警）、`internal/session/dht_tracker_test.go`（udp6→::1 成功路径）、`internal/session/tracker_test.go`（fixture 记录 port/key/event）、`internal/config/load_env_test.go`、`internal/session/export_test.go`（新增 `PeerIDForTest` / `DhtServerFamiliesForTest` 只读 seam）。
- 既有测试的必要调整：`config_test.go`、`load_env_test.go` 因 `Connections` 新增切片字段而由 `==` 改为 `reflect.DeepEqual`；`dht_test.go` 适配 `configureDhtStartingNodes` 新签名（传 `nil` recorder/logger）；env binding 数量断言 18→22。

## Verification

环境：Linux（容器内），Go 1.27，`golangci-lint v2.12.2`。`web/dist` 为 gitignore 的构建产物，先执行 `npm ci && npm run build --prefix web` 以完成 `go build ./...` / `go test ./...`。

**构建与质量门（全部通过）**

| 命令 | 结果 |
| --- | --- |
| `go build ./...` | Success |
| `go vet ./...` | No issues found |
| `gofmt -l internal/ cmd/ web/` | 无输出 |
| `golangci-lint run ./...` | No issues found |
| `go test ./...` | **345 passed**（11 packages；基线 339，新增 6 个用例，均为离线确定性用例） |
| `go test -race ./...` | **345 passed** |
| `npm run typecheck --prefix web` | 通过（未改前端；仅因构建 dist 而顺带执行） |
| `npm run lint --prefix web` | 通过 |
| `npm run test --prefix web` | 45 passed |

未运行 `gh pr checks --watch` / `gh run watch`；按计划，CI 结果不是本次验收条件。

**真实二进制运行（loopback，headless HTTP，不接触公网）**

脚本：临时目录内 `torrentfs -config run.toml torrents`，`timeout -s TERM` 控制运行时长；只 kill 自己启动的 PID。

| 观测项 | 结果 |
| --- | --- |
| `session ready` | `listen_port=0 effective_listen_port=40729 listen_addrs=0.0.0.0:40729,[::]:40729,...`（动态端口下真实端口可见） |
| 运行 **75 s**（跨越两个 1 分钟 table-refresh 周期）后统计 `dht starting nodes unavailable` | **1 条**（`family=udp6 local_addr=[::]:40729 resolved=8 kept=0 reason=dht_udp6_unavailable`）——有界 |
| 同一次运行统计 `error bootstrapping during bucket refresh`（info 级别） | **0 条**（原日志中每 60 s 一条） |
| `level = "debug"` 时同一条记录 | 可见（`level=DEBUG msg="error bootstrapping during bucket refresh: no initial nodes"`） |
| UPnP 相关日志 | 0 条（原日志 `discovered 0 upnp devices`） |
| 第二个进程指向同一 torrents 目录 | 退出码 1，错误 `session: another torrentfs instance already manages this torrents directory (lock "torrents/.metadata/instance.lock")` |
| 第一个进程 SIGTERM | 退出码 0，`session closed` → `stopped` |
| `data/peer_id` | 20 字节；两次真实重启后 `cmp` 一致（`PEER_ID_STABLE=yes`） |

> 记录到但未修改的现象：`listen_host = "127.0.0.1"`（IPv4 字面量）+ IPv6 未关闭时，upstream 会用该字符串去 bind tcp6，启动以 `listen tcp6: address 127.0.0.1: no suitable address found` 失败。这是既有行为（本 PR 未触及该行代码），已在 README 记录规避方式（留空 `listen_host` 或用 `disable_ipv6`）。计划中「不要用 `listen_host` 当族开关」正是针对这一点。

## AC Evidence

| AC | 实现 | 证据 |
| --- | --- | --- |
| P1.1 `effective_listen_port` + listener 地址，`listen_port` 语义不变 | `session.go:307-314`、`EffectiveListenPort`/`listenAddrs` | 真实运行日志（上表第 1 行）；`TestEffectiveListenPortMatchesTrackerAnnounce`：动态端口下 `EffectiveListenPort() != 0` 且等于 tracker announce 的 `port`，首条 announce `event=started` |
| P1.2 每族结构化 DHT 状态 + 有界 WARN + 状态名 | `dht.go:37-116`、`dht.go:118` | `TestDHTRecorderWarnsOncePerStateChange`：同状态连续 3 次只出 1 条 WARN，含 `dht_udp6_unavailable`；恢复时出 1 条 INFO；`snapshot()` 反映 `Ready/Kept/Resolved`。真实运行 75 s 仍只有 1 条 WARN，且每 60 s 的 upstream ERROR 在 info 级别为 0 条 |
| P1.3 凭据脱敏 | `logging.go`（pattern/`sensitiveKey`/`redactURL`/`redactRequest`/`demotingHandler`） | `TestNewRedactsTrackerCredentialFromEveryShape`（text + json 两种 handler，string URL / `*url.URL` / `*http.Request` / `err` 四种形态均不含 `credential`、`passkey`，且保留 `tracker.example` 与 `GET`）；`TestNewDemotesRepeatingDHTBootstrapError` |
| P1.4 status DTO 追加字段，`ready` 语义不变 | `backend.go:48/50-68/144/185/205`、`handlers.go:47-77/120-143` | `go test ./internal/api/...` 既有 `TestTorrentStatus...` 全绿（新增字段为追加，未改 `metainfo_ready`）；`select` 字段对齐计划列出的 `effective_listen_port` / `total_peers` / `active_peers` / `connected_seeders` / `piece_complete` / 每族 DHT |
| P2.1 `disable_ipv4/6` + 禁止同时关闭 | `config.go:163-168/250`、`env.go`、`session.go:186-187` | `TestConnectionSettingsMapToClientConfig`（四个子用例读 `customize` 中的 `cc.DisableIPv4/6`、`cc.NoDefaultPortForwarding`）；`TestDisableIPv6KeepsOnlyIPv4DHTServers`（`DhtServerFamiliesForTest() == ["udp4"]`）、`TestDisableIPv4KeepsOnlyIPv6DHTServers`（IPv6 不可用时 skip）；`TestValidateRejectsInvalidConnections`（两族同关 → `connections.disable_ipv4`） |
| P2.2 `no_port_forwarding` 默认 true | `config.go:172/216`、`session.go:188` | `TestDefaultDisablesPortForwarding`（`Default().Connections.NoPortForwarding == true`）；`TestConnectionSettingsMapToClientConfig`「defaults disable port forwarding」用例；真实运行 0 条 UPnP 日志 |
| P2.3 `bootstrap_nodes` | `config.go:173-177`、`env.go`、`session.go:189-193`、`dht.go:192` | `TestStaticStartingNodesServeEachFamily`（`127.0.0.1:6881` + `[::1]:6881` 解析后分别被 udp4/udp6 过滤各留 1 条）；`TestLoadReadsConnectionSection` / `TestLoadAppliesConnectionEnvironmentBindings`；`TestValidateRejectsInvalidConnections` 三个非法条目用例 |
| P2.4 Docker 固定端口 + 文档 | `docker/torrentfs.toml`、`Dockerfile`、`README.md`、`torrentfs.example.toml` | `TestShippedConfigurationsDecode`：两份随附 TOML 均通过严格解码（未知键会失败）；README 新增「Peer ports, NAT, and single-instance operation」章节与 `-p 6881:6881/tcp -p 6881:6881/udp` 示例 |
| P3.1 peer ID 持久化（损坏即报错） | `identity.go:37-90`、`session.go:212-221` | `TestPeerIDPersistsAcrossRestarts`（跨重启一致、20 字节、带 prefix、文件内容一致）、`TestPeerIDDiffersPerDataDir`、`TestCorruptPeerIDFileFailsStartup`（错误含 `peer ID file`）；真实运行两次重启 `cmp` 一致 |
| P3.2 单实例 flock | `identity.go:92-131`、`session.go:166/402` | `TestSecondInstanceOnSameTorrentsDirFails`（第二实例报 `another torrentfs instance`，close 后同目录可再次打开）；真实运行第二个进程退出码 1 并给出 lock 路径 |
| 计划要求的第 2 项新测试：v4-only resolver 下 udp6 为空 | — | `TestFilterDHTStartingNodesIPv4OnlyResolverMatchesLog`（udp4 保留 2、udp6 为 0，复现本次日志模式） |
| 计划要求的第 3 项新测试：`udp6 → ::1` 成功 ping | — | `TestDHTQuerySucceedsWithinIPv6Loopback`（本次实跑通过，未 skip） |
| 非目标：空-peers 不加重试、`ready` 语义不变、保留 MIO-42 过滤、无 per-torrent DHT/PEX 开关 | `filterDhtStartingNodes` 原样保留；无 announce 重试改动；`ready` 计算未改；未新增 torrent 级开关 | `git diff` 确认；`TestFilterDHTStartingNodesBySocketFamily`（MIO-42 用例）保持通过 |
| Phase 0 运维动作不代做 | 未执行凭据轮换/停实例；未提交任何日志附件 | 仓库内不存在 `pasted-text.txt`；本 Agent 未执行运维命令 |

## Deviations

1. **upstream 那条 ERROR 的降级方式**：计划写「从每分钟 ERROR 降级为有界 WARN」。为真正减少条数（而非只降级别），实现为 `logging` 层的记录级重写——把 upstream 消息前缀 `error bootstrapping during bucket refresh` 降级到 `debug`，并由于内层 handler 不再复检阈值而在低于阈值时直接丢弃；有界 WARN 由 session wrapper 输出。轻微差异：计划把它描述为 session wrapper 内的事，实际多了一层 handler 包装（否则无法减少打印次数）。风险：若上游改写该消息措辞，降级失效，仅退化为继续打印。
2. **`dht recorder` 的接入点扩展**：计划 1.2 只说「在 wrapper 内记录」，实际新增 `dhtRecorder` 并把 `configureDhtStartingNodes` 签名改为三参数（既有测试同步改为传 `nil`），因为 1.4 的「每族 DHT 状态」需要一个 session 级载体。
3. **DHT 状态字段命名**：status `network.dht[]` 使用 `resolved`/`kept`/`ready`/`error` 表达「解析数/过滤后保留数/是否可用/错误」，对应计划 1.2 的描述字段；无上游同名公开 API 可复用。
4. **新增 `PeerIDForTest` / `DhtServerFamiliesForTest` 只读 seam**：计划要求的两条用例（peer ID 一致性、`disable_ipv6` 只产生 udp4 DHT server）无法从既有 seam 观测，按既有 `export_test.go` 惯例新增只读访问器。生产代码不含新导出 API。
5. **`go.mod` x/sys 提升为 direct**：Phase 3 的 flock 使用 `golang.org/x/sys/unix`（Go 生态惯例，且该模块原已在依赖图中；未引入新模块）。`go.sum` 无变化。
6. **`listen_host` 的对族应用**：计划 2.1 只说「不要用 `listen_host` 当族开关」，未要求修改该行；实测其 IPv4 字面量会使 IPv6 监听失败并中止启动，本 PR **只记录到 README**，未改代码（属既有行为，改了会扩大范围）。
7. **未改前端**：新增 JSON 字段为追加，`web/src/types/api.ts` 的 `TorrentStatus` 少声明该字段不影响运行与类型检查；计划未要求 UI 呈现，故未改前端（前端四件套仍全额通过）。

## Risks / Blockers

- **无法从日志归因首次 Tracker「重复下载」拒绝**（外部系统不可见）。本 PR 只能降低复发概率（稳定 peer ID、避免多实例）并让下次可诊断，不能保证消除。
- **upstream 降级依赖消息前缀匹配**（见 Deviations 1）。措辞变化时只会退化为原有噪声，不会造成信息丢失。
- **锁与身份的作用域不一致**：flock 按 `<torrents-dir>`，`peer_id` 按 `<data_dir>`。共享同一 `data_dir` 而 torrents 目录不同的双实例不会被锁拦住（计划明确划定该边界），此时两者会竞争同一 `peer_id` 文件（原子写，最后写入者生效）。
- **`peer_id_prefix` 变更会重生成身份**，对私有 Tracker 相当于换 peer。已在 README 与代码注释说明；如需保持旧身份，需保留原 prefix。
- **`private=1` 的 torrent 与 DHT 发现冲突**：按计划仅记录，未纳入本轮改动；v1.61.0 未按 `Info.Private` 抑制发现，完整修复需改上游。
- **`disable_ipv6` 的 Docker 默认值是行为变更**：依赖 IPv6 的部署需要在 `docker/torrentfs.toml` 或环境变量中显式改回，建议在 release note 中标注；`no_port_forwarding` 默认 true 同理（依赖 UPnP 自动映射的用户需显式开启）。
- **实例锁在 NFS 等文件系统上的语义**：`flock` 依赖本地文件系统语义；不保证 NFS 场景，未做降级策略（计划未要求）。已通过错误路径显式释放避免 fd 泄漏，进程崩溃由内核释放。
- **DHT 弱状态无法自愈**：上游 30 分钟 bootstrap 抑制仍在，`bootstrap_nodes` 只是缓解；文档已说明进程重启是最简单手段。

## Critical Files

Reviewer 重点检查：

- `internal/session/dht.go:16-131` — `DhtFamilyStatus`、`dhtRecorder.observe` 的「有界」判定与 `configureDhtStartingNodes` 是否仍先调用 `existing(server)`；`staticStartingNodes:192` 的解析与族过滤。
- `internal/session/identity.go` — `resolvePeerID` 的优先级（显式 > 落盘 > 生成）、损坏即失败、前缀不匹配时的重生成；`lockInstance` 的 `LOCK_NB` 语义与错误信息可操作性。
- `internal/session/session.go:166`（加锁时机）、`:184-193`（连接配置映射与 bootstrap 覆盖顺序）、`:212-226`（peer ID 在 customize 之后解析）、`:307-314`（ready 日志）、`:356/402/421`（锁释放与置空，避免二次释放/fd 泄漏）。
- `internal/logging/logging.go:24-35/74-107/140-186` — 敏感键集合是否过宽（是否会误伤 `key` 等诊断字段）、`demotingHandler` 的 `Enabled` 复检、`redactRequest` 保留 host。
- `internal/session/backend.go:48-68/182-224` — `NetworkStatus` 为追加字段、`dhtStatus` 的实时合并、锁顺序（在 `s.mu.RLock` 内调用 client/dht 锁，无反向获取）。
- `internal/config/config.go:154-179/247-258/339-352` — 新键默认值与校验；`validateBootstrapNode` 是否接受 `host:0`。
- `internal/api/handlers.go:47-77/120-143` — JSON 字段命名与计划一致（`piece_complete` 按计划原文，Go 侧字段名为 `PieceComplete`）。
- `docker/torrentfs.toml`、`Dockerfile`、`README.md`、`torrentfs.example.toml` — 三个配置入口同步；`TestShippedConfigurationsDecode` 防漂移。
- 测试：`internal/session/network_identity_test.go`、`internal/config/connections_test.go`、`internal/session/dht_test.go`（新增三个用例）。

交付物：commit `3384de5`，PR https://github.com/yakumioto/torrentfs-go/pull/24
