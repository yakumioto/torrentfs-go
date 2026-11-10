# 移除独立的 /experiments 监控页，并入仪表盘实验摘要卡

## 背景

ADR-0005 把上传统计实验收敛进配置 schema 时，决定保留 `/experiments` 作为只读监控页，仪表盘摘要卡与种子详情页的对照数据与之并存。这之后配置页面（`/config`）完成了一次信息架构重做：按分类常驻侧栏导航 + 可搜索的单页长列表，改动预览同时显示旧值与新值。重做之后，`/experiments` 唯一还承担的职责——展示 `experiments.upload` 的当前运行参数与真实/上报对照——与仪表盘摘要卡高度重叠：两者都读同一个 `GET /api/v1/experiments/upload`，摘要卡已经展示五项对照 totals，`/experiments` 只是多展示了模式专属参数（速率/倍率）、`announce_log` 路径与模式说明文案，并提供一个跳到配置页实验分类的链接。用户认为仪表盘首页已经展示得足够详细，这个独立页面不再需要，实验参数本就应该在配置页面里改。

## 决定

- 删除 `web/src/pages/ExperimentsPage.tsx` 与其样式、测试，删除 `/experiments` 路由注册（`web/src/app/App.tsx`）与 Header 的"上传实验监控"图标入口（`web/src/components/layout/Header.tsx`）。不为旧 URL 添加重定向；访问 `/experiments` 落到既有的 `NotFoundPage`。
- `ExperimentsPage` 独有、仪表盘摘要卡之前没有的内容——模式专属参数（`inject` 的虚报速率 / `amplify` 的放大倍率）、抖动百分比、上报上限、随机种子、`announce_log` 路径、Peer 采样间隔、模式说明文案——全部迁移进 `web/src/components/experiments/ExperimentSummaryCard.tsx`。
- 摘要卡不再是整卡可点击的 `<Link to="/experiments">`；原页面"修改实验设置"按钮（`component={Link} to="/config#experiments"`）原样迁入卡片，作为独立的可交互元素，跳到配置页面的"实验"分类。
- 摘要卡补齐 loading（骨架屏）与 error（不可用提示 / 刷新失败但保留旧快照的非阻断提示）两态，复用仪表盘"运行时统计"区块已有的同一套容错展示模式，而不是重新设计一套 UI。
- 后端不变：`GET /api/v1/experiments/upload`、`useExperiment` hook、`ApiClient.getExperiment`、种子详情页的 `TorrentExperimentSummary` 均原样保留——仍有消费方，且监控数据接口本就该留着。
- ADR-0005 的原文不改写：它准确记录了当时的决定与权衡；本 ADR 作为后续旁注，记录为什么又把这部分收回。

## 考虑过的替代方案

- 保留 `/experiments`、只重做视觉：否决。重做之后页面剩余信息量很薄，长期维护一个几乎与仪表盘卡片重复的独立路由，边际价值低于维护成本。
- 并入配置页"实验"分类的只读状态区块（而不是仪表盘卡片）：配置页本职是编辑配置，混入一段只读的实时运行数据会让"编辑 schema"和"查看运行时状态"两种语义挤在同一个页面里；仪表盘已经是"运行时信息"的固定去处（运行时统计、任务摘要都在那），实验的实时对照数据跟它们放在一起更一致。
- 给旧 URL 加重定向（`/experiments` → `/config#experiments` 或 `/`）：否决。页面被删除后不再有对应的"监控"语义可以无缝重定向到；接受旧书签 404，与"不做新功能开关、能直接感知即可"的一贯取舍一致。

## 影响

- `README.md` 的"配置页面与实验监控"小节与其余提到 `/experiments` 的段落需要改写为仪表盘摘要卡承载实验展示。
- 测试：删除 `web/src/test/experiments-page.test.tsx`；`web/src/test/DashboardPage.test.tsx`、`web/src/test/header.test.tsx` 与 `web/src/test/config-page.test.tsx` 中依赖"上传实验监控"链接做页面导航或断言的用例已同步改写，不再引用已删除的页面与路由。
- 后续若要在仪表盘之外再展示实验状态（例如新增实验的独立诊断），应优先考虑扩展摘要卡或种子详情页的对照区块，而不是重开一个专属监控路由。
