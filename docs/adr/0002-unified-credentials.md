# 统一明文凭据，移除密码文件与 bcrypt 配置

## 背景

HTTP 登录只校验密码（`username` 在认证服务里保存但登录时不使用），SMB 协议则强制要求 `username`。此前的凭据散在五处：`http.auth.username`、`http.auth.password_hash`、`http.auth.password_hash_file`、`smb.username`、`smb.password_file`，外加一对特例环境变量 `TORRENTFS_USERNAME` / `TORRENTFS_PASSWORD`。围绕它们还有"必须成对设置""半对被静默忽略""bcrypt 只能用于 HTTP、SMB 必须拿明文"等一串组合规则。

## 决定

- 顶层 `username` / `password` 一对**凭据**，SMB 与 HTTP 共用同一口令；`username` 仅 SMB 使用，HTTP 登录不校验用户名。
- `password` 必填条件：`backend = "smb"` 或 `http.auth.enabled = true`；`username` 必填条件：仅 `backend = "smb"`。
- 删除 `password_hash`、`password_hash_file`、`smb.password_file` 以及共享凭据特例：不再支持任何密码文件，也不再支持 TOML 里的 bcrypt 哈希。
- 启动时由程序从明文生成 bcrypt（供 HTTP 校验），SMB 侧在进程内现算 NT hash。
- 允许在 TOML 里写明文口令；当口令来自 TOML 且非空时，启动打一条不含口令值的 warning，示例配置与 README 明确提示优先用环境变量。
- `config print` 对该键始终输出 `<redacted>`，不提供 reveal。

## 考虑过的替代方案

- **保留 `password_hash` 作为可选形式**（与明文 XOR）：它只能服务 HTTP-only 部署，SMB 仍然需要明文，等于为一个边缘场景加一条分支与一组校验。
- **明文仅限环境变量，TOML 仍只收 bcrypt 哈希**：最接近此前的安全姿态，但不符合"用户自己写 config.toml 传密码"的诉求。
- **通用 `_FILE` 后缀约定**：与既有 `password_hash_file` 键的派生 env 名撞名，且文件名后缀无法与配置键名共存。

## 影响

- TOML 文件从此可能包含明文口令，这是明确的安全姿态变化；缓解手段只有启动 warning、文档提示与 `config print` 脱敏。
- Docker secrets / `*_file` 用户需要改为环境变量传入口令。
- 认证服务的输入从"已校验的哈希"变为"由配置生成的哈希"，配置加载因此承担 bcrypt 生成；`config check` 会实际执行一次哈希生成。
- 迁移表新增删除项：`TORRENTFS_SMB_USERNAME`、`TORRENTFS_SMB_PASSWORD_FILE`，以及从未存在过的 `TORRENTFS_HTTP_AUTH_*` 凭据变量。
