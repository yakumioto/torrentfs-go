# 删除种子前同步上报 stopped，避免丢失上传量

> **状态**：已实现。

## 背景

tracker 只按 announce 里的累计 `uploaded` 结算上传量：一次 announce 报出更大的值，两次之间新增的部分才会被记账。删除种子时，torrentfs 立即调用 `Drop()`；底层库把 `stopped` announce 交给异步调度器，而且只有种子从 client 中消失后才会产生这个事件。等调度器真正发送时，`Torrent` 可能已被回收，发送被跳过（`skipping announce for GCed torrent`），于是最后一次上传量永远不会到达 tracker。

保留 `Torrent` 对象不能解决这个问题：对象还在注册表里时，底层库根本不会产生 `stopped` 事件。

## 决定

删除流程在 `Drop()` 之前同步上报一次 `stopped`（方案 A）：

- 依赖 fork 新增的 `Torrent.AnnounceStopped(ctx)`。它复用常规 tracker announce 的发送路径，在种子仍注册、announce 状态与累计计数器仍可用时，向每个常规 tracker 发送 `stopped` 并等待响应。
- `performDelete` 在关闭本地资源之后、`Drop()` 之前调用它。先关闭读取与缓存，是为了让上报的累计上传量尽量接近最终值。
- 整次 flush 有界：默认 15 秒，由会话创建时固定，调用方的 context 到期即返回。
- flush 失败不改变删除结果。删除操作仍是异步的（`202` + operation id），失败原因记在 operation 的 `warning` 上并写一条 warning 日志；operation 的 `state` 仍然是 `deleted`，`error` 保持为空。API 的 operation 响应新增可选的 `warning` 字段，删除对话框会显示这条非致命提示。
- `stopped` 失败后，底层调度器会按它自己的重试节奏（约一分钟后）再尝试一次，即使这次也失败，删除已经完成，不会被回滚。

## 考虑过的替代方案

- **B：让调用方（pt-boost）根据 `last_announce_at` 推迟删除**：被拒。这是客户端侧的规避，只保护一个调用方；torrentfs 的其他删除入口仍然会丢上传量，而且把 announce 时序知识泄露给外部。
- **C：持久化每个种子最后一次成功上报的累计上传量**：不解决本次问题。它只能防止进程重启后上报值回退，无法产生 tracker 需要的 `stopped` 事件，也无法在删除前补报；作为独立改进保留。
- **删除改为同步等待底层调度器的 `stopped`**：被拒。该事件只有在种子已从 client 移除后才产生，等待期间无法保证对象不被回收；把删除阻塞在不可控的调度器时序上也不合适。
- **flush 失败时让删除失败并允许重试**：被拒。tracker 的记账是外部状态，不该阻止本地数据与元数据被删除；重试删除也无法复活已关闭的 torrent 计数器。

## 影响

- tracker 不可达时，删除操作最多多等 15 秒；多个 tracker 共享同一个 deadline，总时长仍有界。
- 只覆盖常规 tracker（HTTP/UDP）的 announce 状态；WebSocket tracker 不走这套调度器，不在本次范围内。
- `warning` 只存在于内存中的 operation 记录，不会写入 sidecar，也不会在进程重启后保留。
- 进程重启仍会使真实上传量归零（方案 C 未实现）；本次只保证正常删除路径上的最后一次上报。
