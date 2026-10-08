# 配置参考

[English](configuration.en.md) · [返回 README](../README.md)

本文的命令均从仓库根目录执行。完整配置见 [torrentfs.example.toml](../torrentfs.example.toml)，容器默认配置见 [docker/torrentfs.toml](../docker/torrentfs.toml)。二进制构建和运行方法见[部署指南](deployment.md)。

## 加载与优先级

可以不提供配置文件而使用内置默认值，也可以复制示例：

```sh
cp torrentfs.example.toml torrentfs.local.toml
./torrentfs -config ./torrentfs.local.toml "$PWD/torrents"
```

配置按字段合并，优先级为：

```text
环境变量 > TOML 文件 > 内置默认值
```

TOML decoder 会拒绝未知字段。只有下表列出的环境变量会被读取，不会自动为每个 TOML key 生成变量；嵌套 section 用下划线连接，例如 `http.auth.token_ttl` 对应 `TORRENTFS_HTTP_AUTH_TOKEN_TTL`。未列出的环境变量会被忽略。

静态 TOML 和环境变量只在启动时读取，修改后需要重启。上传限速不属于静态配置，由 Web UI/API 持久化并立即应用，见[使用指南](usage.md)。

## 字节与时长格式

`cache.capacity` 和 `http.max_upload_size` 在 TOML 与对应环境变量中都必须写成带单位的整数：

| 单位 | 倍率 | 示例 |
| --- | --- | --- |
| `B`、`KB`、`MB`、`GB`、`TB` | 十进制，逐级乘 1000 | `32MB = 32000000` bytes，`8GB = 8000000000` bytes |
| `KiB`、`MiB`、`GiB`、`TiB` | 二进制，逐级乘 1024 | `2GiB = 2147483648` bytes |

单位区分大小写，不接受裸数字、小数、bits 单位或科学计数法；旧裸数字配置需要改写后才能启动。

`http.auth.token_ttl` 使用 Go duration，例如 `30m`、`1h30m`；必须为正数且不超过 24 小时。Web UI 上传限速输入使用 `1MiB/s`、`32MB/s` 等带单位形式，但 HTTP API 和内部状态文件仍使用整数 bytes/s，不能把单位字符串直接写入 API。

## 主要配置项

| Section | 常用 key | 说明 |
| --- | --- | --- |
| `[http]` | `listen_addr`, `max_upload_size` | HTTP/Web listener；默认 `127.0.0.1:8080`，上传上限默认 `10MiB`；listener 为空则禁用 HTTP |
| `[http.auth]` | `enabled`, `username`, `password_hash`, `password_hash_file`, `token_ttl` | 单用户 bcrypt 登录和内存 Bearer token |
| `[connections]` | `listen_host`, `listen_port`, `disable_ipv4`, `disable_ipv6`, `no_port_forwarding`, `bootstrap_nodes` | peer listener、地址族、UPnP/NAT-PMP 和 DHT bootstrap |
| `[proxy]` | `socks5_url` | 可选 `socks5://` 或 `socks5h://` 出站代理 |
| `[cache]` | `capacity` | 内存 piece cache 硬上限，默认 `2GiB`；必须为正数且不超过水位计算安全上限 |
| `[identity]` | `tracker_user_agent`, `peer_id_prefix`, `extended_handshake_client_version` | tracker/peer 握手身份 |
| `[log]` | `level`, `format`, `add_source` | `debug`/`info`/`warn`/`error`，`text`/`json` 和源码位置 |
| `[mount]` | `allow_other` | 是否允许除挂载用户外的本机 UID 读取 FUSE 挂载 |

本地内置默认值和镜像配置不同：本地 peer 端口为 `0`，地址族都启用；镜像 peer 端口为固定 `6881`，默认禁用 IPv6。两者都默认禁用 UPnP/NAT-PMP 自动端口映射；需要接受入站 peer 时应设置固定端口并同时发布 TCP 和 UDP。

`cache.capacity` 是上限而非预留量，cache 按需增长。容器中应根据内存限制调低它，为协议缓冲、临时副本、Samba 等保留余量，避免 OOM kill。piece length 大于 cache capacity 的 torrent 会在添加时被拒绝。

## HTTP 认证与网络边界

HTTP 默认只监听 loopback，认证关闭。绑定 `0.0.0.0`、空 host 或其他非 loopback 地址时，必须启用完整认证配置。服务没有 TLS，对外部署应放在可信的 TLS reverse proxy 后面并限制可访问网络。

使用 TOML 认证时，设置用户名以及**恰好一个** bcrypt 密码来源：`password_hash` 或 `password_hash_file`。不能使用明文密码：

```toml
[http]
listen_addr = "0.0.0.0:8080"

[http.auth]
enabled = true
username = "alice"
password_hash = "<bcrypt-hash>"
password_hash_file = ""
token_ttl = "30m"
```

- hash file 必须是非空的普通、非符号链接文件，只允许 owner 读取，大小不超过 1024 bytes；服务会验证 bcrypt cost。
- `TORRENTFS_USERNAME` / `TORRENTFS_PASSWORD` 是可选的共享凭据覆盖方式。HTTP auth 启用且这两个变量完整提供时，启动时会生成 cost-10 bcrypt hash，成对覆盖 TOML 的用户名、hash 和 hash file，最终配置不保存明文密码。
- 两个共享变量必须同时提供、都非空；HTTP 密码最多 72 个 UTF-8 bytes，不能截断。SMB 还要求用户名解析为镜像内的 torrentfs runtime Unix 账户，并拒绝密码中的 CR/LF，见[部署指南](deployment.md)。
- HTTP auth 关闭时，Go 配置层忽略共享凭据 pair，以便 SMB-only 启动；但 TOML 中的 username、hash 和 hash file 必须全为空，关闭 auth 不会自动清空这些字段。
- 环境变量可能出现在进程环境和 Docker metadata 中，不应当作 secret store。

`token_ttl` 是无活动过期窗口，有效的认证请求会滑动该窗口；token 只存在 daemon 内存中，重启后全部失效。登录和请求协议见 [API 参考](api.md)。

## 支持的环境变量

| 环境变量 | TOML key / 用途 | 格式 |
| --- | --- | --- |
| `TORRENTFS_CONNECTIONS_LISTEN_HOST` | `connections.listen_host` | 字符串 |
| `TORRENTFS_CONNECTIONS_LISTEN_PORT` | `connections.listen_port` | 十进制整数 |
| `TORRENTFS_CONNECTIONS_DISABLE_IPV4` | `connections.disable_ipv4` | Go boolean |
| `TORRENTFS_CONNECTIONS_DISABLE_IPV6` | `connections.disable_ipv6` | Go boolean |
| `TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING` | `connections.no_port_forwarding` | Go boolean |
| `TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES` | `connections.bootstrap_nodes` | 逗号分隔的 `host:port` 列表 |
| `TORRENTFS_MOUNT_ALLOW_OTHER` | `mount.allow_other` | Go boolean |
| `TORRENTFS_PROXY_SOCKS5_URL` | `proxy.socks5_url` | 字符串 |
| `TORRENTFS_CACHE_CAPACITY` | `cache.capacity` | 带单位字节，如 `1GiB` 或 `32MB` |
| `TORRENTFS_IDENTITY_TRACKER_USER_AGENT` | `identity.tracker_user_agent` | 字符串 |
| `TORRENTFS_IDENTITY_PEER_ID_PREFIX` | `identity.peer_id_prefix` | 字符串 |
| `TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION` | `identity.extended_handshake_client_version` | 字符串 |
| `TORRENTFS_HTTP_LISTEN_ADDR` | `http.listen_addr` | 字符串；空值关闭 HTTP |
| `TORRENTFS_HTTP_MAX_UPLOAD_SIZE` | `http.max_upload_size` | 带单位字节，如 `10MiB` 或 `32MB` |
| `TORRENTFS_HTTP_AUTH_ENABLED` | `http.auth.enabled` | Go boolean |
| `TORRENTFS_HTTP_AUTH_TOKEN_TTL` | `http.auth.token_ttl` | Go duration，如 `30m` |
| `TORRENTFS_LOG_LEVEL` | `log.level` | `debug`/`info`/`warn`/`error` |
| `TORRENTFS_LOG_FORMAT` | `log.format` | `text`/`json` |
| `TORRENTFS_LOG_ADD_SOURCE` | `log.add_source` | Go boolean |
| `TORRENTFS_USERNAME` | HTTP/SMB 共享用户名 | 字符串 |
| `TORRENTFS_PASSWORD` | HTTP/SMB 共享密码 | 明文字符串；见上文的限制 |

例如，覆盖 listener、上传上限和 cache 容量：

```sh
TORRENTFS_HTTP_LISTEN_ADDR=127.0.0.1:8080 \
TORRENTFS_HTTP_MAX_UPLOAD_SIZE=10MiB \
TORRENTFS_CACHE_CAPACITY=1GiB \
./torrentfs "$PWD/torrents"
```

`PUID`、`PGID` 和 `TORRENTFS_SMB_ENABLED` 由 Docker 入口处理，不是 daemon 的 TOML 配置。`TORRENTFS_FUSE_REQUIRED` 是测试门禁，见[开发指南](development.md)。

## 校验与旧配置迁移

- 普通环境变量存在但为空时，字符串字段可以被清空；数字、布尔值和 duration 的空值会报错。
- `connections.listen_port` 必须在 `0..65535`；`disable_ipv4` 和 `disable_ipv6` 不能同时为 `true`；每个 bootstrap 项必须是合法且端口在 `1..65535` 的 `host:port`。
- `proxy.socks5_url` 只能为空、`socks5://` 或 `socks5h://`，且必须包含合法 host；显式端口必须在 `1..65535`，校验错误不会回显 proxy 凭据。
- `identity.peer_id_prefix` 最多 20 bytes；tracker User-Agent 不能包含 CR/LF。
- `http.max_upload_size` 必须大于零；非空 HTTP listener 必须是合法的 `host:port`。
- `[paths]`（包括 `paths.data_dir`）已移除；`TORRENTFS_PATHS_DATA_DIR` 会被忽略，不会恢复旧的磁盘 payload 路径。

迁移旧字段时，将 `cache.capacity_bytes` / `http.max_upload_bytes` 改为 `cache.capacity` / `http.max_upload_size`，将 `TORRENTFS_CACHE_CAPACITY_BYTES` / `TORRENTFS_HTTP_MAX_UPLOAD_BYTES` 改为 `TORRENTFS_CACHE_CAPACITY` / `TORRENTFS_HTTP_MAX_UPLOAD_SIZE`。旧 TOML key 不再接受，旧环境变量不再读取；值的 SI/IEC 单位规则不变。
