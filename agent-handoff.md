# Agent Handoff

## Task

按已批准的 MIO-24 Web UI 计划，在 torrentfs-go 单仓库内交付第一版 React/TypeScript/Vite/Mantine/TanStack Query/React Router 控制台，并先将 `origin/main@e43f720` 与 MIO-25 `origin/agent/implementer/3614d83f20cf@268bbf5` 合并为实现基线；先后完成 Reviewer 三项 confirmed finding 修复，以及与最新 `origin/main` 的 PR 冲突解析。

验收范围：Dashboard、登录/登出、Torrent Detail、Files、Pieces、Magnet/`.torrent` 添加、异步删除；同源生产静态资源嵌入 Go binary；开发 Vite `/api` proxy；不扩展 peers、文件字节偏移或 frontend-specific API。

## Plan

- 实现分支从 `e43f720` 建立，并以 `--no-ff` 合并 `268bbf5`；初始合并提交为 `a04d638`，随后以 merge commit `845ac9e` 合并 `origin/main@6a5bfa0` 解决 PR 冲突，保留 nightly/release/quality 文件及 MIO-25 动态认证。
- 本轮再次 fetch 目标分支确认仍为 `origin/main@6a5bfa0`；`git merge origin/main` 为 no-op，因 `845ac9e` 已包含该 head，未制造重复 merge 或空代码变更。
- `web/` 同包提供 `//go:embed dist/*` 与 public static/SPA fallback；`internal/api` 以 `/api` 前缀分派到认证 API，其余请求进入 public static handler。
- token 仅存内存，首次用 list probe 判断匿名/登录/连接错误；API 401 清 token、清理 query cache 并回登录。
- query 使用固定 keys、5 秒 list/status 刷新、1.5 秒 deletion operation 轮询；Files 只展示 piece-level coverage，Pieces 使用状态优先级、纹理和文字冗余编码。
- frontend dist 不提交，由固定 Node/lockfile pipeline 在 Go 编译前生成；Docker runtime 只携带 binary。

## Review Follow-up

- Detail 页面现在使用 `status.data.torrent ?? detail.data` 渲染 header、aggregate metadata、Delete dialog 和 pending 判定，因此 5 秒 status snapshot 能反映 magnet resolution 与普通 state/progress 更新。
- `operationRefetchInterval` 在 operation data 保留 `deleting` 时检查 404 error，404 立即返回 `false`，同时保留 deleted/delete_failed 的 terminal stop 语义。
- `isGoZeroTime` 让 `0001-01-01T00:00:00Z` 在显示与稳定排序中都作为缺失时间；新增 formatter、sort、detail component 和真实 operation hook 回归测试。

## Changes

- 新增 `web/package.json`、精确版本 `package-lock.json`、`.nvmrc`、Vite/TypeScript/Vitest/ESLint 配置。
- 新增 `web/src/api` client/error、`web/src/types` DTO、`web/src/app` provider/auth state、`web/src/queries` keys/hooks/retry/sort、Dashboard/Detail 页面、布局、状态徽章、Files/Pieces 组件、Add/Delete/Operation dialogs 和深海控制室视觉样式。
- `web/src/app/auth.tsx` 实现匿名 list probe、动态 Bearer login/logout、401 回登录、query cancel/clear、内存 token store；未使用 Cookie、URL token、localStorage、sessionStorage、JWT 或 refresh endpoint。
- `web/embed.go` 和 `web/handler.go` 提供只允许 GET/HEAD 的静态服务、正确 MIME、index no-cache、hashed asset immutable cache、deep-link fallback、missing asset/API 不回退 HTML。
- `internal/api/server.go` 增加 public static + authenticated `/api` outer dispatcher；新增 `web/handler_test.go`、`internal/api/web_test.go` 覆盖 route split、认证、deep link、asset 404、405 和 API 404。
- 新增 `scripts/build-web.sh`、`scripts/http-smoke.sh`；`scripts/nightly-build.sh` 在 Go build 前生成 dist。
- 更新 `.github/actions/go-quality/action.yml`、`.gitea/workflows/ci.yml`、`.github/workflows/nightly.yml`，在 Go quality/package 前固定 Node、npm ci、frontend checks/build 并清理 `web/node_modules`。
- 更新 `Dockerfile` 为 Node builder → Go builder → binary-only runtime；`.dockerignore`/`.gitignore` 排除本地依赖和 dist；README/example config 补充同源 UI、auth 分层、Vite/Docker/build 说明。
- 升级并锁定 React Router 7.18.4、Vite 7.3.6、Vitest 5.0.1、ESLint 10.10.0 及兼容插件；`npm audit`（含生产依赖）为 0 vulnerabilities。
- 本轮修复 `TorrentDetailPage.tsx` 的 status snapshot 来源、`hooks.ts` 的 operation 404 polling、`format.ts`/`sort.ts` 的 Go 零时间语义；扩展组件与 query-hook 回归测试。
- 首次 PR 冲突仅涉及 `internal/api/server.go` 与 `torrentfs.example.toml`：保留 `dispatchAPIAndStatic(s.authenticate(mux), web.Handler())`、动态 `[http.auth]` 配置和双方默认监听/静态分层说明；`845ac9e` 已完成解析且无冲突标记残留。本轮再次检查目标 head 未发现新的文件冲突。

## Verification

- `npm ci --prefix web`：通过。
- `npm run typecheck --prefix web`：通过。
- `npm run lint --prefix web`（原始 ESLint 输出）：通过，0 error/0 warning。
- `npm test --prefix web -- --run`：通过，5 个 test files / 17 tests；包含详情 status snapshot 更新、operation 404 轮询停止和 Go 零时间显示/排序回归。
- `npm run build --prefix web`：通过，Vite 7.3.6 生成 `web/dist`；仅有约 510 kB bundle size 提示，无构建错误。
- `npm audit --package-lock-only --prefix web` 与 `--omit=dev`：均报告 0 vulnerabilities。
- `go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...`：全部通过。
- `golangci-lint run ./...`（v2.12.2）：`0 issues`。
- 在 `origin/main@6a5bfa0` merge 解析完成后重跑 `go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...` 和 `golangci-lint`：全部通过；本轮 `git merge origin/main` no-op，PR head 为 `96397b8`，工作树干净。
- Vite dev server `127.0.0.1:5173`：成功提供入口 HTML；已精确清理启动的进程组。
- Google Chrome headless：真实渲染匿名 Dashboard 与 Detail deep link，页面显示 torrent queue、`payload.txt`、Files 和 Pieces；已精确清理 daemon/Chrome 临时目录。
- `scripts/http-smoke.sh`：通过 root/deep-link、asset MIME/cache、未认证 401/WWW-Authenticate、login no-store、带 token list、logout revoke、未知 API 不回退 HTML。
- Docker runtime 检查：存在 `/usr/local/bin/torrentfs`，无 Node 命令和 `/src/web/dist`。
- `scripts/docker-smoke.sh`：通过真实 FUSE 挂载、payload hash、data-only 控制路径、legacy `.stats` 保留、单文件输入和缺失目录负向用例。
- nightly 构建验证：连续两次构建 archive/checksum 字节一致；tar 根级成员严格为 `torrentfs`、`BUILD_INFO`、`LICENSE`。
- piece 状态色板验证：dataviz validator 在 `#07111f` surface 下对 `#0A9A72,#4F94DC,#C4822E,#3477A8,#D85D4B` 的 lightness/chroma/CVD/normal/contrast 全部 PASS。

## AC Evidence

1. **合并正确基线且保留 CI/release/auth：** 初始实现 merge 的 first parent 为 `e43f720`、second parent 为 `268bbf5`；随后 `845ac9e` 合并 `origin/main@6a5bfa0` 解决 PR 冲突。`git diff` 与构建验证保留 `.github/actions/go-quality/action.yml`、nightly workflow、`scripts/nightly-build.sh`，MIO-25 auth tests、public static/API dispatcher 与动态配置同时通过。
2. **动态认证安全契约：** `web/src/api/client.ts` login 不发旧 Bearer、管理请求发标准 header、logout 接受 204、multipart 不手设 boundary；`web/src/app/auth.tsx` 只持有内存 token；`web/src/test/auth.test.tsx` 3 cases 覆盖 probe/401/login/管理请求；Go MIO-25 auth suite 通过。
3. **Dashboard/Detail/Files/Pieces：** `DashboardPage.tsx` 展示任务状态、进度、大小和 peers unavailable；`TorrentDetailPage.tsx` 共享 status query 并以 `status.data.torrent` 更新 header/metadata/pending；`FilesTable.tsx` 使用半开范围与 piece-level coverage；`PiecesMap.tsx` 采用 checking→complete→partial→known-incomplete→unknown 优先级、wanted ring、available bytes optional tooltip、可访问表格/分批显示；domain 与 detail component tests 覆盖边界。
4. **Add/Delete/operation：** `AddTorrentDialog.tsx` 走 magnet JSON 或 `FormData(file)`；`DeleteTorrentDialog.tsx` 默认 false、purge 二次确认、409 文案、operation terminal/404 行为；`queries/hooks.ts` 负责 invalidation 与 1.5 秒 terminal-stop polling，真实 hook test 验证 404 后调用次数停止。
5. **静态资源与路由边界：** `web/handler.go` + `internal/api/server.go` 实现 public static、`/api` auth split、SPA deep link、asset 404、未知 API 404/401、GET/HEAD 限制；Go static/outer tests 与 Docker HTTP smoke 均通过。
6. **构建/发布/容器：** Node LTS 22.23.2 + lockfile 在 GitHub/Gitea/shared action/nightly/Docker builder 中准备 dist；`web/dist`/node_modules 被忽略；runtime 无 Node/dist；nightly 双构建 archive 可重复且三成员不变；Docker HTTP/FUSE smoke 均通过。
7. **质量与安全：** frontend typecheck/lint/test/build/audit、Go build/vet/test/race/lint、Vite smoke、Chrome headless、HTTP Docker smoke、FUSE smoke 全部通过；未引入新的安全或 API 范围偏差。

## Deviations

- `chromium-cli` 不在本运行时 PATH；使用已安装的 Google Chrome headless 完成实际 Dashboard/Detail 深链接渲染，并保留 Reviewer 已完成的 login/logout、匿名 Dashboard、空列表和 Add dialog 验收记录。
- 按计划未提交生成的 `web/dist`；任何 clean checkout 必须先执行 frontend build pipeline，再运行 Go build/test。

## Risks / Blockers

- Vite bundle 约 510 kB，构建仅发 chunk-size advisory；不影响功能或构建成功。
- npm 安装仍显示 jsdom 传递依赖 `whatwg-encoding` deprecation warning，但完整及生产 audit 均为 0 vulnerabilities。
- 未知 `/api` 路由保留 Go `ServeMux` 标准非 HTML 404；测试锁定不发生 SPA fallback。
- `web/dist` 缺失时 Go embed 会让编译失败，这是有意的单一构建契约，不是静默降级。
- PR API 当前报告 `mergeable=MERGEABLE`；`mergeStateStatus=UNSTABLE` 来自 Sourcery 检查失败，不是代码冲突，CI build/vet/unit/race/lint/FUSE 与 DCO 均已成功。

## Critical Files

- `internal/api/server.go`
- `web/embed.go`
- `web/handler.go`
- `web/src/api/client.ts`
- `web/src/app/auth.tsx`
- `web/src/queries/hooks.ts`
- `web/src/test/queries.test.tsx`
- `web/src/test/TorrentDetailPage.test.tsx`
- `web/src/utils/format.ts`
- `web/src/pages/DashboardPage.tsx`
- `web/src/pages/TorrentDetailPage.tsx`
- `web/src/components/detail/FilesTable.tsx`
- `web/src/components/detail/PiecesMap.tsx`
- `web/src/components/dialogs/AddTorrentDialog.tsx`
- `web/src/components/dialogs/DeleteTorrentDialog.tsx`
- `.github/actions/go-quality/action.yml`
- `.github/workflows/nightly.yml`
- `.gitea/workflows/ci.yml`
- `Dockerfile`
- `scripts/build-web.sh`
- `scripts/http-smoke.sh`
- `scripts/nightly-build.sh`
- `README.md`
