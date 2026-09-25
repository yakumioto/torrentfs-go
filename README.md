# torrentfs

`torrentfs` 将 BitTorrent 内容挂载为只读的 FUSE 文件系统，并提供一个用于管理 torrent 的 HTTP API 和嵌入式 Web UI。

它管理一个已有的、可读写的 `torrents` 目录；torrent 任务只能通过 HTTP API 添加磁力链接或上传 `.torrent` 文件。单文件 torrent 直接呈现为挂载点下的文件，多文件 torrent 保留其目录结构。读取所需的 piece 保存在有界的内存缓存中，不会把 piece 数据写回磁盘。

## 项目定位与设计原则

- **挂载点只承载数据**：FUSE 文件系统是只读的，不提供管理用的 `metadata/` 或 `stats/` 控制目录；添加、删除和状态查询都通过 HTTP API 完成。
- **管理状态与缓存分离**：由 API 管理的 metainfo 持久化在 `torrents-dir/<infohash>.torrent`，未完成的磁力链接意图、registry 和 peer identity 保存在 `torrents-dir/.metadata`，piece 内容只存在于内存。
- **缓存不是下载进度**：`cached_bytes` 表示当前仍驻留在内存中的字节数。piece 会被淘汰，因此这个数值可能下降；进程重启后缓存为空。
- **种子传输累计独立于缓存**：每个任务的 `downloaded_bytes` / `uploaded_bytes` 分别统计 useful payload 和实际发送的 data payload；两者在当前后端 Session 的运行时句柄生命周期内累计，不持久化。
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
├── <infohash>.torrent  API 创建的 canonical metainfo
└── .metadata/
    ├── pending/<infohash>.magnet
    └── state/<infohash>.json
```

启动时只从 `.metadata/state` 恢复任务；完整 metainfo 从根目录的 canonical `<infohash>.torrent` 读取，未完成 magnet 从 `.metadata/pending` 恢复。根目录中手工放入的任意 `.torrent` 文件会被忽略，不会创建任务、进入 FUSE 或阻止 API 删除。同一个 `torrents` 目录同时只能由一个进程管理。

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

本地 `.torrent` 文件必须通过 `POST /api/v1/torrents` 上传；服务会解析并校验 info hash，再以 `<infohash>.torrent` 原子保存，上传文件名不会参与存储命名。磁力链接也通过同一 API 添加。

任务被 API 接受并解析后，内容会出现在 `mnt` 下。单文件 torrent 直接是一个文件，多文件 torrent 是一个目录树；挂载点中的写入、删除和重命名都会返回只读错误。具体 curl 流程见后文。

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
| `GET` | `/api/v1/stats` | 查询本次后端 Session 的全局缓存与传输统计 | `200` |
| `GET` | `/api/v1/torrents/{id}` | 查询单个任务的汇总状态 | `200` |
| `GET` | `/api/v1/torrents/{id}/status` | 查询 piece、文件范围和网络诊断快照 | `200` |
| `DELETE` | `/api/v1/torrents/{id}` | 发起异步删除 | `202` |
| `PUT` | `/api/v1/torrents/{id}/favorite` | 设置或取消任务的收藏标记 | `200` |
| `POST` | `/api/v1/torrents/prune` | 批量删除早于 N 天的未收藏任务 | `200` |
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

下面的流程覆盖登录、列表、添加、状态查询、收藏、批量清理、异步删除和登出。认证关闭时可跳过登录，并省略后续的 `Authorization` header。

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

5. 查看运行时统计、汇总和详细 status。把 `<torrent-id>` 替换为添加响应中的 `id`：

   ```sh
   curl --fail "$BASE_URL/api/v1/stats" \
     --header "Authorization: Bearer $TOKEN"

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
       "downloaded_bytes": 262144,
       "uploaded_bytes": 0,
       "cached_bytes": 262144,
       "created_at": "2026-01-01T00:00:00Z",
       "favorite": false
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

   删除响应为 `202`，初始 operation 状态通常为 `deleting`；随后轮询到 `deleted` 或 `delete_failed`。根目录中的非 registry `.torrent` 文件不会参与删除保护。

7. 收藏与批量清理。收藏标记持久化在任务的 sidecar 中，且**只会让任务豁免批量清理** —— 单个 `DELETE` 仍可删除已收藏任务：

   ```sh
   curl --fail --request PUT "$BASE_URL/api/v1/torrents/$TORRENT_ID/favorite" \
     --header "Authorization: Bearer $TOKEN" \
     --header 'Content-Type: application/json' \
     --data '{"favorite":true}'
   ```

   成功返回 `200` 和更新后的任务对象。`favorite` 必须显式给出；缺字段返回 `400`。

   按任务年龄批量删除时，`older_than_days` 必须是 `1` 到 `106751` 之间的整数（`0`、负数与超过上限的值返回 `400`）。上限来自时长本身：天数会乘以 24 小时得到 Go 的 `time.Duration`（int64 纳秒），再大的值会溢出：

   ```sh
   curl --fail --request POST "$BASE_URL/api/v1/torrents/prune" \
     --header "Authorization: Bearer $TOKEN" \
     --header 'Content-Type: application/json' \
     --data '{"older_than_days":30}'
   ```

   响应列出本次实际发起的删除 operation，以及因收藏而保留的任务数：

   ```json
   {
     "operations": [
       {"operation_id": "<operation-id>", "torrent_id": "<info-hash>", "state": "deleting"}
     ],
     "excluded_favorites": 1
   }
   ```

   批量删除同样走异步 operation 模型：轮询 `GET /api/v1/operations/{id}` 观察每个任务的终态。早于阈值但已收藏的任务永远保留，`excluded_favorites` 即其数量。

   单个候选无法开始删除（例如磁盘写满或只读挂载，侧车写入失败）不会中断整批：其余候选照常发起，该候选记录在 `failures` 中。全部候选都正常发起时该字段**不出现**，因此「有候选但未能删除」与「没有符合条件的任务」不会得到同一响应形状：

   ```json
   {
     "operations": [
       {"operation_id": "<operation-id>", "torrent_id": "<info-hash>", "state": "deleting"}
     ],
     "excluded_favorites": 0,
     "failures": [
       {"torrent_id": "<info-hash>", "error": "session: write state ..."}
     ]
   }
   ```

8. 登出：

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

每个 torrent 的 `downloaded_bytes` 是 `BytesReadUsefulData`（有效内容 payload），`uploaded_bytes` 是 `BytesWrittenData`（实际发送的内容 payload），都不包含 wire overhead。它们从当前 Session 注册该 torrent 的运行时句柄开始累计；页面刷新、多前端读取不会清零，后端重启重建句柄后归零，也不会写入 registry 或其他持久化状态。删除中的行在运行时句柄移除后回落为零。

`GET /api/v1/stats` 返回统一的 Session 运行时快照：`cache.used_bytes` / `cache.capacity_bytes` 分别是已经校验并驻留在共享 piece LRU 中的当前占用和配置硬上限，不包含 staging、临时副本、协议缓冲区或进程 RSS；`transfer.downloaded_bytes` 使用 useful torrent payload，`transfer.uploaded_bytes` 使用实际发送的 torrent data payload，均不包含 wire overhead。统计从后端 Session 创建时开始，后端重启后归零，浏览器刷新或删除任务不会清零/回退，多前端读取同一个累计值。

常见 HTTP 状态：

| 状态 | 典型原因 |
| --- | --- |
| `400` | JSON/multipart body、`Content-Type`、磁力链接或文件名无效 |
| `401` | 认证缺失、格式错误、过期或已撤销；登录凭据错误也返回 `401` |
| `404` | 未知 torrent/operation；认证关闭时 login/logout 也不可用；不存在的静态 asset |
| `409` | torrent 正在删除（包括 `delete_failed` 状态下重新添加同一 info hash） |
| `413` | body 超过限制；登录 body 上限固定为 8 KiB，上传上限由 `http.max_upload_bytes` 控制 |
| `415` | 添加 torrent 时使用了 JSON 或 multipart 之外的 media type |
| `500` | 未分类的内部 session/API 错误 |

受保护 API 的 `401` 响应包含 `WWW-Authenticate: Bearer`。未知路由或方法可能由标准 HTTP handler 返回 `404`/`405`，不要把它们当作 SPA 页面或统一的业务 JSON 错误。

## Web UI

HTTP 服务启用时，同一个 listener 同时提供嵌入式 Web UI 和 `/api/v1`：

- Dashboard 支持按名称或 info hash 搜索、按全部/就绪/错误筛选，显示全局缓存、本次启动下载/上传统计和任务摘要，并提供磁力/文件添加入口；文件页签支持一次选择多个 `.torrent` 文件或拖放文件，浏览器会按“一文件一请求”复用现有上传接口。
- Dashboard 任务列表显示逐任务下载量和上传量；任务、大小、状态和添加时间均可在前端排序，传输量仅展示不参与排序，默认按添加时间倒序。
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

当前实际读取的 21 个受支持环境变量包括 19 个普通 field binding，以及一组供 HTTP 与 SMB 共用的特殊凭据变量；未列出的 TOML key 没有自动生成的环境变量：

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
| `TORRENTFS_USERNAME` | HTTP/SMB 共享凭据 | HTTP 开启认证或 SMB 时必填的用户名 |
| `TORRENTFS_PASSWORD` | HTTP/SMB 共享凭据 | HTTP 开启认证或 SMB 时必填的单行明文密码 |
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
- 普通环境变量存在但为空时，字符串字段可以被清空；数字、布尔值和 duration 的空值会报错。`TORRENTFS_USERNAME` 与 `TORRENTFS_PASSWORD` 是特殊共享凭据，不能只设置其中一个。
- HTTP auth 启用且共享凭据完整时，pair 原子覆盖 TOML 中的 username、password hash 和 hash file；启动时生成 cost-10 bcrypt hash，最终配置不保存明文密码。HTTP 环境密码按 UTF-8 bytes 计数，最多 72 bytes，不能截断。
- HTTP auth 关闭时，Go 配置层忽略共享凭据 pair，以便 SMB-only 启动；如果 TOML 本身仍含 credentials，仅把 auth 关闭不会自动清空，仍会按现有校验失败。
- `connections.listen_port` 必须在 `0..65535`；`disable_ipv4` 和 `disable_ipv6` 不能同时为 `true`；每个 `bootstrap_nodes` 项都必须是合法且端口在 `1..65535` 的 `host:port`。
- `cache.capacity_bytes` 必须大于零；piece length 大于 cache capacity 的 torrent 会在添加时被拒绝。`proxy.socks5_url` 只能为空、`socks5://` 或 `socks5h://`，且必须包含合法 host/port；校验错误不会把 proxy 凭据写入错误信息。
- `identity.peer_id_prefix` 最多 20 bytes；tracker User-Agent 不能包含 CR/LF。`http.max_upload_bytes` 必须大于零，日志 level/format 只能使用上表值。
- HTTP listener 为空表示关闭；非空值必须是合法的 `host:port`。非 loopback listener 必须同时启用完整认证配置。
- 认证关闭时 TOML 中的 username、password hash 和 hash file 必须全为空；认证开启且未使用共享 pair 时，`password_hash` 与 `password_hash_file` 必须恰好设置一个。
- TOML hash file 必须是非空的普通非符号链接文件，只允许 owner 读取，大小不超过 1024 bytes；服务会验证 bcrypt cost。共享密码位于进程环境中，容器 metadata 也可能可见，不应将 Docker environment 当作 secret store。

`TORRENTFS_FUSE_REQUIRED` 不是 daemon 配置，而是测试门禁。`TORRENTFS_PATHS_DATA_DIR` 是已移除的历史变量，不会恢复旧的磁盘 payload 路径。

## 持久化状态与限制

`torrents-dir/.metadata` 是内部实现目录，不会出现在 FUSE 挂载中。常见内容如下：

```text
<torrents-dir>/
├── <info-hash>.torrent          # API 管理的 canonical metainfo
└── .metadata/
    ├── pending/<info-hash>.magnet # 尚未解析完成的磁力意图
    ├── state/<info-hash>.json      # 每个任务的 registry entry
    ├── layout_version              # 一次性旧布局迁移标记
    ├── peer_id                     # 该 torrents 目录的 20 字节 peer identity
    └── instance.lock               # 进程运行期间的独占锁
```

- piece 数据、piece completion、cache hit 计数和临时读取优先级都只在内存中；重启不会从磁盘 rehash 或恢复 piece。
- registry 是任务集合的唯一事实来源；启动不会扫描根目录猜测任务。根目录中手工放置的 `.torrent` 文件会被忽略，不会进入 API/FUSE，也不会阻止删除。
- 上传内容会先校验 info hash，再以 `<info-hash>.torrent` 原子发布；磁力链接先写入 `.metadata/pending/<info-hash>.magnet`，metadata 完成后发布最终文件并清理 pending。
- 首次启动会把当前版本可识别的旧 flat metainfo 和 magnet intent 文件一次性迁移到新布局；目标 hash 冲突、损坏或非 canonical 历史文件会使启动明确失败。写入 `layout_version` 后不再读取旧位置。
- 配置只在启动时读取，修改 TOML 或环境变量后需要重启进程。
- 一个 `torrents` 目录同时只能由一个 torrentfs 进程使用。

## Docker

Dockerfile 使用 Node 22.23.2 构建 Web UI，再使用 Go 1.27 编译包含 `web/dist` 的 `CGO_ENABLED=0` 二进制；运行阶段是安装了 `fuse3`、CA certificates 和 `passwd` 的 Debian bookworm-slim。

### 镜像默认行为

```sh
docker build -t torrentfs .
docker run --rm --env PUID=1000 --env PGID=1000 torrentfs
```

默认命令等价于：

```text
/usr/local/bin/torrentfs -config /etc/torrentfs/torrentfs.toml /torrents
```

镜像会创建 `/torrents`，但 HTTP listener 仍默认绑定容器内的 `127.0.0.1:8080`；`-p 8080:8080` 不会改变 daemon 的监听地址。Docker 配置与本地默认值也不同：镜像把 peer `listen_port` 固定为 `6881`，并默认禁用 IPv6；本地默认 peer 端口为 `0`，地址族都启用。环境变量可以覆盖 `/etc/torrentfs/torrentfs.toml`。

### 运行时 UID/GID

容器镜像是通用镜像，不在构建阶段写入部署主机的 UID/GID。入口必须以 root 启动，用 root 在每次启动时准备账户和 Samba runtime，然后以配置身份运行 torrentfs 和 smbd。不要再使用 `docker run --user`；非 root 入口无法完成初始化，会明确失败。

| 变量 | 默认值 | 约束 |
| --- | --- | --- |
| `PUID` | `1000`（变量未设置时） | 无符号十进制整数，范围 `1..4294967294` |
| `PGID` | `1000`（变量未设置时） | 无符号十进制整数，范围 `1..4294967294` |

显式设置的空值、非数字、负数、`0` 和超出范围的值都会在启动 listener、FUSE 或 Samba 之前失败。`PUID`/`PGID` 只在运行时生效，不需要也不支持 `TORRENTFS_UID`/`TORRENTFS_GID` build args。比如 Unraid/NAS 常见的 `99:100` 配置是：

```sh
docker build -t torrentfs .
docker run --rm \
  --env PUID=99 --env PGID=100 \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  torrentfs
```

入口不会自动 `chown` `/torrents`，因为它可能是 NAS 或宿主机的 bind mount。部署前必须让目标 `PUID:PGID` 对该目录具备读、写、遍历权限；权限检查使用目标数字身份执行，失败时会报告 `runtime identity UID:GID cannot read and write /torrents`。镜像内没有 bind mount 时自带的 `/torrents` 为 `0777`，仅用于保证默认容器可启动。

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

该脚本还需要 `awk` 和 `timeout`；外部 TOML 以只读方式挂载，但 `/torrents` 仍必须可写。脚本同时检查 SMB 凭据缺失、非法用户名和 UID 不匹配时在启动 listener 前失败。

SMB 与组合模式的真实协议检查使用：

```sh
./scripts/docker-smb-smoke.sh
```

它覆盖 SMB-only、HTTP+SMB 共用同一组明文凭据、错误密码、guest 拒绝、只读和日志不泄密。

### 构建并运行 HTTP 服务

公开容器 listener 前必须配置认证。下面的命令从交互式输入读取一组共享明文凭据；Docker `--env NAME` 形式只把已导出的变量传入容器，不把密码字面量放入命令行：

```sh
mkdir -p /srv/torrents
export TORRENTFS_USERNAME=alice
read -r -s -p 'HTTP password: ' TORRENTFS_PASSWORD; printf '\n'
export TORRENTFS_PASSWORD

docker run --rm \
  --publish 8080:8080 \
  --env PUID=1000 --env PGID=1000 \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_USERNAME \
  --env TORRENTFS_PASSWORD \
  torrentfs

unset TORRENTFS_USERNAME TORRENTFS_PASSWORD
```

镜像内置的默认命令使用 `/torrents` 和 `/etc/torrentfs/torrentfs.toml`；环境变量会覆盖内置 TOML。`-p`/`--publish` 只发布端口，不会改变 daemon 实际监听的地址。

### Docker FUSE 挂载

Linux rootful Docker 需要把 FUSE 设备和挂载能力交给容器。入口必须以 root 启动，但只在初始化账户、准备 runtime 和启动 supervisor 时使用 root；实际 torrentfs 进程会通过 `PUID`/`PGID` 降权。不要传 `--user`，否则入口会在任何服务启动前明确失败。

下面的例子把运行身份设为当前宿主机用户；如果当前用户是 root，示例改用专用的非 root `1500:1500`。构建只发生一次，之后可以为不同 NAS/宿主机传入不同运行身份：

```sh
HOST_UID="$(id -u)"
HOST_GID="$(id -g)"
if [ "$HOST_UID" -eq 0 ] || [ "$HOST_GID" -eq 0 ]; then
  PUID=1500
  PGID=1500
else
  PUID="$HOST_UID"
  PGID="$HOST_GID"
fi
export PUID PGID

docker build -t torrentfs .

mkdir -p /srv/torrents /srv/mnt
# /torrents 不会被入口自动 chown；部署方必须预先准备权限。
sudo chown "$PUID:$PGID" /srv/torrents /srv/mnt

docker run --rm \
  --env PUID --env PGID \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor=unconfined \
  --env TORRENTFS_HTTP_LISTEN_ADDR= \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --mount type=bind,src=/srv/mnt,dst=/mnt,bind-propagation=rshared \
  torrentfs -config /etc/torrentfs/torrentfs.toml -mountpoint /mnt /torrents
```

两个 bind source 都必须对 `PUID:PGID` 可读、可写、可遍历。某些系统不需要 `apparmor=unconfined`，但如果 AppArmor 阻止 FUSE，则必须按主机策略放行。`/dev/fuse`、`SYS_ADMIN` 和等价的安全配置不是镜像可以自行授予的权限；缺少它们时容器会挂载失败，而不会静默退化为普通目录。

`/srv/mnt` 所在的主机挂载点必须支持递归双向传播；可以先检查：

```sh
findmnt -T /srv/mnt -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS
```

`/mnt` bind mount 不能设为只读，因为 FUSE daemon 需要在其中创建 submount；FUSE 文件系统本身仍然是只读的。`rshared` 和 rootful `SYS_ADMIN` 会扩大挂载权限边界，不应把此容器暴露给不受信任的调用者。

要让容器接受入站 peer，应使用镜像配置中的固定 `listen_port = 6881`，并在上面的 `docker run` 中追加 `--publish 6881:6881/tcp --publish 6881:6881/udp`。只发布 HTTP 端口不会让 peer 端口可达；动态端口 `0` 也不能预先发布。

可以用 `/proc/<torrentfs-pid>/status` 检查服务身份；`docker exec torrentfs id` 默认执行的是 root shell，不代表实际 daemon 身份。入口会在启动前检查 `/torrents`，不会替宿主机或 NAS 改写其属主。

### 可选的单容器 SMB 只读共享

默认关闭。设置 `TORRENTFS_SMB_ENABLED=true` 后，容器入口在同一个 mount namespace 内先让 torrentfs 挂载只读 FUSE 到固定的内部路径 `/share`，确认该路径的 fstype 为 `fuse.*` 之后才启动 `smbd`，仅监听 TCP 445。宿主机不需要看到 `/share`，也不需要 `rshared` 或跨容器 mount propagation。

| 变量 | 说明 |
| --- | --- |
| `TORRENTFS_SMB_ENABLED` | `true`/`false`（默认 `false`）。只有严格为 `true`、`1` 时才进入 SMB 模式 |
| `TORRENTFS_USERNAME` | 启用 SMB 时必填，必须是镜像内实际 torrentfs runtime Unix account，并解析到相同 runtime UID |
| `TORRENTFS_PASSWORD` | 启用 SMB 时必填的非空单行明文密码；入口只通过 stdin 初始化 Samba passdb |

share 名固定为 `torrentfs`，路径固定为 `/share`；`/torrents` 不会被共享，因为其中包含可写的 `.metadata`、peer identity 和锁文件。share 始终 `read only = yes`，`guest ok = no`，`map to guest = never`，只发布 TCP 445，不启动 `nmbd`，也不暴露 137/138/139。

`/share`、SMB endpoint 和客户端本地路径分别属于不同命名空间：

1. 服务端容器后端是 `/share`，由入口在 SMB 模式下独占并挂载 FUSE；Samba `[torrentfs]` 的 `path` 指向它。
2. SMB endpoint 的 share 名仍是 `torrentfs`：Linux 使用 `//SERVER/torrentfs`，Windows 使用 `\\server\torrentfs`。进入 share 后，内部根目录直接列出 `payload.bin`，路径是 `payload.bin`（Windows 可表示为 `\payload.bin`），不会多一层 `share` 或 `torrentfs`。
3. 客户端自行选择本地 mountpoint，例如 `/mnt/torrentfs-client`；因此挂载后的本地路径是 `/mnt/torrentfs-client/payload.bin`。这个本地前缀属于客户端，服务端 `path` 无法也不应消除它。

可以用 `smbclient` 验证 share 内根目录：

```sh
smbclient //SERVER/torrentfs -A /path/to/credentials -m SMB3 \
  -c 'ls; get payload.bin /tmp/payload.bin'
```

如果需要内核 CIFS 挂载，客户端可以使用自己的目录：

```sh
mkdir -p /mnt/torrentfs-client
mount.cifs //SERVER/torrentfs /mnt/torrentfs-client \
  -o credentials=/path/to/credentials,vers=3.0,ro
ls -l /mnt/torrentfs-client/payload.bin
umount /mnt/torrentfs-client
```

不要把 CIFS mountpoint 设为 `/`，也不要把容器或宿主机的 `/` 作为 Samba `path`；SMB 服务端容器中的 `/share` 由入口管理，不能同时作为客户端 CIFS 挂载点或非空 bind mount 目标。

`/dev/fuse`、`SYS_ADMIN` 和 `CAP_NET_BIND_SERVICE` 是运行前提：SMB 模式下 torrentfs 和 smbd 都以镜像内解析出的专用非 root 身份运行，`CAP_NET_BIND_SERVICE` 让该身份可以绑定 445。权限方案是同 UID：Samba 用 `force user`/`force group` 映射到同一个运行身份，因此不需要 `allow_other`，FUSE 访问范围不会因为 SMB 而扩大。AppArmor/安全策略是否放行由宿主策略决定；缺少设备、capability 或共享凭据时容器会在启动任何 listener 之前以非零状态失败，并输出诊断，不会退化成共享一个普通目录。

SMB 密码必须是非空单行值；入口会拒绝 CR/LF，并以明文通过 stdin 传给 `smbpasswd`，不把密码写入命令行参数、生成的 Samba 配置或日志。由于接口使用环境变量，密码会出现在容器进程环境和 Docker metadata 中，拥有 inspect 权限者可见；部署时应将 Docker environment 视为明文配置而不是 secret store。

```sh
HOST_UID="$(id -u)"
HOST_GID="$(id -g)"
# SMB requires a non-root runtime identity. A root host user can choose a
# dedicated numeric identity; a non-root host user can reuse their own UID/GID.
if [ "$HOST_UID" -eq 0 ] || [ "$HOST_GID" -eq 0 ]; then
  PUID=1500
  PGID=1500
else
  PUID="$HOST_UID"
  PGID="$HOST_GID"
fi
export PUID PGID

IMAGE=torrentfs
# The runtime identity owns the bind-mounted torrents directory.
sudo install -d -o "$PUID" -g "$PGID" -m 0755 /srv/torrents

docker build -t "$IMAGE" .
export TORRENTFS_USERNAME=torrentfs
read -r -s -p 'Shared HTTP/SMB password: ' TORRENTFS_PASSWORD; printf '\n'
export TORRENTFS_PASSWORD

docker run --rm \
  --env PUID --env PGID \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --cap-add NET_BIND_SERVICE \
  --security-opt apparmor=unconfined \
  --publish 445:445 \
  --publish 8080:8080 \
  --publish 6881:6881/tcp --publish 6881:6881/udp \
  --mount type=bind,src=/srv/torrents,dst=/torrents \
  --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
  --env TORRENTFS_HTTP_AUTH_ENABLED=true \
  --env TORRENTFS_SMB_ENABLED=true \
  --env TORRENTFS_USERNAME \
  --env TORRENTFS_PASSWORD \
  "$IMAGE"

unset PUID PGID TORRENTFS_USERNAME TORRENTFS_PASSWORD
```

The `sudo install` step is intentional: torrentfs writes `/torrents/.metadata`
with the configured runtime UID. The entrypoint never chowns `/torrents`, so
NAS deployments must pre-create or adjust the directory for the same numeric
`PUID:PGID`. The root-host example deliberately uses UID/GID 1500; SMB mode
rejects runtime UID 0. The stable Samba Unix account name is `torrentfs` even
when its numeric UID/GID is changed at startup.

如果只需要 SMB，可以省略 `--publish 8080` 以及 HTTP listener/auth 相关参数；仍必须传入同一组 `TORRENTFS_USERNAME` / `TORRENTFS_PASSWORD`，而 Go 配置层会忽略这组 pair。镜像内置的 HTTP listener 保持 container-local `127.0.0.1:8080`，不发布即可。

身份与退出语义：

- 容器入口以 root 启动，仅为完成账户/runtime 初始化、passdb 初始化、运行身份切换和绑定 445；torrentfs、`smbd` 及其子进程都以 `PUID:PGID` 的非 root 数字身份运行，FUSE 与 SMB 文件访问身份一致。`PUID=0` 或 `PGID=0` 时会明确拒绝启动。
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

  宿主 UID 与容器运行身份相同时，容器写入的 `.metadata` 与清理路径恰好一致，容易掩盖权限问题；用 `TORRENTFS_SMOKE_UID`/`TORRENTFS_SMOKE_GID` 指定一个不同的运行时身份，即可在同一台机器上验证跨 UID 的 metadata 可遍历性与 scratch 目录清理（这两个变量现在只覆盖 smoke 的运行时环境，不是镜像构建参数）：

  ```sh
  TORRENTFS_SMOKE_UID=1500 TORRENTFS_SMOKE_GID=1500 ./scripts/docker-smb-smoke.sh
  ```

`TORRENTFS_FUSE_REQUIRED` 只控制测试门禁，不是 daemon 的运行时配置。CI nightly 的多平台 OCI 构建与本地 `scripts/nightly-build.sh` 归档脚本是不同入口；本 README 的命令用于本地构建、运行和验证，不把手工归档脚本写成 nightly 发布保证。

## 许可证

Mozilla Public License 2.0，见 [LICENSE](LICENSE)。
