# torrentfs

`torrentfs` 将 BitTorrent 内容挂载为只读的 FUSE 文件系统，并提供一个用于管理 torrent 的 HTTP API 和嵌入式 Web UI。

它管理一个已有的、可读写的 `torrents` 目录：目录中的直接 `.torrent` 文件会被扫描，API 也可以添加磁力链接或上传 `.torrent` 文件。单文件 torrent 直接呈现为挂载点下的文件，多文件 torrent 保留其目录结构。读取所需的 piece 保存在有界的内存缓存中，不会把 piece 数据写回磁盘。

## 项目定位与设计原则

- **挂载点只承载数据**：FUSE 文件系统是只读的，不提供管理用的 `metadata/` 或 `stats/` 控制目录；添加、删除和状态查询都通过 HTTP API 完成。
- **管理状态与缓存分离**：由 API 管理的 metainfo、未完成的磁力链接意图和 peer identity 持久化在 `torrents-dir/.metadata`，piece 内容只存在于内存。
- **缓存不是下载进度**：`cached_bytes` 表示当前仍驻留在内存中的字节数。piece 会被淘汰，因此这个数值可能下降；进程重启后缓存为空。
- **显式的网络边界**：HTTP 默认只监听 loopback。绑定非 loopback 地址时必须启用认证；服务本身不终止 TLS，应放在 TLS reverse proxy 后面。
- **API 与 UI 分层**：API 的 status 快照包含 piece、文件范围以及 `network`/DHT 诊断字段；当前 Web UI 展示文件和 piece 缓存视图，但不展示 peer/DHT 统计。

### 工作结构

```text
cmd/torrentfs          CLI、配置加载、进程生命周期和优雅退出
        │
        ├── internal/session      torrent 生命周期、metainfo、网络和内存 cache
        ├── internal/filesystem   只读 FUSE 数据树
        ├── internal/api          HTTP 路由、认证、管理和 status 快照
        └── web                   React UI；构建后由 Go embed 到二进制

<torrents-dir>
├── *.torrent            直接扫描的用户源文件（只扫描目录顶层）
└── .metadata/           API 管理的 metainfo、磁力意图和运行状态
```

启动时会恢复 `.metadata`，并只检查目录顶层名称以小写 `.torrent` 结尾的条目，不递归子目录。只有普通、非符号链接文件会被接受为有效源；匹配后缀的符号链接和目录会被识别为 invalid source，不会被加载为 torrent。文件写入方应先写入临时名称，再在同一目录中原子重命名为 `.torrent`。同一个 info hash 的多个源文件共享一个 torrent；同一个 `torrents` 目录同时只能由一个进程管理。

## 快速开始

### 环境要求

- Go 1.27 或更高版本。
- Node.js 使用 `web/.nvmrc` 指定的版本（当前为 22.23.2）以及 npm。
- 只有在挂载 FUSE 时才需要 Linux FUSE3、`/dev/fuse` 和相应的挂载权限；只运行 HTTP API/Web UI 不需要实际挂载设备。
- `curl` 可用于验证 HTTP API；Docker smoke 脚本还需要 Docker 和 `python3`。

所有命令都从仓库根目录执行。Go 使用 `web/embed.go` 嵌入 `web/dist`，因此 clean checkout 必须先生成前端产物，不能直接跳过 Web 构建执行 Go 命令。

### 构建二进制

```sh
git clone git@github.com:yakumioto/torrentfs-go.git
cd torrentfs-go

./scripts/build-web.sh
go build -o ./torrentfs ./cmd/torrentfs

mkdir -p "$PWD/torrents" "$PWD/mnt"
```

`./scripts/build-web.sh` 会使用 lockfile 安装前端依赖、生成 `web/dist`，然后删除 `web/node_modules`；它不会删除 `web/dist`。如果已经执行过等价的前端构建，也可以直接运行 `go run ./cmd/torrentfs`。

### 三种运行模式

不指定 `-config` 时使用内置默认值：HTTP 服务监听 `127.0.0.1:8080`，认证关闭，peer 监听端口由客户端选择。`<torrents-dir>` 必须事先存在、可读写且是非符号链接目录。

**HTTP API 和 Web UI（headless）**：省略 `-mountpoint` 即可；默认 HTTP listener 非空，因此这是默认模式。

```sh
./torrentfs "$PWD/torrents"
```

在另一个终端打开 `http://127.0.0.1:8080/` 或检查服务：

```sh
curl --fail http://127.0.0.1:8080/
curl --fail http://127.0.0.1:8080/api/v1/torrents
```

**HTTP API/Web UI 与 FUSE 同时运行**：提供 `-mountpoint`，HTTP 仍会启动。

```sh
./torrentfs -mountpoint "$PWD/mnt" "$PWD/torrents"
```

**只运行 FUSE**：通过环境变量把 listener 置空；此时必须提供 `-mountpoint`。

```sh
TORRENTFS_HTTP_LISTEN_ADDR= \
  ./torrentfs -mountpoint "$PWD/mnt" "$PWD/torrents"
```

也可以通过示例 TOML 启动 headless 服务：

```sh
./torrentfs -config ./torrentfs.example.toml "$PWD/torrents"
```

将本地 `.torrent` 文件放入目录时，先使用临时名称，再在同一文件系统中原子重命名：

```sh
cp /path/to/input.torrent "$PWD/torrents/input.torrent.part"
mv "$PWD/torrents/input.torrent.part" "$PWD/torrents/input.torrent"
```

文件被识别并解析后，内容会出现在 `mnt` 下。单文件 torrent 直接是一个文件，多文件 torrent 是一个目录树；挂载点中的写入、删除和重命名都会返回只读错误。也可以通过 HTTP API 添加磁力链接或上传文件，见后文 curl 流程。

按 `Ctrl-C` 或向进程发送 `SIGTERM` 可停止服务。关闭时会先停止 HTTP 服务和 session，再卸载 FUSE，以便取消仍在等待 piece 的读取。

`go run ./cmd/torrentfs ...` 可以替代 `./torrentfs ...`，但同样必须先生成 `web/dist`。

## CLI 与进程生命周期

命令行入口只接受两个 flag 和一个 positional 参数：

```text
torrentfs -mountpoint <dir> [-config <file>] <torrents-dir>
```

| 参数 | 说明 |
| --- | --- |
| `-mountpoint <dir>` | FUSE 挂载目录；HTTP listener 启用时可以省略 |
| `-config <file>` | 可选 TOML 配置文件；未提供时使用内置默认值和环境变量 |
| `<torrents-dir>` | 唯一的 positional 参数；必须是已存在、可读写、非 symlink 的目录 |
| `-h` / `--help` | 输出帮助并退出 |

单个 `.torrent` 文件不能作为 positional 参数；程序不会替用户创建 `torrents` 目录。启动时先校验参数和配置，再创建 logger、恢复 session，按需启动 HTTP 和 FUSE。HTTP listener 非空时，即使提供了 `-mountpoint` 也会同时提供 API/Web UI。

退出码：

| 退出码 | 含义 |
| --- | --- |
| `0` | 收到 `SIGINT`/`SIGTERM` 后正常关闭 |
| `1` | HTTP、session 或 FUSE unmount 的运行时错误 |
| `2` | flag 用法、positional 参数、目录或配置校验错误 |

正常关闭顺序是 HTTP API → session → FUSE unmount。session 会先取消和排空未完成的读取；FUSE unmount 使用 30 秒 deadline。若另一 mount namespace 仍持有传播的副本，进程会输出诊断并以 `1` 退出，而不是把未完成的 lazy unmount 当作成功。

## FUSE 挂载布局

```text
<mount>/
├── <single-name>       # 没有目录结构的 single-file torrent，直接是普通文件
└── <multi-name>/       # multi-file torrent，保留 torrent 的目录结构
    └── <relative-file>
```

挂载点只呈现 torrent 数据：

- 所有节点都是只读的，创建、写入、删除和重命名会失败。
- 不会挂载 `.metadata`，也没有 `metadata/` 或 `stats/` 管理目录。
- torrent 的显示名称发生冲突时会追加 hash 前缀以区分。
- single-file torrent 不会额外包一层目录，因此播放器可以直接打开例如 `<mount>/movie.mp4`。

## HTTP API

HTTP 服务和 Web UI 共用同一个 listener。默认地址是 `http://127.0.0.1:8080`，下表列出当前注册的全部管理路由：

| 方法 | 路径 | 用途 | 成功状态 |
| --- | --- | --- | --- |
| `POST` | `/api/v1/auth/login` | 使用配置的用户名和密码换取 Bearer token | `200` |
| `POST` | `/api/v1/auth/logout` | 撤销当前 Bearer token | `204` |
| `POST` | `/api/v1/torrents` | 通过 JSON 磁力链接或 multipart 上传添加 torrent | `201` |
| `GET` | `/api/v1/torrents` | 列出所有任务 | `200` |
| `GET` | `/api/v1/torrents/{id}` | 查询单个任务的汇总状态 | `200` |
| `GET` | `/api/v1/torrents/{id}/status` | 查询 piece、文件范围和网络诊断快照 | `200` |
| `DELETE` | `/api/v1/torrents/{id}` | 发起异步删除 | `202` |
| `GET` | `/api/v1/operations/{id}` | 查询删除 operation | `200` |

`{id}` 是 40 个字符的小写十六进制 info hash。业务错误使用 `{"error":"..."}` JSON；未知 API 路径和不匹配的方法由标准 `net/http` 路由处理，不能假定所有错误响应都是 JSON。

### 认证

默认配置适合本机使用：HTTP 只监听 loopback 且认证关闭，此时可以直接请求 API：

```sh
BASE_URL=http://127.0.0.1:8080
curl --fail "$BASE_URL/api/v1/torrents"
```

启用认证时，只有 `POST /api/v1/auth/login` 不需要 token；其他 `/api/` 请求都必须带且只能带一个 `Authorization: Bearer <token>` header。静态 Web shell 和 assets 的 `GET`/`HEAD` 仍然公开，这只允许浏览器加载 UI，不会公开 torrent 数据。

配置认证时必须设置用户名以及**恰好一个** bcrypt 密码来源：`password_hash` 或 `password_hash_file`。后者应指向一个普通、非符号链接且只允许文件所有者读取的文件；配置值不能是明文密码。例如：

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

`token_ttl` 必须为正的 Go duration，最长 24 小时。token 只保存在 daemon 内存中，在有效请求后滑动过期时间，进程重启后全部失效；服务不使用 Cookie、URL 参数、JWT 或 refresh token。

### curl 使用流程

下面的流程覆盖登录、列表、添加、状态查询、异步删除和登出。认证关闭时可跳过登录，并省略后续的 `Authorization` header。

1. 登录。登录请求必须使用 `application/json`：

   ```sh
   BASE_URL=http://127.0.0.1:8080
   curl --fail --request POST "$BASE_URL/api/v1/auth/login" \
     --header 'Content-Type: application/json' \
     --data '{"username":"alice","password":"<password>"}'
   ```

   成功响应包含 `token`、`token_type`（值为 `Bearer`）和 `expires_in`。把响应中的 token 保存为环境变量：

   ```sh
   TOKEN='<token-from-login-response>'
   ```

2. 列出任务：

   ```sh
   curl --fail "$BASE_URL/api/v1/torrents" \
     --header "Authorization: Bearer $TOKEN"
   ```

3. 添加磁力链接。将示例中的 info hash 替换为真实磁力链接：

   ```sh
   curl --fail --request POST "$BASE_URL/api/v1/torrents" \
     --header "Authorization: Bearer $TOKEN" \
     --header 'Content-Type: application/json' \
     --data '{"magnet_uri":"magnet:?xt=urn:btih:<info-hash>"}'
   ```

   成功返回 `201` 和任务对象。磁力链接在 metainfo 到达前可能处于 `adding` 状态；这时 status 仍会返回 `200`，但 `metainfo_ready` 为 `false`，`pieces` 和 `files` 为空。

4. 上传 `.torrent` 文件：

   ```sh
   curl --fail --request POST "$BASE_URL/api/v1/torrents" \
     --header "Authorization: Bearer $TOKEN" \
     --form 'file=@./input.torrent'
   ```

   不要手动设置 `Content-Type: multipart/form-data`。`curl` 必须自动生成包含 boundary 的 header；手动覆盖它会使服务无法解析表单。上传请求默认最多 10 MiB，可通过 `http.max_upload_bytes` 调整。

5. 查看汇总和详细 status。把 `<torrent-id>` 替换为添加响应中的 `id`：

   ```sh
   TORRENT_ID='<torrent-id>'
   curl --fail "$BASE_URL/api/v1/torrents/$TORRENT_ID" \
     --header "Authorization: Bearer $TOKEN"

   curl --fail "$BASE_URL/api/v1/torrents/$TORRENT_ID/status" \
     --header "Authorization: Bearer $TOKEN"
   ```

   status 响应的主要字段如下：

   ```json
   {
     "torrent": {
       "id": "<info-hash>",
       "info_hash": "<info-hash>",
       "name": "example",
       "state": "ready",
       "total_bytes": 262144,
       "cached_bytes": 262144,
       "created_at": "2026-01-01T00:00:00Z"
     },
     "metainfo_ready": true,
     "piece_length": 262144,
     "pieces": [
       {"index": 0, "cached": true, "cached_bytes": 262144, "pinned": false}
     ],
     "files": [
       {"path": "file.bin", "size": 262144, "piece_start": 0, "piece_end": 1}
     ],
     "network": {
       "effective_listen_port": 6881,
       "total_peers": 0,
       "pending_peers": 0,
       "active_peers": 0,
       "connected_seeders": 0,
       "piece_complete": 0,
       "dht": []
     }
   }
   ```

   `files` 中的 piece 范围是半开区间 `[piece_start, piece_end)`，指向同一份绝对、从零开始的 `pieces` 数组。`network` 是当前 raw API 已返回的诊断字段；Web UI 尚未提供 peer/DHT 面板。

6. 删除任务并轮询 operation。删除不是同步完成的：

   ```sh
   DELETE_RESPONSE="$({ curl --fail --request DELETE "$BASE_URL/api/v1/torrents/$TORRENT_ID" \
     --header "Authorization: Bearer $TOKEN"; })"
   printf '%s\n' "$DELETE_RESPONSE"
   OPERATION_ID='<operation_id-from-delete-response>'

   curl --fail "$BASE_URL/api/v1/operations/$OPERATION_ID" \
     --header "Authorization: Bearer $TOKEN"
   ```

   删除响应为 `202`，初始 operation 状态通常为 `deleting`；随后轮询到 `deleted` 或 `delete_failed`。如果用户目录中仍有顶层 `.torrent` 文件引用该 info hash，删除会返回 `409`。

7. 登出：

   ```sh
   curl --fail --request POST "$BASE_URL/api/v1/auth/logout" \
     --header "Authorization: Bearer $TOKEN"
   ```

   成功响应为 `204` 空 body。

### 状态与错误语义

`state` 描述生命周期，不是下载或完成百分比：

| `state` | 含义 |
| --- | --- |
| `adding` | metainfo 尚未可用，例如磁力链接仍在解析 |
| `ready` | metainfo 已可用，可以读取当前缓存中的内容 |
| `error` | 无法取得或处理 metainfo |
| `deleting` | 删除正在进行 |
| `delete_failed` | 删除失败，可再次处理 |
| `deleted` | 删除 operation 的终态，不会作为任务持久化 |

`cached_bytes` 是内存 cache 当前的占用量，而不是已经下载过的字节数；piece 被淘汰后该值会下降，重启后从零开始。`ready` 不表示整个 torrent 已经下载完成，也不表示所有 piece 都在内存中。

常见 HTTP 状态：

| 状态 | 典型原因 |
| --- | --- |
| `400` | JSON/multipart body、`Content-Type`、磁力链接或文件名无效 |
| `401` | 认证缺失、格式错误、过期或已撤销；登录凭据错误也返回 `401` |
| `404` | 未知 torrent/operation；认证关闭时 login/logout 也不可用；不存在的静态 asset |
| `409` | torrent 正在删除（包括 `delete_failed` 状态下重新添加同一 info hash），或仍被用户拥有的 `.torrent` 文件引用 |
| `413` | body 超过限制；登录 body 上限固定为 8 KiB，上传上限由 `http.max_upload_bytes` 控制 |
| `415` | 添加 torrent 时使用了 JSON 或 multipart 之外的 media type |
| `500` | 未分类的内部 session/API 错误 |

受保护 API 的 `401` 响应包含 `WWW-Authenticate: Bearer`。未知路由或方法可能由标准 HTTP handler 返回 `404`/`405`，不要把它们当作 SPA 页面或统一的业务 JSON 错误。

## Web UI

HTTP 服务启用时，同一个 listener 同时提供嵌入式 Web UI 和 `/api/v1`：

- Dashboard 支持按名称或 info hash 搜索、按全部/就绪/错误筛选，显示任务摘要，并提供磁力/文件添加入口。
- 任务详情页提供概览、文件和数据块三个 tab；文件视图显示 piece 范围和缓存覆盖，piece map 区分 cached、pinned 和 uncached。
- 删除由 UI 发起后会轮询 operation；请求失败、连接断开和 session 过期会显示对应的错误或重新登录状态。
- 查询默认每 5 秒刷新；浏览器页面不可见时不会在后台继续刷新。
- 认证开启时，UI 把 opaque Bearer token 放在当前 tab 的 `sessionStorage` 中；服务端 token 仍只存在 daemon 内存中。服务重启或 token 过期后需要重新登录。

生产 UI 由 Go embed 提供。静态服务只接受 `GET` 和 `HEAD`：根路径和无扩展名的前端 deep link 会回退到 `index.html`，带扩展名的未知 asset 返回 `404`；`index.html` 使用 `no-cache`，构建后的 assets 使用长期 immutable 缓存。

开发时可以单独运行 Vite：

```sh
npm ci --prefix web
npm run dev --prefix web
```

Vite 默认监听 `127.0.0.1:5173`，并把 `/api` 代理到 `http://127.0.0.1:8080`。后端仍需在另一个终端运行；生产构建使用同源的相对 `/api/v1` 请求。

## 配置

可以直接使用内置默认值，也可以复制仓库中的完整示例：

```sh
cp torrentfs.example.toml torrentfs.local.toml
go run ./cmd/torrentfs -config ./torrentfs.local.toml "$PWD/torrents"
```

配置优先级按字段合并：环境变量 > TOML 文件 > 内置默认值。仓库只为下表列出的受支持字段提供精确的 `TORRENTFS_` 环境变量映射，不会自动为每个 TOML key 生成变量；嵌套 section 用下划线连接，例如 `http.auth.token_ttl` 对应 `TORRENTFS_HTTP_AUTH_TOKEN_TTL`。未列出的变量会被忽略。

### 主要配置项

| Section | 常用 key | 说明 |
| --- | --- | --- |
| `[http]` | `listen_addr`, `max_upload_bytes` | HTTP/Web listener；默认 `127.0.0.1:8080`，上传默认上限 10 MiB；为空则禁用 HTTP |
| `[http.auth]` | `enabled`, `username`, `password_hash`, `password_hash_file`, `token_ttl` | 单用户 bcrypt 登录和内存 Bearer token |
| `[connections]` | `listen_host`, `listen_port`, `disable_ipv4`, `disable_ipv6`, `no_port_forwarding`, `bootstrap_nodes` | peer listener、地址族、UPnP/NAT-PMP 和 DHT bootstrap |
| `[proxy]` | `socks5_url` | 可选 `socks5://` 或 `socks5h://` 出站代理 |
| `[cache]` | `capacity_bytes` | 内存 piece cache 硬上限，默认 2 GiB；必须为正数 |
| `[identity]` | `tracker_user_agent`, `peer_id_prefix`, `extended_handshake_client_version` | tracker/peer 握手身份 |
| `[log]` | `level`, `format`, `add_source` | `debug`/`info`/`warn`/`error`，`text`/`json` 和源码位置 |
| `[mount]` | `allow_other` | 是否允许除挂载用户外的本机 UID 读取 FUSE 挂载 |

重要配置关系：

- `http.listen_addr` 默认为 loopback。绑定 `0.0.0.0`、空 host 或其他非 loopback 地址时，必须同时启用完整的 `[http.auth]` 配置。
- 服务没有 TLS。公开 HTTP listener 时，应在可信的 TLS reverse proxy 后面使用，并限制可访问网络。
- `password_hash` 与 `password_hash_file` 不能同时设置，也不能都为空；密码来源必须是 bcrypt hash，而不是明文。
- `token_ttl` 是无活动过期窗口；有效的认证请求会滑动该窗口，最长为 24 小时。
- `connections.listen_port = 0` 会选择临时端口。需要接受入站 peer 时应设置固定端口，并同时发布 TCP 和 UDP；`no_port_forwarding` 默认关闭 UPnP/NAT-PMP 自动映射。
- `disable_ipv4` 和 `disable_ipv6` 最多只能启用一个；同时禁用会留下没有传输协议的 session。
- `cache.capacity_bytes` 是上限而非预留量，cache 按需增长。容器中应根据内存限制调低它，避免进程被 OOM kill。
- `mount.allow_other` 默认关闭。启用后所有本机 UID 都可能读取挂载；非 root 挂载还需要 `/etc/fuse.conf` 中允许 `user_allow_other`。

### 环境变量与校验

当前实际读取的 22 个受支持环境变量映射如下；未列出的 TOML key 没有自动生成的环境变量：

| 环境变量 | TOML key | 格式 |
| --- | --- | --- |
| `TORRENTFS_CONNECTIONS_LISTEN_HOST` | `connections.listen_host` | 字符串 |
| `TORRENTFS_CONNECTIONS_LISTEN_PORT` | `connections.listen_port` | 十进制整数 |
| `TORRENTFS_CONNECTIONS_DISABLE_IPV4` | `connections.disable_ipv4` | Go boolean |
| `TORRENTFS_CONNECTIONS_DISABLE_IPV6` | `connections.disable_ipv6` | Go boolean |
| `TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING` | `connections.no_port_forwarding` | Go boolean |
| `TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES` | `connections.bootstrap_nodes` | 逗号分隔的 `host:port` 列表 |
| `TORRENTFS_MOUNT_ALLOW_OTHER` | `mount.allow_other` | Go boolean |
| `TORRENTFS_PROXY_SOCKS5_URL` | `proxy.socks5_url` | 字符串 |
| `TORRENTFS_CACHE_CAPACITY_BYTES` | `cache.capacity_bytes` | 十进制整数 |
| `TORRENTFS_IDENTITY_TRACKER_USER_AGENT` | `identity.tracker_user_agent` | 字符串 |
| `TORRENTFS_IDENTITY_PEER_ID_PREFIX` | `identity.peer_id_prefix` | 字符串 |
| `TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION` | `identity.extended_handshake_client_version` | 字符串 |
| `TORRENTFS_HTTP_LISTEN_ADDR` | `http.listen_addr` | 字符串；空值关闭 HTTP |
| `TORRENTFS_HTTP_MAX_UPLOAD_BYTES` | `http.max_upload_bytes` | 十进制整数 |
| `TORRENTFS_HTTP_AUTH_ENABLED` | `http.auth.enabled` | Go boolean |
| `TORRENTFS_HTTP_AUTH_USERNAME` | `http.auth.username` | 字符串 |
| `TORRENTFS_HTTP_AUTH_PASSWORD_HASH` | `http.auth.password_hash` | bcrypt 字符串 |
| `TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE` | `http.auth.password_hash_file` | 文件路径 |
| `TORRENTFS_HTTP_AUTH_TOKEN_TTL` | `http.auth.token_ttl` | Go duration，如 `30m` |
| `TORRENTFS_LOG_LEVEL` | `log.level` | `debug`/`info`/`warn`/`error` |
| `TORRENTFS_LOG_FORMAT` | `log.format` | `text`/`json` |
| `TORRENTFS_LOG_ADD_SOURCE` | `log.add_source` | Go boolean |

例如，可以用环境变量覆盖默认 listener 和 cache 上限：

```sh
TORRENTFS_HTTP_LISTEN_ADDR=127.0.0.1:8080 \
TORRENTFS_HTTP_MAX_UPLOAD_BYTES=10485760 \
TORRENTFS_CACHE_CAPACITY_BYTES=1073741824 \
./torrentfs "$PWD/torrents"
```

加载规则和边界：

- 配置按字段合并，优先级为环境变量 > TOML 文件 > 内置默认值。严格 TOML decoder 会拒绝未知字段；旧的 `[paths]` section（包括 `paths.data_dir`）不再支持，旧的 `TORRENTFS_PATHS_DATA_DIR` 会被忽略。
- 环境变量存在但为空时，字符串字段可以被清空；数字、布尔值和 duration 的空值会报错。环境覆盖发生在交叉字段校验前，因此切换认证密码来源时要显式清空不再使用的 `TORRENTFS_HTTP_AUTH_PASSWORD_HASH`。
- `connections.listen_port` 必须在 `0..65535`；`disable_ipv4` 和 `disable_ipv6` 不能同时为 `true`；每个 `bootstrap_nodes` 项都必须是合法且端口在 `1..65535` 的 `host:port`。
- `cache.capacity_bytes` 必须大于零；piece length 大于 cache capacity 的 torrent 会在添加时被拒绝。`proxy.socks5_url` 只能为空、`socks5://` 或 `socks5h://`，且必须包含合法 host/port；校验错误不会把 proxy 凭据写入错误信息。
- `identity.peer_id_prefix` 最多 20 bytes；tracker User-Agent 不能包含 CR/LF。`http.max_upload_bytes` 必须大于零，日志 level/format 只能使用上表值。
- HTTP listener 为空表示关闭；非空值必须是合法的 `host:port`。非 loopback listener 必须同时启用完整认证配置。
- 认证关闭时 username、password hash 和 hash file 必须全为空；认证开启时 username 非空、token TTL 为 `>0` 且 `<=24h`，并且 `password_hash` 与 `password_hash_file` 必须恰好设置一个。
- bcrypt hash file 必须是非空的普通非符号链接文件，只允许 owner 读取，大小不超过 1024 bytes；服务会验证 bcrypt cost，不能把明文密码放入配置或环境变量。

`TORRENTFS_FUSE_REQUIRED` 不是 daemon 配置，而是测试门禁。`TORRENTFS_PATHS_DATA_DIR` 是已移除的历史变量，不会恢复旧的磁盘 payload 路径。

## 持久化状态与限制

`torrents-dir/.metadata` 是内部实现目录，不会出现在 FUSE 挂载中。常见内容如下：

```text
<torrents-dir>/
├── input.torrent
└── .metadata/
    ├── <info-hash>.torrent       # API 管理的 canonical metainfo
    ├── <info-hash>.magnet        # 尚未解析完成的磁力意图
    ├── peer_id                   # 该 torrents 目录的 20 字节 peer identity
    ├── state/<info-hash>.json    # 中断删除的 sidecar（如有）
    └── instance.lock             # 进程运行期间的独占锁
```

- piece 数据、piece completion、cache hit 计数和临时读取优先级都只在内存中；重启不会从磁盘 rehash 或恢复 piece。
- 顶层用户源只按大小写敏感的 `.torrent` 后缀选择，不递归子目录；`.torrent.part`、`.TORRENT` 和 torrent 命名的目录不会成为源。匹配后缀的符号链接会被识别为 invalid source，不会被跟随加载；使用普通文件并采用临时文件后 atomic rename。
- 配置只在启动时读取，修改 TOML 或环境变量后需要重启进程。
- 磁力链接会先持久化为 `.metadata/<info-hash>.magnet`，解析到 metainfo 后再保存 canonical `.torrent`；删除会清理该 hash 的内部来源，但不会删除用户拥有的顶层 `.torrent` 文件。
- 一个 `torrents` 目录同时只能由一个 torrentfs 进程使用。

## Docker

Dockerfile 使用 Node 22.23.2 构建 Web UI，再使用 Go 1.27 编译包含 `web/dist` 的 `CGO_ENABLED=0` 二进制；运行阶段是安装了 `fuse3`、CA certificates 和 `passwd` 的 Debian bookworm-slim。

### 镜像默认行为

```sh
docker build -t torrentfs .
docker run --rm torrentfs
```

默认命令等价于：

```text
/usr/local/bin/torrentfs -config /etc/torrentfs/torrentfs.toml /torrents
```

镜像会创建 `/torrents`，但 HTTP listener 仍默认绑定容器内的 `127.0.0.1:8080`；`-p 8080:8080` 不会改变 daemon 的监听地址。Docker 配置与本地默认值也不同：镜像把 peer `listen_port` 固定为 `6881`，并默认禁用 IPv6；本地默认 peer 端口为 `0`，地址族都启用。环境变量可以覆盖 `/etc/torrentfs/torrentfs.toml`。

### HTTP-only 检查

仓库提供不需要 FUSE 挂载的 HTTP smoke test：

```sh
./scripts/http-smoke.sh
```

脚本需要 Docker、`curl` 和 `python3`。它会构建本地镜像并检查静态 root/deep link、缺失 asset、认证 `401`、`WWW-Authenticate`、登录、带 token 的列表和登出；它不会把任何内容发布到远端 registry。

要验证内置配置、默认 CMD、环境变量覆盖和外部 TOML 文件，可以运行：

```sh
./scripts/docker-config-smoke.sh
```

该脚本还需要 `awk` 和 `timeout`；外部 TOML 以只读方式挂载，但 `/torrents` 仍必须可写。

### 构建并运行 HTTP 服务

公开容器 listener 前必须配置认证。下面的命令使用 bind-mounted bcrypt hash 文件；请先创建该文件并将 `<bcrypt-hash>` 替换为真实 hash，不要把明文密码写入环境变量：

```sh
mkdir -p /srv/torrents /srv/secrets
# /srv/secrets/torrentfs-password-hash 只包含一行 bcrypt hash，权限应为 600

docker run --rm \
  --publish 8080:8080 \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/secrets/torrentfs-password-hash,dst=/run/secrets/password-hash,readonly \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_HTTP_AUTH_USERNAME=alice \
  --env TORRENTFS_HTTP_AUTH_PASSWORD_HASH= \
  --env TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE=/run/secrets/password-hash \
  torrentfs
```

镜像内置的默认命令使用 `/torrents` 和 `/etc/torrentfs/torrentfs.toml`；环境变量会覆盖内置 TOML。`-p`/`--publish` 只发布端口，不会改变 daemon 实际监听的地址。

### Docker FUSE 挂载

Linux rootful Docker 需要把 FUSE 设备和挂载能力交给容器。Dockerfile 没有 `USER` 指令；如果省略 `--user`，daemon 会以 root 运行。默认 `mount.allow_other=false` 时，root 创建的挂载通常只对 root 的 uid/gid 可读，宿主机普通用户可能得到 `EACCES`。下面是仓库 smoke test 使用的 host-user 路径：

```sh
HOST_UID="$(id -u)"
HOST_GID="$(id -g)"

docker build \
  --build-arg TORRENTFS_UID="$HOST_UID" \
  --build-arg TORRENTFS_GID="$HOST_GID" \
  -t torrentfs .

mkdir -p /srv/torrents /srv/mnt
# 如果目录不属于当前用户，需要管理员执行此命令。
chown "$HOST_UID:$HOST_GID" /srv/torrents /srv/mnt

docker run --rm \
  --user "$HOST_UID:$HOST_GID" \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  --env TORRENTFS_HTTP_LISTEN_ADDR= \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  torrentfs -config /etc/torrentfs/torrentfs.toml -mountpoint /mnt /torrents
```

`TORRENTFS_UID/GID` build args 必须与运行时 `--user` 一致；两个 bind source 都必须对该用户可写。某些系统不需要 `apparmor=unconfined`，但如果 AppArmor 阻止 FUSE，则必须按主机策略放行。`/dev/fuse`、`SYS_ADMIN` 和等价的安全配置不是镜像可以自行授予的权限；缺少它们时容器会挂载失败，而不会静默退化为普通目录。

`/srv/mnt` 所在的主机挂载点必须支持递归双向传播；可以先检查：

```sh
findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
```

`/mnt` bind mount 不能设为只读，因为 FUSE daemon 需要在其中创建 submount；FUSE 文件系统本身仍然是只读的。`rshared` 和 rootful `SYS_ADMIN` 会扩大挂载权限边界，不应把此容器暴露给不受信任的调用者。

要让容器接受入站 peer，应使用镜像配置中的固定 `listen_port = 6881`，并在上面的 `docker run` 中追加 `--publish 6881:6881/tcp --publish 6881:6881/udp`。只发布 HTTP 端口不会让 peer 端口可达；动态端口 `0` 也不能预先发布。

### 以宿主机用户运行 FUSE

镜像默认不声明 `USER`，可以用匹配宿主机 UID/GID 的构建参数和运行参数降低 daemon 身份：

```sh
HOST_UID="$(id -u)"
HOST_GID="$(id -g)"

docker build \
  --build-arg TORRENTFS_UID="$HOST_UID" \
  --build-arg TORRENTFS_GID="$HOST_GID" \
  -t torrentfs .

mkdir -p /srv/torrents /srv/mnt
# 如果目录不属于当前用户，需要管理员执行此命令。
chown "$HOST_UID:$HOST_GID" /srv/torrents /srv/mnt

docker run --detach --name torrentfs \
  --user "$HOST_UID:$HOST_GID" \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  --env TORRENTFS_HTTP_LISTEN_ADDR= \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  torrentfs -mountpoint /mnt /torrents
```

两个 bind source 都必须对该用户可写：`/torrents` 要保存 `.metadata`，`/mnt` 要允许 FUSE 创建 submount。若 `/dev/fuse` 是 `root:fuse` 且用户不在 fuse 组，还需要按宿主机策略补充 `--group-add`。可以用 `docker exec torrentfs id`、`findmnt -T /srv/mnt` 和 `docker stop torrentfs` 检查身份、传播和清理结果。

### 可选的单容器 SMB 只读共享

默认关闭。设置 `TORRENTFS_SMB_ENABLED=true` 后，容器入口在同一个 mount namespace 内先让 torrentfs 挂载只读 FUSE 到固定的内部路径 `/mnt/torrentfs`，确认该路径的 fstype 为 `fuse.*` 之后才启动 `smbd`，仅监听 TCP 445。宿主机不需要看到 `/mnt/torrentfs`，也不需要 `rshared` 或跨容器 mount propagation。

| 变量 | 说明 |
| --- | --- |
| `TORRENTFS_SMB_ENABLED` | `true`/`false`（默认 `false`）。只有严格为 `true`、`1` 时才进入 SMB 模式 |
| `TORRENTFS_SMB_USERNAME` | 可选。默认使用镜像构建参数解析出的 torrentfs 运行账号；若覆盖，必须解析到同一个运行 UID |
| `TORRENTFS_SMB_PASSWORD_FILE` | 启用 SMB 时必填。指向只读 secret 文件，密码只经 stdin 写入 Samba passdb |

share 名固定为 `torrentfs`，路径固定为 `/mnt/torrentfs`；`/torrents` 不会被共享，因为其中包含可写的 `.metadata`、peer identity 和锁文件。share 始终 `read only = yes`，`guest ok = no`，`map to guest = never`，只发布 TCP 445，不启动 `nmbd`，也不暴露 137/138/139。

`/dev/fuse`、`SYS_ADMIN` 和 `CAP_NET_BIND_SERVICE` 是运行前提：SMB 模式下 torrentfs 和 smbd 都以镜像内解析出的专用非 root 身份运行，`CAP_NET_BIND_SERVICE` 让该身份可以绑定 445。权限方案是同 UID：Samba 用 `force user`/`force group` 映射到同一个运行身份，因此不需要 `allow_other`，FUSE 访问范围不会因为 SMB 而扩大。AppArmor/安全策略是否放行由宿主策略决定；缺少设备、capability 或 secret 时容器会在启动任何 listener 之前以非零状态失败，并输出诊断，不会退化成共享一个普通目录。

secret 文件必须是普通、非符号链接、非空、单行、不超过 1024 字节，且不可被 group/other 读取（例如 `chmod 400`）。下面的示例同时发布 HTTP 和 SMB，因此准备了两个 secret：SMB 密码明文文件，以及 HTTP 认证用的单行 bcrypt hash 文件。两者都不提交到仓库：

```sh
mkdir -p /srv/torrents /srv/secrets
printf '%s\n' '<smb-password>' > /srv/secrets/smb-password
# 任意工具生成的单行 bcrypt hash 均可；这里用 apache2-utils 的 htpasswd，
# 没有本地 htpasswd 时可以用容器代替：
#   docker run --rm httpd:2.4 htpasswd -nbBC 10 '' '<http-password>'
htpasswd -bnBC 10 '' '<http-password>' | tr -d ':\n' > /srv/secrets/torrentfs-password-hash
printf '\n' >> /srv/secrets/torrentfs-password-hash
chmod 400 /srv/secrets/smb-password /srv/secrets/torrentfs-password-hash

docker run --rm \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --cap-add NET_BIND_SERVICE \
  --security-opt apparmor=unconfined \
  --publish 445:445 \
  --publish 8080:8080 \
  --publish 6881:6881/tcp --publish 6881:6881/udp \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/secrets/smb-password,dst=/run/secrets/smb-password,readonly \
  --mount type=bind,src=/srv/secrets/torrentfs-password-hash,dst=/run/secrets/torrentfs-password-hash,readonly \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_HTTP_AUTH_USERNAME=alice \
  --env TORRENTFS_HTTP_AUTH_PASSWORD_HASH= \
  --env TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE=/run/secrets/torrentfs-password-hash \
  --env TORRENTFS_SMB_ENABLED=true \
  --env TORRENTFS_SMB_PASSWORD_FILE=/run/secrets/smb-password \
  torrentfs
```

如果只需要 SMB，可以省略 `--publish 8080`、`--env TORRENTFS_HTTP_*` 和 `--env TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE` 三组 HTTP 参数：镜像内置的 HTTP listener 保持 container-local `127.0.0.1:8080`，不发布即可。

身份与退出语义：

- 容器入口以 root 启动，仅为完成 passdb 初始化、运行身份切换和绑定 445；torrentfs、`smbd` 及其子进程都以专用非 root 身份运行，FUSE 与 SMB 文件访问身份一致。构建参数把运行 UID 设为 `0` 时，SMB 模式会明确拒绝启动。
- 用户传入自己的 `-mountpoint` 时入口会拒绝启动，避免 Samba path 与 FUSE path 分叉。
- torrentfs 或 smbd 任一核心进程异常退出、或 FUSE mount 在运行期消失，容器都会停止另一个进程并以非零状态退出。
- 收到 `SIGTERM`/`SIGINT` 时先有界停止并回收 Samba（超时才 `SIGKILL`），再通知 torrentfs 执行既有的 HTTP → session → FUSE unmount 关闭链；正常关闭返回 0，强制终止会记录日志并返回非零。
- 客户端随机 seek（例如播放器跳到文件中段）会经 Samba `pread` → FUSE `Read(off)` → piece planner/cache 拉取对应 pieces，读取链路与 HTTP/FUSE 模式完全一致。内存 cache 与 Samba 共享同一 cgroup，规划 cache 余量时要把两者算在一起。
- 显式开启 `mount.allow_other=true` 会扩大同一 user namespace 内的访问面；SMB 模式本身不依赖它，也不由入口强制打开。

## 构建、测试与贡献

前端构建产物被 Go embed，因此本地 quality 检查应先完成前端检查和构建，再执行 Go 命令。下面的顺序与 CI 可复用 action 一致：

```sh
npm ci --prefix web
npm run typecheck --prefix web
npm run lint --prefix web
npm test --prefix web -- --run
npm run build --prefix web
rm -rf -- web/node_modules

go build ./...
go vet ./...
go test ./...
go test -race ./...

go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
golangci-lint run ./...
```

如果只需要生成供 Go 使用的前端产物，可以运行：

```sh
./scripts/build-web.sh
```

该脚本执行 lockfile 安装、构建 `web/dist` 并删除 `web/node_modules`，不会删除 `web/dist`。

验证按依赖分层：

- **普通前端/Go 检查**：执行上面的 typecheck、lint、Vitest、build、`go build`、`go vet`、`go test`、race test 和 `golangci-lint`。
- **HTTP-only 容器检查**：执行 `./scripts/http-smoke.sh`；它不需要 FUSE。`./scripts/docker-config-smoke.sh` 另外验证镜像内置配置、默认 CMD、环境变量覆盖和外部 TOML。
- **FUSE-required 测试**：真实 FUSE 测试需要 `/dev/fuse`、`fusermount`/`fusermount3` 和挂载权限；缺少 FUSE 时普通测试会跳过这些用例。要把缺少前置条件视为失败，可使用：

  ```sh
  TORRENTFS_FUSE_REQUIRED=1 go test -race -run 'TestFuse|TestSessionIncomplete' ./...
  ```

- **rootful Docker/FUSE 检查**：Linux Docker daemon 还需要 `/dev/fuse`、`SYS_ADMIN` 或等效 capability、通常的 `apparmor=unconfined`、`findmnt`、`python3`、`sha256sum` 和 `timeout`；确认 `/srv/mnt` 所在 host mount 支持 `rshared` 后再执行：

  ```sh
  ./scripts/docker-smoke.sh
  ```

- **单容器 SMB 检查**：需要 Linux Docker daemon、`/dev/fuse`、`SYS_ADMIN`、`CAP_NET_BIND_SERVICE`、通常的 `apparmor=unconfined`、`python3`、`sha256sum`、`dd` 和 `timeout`。脚本构建镜像与独立 SMB client 镜像，在隔离 Docker network 里验证认证与 guest 拒绝、目录列举、全量读取哈希、只读拒写、`.metadata` 不可见、容器与宿主 UID 不同时 `.metadata` 仍可读、正常 SIGTERM 顺序，以及 smbd/torrentfs 异常退出的联动和退出码。所有 client 操作共用同一个有界超时（`CLIENT_TIMEOUT`，smbclient 自身用 `-t`），失败时打印应用日志与 web seed 日志：

  ```sh
  ./scripts/docker-smb-smoke.sh
  ```

  脚本还会在 client 容器内用 `mount.cifs` 只读挂载 share，并以非零大偏移 `dd iflag=skip_bytes,count_bytes` 读取大于内存 cache 的区间，与源文件对应切片比对，证明随机 seek 走的是现有 piece planner/cache，而不是顺序下载。这一步需要宿主机内核提供 CIFS 模块并允许 nested `mount.cifs`：脚本会区分「宿主不具备 CIFS 能力」和「挂载成功但数据错误」——前者打印明确的 `host cannot mount CIFS` 说明并继续（此时由 required 的 `TORRENTFS_FUSE_REQUIRED=1 go test -race -run 'TestFuse|TestSessionIncomplete' ./...`，含 `>4 GiB` 虚拟文件的大偏移用例，承担随机读取证据），后者直接失败。需要无条件跳过时设置 `TORRENTFS_SMB_SKIP_CIFS=1`。CI 与 nightly 都不设置该变量，因此在支持 CIFS 的 runner 上会自动执行完整比对。

  宿主 UID 与镜像内运行身份相同时，容器写入的 `.metadata` 与清理路径恰好一致，容易掩盖权限问题；用 `TORRENTFS_SMOKE_UID`/`TORRENTFS_SMOKE_GID` 指定一个不同的构建期运行身份，即可在同一台机器上验证跨 UID 的 metadata 可遍历性与 scratch 目录清理（CI runner 的 UID 与镜像默认值本就不同）：

  ```sh
  TORRENTFS_SMOKE_UID=1500 TORRENTFS_SMOKE_GID=1500 ./scripts/docker-smb-smoke.sh
  ```

`TORRENTFS_FUSE_REQUIRED` 只控制测试门禁，不是 daemon 的运行时配置。CI nightly 的多平台 OCI 构建与本地 `scripts/nightly-build.sh` 归档脚本是不同入口；本 README 的命令用于本地构建、运行和验证，不把手工归档脚本写成 nightly 发布保证。

## 许可证

Mozilla Public License 2.0，见 [LICENSE](LICENSE)。
