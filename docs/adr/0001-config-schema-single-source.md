# 配置以单一 schema 为唯一真相源

## 背景

同一批配置项此前要在五处手工同步：`Config` 与 `fileConfig` 双胞胎结构体、`environmentBindings` 手工绑定表、`torrentfs.example.toml`、`docker/torrentfs.toml`，以及 README。默认值也有两处来源（`Default()` 与 `outputConfig()` 的硬编码），环境变量名靠人手逐条维护，遗留别名（`[mount]`、`-mountpoint`、`TORRENTFS_MOUNT_*`、`TORRENTFS_SMB_ENABLED`、`TORRENTFS_STRM_ENABLED`）与"显式设置"检测把加载路径切得很碎。README 写的"环境变量 > TOML > 默认值"在实际代码里有五处不成立。

## 决定

- 以 Go 结构体（`toml` tag + 配置项注释）作为唯一的**配置 schema**。默认值只存在于 `Default()`，TOML 解码直接覆盖 `Default()` 实例；删除 `fileConfig` 双胞胎与 `outputConfig()` 里的第二套默认值。
- 环境变量名由配置项点路径机械派生（`http.auth.token_ttl` → `TORRENTFS_HTTP_AUTH_TOKEN_TTL`），唯一例外是配置文件入口 `TORRENTFS_CONFIG`。不再维护逐条绑定表。
- `torrentfs.example.toml`、`docker/torrentfs.toml`（镜像默认值 overlay）、README 的配置速查由 schema 生成；CI 运行 `scripts/config-docs-check.sh` 重新生成这些文件并逐字节比对，任何漂移使流水线失败。
- 一次性破坏式重组：`filesystem.backend` 提升为顶层 `backend`；删除 `[mount]` 与全部遗留 CLI/env 别名；`smb.strm` 用 `auto|on|off` 替代 `*bool` 三态；新增可见的 `[lab]` 配置节；非配置变量移出 `TORRENTFS_` 命名空间（改用 `TF_*`）；未知 `TORRENTFS_*` 与已删除的旧名在启动时报错并给出新名。
- 提供 `torrentfs config check` 与 `torrentfs config print`（脱敏、可标注来源），作为校验与审计入口。

## 考虑过的替代方案

- **独立配置项表驱动**：表与结构体会变成两份定义，除非反过来由结构体生成表，而那就是本方案。
- **外部 schema 文件 + codegen**：最显式，但给一个"单二进制、零工具链"的项目引入生成步骤与第二种描述语言。
- **精选 env 覆盖**（保留现有手工清单）：只是把现状合法化，没有消除两处维护。
- **TOML 内 `${ENV}` 插值**：引入另一套展开语义，且没有让配置项与来源的关系变得可推导。

## 影响

- 这是一次破坏性升级：所有被删除的 TOML 键、env 名与 CLI 参数都需要迁移表与启动错误提示。
- `config.Load` 不再读取任何 secret 文件；`Load` 是**取值规则**的唯一校验入口。属于进程的规则（至少一个输出）与上传统计实验的跨字段规则由命令层在加载后聚合：启动与 `config check` 走同一个组合函数，因此一次运行报出全部问题；`config print` 只读取值规则，所以仍能审计一份关闭了全部输出的配置。
- schema 的配置项注释升级为面向用户的文档素材，注释质量直接影响生成出来的参考文档。
- `[lab]` 从"只存在于开发文档的实验"变成正式可见的配置节，需要与 smb/fuse 同等对待。
