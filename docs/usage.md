# 使用指南

[English](usage.en.md) · [返回 README](../README.md)

部署与运行见[部署指南](deployment.md)，请求示例和机器协议见 [API 参考](api.md)。本指南说明文件布局、Web UI、任务管理、运行时统计和常见问题。

## 添加与读取任务

任务通过 Web UI 或 `POST /api/v1/torrents` 添加：支持磁力链接与 `.torrent` 上传，不会自动导入手工复制到 `torrents` 目录中的文件。上传文件名不参与存储命名，服务解析并校验 info hash 后以 `<infohash>.torrent` 保存。

磁力链接可能先处于 `adding`，直到 peers 提供元信息。`ready` 只表示 metainfo 可用、能够建立文件视图；不表示所有内容已下载或所有 piece 都在缓存中。读取仍可能等待 peers，缓存淘汰后也可能重新获取内容。

每次前台内容读取最多等待 `mount.read_timeout`（默认 `30s`）。没有来源或来不及取得已验证 piece 时，超时后只结束当前读取并返回读取错误（客户端可能显示超时或 I/O 错误），不删除种子或取消其他读取；这个期限不限制整个视频或文件句柄的播放时长。慢来源或大 piece 可调长该值，也可用 `TORRENTFS_MOUNT_READ_TIMEOUT` 覆盖，修改后重启，见[配置参考](configuration.md)。

HTTP API/Web UI 只负责管理，没有通用 torrent 内容下载或播放端点。内容通过原生 FUSE、容器内 SMB 或宿主可见的 FUSE 读取。

## FUSE 文件布局

```text
<mount>/
├── <single-name>        未分类 single-file torrent，直接是文件
├── <multi-name>/        未分类 multi-file torrent
│   └── <relative-file>
└── <category>/          分类目录，即使为空也会出现
    ├── <single-name>
    └── <multi-name>/
        └── <relative-file>
```

- 所有节点只读；创建、写入、删除和重命名都返回只读错误。
- 不会挂载 `.metadata`，也没有 `metadata/` 或 `stats/` 管理目录。
- 单文件 torrent 不会额外包一层目录，可直接打开 `<mount>/movie.mp4`；分类后为 `<mount>/<category>/movie.mp4`。
- 多文件 torrent 保留相对目录树。
- 显示名称冲突时，在各自父目录内追加 hash 前缀消歧；不同分类内的同名任务互不影响。

managed subtitle 以普通只读文件合并到视频旁边：

```text
<mount>/
├── movie.mp4            single-file torrent
├── movie.srt            同一 torrent 的字幕，作为 root sibling
└── Show/
    ├── E01.mkv
    └── E01.srt          与视频同目录、同 basename
```

字幕节点权限为 `0444`。替换后新打开的 handle 读取新版本，已有 handle 继续读取原先打开的快照；具体一致性保证见[开发指南](development.md)。

## Web UI

HTTP 启用时，同一个 listener 提供嵌入式 Web UI 和 `/api/v1`。

- Dashboard 按名称或 info hash 搜索，按全部/就绪/错误筛选，显示全局缓存、本次启动下载/上传统计及任务摘要。
- 添加入口支持磁力链接、选择多个 `.torrent` 或拖放；多个文件按“一文件一请求”上传。
- 任务列表显示逐任务下载量、上传量；名称、大小、状态和添加时间可排序，传输量只展示不参与排序，默认按添加时间倒序。
- 详情页包含概览、文件、数据块三个 tab；文件页显示 piece 范围和缓存覆盖，piece map 区分 cached、pinned、uncached。
- 任务详情页提供字幕上传/替换，展示服务端推导的视频目标、torrent 相对路径和 FUSE 路径；不可上传时说明原因。
- 文件 tab 单独展示 managed subtitles 的视频、挂载路径、格式、大小和更新时间，不混入 payload 文件表或 piece 覆盖统计。
- 删除会轮询异步 operation；断线、请求失败、token 过期和字幕清理失败都会给出提示。删除弹窗说明字幕会一起清理。
- Header 齿轮打开上传限速设置，支持带单位的上限输入和可选时段，保存后立即生效。
- 查询默认每 5 秒刷新，页面不可见时停止后台刷新。当前 UI 不提供 peer/DHT 面板，网络诊断字段由 raw API 返回。

认证 token 放在当前 tab 的 `sessionStorage`，服务端 token 只在 daemon 内存中。服务重启或 token 过期后需要重新登录；UI 静态 shell 公开不等于 API 或 torrent 内容公开。

## 分类、收藏与删除

### 分类

分类名称既是稳定标识，也是 FUSE 根目录下的单层目录名。可创建、归类和解除归类；当前不支持重命名、删除、多级分类或一个任务多个分类。

分类名称必须是单个路径组件，不能含 `/`、反斜杠或 NUL，不能含首尾空白。分类创建后即使为空也会出现在根目录，不能遮蔽已有未分类 torrent 或 root-level managed subtitle。

解除归类使用空字符串，只改变虚拟路径和 sidecar，不移动 payload、piece cache 或字幕文件。创建和归类的请求格式见 [API 参考](api.md)。

### 收藏与清理

收藏标记持久化在任务 sidecar 中，**只豁免按年龄的批量清理**，不保护任务免受单个 `DELETE`。

批量清理为符合年龄条件的非收藏任务发起异步删除；部分候选无法发起时，不中断其他候选，失败项记录在 API 的 `failures` 中。每个 operation 都需要观察终态。

### 删除失败

删除会清理 managed subtitles。清理未完成时 operation 为 `delete_failed`，带 `subtitle_cleanup_failed`；任务不会恢复为可读取内容。修复存储权限或空间后，再次删除同一任务会复用原 operation id 幂等重试，进程重启也会自动重试一次。

## 上传与替换字幕

通过详情页的“上传字幕”选择服务端提供的目标视频，再选择一个字幕文件。服务端决定目标路径，不接受任意挂载点写入。

- 字幕必须与视频同目录、同 basename，逐码点区分大小写，不做模糊匹配。例如 `Movie.MKV` 对应 `Movie.srt`，`Season 1/E01.mp4` 对应 `E01.srt`。
- 扩展名只允许小写 `.srt`、`.ass`、`.vtt`；`.SRT` 或 `Movie.zh-CN.srt` 等语言后缀变体不支持。
- 同路径、同扩展名的 managed subtitle 可以原子替换；不同扩展名可并存。内容按原始字节保存，不转码，不修改 BOM 或换行。
- 不能覆盖种子自带 payload 文件。相同 stem 的多个视频、single-file torrent 可见名称被 hash 消歧等情况会使目标不可上传。
- 元信息未就绪、没有支持的视频、任务正在删除、名称冲突时，UI 会说明原因。

字幕写入 `torrents-dir/.metadata/subtitles` 后由只读 FUSE 投影；`docker cp`、FUSE 和 SMB 都不是写入口，不需要通过放宽挂载权限上传。支持的视频扩展名、精确路径规则和错误码见 [API 参考](api.md)。

播放器可能缓存字幕，上传或替换后需要重新扫描或重新打开视频。仅监听 inotify 且不做定期 rescan 的消费方，替换后不一定自动刷新。部署时应手工验证：

1. 新上传字幕 → 服务重新扫描或重新打开视频 → 字幕可见。
2. 同名替换 → 消费方重新读取 → 新内容生效。
3. 删除 torrent → 字幕随任务从挂载点消失。

## 上传限速

限制的是同一个 Session 内所有 torrent、peer 合计的 BitTorrent payload 上传速率，单位为 **bytes/s**，不是 bits/s。不约束 `.torrent`/字幕 HTTP 请求体，也不涵盖 tracker、握手等协议开销。

Header 齿轮 → “上传限速设置”可设置 `1MiB/s`、`32MB/s` 等上限，并选择是否只在指定时段限速。UI 无损转换为 API 的整数 bytes/s：

- rate 为 `0` 且无 schedule：不限速，默认行为。
- rate 为正数且无 schedule：全天限速。
- rate 为正数且有 schedule：窗口内限速，窗口外不限速。

时段按**服务端本地时间**（`time.Local`，容器中通常是 UTC），采用 24 小时制 `HH:MM` 和半开区间 `[start, end)`。例如 `08:00` 整开始、`22:00` 整恢复不限速。start/end 必须同时提供且 `start < end`，只支持同一自然日内的单窗口，不支持跨午夜、星期、节假日或多窗口；有 schedule 时 rate 必须为正数。

设置立即调整现有限速器，不重建 client/torrent，不中断上传；时段切换亦如此，并处理夏令时的缺失/重复小时。底层 token bucket 允许发送完整请求 chunk 所需的有界 burst，因此限制持续聚合速率，不是每个瞬时网络包的硬上限；边界前进入缓冲的 chunk 可能短暂跨越边界。

设置没有 TOML key 或环境变量，保存在当前目录的 `.metadata/upload_rate.json`，下次启动在创建 BitTorrent client 之前恢复。每个 `torrents-dir` 独立保存，文件缺失表示不限速，首次保存才创建文件。内部格式为 `version: 1`，文件损坏、未知字段或不支持的版本会导致启动失败，不会静默退回不限速。

UI/API 禁用时，已保存的规则仍会在启动时加载，但没有在线修改入口；手工调整内部文件需要重启。保存前的写入失败保持旧规则不变；rename 后目录同步失败表示新规则已生效但持久性未确认，UI 会提供重新读取入口，API 语义见 [API 参考](api.md)。

## 状态、缓存与传输统计

| `state` | 含义 |
| --- | --- |
| `adding` | metainfo 尚不可用，例如磁力链接仍在解析 |
| `ready` | metainfo 可用，可以建立文件视图并按需读取 |
| `error` | 无法取得或处理 metainfo |
| `deleting` | 删除进行中 |
| `delete_failed` | 删除失败，可再次处理 |
| `deleted` | 删除 operation 终态，不作为任务持久化 |

`cached_bytes` 表示当前仍驻留在内存的字节数，不是下载进度；piece 淘汰后可能下降，重启后从零开始。`cache.used_bytes` / `cache.capacity_bytes` 是共享 piece LRU 的当前占用和配置硬上限，不包含 staging、临时副本、协议缓冲或进程 RSS。

逐任务 `downloaded_bytes` 使用 useful payload（`BytesReadUsefulData`），`uploaded_bytes` 使用实际发送的 data payload（`BytesWrittenData`），不包含 wire overhead。从当前后端 Session 注册运行时句柄起累计；浏览器刷新或多前端查询不清零，后端重启归零，不持久化。任务句柄移除后，其删除中行的传输量会回落为零。

全局 `GET /api/v1/stats` 的传输统计从 Session 创建时起累计；删除任务不会让全局计数清零或回退，多前端读取同一个累计值。缓存统计仍是当前占用，不能用它替代累计传输量。

## 持久化与恢复

```text
<torrents-dir>/
├── <infohash>.torrent             API 管理的 canonical metainfo
└── .metadata/
    ├── categories.json           分类名称与创建时间
    ├── pending/<infohash>.magnet  尚未解析完成的磁力意图
    ├── state/<infohash>.json      任务 registry entry
    ├── subtitles/<infohash>/…    managed subtitles，镜像 torrent 相对路径
    ├── upload_rate.json          上传限速设置
    ├── layout_version            一次性旧布局迁移标记
    ├── peer_id                   该目录的 20 字节 peer identity
    └── instance.lock             运行时独占锁
```

- `.metadata` 是内部实现目录，不出现在 FUSE/SMB 中，同一个目录只能由一个进程管理。
- registry 是任务集合的唯一事实来源；启动从 `.metadata/state` 恢复任务、从根目录读取 canonical metainfo、从 pending 恢复未完成 magnet，不扫描根目录猜测任务。
- piece 数据、completion、cache hit、临时读取优先级与传输计数只在内存中，重启不会 rehash 或恢复 piece。
- 分类文件缺失表示没有分类；旧 sidecar 缺 `category` 时按未分类恢复，非空悬空分类引用会使启动失败。
- 首次启动会将可识别的旧 flat metainfo/magnet intent 一次性迁移到新布局；hash 冲突、损坏或非 canonical 历史文件会导致明确失败，写入 `layout_version` 后不再读旧位置。
- 字幕只为 `ready` 任务扫描和校验，删除中的任务不会进入可见 index。无法唯一对应视频的 sidecar、符号链接、设备或特殊文件会使启动失败，而不是被收编或暴露。
- 启动会清理上次崩溃留下的内部临时文件；字幕删除只有在目录已不存在且父目录同步后才完成 operation。

## 常见问题

| 现象 | 原因与处理 |
| --- | --- |
| API 返回 `401` | token 缺失、格式错误、过期或已撤销；重新登录 |
| magnet 一直 `adding` | peers 尚未提供 metainfo，检查来源与网络可用性 |
| `ready` 后读取等待 | 所需 piece 不在缓存，仍需 peers 提供，不代表完整下载已完成 |
| 视频或封面显示超时或 I/O 错误 | 单次读取可能超过 `mount.read_timeout`，默认 `30s`；检查来源与网络，慢来源或大 piece 可调长配置后重启 |
| 字幕上传 `413` | 请求超过 `http.max_upload_size`，默认 `10MiB`；调高配置后重启 |
| `415` + `subtitle_name_mismatch` | basename 不严格对应或多了语言后缀，改名后重试 |
| `415` + `subtitle_format_unsupported` | 扩展名不是小写 `.srt`/`.ass`/`.vtt` |
| `409` + `subtitle_payload_conflict` | 目标是种子自带文件，不能覆盖 |
| `409` + `subtitle_name_conflict` | 同 stem 视频有歧义，或 single-file 可见名已被消歧 |
| `409` + `torrent_deleting` | 任务处于 `deleting` 或 `delete_failed`，先完成删除 |
| 添加任务返回 `subtitle_namespace_conflict` | 新任务会让已有视频改名或遮蔽字幕路径；解决已有命名冲突后重试 |
| `503` + `subtitle_storage_unavailable` | 字幕目录缺失、不可写或被 symlink/普通文件替换，检查目录与权限 |
| `507` + `subtitle_storage_full` | 磁盘或配额不足；释放空间后重试，失败时旧字幕保持完整 |
| `delete_failed` + `subtitle_cleanup_failed` | 修复存储权限/空间，再次删除同一任务或重启以重试 |
| 启动报字幕匹配或 namespace conflict | 内部目录有不匹配 sidecar，或旧版本/手工修改产生冲突；先停服务并备份管理目录，再移出不受管理的文件或冲突任务的 state/metainfo，避免直接覆盖现有数据 |
| 上传成功但播放器仍用旧字幕 | 消费方缓存或未 rescan，重新扫描/打开文件 |
| `docker cp` 写挂载点失败 | 预期只读行为，改用 API/UI 上传 |

目录权限、容器 listener、SMB 和端口问题见[部署指南](deployment.md)，完整错误协议见 [API 参考](api.md)。
