# Agent Handoff

## Task

将 `torrentfs-go` 的每日 Nightly GitHub Actions 从归档/GitHub prerelease 发布改为仅发布 OCI 镜像。每日构建、验证、GHCR 登录和 push 必须全部在 GitHub Actions runner 内完成；镜像目标为 `ghcr.io/yakumioto/torrentfs-go`，支持 `linux/amd64` 与 `linux/arm64`。保留 schedule、`workflow_dispatch`、main-only guard、精确 SHA checkout 和现有质量门禁，不改变正式 release 能力。

## Plan

- 保留现有每日 `16:17 UTC` schedule、无输入 `workflow_dispatch`、并发控制、受信任 `main` guard，以及 `test`/`lint`/`fuse` 质量 jobs。
- 删除 Nightly 专用 archive、checksum、Actions artifact、GitHub prerelease、Release asset 和过期 prerelease cleanup 路径。
- 新增单一 `image` job，严格依赖 `guard`、`test`、`lint`、`fuse` 全部成功；在同一个 GitHub Actions runner 中分别构建并启动两个单平台验证镜像，验证完成后才登录 GHCR 并 push 多平台镜像。
- 使用 commit committer timestamp 的 UTC 日期、完整 commit SHA 的前 7 位和 `github.run_id` 生成不可变 `nightly-YYYYMMDD-<short-sha>-<run-id>` 标签，并通过 OCI labels 写入完整 revision、source、created timestamp、version 和 ref name。
- 不发布 `latest`、stable、semver 或其他 alias。明确关闭 provenance/SBOM，避免为本次最小化 GHCR 发布额外授予 `id-token: write`；后续若需要 attestations，应单独审批权限和保留策略。
- 不在 workflow 中删除 registry tags 或历史 GitHub nightly releases；保留策略交给 GHCR 配置/GC，历史 GitHub nightly release/tag 不再由每日 workflow 清理。

## Changes

- `.github/workflows/nightly.yml`
  - 移除 `package`、`release`、`cleanup` jobs 及其 archive/checksum、Actions artifact、GitHub Release API 和 `contents: write` 权限。
  - 新增 `image` job，权限仅为 `contents: read` 与 `packages: write`。
  - 使用固定 SHA 的 Docker QEMU、Buildx、login 和 build-push actions；使用 `GITHUB_TOKEN` 登录 `ghcr.io`。
  - 在 push 前分别以 `load: true` 构建 `linux/amd64` 与 `linux/arm64` 临时镜像，执行 `docker image inspect` 标签/架构检查，并用 `torrentfs -h` 做无状态启动验证。
  - 只有质量 jobs 与两套镜像验证都成功后，才以唯一 nightly tag push `linux/amd64,linux/arm64` manifest。
- `README.md`
  - 将 Nightly 说明更新为 GHCR 多平台 OCI 镜像、不可变标签、完整 commit label、runner-only 验证和 pull/inspect 示例。
  - 删除归档、checksum、GitHub prerelease、Actions artifact retention 说明，并记录不发布 alias、不清理历史 release/registry tag。
- 未修改 `Dockerfile`、`.github/workflows/ci.yml`、`.github/actions/go-quality/action.yml`、`.gitea/workflows/ci.yml` 或 `scripts/nightly-build.sh`；仓库中没有可供本次修改的 stable release workflow。

## Verification

- `go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/nightly.yml`：通过（下载并运行 actionlint v1.7.12；本机未预装 actionlint）。
- `git diff --check`：通过。
- Nightly 静态断言：schedule、`workflow_dispatch`、main guard、四项 needs 成功条件、GHCR `packages: write`、双平台配置均存在；archive/Release/artifact/cleanup/写入 contents/id-token 路径均不存在：通过。
- metadata 模拟：commit-derived UTC date、40 位 SHA、数字 run ID 和 `nightly-YYYYMMDD-<short-sha>-<run-id>` 格式：通过。
- 受保护文件差异检查：CI、共享质量 action、Gitea workflow、Dockerfile 和归档脚本无变更：通过。
- 未在开发者工作站执行 Docker build/push 或以本地质量结果替代 CI；实际 GHCR 发布、双架构 QEMU 构建和 runner smoke 需由 main 上的定时/手动 workflow 运行验证。

## AC Evidence

- 每日成功运行发布目标 GHCR 中的双架构 nightly OCI image：`image` job 保留 schedule，最终 build-push 使用 `ghcr.io/yakumioto/torrentfs-go:<nightly-tag>` 和 `linux/amd64,linux/arm64`。
- 每日构建不创建 GitHub Release、不上传 Release assets、Actions artifacts 或其他 packages：相关 jobs/Actions/API 已从 Nightly workflow 删除；唯一发布步骤是目标 GHCR image push。
- 标签追溯日期和 commit：tag 使用 checkout 后 commit 的 UTC committer date、SHA 前 7 位和 run ID；OCI `org.opencontainers.image.revision` 写入完整 SHA。
- 构建/验证失败不发布成功镜像：`image` job 只在四个质量依赖结果均为 `success` 时运行；GHCR login 位于两架构 build/inspect/startup 步骤之后；最终 push 是最后的 build-push 步骤。
- 正式 release 流程保持可用：未发现 stable release workflow，本次没有新增、删除或改写 stable release/tag 逻辑；现有 CI、质量 action、Gitea workflow 和 Dockerfile 保持不变。

## Deviations

无超出已批准范围的偏差。Owner 将扩展的 HTTP/FUSE Docker smoke 范围暂缓处理，因此使用计划允许的无状态 `-h` 启动检查，不接入会自行构建镜像的本地 smoke 脚本。

## Risks / Blockers

- 需要在 GitHub Actions 的受信任 `main` 运行中确认仓库已启用目标 GHCR package，并且 `GITHUB_TOKEN` 的 `packages: write` 可用；本地环境无法替代该权限验证。
- arm64 验证依赖 runner 上的 QEMU/binfmt，最终镜像构建依赖现有 Dockerfile 的跨架构基础镜像；Dockerfile 的 mutable base image tags 是既有风险，本次未扩大范围处理。
- 未启用 provenance/SBOM；这是为保持最小权限而作的明确取舍，不影响 tag 和 OCI revision label 的追溯能力。

## Critical Files

- `.github/workflows/nightly.yml`
- `README.md`
- `Dockerfile`（复用且未修改）
- `.github/workflows/ci.yml`（回归对照且未修改）
- `.github/actions/go-quality/action.yml`（质量门禁且未修改）
- `scripts/nightly-build.sh`（归档工具保留但不再由 Nightly 调用）
