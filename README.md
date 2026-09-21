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

启动时会恢复 `.metadata`，并扫描目录顶层的普通、非符号链接、名称以小写 `.torrent` 结尾的文件。文件写入方应先写入临时名称，再在同一目录中原子重命名为 `.torrent`。同一个 info hash 的多个源文件共享一个 torrent；同一个 `torrents` 目录同时只能由一个进程管理。

## 快速开始

### 环境要求

- Go 1.27 或更高版本。
- Node.js 使用 `web/.nvmrc` 指定的版本（当前为 22.23.2）以及 npm。
- 只有在挂载 FUSE 时才需要 Linux FUSE3、`/dev/fuse` 和相应的挂载权限；只运行 HTTP API/Web UI 不需要实际挂载设备。
- `curl` 可用于验证 HTTP API。

Go 使用 `web/embed.go` 嵌入 `web/dist`，因此第一次 `go build`、`go test` 或 `go run` 前必须先生成 `web/dist`。

### 本地构建并运行

```sh
git clone git@github.com:yakumioto/torrentfs-go.git
cd torrentfs-go

npm ci --prefix web
npm run build --prefix web

mkdir -p "$PWD/torrents" "$PWD/mnt"
```

在第一个终端启动服务和挂载：

```sh
go run ./cmd/torrentfs -mountpoint "$PWD/mnt" "$PWD/torrents"
```

不指定 `-config` 时使用内置默认值：HTTP 服务监听 `127.0.0.1:8080`，认证关闭，peer 监听端口由客户端选择。保持进程运行后，在第二个终端检查服务并添加一个本地 `.torrent` 文件：

```sh
curl --fail http://127.0.0.1:8080/

cp /path/to/input.torrent "$PWD/torrents/input.torrent.part"
mv "$PWD/torrents/input.torrent.part" "$PWD/torrents/input.torrent"
```

文件被识别并解析后，内容会出现在 `mnt` 下。单文件 torrent 直接是一个文件，多文件 torrent 是一个目录树；挂载点中的写入、删除和重命名都会返回只读错误。也可以通过下面的 HTTP API 添加磁力链接或上传文件。

按 `Ctrl-C` 或向进程发送 `SIGTERM` 可停止服务。关闭时会先停止 HTTP 服务和 session，再卸载 FUSE，以便取消仍在等待 piece 的读取。

### 仅运行 HTTP API 和 Web UI

启用 HTTP 后可以省略 `-mountpoint`，运行 headless 模式：

```sh
mkdir -p "$PWD/torrents"
go run ./cmd/torrentfs -config ./torrentfs.example.toml "$PWD/torrents"
```

`-config` 指向的 TOML 文件会在每次启动时读取。如果把 `[http].listen_addr` 设为空字符串，HTTP 服务会被禁用，此时必须提供 `-mountpoint`。

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
       "total_bytes": 1234,
       "cached_bytes": 262144,
       "created_at": "2026-01-01T00:00:00Z"
     },
     "metainfo_ready": true,
     "piece_length": 262144,
     "pieces": [
       {"index": 0, "cached": true, "cached_bytes": 262144, "pinned": false}
     ],
     "files": [
       {"path": "file.bin", "size": 1234, "piece_start": 0, "piece_end": 1}
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
| `409` | torrent 正在删除，或仍被用户拥有的 `.torrent` 文件引用 |
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

配置优先级按字段合并：环境变量 > TOML 文件 > 内置默认值。每个叶子 TOML key 都有对应的 `TORRENTFS_` 环境变量；嵌套 section 用下划线连接，例如 `http.auth.token_ttl` 对应 `TORRENTFS_HTTP_AUTH_TOKEN_TTL`。只有已支持的精确变量名会被读取。

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

### 环境变量示例

```sh
TORRENTFS_HTTP_LISTEN_ADDR=127.0.0.1:8080 \
TORRENTFS_HTTP_MAX_UPLOAD_BYTES=10485760 \
TORRENTFS_CACHE_CAPACITY_BYTES=1073741824 \
go run ./cmd/torrentfs "$PWD/torrents"
```

环境变量可以覆盖 TOML 中对应字段，也可以用空字符串清除字符串值；数字、布尔值和 duration 不能使用空字符串。认证切换密码来源时，要显式清空不再使用的 `TORRENTFS_HTTP_AUTH_PASSWORD_HASH`，并保留 `TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE`。

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
- 顶层用户 `.torrent` 文件只读到目录第一层，不递归子目录；符号链接、临时扩展名和 torrent 命名的目录会被忽略。
- 配置只在启动时读取，修改 TOML 或环境变量后需要重启进程。
- 磁力链接会先持久化为 `.metadata/<info-hash>.magnet`，解析到 metainfo 后再保存 canonical `.torrent`；删除会清理该 hash 的内部来源，但不会删除用户拥有的顶层 `.torrent` 文件。
- 一个 `torrents` 目录同时只能由一个 torrentfs 进程使用。

## Docker

### HTTP-only 检查

仓库提供不需要 FUSE 挂载的 HTTP smoke test：

```sh
./scripts/http-smoke.sh
```

脚本需要 Docker、`curl` 和 `python3`。它会构建本地镜像并检查静态 root/deep link、缺失 asset、认证 `401`、`WWW-Authenticate`、登录、带 token 的列表和登出；它不会把任何内容发布到远端 registry。

### 构建并运行 HTTP 服务

Dockerfile 使用 Node 22.23.2 构建 Web UI，再使用 Go 1.27 编译包含 `web/dist` 的二进制，运行阶段是带 `fuse3` 的 Debian 镜像：

```sh
docker build -t torrentfs .
```

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

Linux rootful Docker 需要把 FUSE 设备和挂载能力交给容器：

```sh
mkdir -p /srv/torrents /srv/mnt
docker run --rm \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  --env TORRENTFS_HTTP_LISTEN_ADDR= \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  torrentfs -config /etc/torrentfs/torrentfs.toml -mountpoint /mnt /torrents
```

某些系统不需要 `apparmor=unconfined`，但如果 AppArmor 阻止 FUSE，则必须按主机策略放行。`/dev/fuse`、`SYS_ADMIN` 和等价的安全配置不是镜像可以自行授予的权限；缺少它们时容器会挂载失败，而不会静默退化为普通目录。

`/srv/mnt` 所在的主机挂载点必须支持递归双向传播；可以先检查：

```sh
findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
```

`/mnt` bind mount 不能设为只读，因为 FUSE daemon 需要在其中创建 submount；FUSE 文件系统本身仍然是只读的。`rshared` 和 rootful `SYS_ADMIN` 会扩大挂载权限边界，不应把此容器暴露给不受信任的调用者。

要让容器接受入站 peer，应在配置中使用固定的 `listen_port` 并发布两个传输协议，例如：

```sh
docker run --rm \
  -p 6881:6881/tcp \
  -p 6881:6881/udp \
  ...
```

## 构建、测试与贡献

前端构建产物被 Go embed，因此本地质量检查应先完成前端检查和构建，再执行 Go 命令：

```sh
npm ci --prefix web
npm run typecheck --prefix web
npm run lint --prefix web
npm test --prefix web -- --run
npm run build --prefix web
rm -rf -- web/node_modules

go build ./...
go test ./...
go test -race ./...
```

如果只需要生成供 Go 使用的前端产物，可以运行：

```sh
./scripts/build-web.sh
```

该脚本执行 lockfile 安装和 `npm run build`，完成后删除 `web/node_modules`。真实 FUSE 测试需要 `/dev/fuse`、`fusermount`/`fusermount3` 和挂载权限；环境不具备 FUSE 时相关测试会跳过。要把 FUSE 缺失视为失败，可使用：

```sh
TORRENTFS_FUSE_REQUIRED=1 go test -race -run 'TestFuse|TestSessionIncomplete' ./...
```

需要完整 Docker/FUSE 环境时，还可以运行：

```sh
./scripts/docker-smoke.sh
```

## 许可证

Mozilla Public License 2.0，见 [LICENSE](LICENSE)。
