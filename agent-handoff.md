# Agent Handoff

## Task

**MIO-44**（issue `01a0bca0-27de-7725-a018-7cd72301a5fb`）：私有 torrent（`private=1`，BEP27）未按规范抑制 DHT/PEX 发现。

目标：让 torrentfs-go 对 `private=1` 的 torrent 遵守 BEP27 —— 不通过 DHT 宣告/查询、不参与 PEX、不参与 LPD，仅使用 torrent 内声明的 tracker；同时公开 torrent 的发现行为逐字节不变，不扩大范围。

本轮（Implementer）任务：按已获 yakumioto 批准的执行计划实现，并追加审批人约束「**开工前先同步最新 main 分支代码**」。计划的「关键实施步骤」「验证方式」「主要风险」各节即本轮验收标准。

## Plan

- 依据：Orchestrator 的调度评论 `01a0bca8-3ac4-720c-af0d-fbd557d12d69` 及其附件 `agent-handoff.md`（Approval: REQUIRED），审批人 yakumioto 评论 `01a0bcbb-d416-740c-9813-04e0b0b750a0`：「批准，同步最新 main 分支代码」。
- 采用方案 B（推荐）：**fork + `go.mod` `replace` 的最小反向移植** —— fork `anacrolix/torrent` 到 `v1.61.0`，只打 BEP27 的 3 个 hunk（与上游 `76452a2c8a2f` 逐字一致），打 tag 后在 `go.mod` 加 `replace`。
- 关键约束：
  - torrentfs-go **生产代码零改动**，隔离逻辑全在依赖内；
  - `go.mod` 的 require 块不得变动；
  - 不新增任何 per-torrent DHT/PEX 配置项；
  - 不碰 MIO-43 的其余 Phase 内容。

### 前置：同步最新 main

`multica repo checkout` 后 HEAD 已在最新 `main`（`4efcefa`，MIO-43 合并提交），`git rev-list --left-right --count HEAD...origin/main` = `0 0`，无需 rebase。计划撰写于 `171804b`，但 MIO-43 之后改动了 `internal/session/dht.go`（+162 行）、`session.go`、`export_test.go`。已在 `4efcefa` 上**逐条复核计划的代码事实**（`newWithClientConfig` 与 `customize` seam、`Source{Metainfo}`、`buildSingleFileTorrentBytes`、`NewWithClientConfig` 等），全部成立，计划无需改动。

## Changes

### fork 仓库 `github.com/yakumioto/torrent`（新建，public）

- 由 `gh repo fork anacrolix/torrent` 建立，bench 到 tag `v1.61.0`，分支 `bep27-v1.61.0`，tag **`v1.61.0-bep27.1`**（commit `a113bc7`）。
- 相对 `v1.61.0` 的全部差异（`git diff v1.61.0 --stat`）：**2 个文件、22 行新增、0 行删除**：
  - `torrent.go`：新增 `Torrent.isPrivate()`（`t.info != nil && t.info.Private != nil && *t.info.Private`）；`dhtAnnouncer` 内层循环在 `t.closed` 检查后插入 `if t.isPrivate() { goto wait }`（每轮重查，覆盖 magnet metadata 晚到）。
  - `pexconn.go`：`pexConnState.Init` 在 `DisablePEX` 早退之后插入 `if c.t.isPrivate() { return }`。
- 无 `client.go` hunk：v1.61.0 完全没有 LPD 代码，无处可改。
- 3 个 hunk 已用脚本**逐字节**比对上游 `76452a2c8a2f` 的对应代码块，三者均 `IDENTICAL`。

### torrentfs-go（本 issue 交付物）

| 文件 | 变更 |
| --- | --- |
| `go.mod` | 末尾新增 `replace github.com/anacrolix/torrent => github.com/yakumioto/torrent v1.61.0-bep27.1` 及说明性注释（共 +8 行）。require 块**逐行未变**。 |
| `go.sum` | 2 行新增（fork 的 `h1:` 与 `/go.mod h1:`）+ 2 行删除（原 `anacrolix/torrent v1.61.0` 两行）。 |
| `internal/session/export_test.go` | 新增 2 个**测试专用** seam：`UnderlyingTorrentForTest(st) *torrent.Torrent`、`UnderlyingClientForTest(s) *torrent.Client`（+14 行）。不进生产代码。 |
| `internal/session/bep27_test.go` | **新增**（294 行）：T1（DHT 离线确定性）、T2（PEX loopback 确定性）及其正控。 |
| `README.md` | 新增 `## Private torrents (BEP 27)` 一节（+39 行）：隔离语义、`.torrent` vs magnet 的差异与合规指引、无 tracker 的私有 torrent 结果、依赖 pin 与退出条件。 |

**`internal/**`、`cmd/**` 的生产代码零改动。**

### 观测点（为什么测试能确定性离线）

- DHT 活动：`Torrent.writeStatus` 的 `numDHTAnnounces`（v1.61.0 `torrent.go:964` 输出 `DHT Announces: %d`，自增在 `torrent.go:2427`，位于任何 DHT I/O **之前**）。v1.61.0 无 per-torrent 的 `WriteStatus` 导出方法，故经 `Client.WriteStatus` 读并按 torrent 名归属。
- PEX 活动：`ClientConfig.Callbacks.PeerConnReadExtensionMessage`，用 `PeerConn.LocalLtepProtocolMap.LookupId` 判 `ut_pex`，用 `PeerConn.Torrent().InfoHash()` 归属 torrent。
- 连接活动：`ClientConfig.Callbacks.PeerConnAdded` 按 infohash 计数，用来证明私有 swarm **确实建连**。

## Verification

全部在 `4efcefa` + 上述改动上执行，本机（Go 1.27.0，Linux）。

### fork 侧

| 检查 | 结果 |
| --- | --- |
| `git diff v1.61.0 --stat` | 2 files changed, 22 insertions(+), 0 deletions(-) —— 即全部差异 |
| 3 hunk 与上游 `76452a2c8a2f` 逐字节比对 | 3/3 `IDENTICAL` |
| `go build ./...` | 通过 |
| `go vet ./...` | 退出码 1，42 条 —— 与 pristine `v1.61.0` **完全相同**（42 条、退出码 1），即我们的 hunk 未引入任何新 vet 问题 |
| `go test . ./metainfo/... ./peer_protocol/... ./bencode/...` | 全部 `ok` |

> 上游自带的 `storage/possum` 子包在**链接期**失败：`cannot find -l:libpossum.a` —— 该包要求 module cache 外预编译的 Rust 静态库，属环境限制，与本次 hunk 无关（其他子包不受影响）。

### torrentfs-go 侧

| 检查 | 结果 |
| --- | --- |
| `go build ./...` | 通过（需先 `npm ci` + `npm run build --prefix web` 生成被 gitignore 的 `web/dist`，与 CI 一致） |
| `go vet ./...` | 通过（0 问题） |
| `gofmt -l`（除 `web/`） | 无输出 |
| `golangci-lint run ./...`（**v2.12.2，与 CI 同版本**） | **0 issues** |
| `go test ./...` | 全绿（10 个包） |
| `go test -race ./...` | 通过（见下方 Risks 关于一次偶发；非本次改动引入） |
| `TORRENTFS_FUSE_REQUIRED=1 go test -race -run 'TestFuse|TestSessionIncomplete' ./...` | 通过（本机有 `/dev/fuse`，真实挂载测试执行而非跳过） |
| 前端 `npm run typecheck / lint / test -- --run` | 通过（vitest 45 passed） |
| `go mod tidy` 后 `go.mod` vs `main` | 仅多出 `replace` + 注释（+8 行），**require 块逐行不变** |

### 关键证据：测试不是空跑（fork 是 load-bearing 的）

把 `replace` 临时移除（回到 pristine `v1.61.0`）后重跑新增测试，**两个都失败**，且失败点正是否定断言：

```
--- FAIL: TestBEP27PrivateTorrentDoesNotAnnounceToDHT
    bep27_test.go:181: private torrent announced to DHT 1 times, want 0
--- FAIL: TestBEP27PrivateTorrentDoesNotExchangePEX
    bep27_test.go:289: private infohash received 1 PEX messages over 1 connections, want 0
```

恢复 `replace` 后两个测试再次通过（各约 3s）。这证明：T1/T2 的否定断言依赖 fork 的 guard，而非「什么都没发生」。

### 依赖零漂移的硬证据

`go.sum` 里 fork 的 `/go.mod` 哈希与上游 `anacrolix/torrent v1.61.0` 的 `/go.mod` 哈希**完全相同**：

```
h1:yKUKuZSSDdyOsCbuH+rDOpswl/g546gICapdrU7aUmQ=
```

即 fork 的 `go.mod` 与 `v1.61.0` 逐字节一致 —— 除 3 个 BEP27 hunk 外，依赖树没有任何变动。

## AC Evidence

计划的「关键实施步骤」「验证方式」「主要风险」即本节验收标准，逐条取证：

| AC | 实现 | 证据 |
| --- | --- | --- |
| Step 1：建 fork、bench `v1.61.0`、打 3 hunk、打 tag | fork `github.com/yakumioto/torrent`，tag `v1.61.0-bep27.1` | `git ls-remote --tags` 含 `refs/tags/v1.61.0-bep27.1`；`gh repo view` = public, isFork=true；`git diff v1.61.0 --stat` = 2 files/22 insertions/0 deletions |
| Step 1 附加：hunk 与上游逐字一致 | 3 处插入 | 脚本比对 3/3 `IDENTICAL` |
| Step 2：`go.mod` 加 `replace`，`go mod tidy`；require 块不变 | `replace` + 注释 | `go.mod` diff vs `main` = `+8 -0`，require 块零行变化 |
| Step 2 附加：`go.sum` 变化受控 | 2 加 2 删（模块路径键位随 `replace` 改变） | `anacrolix/torrent v1.61.0` 两行被 `yakumioto/torrent v1.61.0-bep27.1` 两行取代；`/go.mod` 哈希逐字节相同 |
| Step 3：生产代码零改动，不新增开关 | `internal/**`、`cmd/**` 未改 | `git status --short` 仅 `README.md`/`go.mod`/`go.sum`/`export_test.go`（测试 seam）/`bep27_test.go`（新增） |
| Step 4：magnet 策略为决策 + 文档化，不改代码 | README 新增一节说明窗口与合规指引 | `README.md` 新增 `## Private torrents (BEP 27)`，明确 `.torrent` 全程隔离、magnet 存在 metadata 落地窗口、合规场景改用 `.torrent` |
| Step 5 / T1：私有 torrent 不进入 DHT 路径，公开行为不变 | `bep27_test.go` T1 | 附加前（pristine）：FAIL「announced to DHT 1 times」；附加后：PASS。公开 torrent 正控在 3s 内 `DHT Announces ≥ 1` |
| Step 5 / T2：私有 torrent 不参与 PEX，公开行为不变 | `bep27_test.go` T2 | 附加前：FAIL「received 1 PEX messages over 1 connections」；附加后：PASS。公开 infohash 正控收到 ≥1 条 ut_pex，且私有 infohash 有 ≥1 条已建连接（证明否定断言非空跑） |
| T3：公开 torrent 行为不回归 | 两个正控 + 既有测试原样通过 | `fuse_swarm_test.go`（真实 loopback swarm + FUSE）、`dht_test.go`、`dht_tracker_test.go`、`proxy_test.go`、`tracker_test.go` 全部 PASS |
| 验证：`go build`/`vet`/`gofmt` | — | 均干净 |
| 验证：`go test` / `-race` | — | 全绿（见 Risks 的偶发说明） |
| 验证：golangci-lint | — | v2.12.2，`0 issues` |
| 验证：FUSE job（`TORRENTFS_FUSE_REQUIRED=1`） | — | 通过；本机有 `/dev/fuse`，真实挂载用例执行 |
| 约束：不扩大范围 | 未新增配置项、未改 `internal/session/dht.go` / `proxy.go`、未碰 MIO-43 其余 Phase | 见 Step 3 行 |

## Deviations

1. **`go.sum` 不是「只新增行」，而是 2 加 2 删。** 计划 Step 2 的验收断言写的是「`go.sum` 只新增行」。实测 Go 在 `replace` 到不同模块路径时，会按**替换后的模块路径**记录校验和，于是原 `github.com/anacrolix/torrent v1.61.0` 的两行被 fork 的两行取代。这是 Go 的既定行为，不构成依赖漂移：`/go.mod` 哈希与上游逐字节相同，`z`（zip）哈希变化仅因模块路径不同。计划断言的**实质**（require 块不变、无无关版本变动）成立，故未回炉。
2. **计划 Step 5 提到的 `UnderlyingTorrentForTest` 之外，另加了 `UnderlyingClientForTest`。** 因为 v1.61.0 **没有导出** per-torrent 的 `WriteStatus`（`Torrent.writeStatus` 未导出），只能经 `Client.WriteStatus` 读并按 torrent 名归属。两者都是 `export_test.go` 里的测试专用 seam，不影响生产代码。这属于实现细节层面的合理调整，未改变方案。

## Risks / Blockers

1. **发现一个既有的、偶发的 data race（属于 MIO-43 的代码，不在本 issue 范围）。** `go test -race ./...` 有一次出现 2 条 race 报告，位置是 `TestStorageUsesInjectedLogger`：DHT bootstrap goroutine 经 `dhtRecorder.observe`（`internal/session/dht.go:86`，**在释放 `dhtRecorder.mu` 之后**调用 `logger.Warn`）→ `logging.demotingHandler` → 测试的 `bytes.Buffer`，与测试自身读取/写入该 buffer 竞争。**已在 pristine `4efcefa`（未做任何改动）上复现**：同一测试、同一栈（`dhtRecorder.observe` → `demotingHandler.Handle` → `bytes.Buffer`），在 CPU 负载下 `-count=4` 触发 4 条。结论：**既有缺陷，非本次改动引入**；但本轮 T1 为了驱动 DHT 路径会开启 DHT，客观上提高了该 race 的暴露概率。修复它需要改 `internal/logging` / `dht.go` 的锁与日志调用顺序，超出本 issue「生产代码零改动、不扩大范围」的约束，**未自行修改**，建议单独立项。
2. **fork 治理**：fork 必须钉在 `v1.61.0` + 这 3 个 hunk。退出条件：上游一旦发布包含 `76452a2c8a2f` 的正式版本，删掉 `replace` 并按正常节奏评估升级即可。已把这段说明写进 `go.mod` 注释与 README。
3. **依赖可解析性**：fork 已确认为 public，tag `v1.61.0-bep27.1` 为合法 semver 预发布，`go mod tidy` 与 `go build` 在本机均能拉到。
4. **magnet 窗口（计划的已知边界，非本轮缺陷）**：magnet 在 metadata 落地前 `isPrivate()` 为 false，DHT 照常；metadata 一到下一轮即停。合规场景请用 `.torrent` 添加。已在 README 写明。
5. **LPD 未来风险**：v1.61.0 无 LPD，补丁也不引入。已按计划在 `bep27_test.go` 顶部留注释：若将来启用上游 LPD，必须重新验证 BEP27 guard。
6. **上游 `storage/possum` 在本机无法链接**（缺预编译 Rust 库），故 fork 侧未跑通该子包的测试；与本次 hunk 无关。

## Critical Files

Reviewer 重点检查：

- `go.mod:97-103` —— `replace` 与说明性注释；确认 require 块未动。
- `go.sum:349-350` —— fork 两行；确认原 `anacrolix/torrent v1.61.0` 两行已按替换路径被取代、无其他变动（`git diff go.sum` 应恰为 2 加 2 删）。
- `internal/session/bep27_test.go` —— T1/T2 的正控与否定断言；重点看「公开正控必须先命中」与「私有连接数 ≥1」是否足以排除空跑。
- `internal/session/export_test.go` —— 2 个测试专用 seam。
- `README.md` —— `## Private torrents (BEP 27)` 一节的措辞是否与实现语义一致（尤其 magnet 窗口与无 tracker 的结果）。
- fork `github.com/yakumioto/torrent`（tag `v1.61.0-bep27.1`，commit `a113bc7`）：`torrent.go`（`isPrivate()` 与 `dhtAnnouncer` guard）、`pexconn.go`（`Init` guard）；与上游 `76452a2c8a2f` 逐字比对。
