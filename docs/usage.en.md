# Usage guide

[简体中文](usage.md) · [README](../README.en.md)

Use the [quick start](../README.en.md#quick-start) to start a service and read your first file. Alternate modes and permissions are in the [deployment guide](deployment.en.md); scripted operations are in the [API reference](api.en.md).

## Add and read torrents

Add a magnet or upload a `.torrent` through the Web UI or API. Arbitrary `.torrent` files copied into the managed directory are not imported. A magnet can remain `adding` until peers supply metainfo.

Read content through the FUSE tree or SMB share. The HTTP API manages tasks but has no generic content download or playback route. Reads fetch the needed pieces on demand; a player or indexer may wait when pieces are not cached or peers are unavailable. Random seeks fetch the corresponding pieces rather than requiring a sequential full download.

Each foreground content read is bounded by `mount.read_timeout` (`30s` by default). If no source provides a verified piece in time, only that read ends with a read error after the deadline (the client may report a timeout or an I/O error); the torrent is not deleted and other reads are not cancelled. This is not a limit on the whole playback session or file handle. Raise it for slow swarms or large pieces, or override it with `TORRENTFS_MOUNT_READ_TIMEOUT`, then restart; see the [configuration guide](configuration.en.md).

## FUSE and SMB layout

```text
<mount>/
├── <single-name>       # unclassified single-file torrent
├── <multi-name>/       # unclassified multi-file torrent
│   └── <relative-file>
└── <category>/         # category directory, including an empty category
    ├── <single-name>
    └── <multi-name>/
        └── <relative-file>
```

- Every node is read-only. Create, write, delete, and rename operations fail.
- `.metadata` is never mounted, and there are no `metadata/` or `stats/` control directories.
- A single-file torrent is not wrapped in an extra directory: an unclassified video is `<mount>/movie.mp4`.
- Display-name collisions are disambiguated with an info-hash prefix within the affected parent directory. Identical names in different categories do not conflict.
- Category changes affect only virtual paths; they do not move payload, cache, or managed subtitle files.

Managed subtitles are projected beside their video. A single-file video has a subtitle sibling at the root; a multi-file video has a subtitle in the same subdirectory. Both remain read-only.

## Web UI

The embedded UI shares the HTTP listener with `/api/v1`:

- The dashboard searches by name or info hash, filters tasks, displays cache and runtime transfer counters, and accepts magnets or one or more `.torrent` files.
- Task details show files, piece ranges, cache coverage, and piece states. Network/DHT fields are available in the raw API; the UI does not provide a peer/DHT panel.
- Categories, favorites, and asynchronous deletion are managed without writing through the mount.
- Subtitle upload/replacement uses server-provided video targets and mount paths. Managed subtitles are listed separately from payload files and do not contribute to piece coverage.
- The header's settings button opens upload-rate settings.

Queries normally refresh every five seconds and stop polling while the page is hidden. When authentication is enabled, the current tab holds the opaque token in `sessionStorage`; the daemon keeps tokens only in memory. Restarting the service or expiring a token requires login again.

Frontend development is covered in the [development guide](development.en.md).

## Categories, favorites, and cleanup

A category name is both a stable identifier and one directory level in the FUSE tree. You can create a category, assign a task, or clear its category. Empty categories remain visible. This version does not support renaming, deleting, nesting, or assigning multiple categories to one task.

Favorite flags are persisted with the task. They exempt tasks from age-based prune operations, not from a direct delete request. Deletion and prune are asynchronous; the UI polls the returned operations. Managed subtitles are removed with the task.

If subtitle cleanup fails, the task stays hidden rather than reappearing in the filesystem. Repair storage permissions or space, then retry deletion; the existing operation is reused. A restart also retries failed cleanup. API payloads and error codes are described in the [API reference](api.en.md).

## Managed subtitles

Torrent payload files are immutable. Upload subtitles through the management UI/API, not by writing into the mount or using `docker cp` against `/mnt` or `/share`.

In task details, select a server-provided video target, select one subtitle, and check the displayed torrent-relative and mount paths before uploading. The UI reports creation, replacement, or an actionable error. When no target is available, it explains whether metainfo is missing, the video format is unsupported, the name is ambiguous, or the task is being deleted.

Naming rules:

- Supported subtitle extensions are lowercase `.srt`, `.ass`, and `.vtt`; uppercase forms such as `.SRT` are rejected.
- The video and subtitle basenames must match exactly, including case. For `Season 1/E01.mkv`, use `E01.srt`, not `E01.zh-CN.srt`.
- The subtitle is stored beside the selected video and preserved byte-for-byte; no transcoding or newline normalization is performed.
- An existing torrent payload subtitle is never overwritten. A managed subtitle at the same path can be atomically replaced, and different supported extensions may coexist.
- Ambiguous video names or a single-file mount name changed by collision disambiguation can make a target unavailable.

New opens see a replaced subtitle; an already-open handle continues reading its original snapshot. A player may cache subtitles or require an explicit rescan. Merely watching inotify is not a guarantee that an external consumer refreshes replacement content.

For a media-service deployment, verify three cases: first upload becomes visible, replacement is reread, and deletion removes the subtitle with the task. Record whether that service requires a rescan.

## Upload-rate settings

Upload limiting applies to the aggregate BitTorrent payload upload rate across a session's torrents and peers, measured in bytes per second, not bits per second. It does not limit `.torrent`/subtitle HTTP request bodies, tracker traffic, or protocol overhead.

Use the header settings button or the corresponding API. This is not static TOML configuration and has no environment-variable binding. The UI accepts exact unit values such as `1MiB/s` and `32MB/s`, then sends a normalized integer bytes-per-second value to the API.

| Rate and schedule | Behavior |
| --- | --- |
| Zero rate, no schedule | Unlimited; default |
| Positive rate, no schedule | Limited all day |
| Positive rate, schedule | Limited within the configured window; unlimited outside it |

Schedules use server-local time, commonly UTC in a container, in 24-hour `HH:MM` format. The interval is half-open: `08:00` starts limiting and `22:00` ends it. Both endpoints are required, `start < end`, and the window must remain within one calendar day. Overnight, weekday, holiday, and multiple-window schedules are not supported. A scheduled limit must have a positive rate.

Saving takes effect immediately without rebuilding the client or interrupting active torrents. The limiter allows a bounded burst for a complete request chunk; it describes sustained aggregate rate, not a hard instantaneous packet limit.

The setting is persisted as `torrents-dir/.metadata/upload_rate.json` and loaded before the BitTorrent client starts. Each managed directory has its own setting. A missing file means unlimited; the file is created on first save. Invalid content, unknown fields, or an unsupported version cause startup to fail rather than silently disabling the limit.

A warning after the atomic rename means the new rule is active but restart durability could not be confirmed; use the UI's reread action or inspect the [API response](api.en.md). Before-rename failures leave both running and persisted rules unchanged. With HTTP disabled, saved rules still load; manual editing requires stopping and restarting the daemon.

## States and statistics

Task lifecycle is not a completion percentage:

| State | Meaning |
| --- | --- |
| `adding` | Metainfo is not available yet, commonly for a magnet |
| `ready` | Metainfo is available and the file view can be read |
| `error` | Metainfo could not be obtained or processed |
| `deleting` | Deletion is in progress |
| `delete_failed` | Deletion failed and can be retried |
| `deleted` | Terminal state of a deletion operation |

`ready` does not mean every piece has been downloaded. Reads may wait for peers.

`cached_bytes` is current in-memory cache occupancy, not the number of bytes ever downloaded. Eviction can lower it, and restart empties the cache. `downloaded_bytes` counts useful payload received; `uploaded_bytes` counts actual torrent data payload sent. Neither includes protocol overhead or persists across backend restarts.

The session's global transfer totals do not reset on browser refresh or task deletion. Per-task counters follow that task's runtime handle and can fall to zero when the handle is removed. Cache capacity limits the shared piece cache, not staging, temporary copies, protocol buffers, or process RSS.

## Persistent state

```text
<torrents-dir>/
├── <infohash>.torrent
└── .metadata/
    ├── categories.json
    ├── pending/<infohash>.magnet
    ├── state/<infohash>.json
    ├── subtitles/<infohash>/<torrent-relative path>
    ├── upload_rate.json
    ├── layout_version
    ├── peer_id
    └── instance.lock
```

The registry is the task source of truth. Startup restores registered tasks using canonical metainfo and pending magnet intents, rather than scanning arbitrary root-level `.torrent` files. Only one process can manage the directory at a time.

Task state, categories, favorites, upload-rate settings, peer identity, and managed subtitles persist. Piece bytes, completion, cache-hit counts, transfer counters, and transient read priorities exist only in memory. Restart neither rehashes nor restores piece payloads.

Managed subtitles are stored separately from payload under `.metadata/subtitles` and cleaned up with their task. Unsupported files or unsafe entries in managed state can cause an explicit startup failure; do not use `.metadata` as an arbitrary import directory.

Static TOML/environment changes require restart. Upload-rate settings are the exception: API/UI changes apply immediately and are restored at startup.

## Troubleshooting

| Symptom | Likely cause and action |
| --- | --- |
| `401` from the API | Missing, malformed, expired, or revoked Bearer token; log in again. |
| Magnet remains `adding` | Peers have not supplied metainfo; check source availability and network conditions. |
| A `ready` read waits | The required pieces are not cached; available peers still need to provide them. |
| Video or cover reads report timeout or I/O errors | One read may have exceeded `mount.read_timeout`, default `30s`; check source/network availability, or increase the setting for slow swarms or large pieces and restart. |
| `docker cp` into `/mnt` or `/share` fails | Expected: the projected tree is read-only. Use UI/API subtitle upload. |
| Subtitle upload returns `413` | The body exceeds `http.max_upload_size`; adjust static configuration and restart. |
| Subtitle upload returns `415` | Check the exact basename and lowercase `.srt`/`.ass`/`.vtt` extension. |
| Subtitle upload returns `409` | The target conflicts with payload or another visible name, or the task is being deleted. |
| Subtitle upload returns `503` or `507` | Repair unavailable/unwritable storage or free disk space/quota, then retry. |
| Deletion reports subtitle cleanup failure | Repair storage and retry the same task deletion; do not expect the hidden task to reappear. |
| Player still shows old subtitles | Reopen or rescan using that player's own refresh behavior. |

Deployment-specific failures are covered in the [deployment guide](deployment.en.md); stable subtitle and upload-rate error codes are in the [API reference](api.en.md).
