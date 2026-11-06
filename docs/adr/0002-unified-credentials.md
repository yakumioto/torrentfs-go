# 统一明文凭据，移除密码文件与 bcrypt 配置

## 背景

HTTP 登录只校验密码（`username` 在认证服务里保存但登录时不使用），SMB 协议则强制要求 `username`。此前的凭据散在五处：`http.auth.username`、`http.auth.password_hash`、`http.auth.password_hash_file`、`smb.username`、`smb.password_file`，外加一对特例环境变量 `TORRENTFS_USERNAME` / `TORRENTFS_PASSWORD`。围绕它们还有"必须成对设置""半对被静默忽略""bcrypt 只能用于 HTTP、SMB 必须拿明文"等一串组合规则。

## 决定

- 顶层 `username` / `password` 一对**凭据**，SMB 与 HTTP 共用同一口令；`username` 仅 SMB 使用，HTTP 登录不校验用户名。
- `password` 必填条件：`backend = "smb"` 或 `http.auth.enabled = true`；`username` 必填条件：仅 `backend = "smb"`。
- 删除 `password_hash`、`password_hash_file`、`smb.password_file` 以及共享凭据特例：不再支持任何密码文件，也不再支持 TOML 里的 bcrypt 哈希。`TORRENTFS_USERNAME` / `TORRENTFS_PASSWORD` 不再是特例，它们就是顶层 `username` / `password` 机械派生的环境变量名。
- 启动时由程序从明文生成 bcrypt（供 HTTP 校验），SMB 侧在进程内现算 NT hash；加载不再读取任何 secret 文件。
- 允许在 TOML 里写明文口令；当口令来自 TOML 且非空时，启动打一条不含口令值的 warning，示例配置与 README 明确提示优先用环境变量。
- `config print` 对该键始终输出 `<redacted>`，不提供 reveal。

## 实现要点

- **认证服务的输入是哈希，不是明文**：`auth.New(cfg config.Auth, passwordHash []byte)`。配置层负责一次性生成 bcrypt（`bcrypt.DefaultCost`），认证服务因此永远看不到明文、也不再需要读文件。哈希文件权限、bcrypt cost 下限等校验保留在服务里，但"从哪读"这一层消失了。
- **口令长度上限 72 bytes**（bcrypt 的限制）由配置层校验：bcrypt 会静默截断更长的口令，与其让 HTTP 登录与 SMB 使用两个不同的口令，不如在配置阶段报错。
- **来源是配置层的一等概念**：加载器返回 `Sources`（每个配置项的来源：`default` / `file` / `environment`），明文口令告警据此判断"口令是否来自配置文件"。同一份记录可直接支撑 #75 的 `config print --sources`，无需重复实现合并逻辑。
- **敏感项由 `secret:"true"` 结构体标签声明**，schema 派生为 `SchemaKey.Secret`，供 #75 脱敏与 #76 的生成器提示使用。

## 考虑过的替代方案

- **保留 `password_hash` 作为可选形式**（与明文 XOR）：它只能服务 HTTP-only 部署，SMB 仍然需要明文，等于为一个边缘场景加一条分支与一组校验。
- **明文仅限环境变量，TOML 仍只收 bcrypt 哈希**：最接近此前的安全姿态，但不符合"用户自己写 config.toml 传密码"的诉求。
- **通用 `_FILE` 后缀约定**：与既有 `password_hash_file` 键的派生 env 名撞名，且文件名后缀无法与配置键名共存。

## 影响

- TOML 文件从此可能包含明文口令，这是明确的安全姿态变化；缓解手段只有启动 warning、文档提示与 `config print` 脱敏。
- Docker secrets / `*_file` 用户需要改为环境变量传入口令。
- 认证服务的输入从"已校验的哈希"变为"由配置生成的哈希"，配置加载因此承担 bcrypt 生成；`config check` 会实际执行一次哈希生成。
- 迁移表新增删除项：`TORRENTFS_SMB_USERNAME`、`TORRENTFS_SMB_PASSWORD_FILE`，以及从未存在过的 `TORRENTFS_HTTP_AUTH_*` 凭据变量。
- `http.auth` 节只剩 `enabled` 与 `token_ttl`；认证服务不再保存用户名，`Principal` 不再携带用户名。
