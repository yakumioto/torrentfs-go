# Agent Handoff

## Task

完成 MIO-41：在不改变 React、Mantine、现有后端接口、路由、认证模型和业务行为的前提下，将 TorrentFS 登录页、任务列表页和任务详情页统一重构为浅色、简洁、现代、全中文且适配桌面端与移动端的界面。

验收范围包含登录、任务列表、搜索与状态筛选、添加磁力链接、上传 `.torrent` 文件、任务详情、文件列表、数据块状态、异步删除，以及加载、空、错误和后台刷新失败状态。

## Plan

依据 Orchestrator 的 `Approval: NOT REQUIRED` 执行计划实施。保持现有路由、认证竞态与 token 存储、API DTO/方法/路径/payload、query key、5 秒刷新、1.5 秒删除轮询、后台快照保留和删除无 `purge` 语义不变；`ready` 仅表达“就绪”，`cached_bytes` 仅表达“缓存占用”。

## Changes

- 将 HTML metadata、Mantine theme、CSS token 和全局样式切换到浅色文件工作台视觉，保留焦点环、reduced-motion 与对比度门槛。
- 重构认证壳层、登录页、连接失败/加载页和 Header；桌面端采用品牌区与表单区，移动端收敛为单列，并为所有操作补充中文 accessible name。
- 将 Dashboard、任务行、状态 badge、缓存占用轨道和空/错误/旧快照状态中文化并重新分层；保留搜索、筛选、排序和查询数据流。
- 中文化添加、删除和 operation 弹窗；上传继续使用浏览器 multipart boundary，删除继续使用现有异步 operation 轮询和无自动跳转行为。
- 按 Reviewer 反馈修复添加弹窗的 413 展示：磁力模式显示请求大小提示，文件模式保留 `.torrent` 文件大小提示，并为两种模式补充组件回归覆盖。
- 重构详情、概览、文件表格和数据块地图；文件表格在移动端使用单一 DOM 转为字段卡片，Piece map 继续每批最多渲染 1200 项、保留纹理和可访问明细表。
- 新增 `web/src/utils/user-facing-error.ts`，在展示层按已知 HTTP 状态映射中文错误，避免未知服务端英文字符串直接泄漏到 UI；transport 层仍保留原始 `ApiError`。
- 新增 Dashboard、添加弹窗和展示层错误测试，并同步现有测试的中文 accessible name 与浅色 provider。

## Verification

- `npm ci --prefix web`：通过，依赖审计无漏洞。
- `npm run typecheck --prefix web`：通过。
- `npm run lint --prefix web`：通过。
- `npm test --prefix web -- --run`：通过，14 个测试文件、45 个测试全部通过；添加弹窗 5 个测试覆盖磁力/文件两种 413 文案。
- `npm run build --prefix web`：通过；Vite 仅报告现有单 bundle 超过 500 kB 的非阻断提示。
- `go test ./...`：通过，所有 Go 包通过。
- `git diff --check`：通过。
- Chrome/Playwright 浏览器 smoke：认证登录、390px 移动端 Header、添加任务弹窗、真实磁力提交、进入详情后的“正在等待元数据”、详情 404 错误态均通过；桌面 1440px 登录页、任务列表和移动端登录/任务/详情截图已人工检查，无非预期控制台异常（认证初始 401 与详情 404 为预期请求）。

## AC Evidence

- 登录页、任务列表页、任务详情页统一浅色视觉：`web/src/styles/*`、`web/src/pages/*`、`web/src/components/layout/*` 与 Chrome 桌面/移动截图验证。
- 用户可见文案中文化：页面标题、按钮、状态、Tabs、菜单、tooltip、ARIA、文件覆盖率和 operation 文案均已更新；品牌名、文件名、hash、技术单位和 `.torrent` 保留为领域数据。
- 状态与缓存占用清晰：`StateBadge` 使用中文状态与 soft background，`TorrentProgress` 明确标注“缓存占用”，未将其解释为下载完成度；`ready` 显示“就绪”。
- 文件结构与数据块移动可读：`FilesTable` 使用 `data-label` 的 responsive table-to-card；`PiecesMap` 保留真实状态、纹理、legend、可访问表格与 1200 分页。
- 搜索、筛选、添加磁力、上传文件、删除任务保持可用：API/query/hook 未改；新增 `DashboardPage.test.tsx`、`AddTorrentDialog.test.tsx`，其中添加弹窗覆盖磁力 413 不显示文件错误、文件 413 保留准确提示，既有删除与 query 回归测试全部通过。
- 未引入无 API 支撑指标：未新增速度、Peer、Tracker、ETA、设置或虚构下载完成度；详情仅展示已有 Torrent、status、文件和 Piece 字段。
- 加载、空、错误和后台刷新失败状态保留：现有 `refresh-resilience`、详情、认证测试通过，新增 Dashboard 空态/筛选覆盖通过。
- 前端构建、类型检查、lint、Vitest 和 Go 测试均通过。

## Deviations

None. 仅将浏览器自动化验证使用的临时服务、配置和截图保存在 `/tmp`，未进入仓库；仓库未提交 `web/node_modules` 或 `web/dist`。

## Risks / Blockers

- Vite 构建仍提示主 bundle 超过 500 kB；这是性能提示，不影响构建或本次功能验收，未引入额外依赖或扩大范围。
- 真实浏览器 smoke 使用空任务目录，因此移动端文件/Piece 的真实长列表布局由组件测试、静态 CSS 审计和详情空态验证覆盖；数据块分页与文件覆盖率逻辑未改变。

## Critical Files

- `web/src/styles/theme.ts`
- `web/src/styles/tokens.css`
- `web/src/styles/global.css`
- `web/src/styles/auth-shell.module.css`
- `web/src/app/auth.tsx`
- `web/src/components/layout/Header.tsx`
- `web/src/pages/LoginPage.tsx`
- `web/src/pages/DashboardPage.tsx`
- `web/src/pages/TorrentDetailPage.tsx`
- `web/src/components/torrents/TorrentProgress.tsx`
- `web/src/components/torrents/StateBadge.tsx`
- `web/src/components/dialogs/AddTorrentDialog.tsx`
- `web/src/components/dialogs/DeleteTorrentDialog.tsx`
- `web/src/components/detail/FilesTable.tsx`
- `web/src/components/detail/PiecesMap.tsx`
- `web/src/utils/user-facing-error.ts`
- `web/src/test/DashboardPage.test.tsx`
- `web/src/test/AddTorrentDialog.test.tsx`
- `web/src/test/user-facing-error.test.ts`
