# HTTP API 参考

[English](api.en.md) · [返回 README](../README.md)

HTTP 服务和 Web UI 共用 listener，默认地址为 `http://127.0.0.1:8080`。API 只提供任务管理和诊断，不提供 torrent 内容下载/播放端点；内容通过 FUSE/SMB 读取。

## 路由

| 方法 | 路径 | 用途 | 成功状态 |
| --- | --- | --- | --- |
| `POST` | `/api/v1/auth/login` | 使用配置凭据换取 Bearer token | `200` |
| `POST` | `/api/v1/auth/logout` | 撤销当前 token | `204` |
| `POST` | `/api/v1/torrents` | 添加 JSON 磁力链接或 multipart `.torrent` | `201` |
| `GET` | `/api/v1/torrents` | 列出任务；每项包含 `category`，未分类为 `""` | `200` |
| `GET` | `/api/v1/categories` | 按名称排序列出分类 | `200` |
| `POST` | `/api/v1/categories` | 创建分类 | `201` |
| `PUT` | `/api/v1/torrents/{id}/category` | 设置或解除分类 | `200` |
| `GET` | `/api/v1/stats` | Session 全局缓存和传输统计 | `200` |
| `GET` | `/api/v1/settings/upload-rate` | 读取上传限速设置 | `200` |
| `PUT` | `/api/v1/settings/upload-rate` | 保存并应用上传限速设置 | `200` |
| `GET` | `/api/v1/torrents/{id}` | 任务汇总状态 | `200` |
| `GET` | `/api/v1/torrents/{id}/status` | piece、文件范围、字幕和网络诊断 | `200` |
| `DELETE` | `/api/v1/torrents/{id}` | 发起异步删除 | `202` |
| `PUT` | `/api/v1/torrents/{id}/subtitles` | 上传或替换一个 managed subtitle | `201` 新建 / `200` 替换 |
| `PUT` | `/api/v1/torrents/{id}/favorite` | 设置或取消收藏 | `200` |
| `POST` | `/api/v1/torrents/prune` | 批量删除早于 N 天的未收藏任务 | `200` |
| `GET` | `/api/v1/operations/{id}` | 查询异步删除 operation | `200` |

任务 `{id}` 是 40 个字符的小写十六进制 info hash。业务错误使用 `{"error":"..."}` JSON；未知路径和不匹配的方法由标准 `net/http` 路由处理，不能假设所有错误响应都是 JSON。

## 认证

本机默认配置认证关闭，可以直接请求：

```sh
BASE_URL=http://127.0.0.1:8080
curl --fail "$BASE_URL/api/v1/torrents"
```

认证开启时，只有 `POST /api/v1/auth/login` 不需要 token；其他 `/api/` 请求必须且只能带一个 `Authorization: Bearer <token>` header。静态 Web shell/assets 的 `GET`/`HEAD` 仍公开，只允许加载 UI，不公开 torrent 数据。非 loopback listener 必须认证，具体 bcrypt 来源和共享环境凭据见[配置参考](configuration.md)。

登录必须用 `application/json`。返回 `token`、`token_type`（`Bearer`）和 `expires_in`。token 是 opaque 内存 token，不使用 Cookie、URL 参数、JWT 或 refresh token；有效请求滑动过期窗口，重启全部失效。

以下 Bash 流程需要 `curl` 和 `jq`，共享凭据变量应已设置。认证关闭时跳过登录，并省略其他请求的 Authorization header：

```bash
BASE_URL=http://127.0.0.1:8080
LOGIN_BODY="$(jq -n \
  --arg username "$TORRENTFS_USERNAME" \
  --arg password "$TORRENTFS_PASSWORD" \
  '{username: $username, password: $password}')"
TOKEN="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/auth/login" \
  --header 'Content-Type: application/json' \
  --data "$LOGIN_BODY" | jq -r .token)"
```

登出返回 `204` 空 body：

```sh
curl --fail --request POST "$BASE_URL/api/v1/auth/logout" \
  --header "Authorization: Bearer $TOKEN"
```

## 添加与查询

将 magnet 示例替换为真实且有可用来源的磁力链接：

```bash
MAGNET_URI='magnet:?xt=urn:btih:<info-hash>'
TORRENT_ID="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data "$(jq -n --arg magnet_uri "$MAGNET_URI" '{magnet_uri: $magnet_uri}')" \
  | jq -r .id)"
```

也可上传本地 `.torrent`；将 `example.torrent` 替换为实际文件：

```bash
TORRENT_ID="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN" \
  --form "file=@$PWD/example.torrent" | jq -r .id)"
```

不要手动设置 `Content-Type: multipart/form-data`，curl 必须自动生成 boundary。上传默认最多 `10MiB`，`http.max_upload_size` 同时限制 `.torrent` 和字幕请求体。服务校验 info hash 并以 `<infohash>.torrent` 原子保存，上传文件名不参与存储命名。

列表、全局统计、任务汇总和详细 status：

```sh
curl --fail "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN"
curl --fail "$BASE_URL/api/v1/stats" \
  --header "Authorization: Bearer $TOKEN"
curl --fail "$BASE_URL/api/v1/torrents/$TORRENT_ID" \
  --header "Authorization: Bearer $TOKEN"
curl --fail "$BASE_URL/api/v1/torrents/$TORRENT_ID/status" \
  --header "Authorization: Bearer $TOKEN"
```

magnet 在 metainfo 到达前可能为 `adding`；status 仍返回 `200`，但 `metainfo_ready` 为 `false`，`pieces` 和 `files` 为空。`ready` 不等于完整下载，读取仍可能等待 peers。

### Status 字段

下面示例展示主要字段：

```json
{
  "torrent": {
    "id": "<info-hash>",
    "info_hash": "<info-hash>",
    "name": "example",
    "category": "movies",
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
  },
  "subtitle_targets": [],
  "subtitles": []
}
```

- `files` 的范围为半开区间 `[piece_start, piece_end)`，指向同一份绝对、从零开始的 `pieces` 数组。
- `network` 是 raw API 的诊断字段，当前 Web UI 不提供 peer/DHT 面板。
- `subtitle_targets` 包含受支持视频的 `video_path`、`mount_path`、`expected_basename`、`uploadable`，不可上传时提供稳定 `reason`，前端不自行推导规则。
- `subtitles` 包含 managed subtitle 的 `video_path`、`path`、`mount_path`、`format`、`size`、`updated_at`，不参与 payload/piece 覆盖统计。

`state` 是生命周期而非下载百分比。`cached_bytes` 是当前内存占用；逐任务传输计数在运行时句柄生命周期内累计，全局传输计数在 Session 生命周期内累计。精确定义、重启和删除行为见[使用指南](usage.md)。

## 分类与收藏

创建分类并归类：

```sh
curl --fail --request POST "$BASE_URL/api/v1/categories" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"name":"movies"}'

curl --fail --request PUT "$BASE_URL/api/v1/torrents/$TORRENT_ID/category" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"category":"movies"}'
```

分类必须是无首尾空白的单个路径组件，不能含 `/`、反斜杠或 NUL；空分类也投影为根目录。解除归类发送 `{"category":""}`。unknown category/torrent 返回 `404`，重复分类、删除中的任务或 namespace 冲突返回 `409`。

收藏：

```sh
curl --fail --request PUT "$BASE_URL/api/v1/torrents/$TORRENT_ID/favorite" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"favorite":true}'
```

返回 `200` 和更新后的任务对象。`favorite` 必须显式提供，缺字段为 `400`；收藏只豁免批量清理，单个 `DELETE` 仍可删除。

## 删除与批量清理

删除返回 `202` 和 operation id，不是同步完成：

```sh
curl --fail --request DELETE "$BASE_URL/api/v1/torrents/$TORRENT_ID" \
  --header "Authorization: Bearer $TOKEN"

OPERATION_ID='<operation_id-from-delete-response>'
curl --fail "$BASE_URL/api/v1/operations/$OPERATION_ID" \
  --header "Authorization: Bearer $TOKEN"
```

初始状态通常为 `deleting`，轮询到 `deleted` 或 `delete_failed`。根目录中非 registry `.torrent` 文件不参与删除保护。字幕清理失败时返回稳定 `error_code: subtitle_cleanup_failed`；修复存储后再次 `DELETE` 同一任务会复用 operation id，重启也自动重试一次。

按年龄清理：

```sh
curl --fail --request POST "$BASE_URL/api/v1/torrents/prune" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"older_than_days":30}'
```

`older_than_days` 必须是 `1..106751` 的整数；上限避免乘以 24 小时转换为 int64 纳秒 duration 时溢出。正常响应：

```json
{
  "operations": [
    {"operation_id": "<operation-id>", "torrent_id": "<info-hash>", "state": "deleting"}
  ],
  "excluded_favorites": 1
}
```

单个候选无法开始删除不会中断整批；该项记录在 `failures`，其他项照常发起。全部候选正常时不返回此字段，因此不能将失败候选与无候选混为一谈：

```json
{
  "operations": [],
  "excluded_favorites": 0,
  "failures": [
    {"torrent_id": "<info-hash>", "error": "session: write state ..."}
  ]
}
```

## 字幕上传

请求是 multipart，提交目标视频的 display path 和字幕文件；目标字幕路径由服务端推导。不要手动设置 Content-Type：

```sh
curl --fail --request PUT "$BASE_URL/api/v1/torrents/$TORRENT_ID/subtitles" \
  --header "Authorization: Bearer $TOKEN" \
  --form 'video_path=Season 1/E01.mkv' \
  --form 'file=@./E01.srt'
```

新建返回 `201`，同路径替换返回 `200`：

```json
{
  "torrent_id": "<info-hash>",
  "video_path": "Season 1/E01.mkv",
  "path": "Season 1/E01.srt",
  "mount_path": "Show/Season 1/E01.srt",
  "format": "srt",
  "size": 1234,
  "updated_at": "2026-09-26T12:00:00Z",
  "replaced": false
}
```

`path` 是 torrent 相对路径，`mount_path` 是挂载点可见路径，包含分类或已消歧的 torrent 根名。

### 名称与格式规则

| 规则 | 说明 |
| --- | --- |
| 视频路径 | 必须是 `ready` 任务的 payload 文件，精确匹配 metainfo 的 slash display path；不接受绝对路径、反斜杠、`.`、`..` 或宿主/FUSE 前缀 |
| 视频扩展名 | `.3gp`、`.avi`、`.flv`、`.m2ts`、`.m4v`、`.mkv`、`.mov`、`.mp4`、`.mpeg`、`.mpg`、`.mts`、`.ts`、`.webm`、`.wmv`，大小写不敏感 |
| 字幕扩展名 | 只允许小写 `.srt`、`.ass`、`.vtt`，`.SRT` 等形式返回 `415` |
| basename | 去掉各自最终扩展名后逐码点完全一致，区分大小写，不做模糊匹配；`Movie.MKV` 只接受 `Movie.srt` / `.ass` / `.vtt` |
| 语言后缀 | 不支持 `Movie.zh-CN.srt` 等变体 |
| 目标位置 | 与所选视频同目录，保留字幕文件名；原始字节保存，不转码、不改 BOM/换行 |
| payload 覆盖 | 目标已是 torrent 自带文件时返回 `409`，永不覆盖 |
| 替换 | 同任务、同路径、同扩展名原子替换，不同扩展名可并存 |
| 歧义 | 同目录 `movie.mkv` 和 `movie.mp4` 争用 `movie.srt` 时，两者都不可上传 |
| single-file | 视频与字幕为挂载点下的同级文件，分类任务位于分类目录内；视频可见名因 hash 消歧而改变时，该 target 不可上传 |
| 重启恢复 | 以目录、字幕扩展名和视频 expected basename 匹配唯一目标，不能唯一对应的 sidecar 会使启动失败 |
| 新任务保护 | 新任务会让已有 single-file 视频改名或遮蔽字幕路径时，添加返回 `409`，不会发布 registry、metainfo 或可见状态 |

并发、原子发布和 no-follow 存储保证见[开发指南](development.md)，播放器验证见[使用指南](usage.md)。

### 字幕错误码

失败响应保留 `error`，另带稳定 `code`：

| `code` | HTTP | 含义 |
| --- | --- | --- |
| `subtitle_name_mismatch` | `415` | basename 不严格对应 |
| `subtitle_format_unsupported` | `415` | 扩展名不是小写受支持格式 |
| `subtitle_video_not_found` | `404` | video_path 不是受支持 payload 视频 |
| `subtitle_name_conflict` | `409` | stem 或挂载名称有歧义 |
| `subtitle_payload_conflict` | `409` | 目标属于 payload |
| `torrent_deleting` | `409` | 任务正在删除，包括 `delete_failed` |
| `subtitle_storage_unavailable` | `503` | 目录缺失/非受管理目录，或 create/write/rename 返回 `EROFS`、`EACCES`、`EPERM` |
| `subtitle_storage_full` | `507` | 任一 I/O 阶段返回 `ENOSPC`、`EDQUOT` |
| `subtitle_write_failed` | `500` | 其他写入失败，包括客户端中断 |

添加任务还可能返回 `subtitle_namespace_conflict`（`409`），表示新任务会破坏已有字幕的可见 namespace。`413` 表示请求体超过 `http.max_upload_size`。错误不会回显宿主路径。

## 上传限速设置

读取和保存使用 `GET` / `PUT /api/v1/settings/upload-rate`。API 使用整数 **bytes/s**，不接受 `"1MiB/s"` 等 UI 单位字符串：

```sh
curl --fail "$BASE_URL/api/v1/settings/upload-rate" \
  --header "Authorization: Bearer $TOKEN"

curl --fail --request PUT "$BASE_URL/api/v1/settings/upload-rate" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"rate_limit_bytes_per_second":1048576,"schedule":{"start":"08:00","end":"22:00"}}'
```

rate 为 `0` 且 schedule 为 `null` 表示不限速；正 rate 且无 schedule 表示全天限速；有 schedule 时 rate 必须为正数。时段按服务端本地时间、分钟精度的 `[start, end)`，start/end 同时提供且 `start < end`，只支持同一自然日单窗口；非法值为 `400`。UI 和持久化行为见[使用指南](usage.md)。

保存先写临时文件、`fsync`、原子 rename。两类存储失败必须区分：

| `code` | HTTP | `applied` | 含义 |
| --- | --- | --- | --- |
| `upload_rate_settings_storage_unavailable` | `503` | `false` | rename 前失败，旧磁盘规则与运行规则均不变 |
| `upload_rate_settings_durability_unconfirmed` | `503` | `true` | rename 后目录同步失败，新规则已生效，但重启后恢复结果未确认 |

## 常见 HTTP 状态

| 状态 | 典型原因 |
| --- | --- |
| `400` | JSON/multipart、Content-Type、磁力链接、文件名或设置值无效 |
| `401` | 认证缺失、格式错误、过期/撤销，或登录凭据错误 |
| `404` | 未知 torrent/operation；认证关闭时 login/logout 不可用；静态 asset 不存在 |
| `409` | 删除状态或 namespace/字幕目标冲突；`delete_failed` 时重新添加相同 info hash 也冲突 |
| `413` | 超过 body 上限；登录固定为 `8KiB`，torrent/字幕由 `http.max_upload_size` 控制 |
| `415` | 添加请求不是 JSON/multipart，或字幕名称/扩展名不受支持 |
| `500` | 未分类的内部 session/API 错误 |
| `503` | 字幕存储不可用，或上传限速写入失败/持久性未确认 |
| `507` | 字幕存储空间不足 |

受保护 API 的 `401` 带 `WWW-Authenticate: Bearer`。未知路径/方法可能返回标准 handler 的 `404`/`405`，不是 SPA 页面，也不保证业务 JSON 错误形状。
