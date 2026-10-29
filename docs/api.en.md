# HTTP API reference

[简体中文](api.md) · [README](../README.en.md)

The HTTP listener serves the embedded Web UI and management/diagnostic API at `/api/v1`, defaulting to `http://127.0.0.1:8080`. Content is read through FUSE or SMB after selecting that file backend. Explicit STRM configuration additionally enables a separate signed `/stream/...` content endpoint.

Commands run from the repository root. Listener and credential configuration is documented in the [configuration guide](configuration.en.md).

## Routes

| Method | Path | Purpose | Success |
| --- | --- | --- | --- |
| `POST` | `/api/v1/auth/login` | Exchange the configured password for a Bearer token | `200` |
| `POST` | `/api/v1/auth/logout` | Revoke the current token | `204` |
| `POST` | `/api/v1/torrents` | Add a magnet JSON body or multipart `.torrent` upload | `201` |
| `GET` | `/api/v1/torrents` | List tasks | `200` |
| `GET` | `/api/v1/categories` | List categories | `200` |
| `POST` | `/api/v1/categories` | Create `{"name":"movies"}` | `201` |
| `PUT` | `/api/v1/torrents/{id}/category` | Set or clear a category | `200` |
| `GET` | `/api/v1/stats` | Read session cache and transfer statistics | `200` |
| `GET` | `/api/v1/settings/upload-rate` | Read upload-rate settings | `200` |
| `PUT` | `/api/v1/settings/upload-rate` | Update upload-rate settings | `200` |
| `GET` | `/api/v1/torrents/{id}` | Read a task summary | `200` |
| `GET` | `/api/v1/torrents/{id}/status` | Read pieces, files, and network diagnostics | `200` |
| `DELETE` | `/api/v1/torrents/{id}` | Start asynchronous deletion | `202` |
| `PUT` | `/api/v1/torrents/{id}/subtitles` | Upload or replace a managed subtitle | `201` / `200` |
| `PUT` | `/api/v1/torrents/{id}/favorite` | Set or clear the favorite flag | `200` |
| `POST` | `/api/v1/torrents/prune` | Delete old non-favorite tasks | `200` |
| `GET` | `/api/v1/operations/{id}` | Poll an asynchronous operation | `200` |
| `GET` / `HEAD` | `/stream/{hash}/{signature}/{path...}` | Optional file-scoped signed content read, separate from management Bearer authentication | `200` / `206` / `304` |

A torrent ID is its 40-character lowercase hexadecimal info hash. Operation IDs come from deletion responses. Business errors use `{"error":"..."}` JSON, but unknown paths and unmatched methods may be handled by the standard `net/http` router; not every error is JSON.

## Authentication

With the local default listener and authentication disabled, requests can be made directly:

```sh
BASE_URL=http://127.0.0.1:8080
curl --fail "$BASE_URL/api/v1/torrents"
```

When authentication is enabled, `POST /api/v1/auth/login` is the only unauthenticated `/api/` route. Other API requests require exactly one `Authorization: Bearer <token>` header; query and cookie authentication are not accepted. Static Web UI assets remain public so the browser can load the shell, but that does not grant access to torrent data. Explicitly enabled `/stream/...` is a separate file-scoped signature route outside the Bearer middleware, not a relaxation of management API authentication.

Management tokens are opaque and stored only in daemon memory. Valid requests slide their inactivity expiry; service restart invalidates all tokens. The management API does not accept cookies or URL tokens and does not use JWTs or refresh tokens. `token_ttl` is a positive Go duration no longer than 24 hours. Protected `401` responses include `WWW-Authenticate: Bearer`.

### Login and logout

A login request must use `application/json` and contain only the string field `password`; unknown fields, including `username`, return `400`. The following examples require `curl` and `jq` and assume that the shared password variable has been prepared as in the [deployment guide](deployment.en.md):

```sh
BASE_URL=http://127.0.0.1:8080
LOGIN_BODY="$(jq -n \
  --arg password "$TORRENTFS_PASSWORD" \
  '{password: $password}')"
TOKEN="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/auth/login" \
  --header 'Content-Type: application/json' \
  --data "$LOGIN_BODY" | jq -r .token)"
```

The response contains `token`, `token_type` (`Bearer`), and `expires_in`. Login bodies are limited to 8 KiB. With authentication disabled, skip login and omit the authorization headers in subsequent examples.

Logout revokes the current token and returns an empty `204` response:

```sh
curl --fail --request POST "$BASE_URL/api/v1/auth/logout" \
  --header "Authorization: Bearer $TOKEN"
```

## Signed HTTP content reads (STRM)

`strm.base_url` is empty by default, so `/stream` and `/stream/*` return `404` rather than the SPA. A non-empty value enables separate `GET` / `HEAD` handlers. Obtain the URL from a generated `.strm` file; there is no additional login token or management link-export endpoint:

```text
<base_url>/stream/<40-character lowercase hexadecimal infohash>/<signature>/<original torrent-relative path, escaped per segment>
```

`signature` is the full HMAC-SHA256 binding a versioned domain, info hash, and **original torrent display path**, encoded as unpadded base64url (43 characters). The path is not a host path, category path, or STRM display name. Chinese characters, spaces, `%`, `#`, `?`, and `+` are escaped per component. The server decodes once and rejects empty components, `.`, `..`, NUL, backslashes, and encoded slashes. For example, `%252F` becomes a literal `%2F` in a filename, not a second-decoded `/`. A reverse proxy must strip any external prefix before forwarding `/stream/...`.

The URL is a read-only file-scoped credential, without a management password, Bearer token, or Cookie. Its holder can read that file, not manage tasks or read other files. Only registered, ready, existing payload files are served. Restart and category changes preserve signatures; deleting the task or rotating the key invalidates old links. Key storage and rotation are in the [usage guide](usage.en.md). Protect full URLs and reverse-proxy logs, and use HTTPS outside a trusted LAN.

Copy the exact URL from a `.strm` file without its trailing newline; do not construct the signature manually:

```sh
STREAM_URL='<copy-the-exact-signed-URL-from-a-strm-file>'
curl --fail --head "$STREAM_URL"
curl --fail --range 0-1023 "$STREAM_URL" --output ./range.bin
```

These requests have no `Authorization` header. Do not expose real links in shared logs, issue reports, or debugging output.

- `http.ServeContent` handles full GET, HEAD, single/multipart Range, conditional requests, and `If-Range`. Satisfied ranges return `206`; unsatisfiable ranges return `416`. An `If-Range` mismatch falls back to the full response under standard semantics.
- Responses use an explicit `Content-Type` (`application/octet-stream` for unknown extensions), ETag, Last-Modified, and Content-Length where applicable, plus `Cache-Control: private, no-store` and `Referrer-Policy: no-referrer`. Lengths and offsets are int64, including files larger than 4 GiB. MIME detection does not sniff content; HEAD, metadata, and bodyless conditional responses do not fetch payload.
- Reads reuse the Session cache, readahead, and `filesystem.read_timeout`. Client cancellation cancels that request's reads and releases its reader. A read timeout or other read failure after the response starts aborts the transfer; it does not append JSON or guarantee a `504`. Clients must treat a truncated response as failure.

| Status | Stream meaning |
| --- | --- |
| `200` / `206` | Full response / satisfied Range response |
| `304` / `412` | Standard conditional-request outcome, depending on request headers |
| `403` | Canonically formatted signature with a mismatched MAC, including cross-file/hash reuse |
| `404` | Disabled, malformed path/signature format, or a valid signature for an unknown/not-ready task or file |
| `405` | A method other than GET/HEAD while enabled; includes `Allow: GET, HEAD` |
| `416` | Unsatisfiable Range |
| `503` | HTTP is shutting down or the Session is closed |
| `500` | Other open/metadata errors before content response headers are sent |

## Add and inspect a task

Replace the placeholder with a real magnet:

```sh
MAGNET_URI='magnet:?xt=urn:btih:<info-hash>'
TORRENT_ID="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data "$(jq -n --arg magnet_uri "$MAGNET_URI" '{magnet_uri: $magnet_uri}')" \
  | jq -r .id)"
```

Alternatively upload a local `.torrent`:

```sh
TORRENT_ID="$(curl --fail --silent --show-error \
  --request POST "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN" \
  --form "file=@$PWD/example.torrent" \
  | jq -r .id)"
```

Do not set `Content-Type: multipart/form-data` manually; curl generates the required boundary. The default upload limit is `10MiB`, configured by `http.max_upload_size`. The service validates the info hash and stores canonical `<infohash>.torrent` metainfo; the uploaded filename is not a storage identifier. Copying a file into the managed directory is not an import operation.

List tasks and inspect the new task:

```sh
curl --fail "$BASE_URL/api/v1/torrents" \
  --header "Authorization: Bearer $TOKEN"
curl --fail "$BASE_URL/api/v1/torrents/$TORRENT_ID" \
  --header "Authorization: Bearer $TOKEN"
curl --fail "$BASE_URL/api/v1/torrents/$TORRENT_ID/status" \
  --header "Authorization: Bearer $TOKEN"
```

A magnet can report `adding` with `metainfo_ready: false` and empty `pieces` and `files` arrays until peers supply metadata. `ready` means a file view is available, not that every piece has been downloaded.

Status snapshots describe absolute zero-based pieces; file ranges are half-open `[piece_start, piece_end)` intervals into that same piece array. Network and DHT fields are available in the raw status response, even though the UI does not provide a peer/DHT panel. `subtitle_targets` and `subtitles` describe the managed overlay separately; payload file/piece coverage semantics do not change.

## Categories and favorites

Create and assign a category:

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

Send `{"category":""}` to clear a task's category. Category names must be single path components without surrounding whitespace, `/`, `\`, or NUL. Unknown categories or tasks return `404`; duplicates, deleting tasks, and namespace conflicts return `409`. Task list objects always include `category`, with `""` for unclassified tasks.

Set or clear a favorite:

```sh
curl --fail --request PUT "$BASE_URL/api/v1/torrents/$TORRENT_ID/favorite" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"favorite":true}'
```

`favorite` must be explicitly supplied; omission returns `400`. Favorites exempt tasks only from age-based prune, not from a direct `DELETE`.

## Asynchronous deletion and prune

Deletion returns `202` and an operation ID:

```sh
curl --fail --request DELETE "$BASE_URL/api/v1/torrents/$TORRENT_ID" \
  --header "Authorization: Bearer $TOKEN"
```

Use the returned operation ID to poll:

```sh
OPERATION_ID='<operation-id-from-delete-response>'
curl --fail "$BASE_URL/api/v1/operations/$OPERATION_ID" \
  --header "Authorization: Bearer $TOKEN"
```

An operation starts in `deleting` and reaches `deleted` or `delete_failed`. Managed subtitle cleanup is part of completion. A cleanup failure can report `error_code: subtitle_cleanup_failed`; after repairing storage, a repeated delete reuses the same operation ID. Restart also retries cleanup.

To prune old non-favorite tasks:

```sh
curl --fail --request POST "$BASE_URL/api/v1/torrents/prune" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"older_than_days":30}'
```

`older_than_days` must be an integer in `1..106751`; the bound avoids overflow when days are converted to a Go duration. The response lists asynchronous `operations` and `excluded_favorites`. If individual candidates fail to start deletion, successful candidates continue and those failures appear in `failures`. The field is omitted when there are no such failures.

## Managed subtitle upload

Upload is multipart and accepts the target video's display path plus one subtitle file. The server derives the final subtitle path rather than allowing arbitrary writes below the mount:

```sh
curl --fail --request PUT "$BASE_URL/api/v1/torrents/$TORRENT_ID/subtitles" \
  --header "Authorization: Bearer $TOKEN" \
  --form 'video_path=Season 1/E01.mkv' \
  --form 'file=@./E01.srt'
```

A new subtitle returns `201`; replacement returns `200`. Do not manually set the multipart content type. `http.max_upload_size` also caps this request.

The target must be a supported payload video in a ready task. Paths must match the metainfo's slash-separated display path, not an absolute, host, or FUSE path. The video and subtitle basenames must match exactly. Supported subtitle extensions are lowercase `.srt`, `.ass`, and `.vtt`; language suffixes and uppercase extensions are rejected. The target must not collide with immutable payload, ambiguous video names, or a disambiguated single-file mount name.

Video targets use the 14 case-insensitive extensions listed in the [usage guide](usage.en.md). ISO is a STRM-only addition, not a managed-subtitle video target.

Use `subtitle_targets` from status to select a target. Each target provides `video_path`, `mount_path`, `expected_basename`, `uploadable`, and a stable reason when unavailable. Existing `subtitles` provide paths, format, size, and update time. The client must not infer uploadability independently.

| Stable error code | HTTP | Meaning |
| --- | --- | --- |
| `subtitle_name_mismatch` | `415` | Subtitle and video basenames do not match |
| `subtitle_format_unsupported` | `415` | Extension is not lowercase `.srt`/`.ass`/`.vtt` |
| `subtitle_video_not_found` | `404` | The path is not a supported payload video |
| `subtitle_name_conflict` | `409` | Subtitle target or visible mount name is ambiguous |
| `subtitle_payload_conflict` | `409` | Target path belongs to immutable payload |
| `torrent_deleting` | `409` | Task is deleting or has a failed deletion |
| `subtitle_storage_unavailable` | `503` | Managed storage is unavailable or not writable |
| `subtitle_storage_full` | `507` | Disk space or quota is exhausted |
| `subtitle_write_failed` | `500` | Other write failure, including interrupted requests |

Adding a torrent can also return `subtitle_namespace_conflict` (`409`) if it would rename an existing single-file video or occupy an existing managed subtitle's visible root path. No new task state is published in that case.

`docker cp` into `/mnt` or `/share` is not an alternative. See the [usage guide](usage.en.md) for replacement snapshots and player rescanning.

## Upload-rate settings

Both routes use the normal authentication rules. The API uses an integer number of bytes per second; human-readable unit strings are a UI input format, not the machine protocol.

```sh
curl --fail "$BASE_URL/api/v1/settings/upload-rate" \
  --header "Authorization: Bearer $TOKEN"
curl --fail --request PUT "$BASE_URL/api/v1/settings/upload-rate" \
  --header "Authorization: Bearer $TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"rate_limit_bytes_per_second":1048576,"schedule":{"start":"08:00","end":"22:00"}}'
```

Zero rate with `schedule: null` means unlimited; a positive rate without a schedule applies all day. A schedule uses server-local `HH:MM` time and a half-open interval. Both endpoints are required, `start < end`, and only a same-day window is supported. A scheduled rate must be positive; invalid input returns `400`.

Settings take effect immediately and persist to `.metadata/upload_rate.json`. A failure before atomic rename returns `503` with `upload_rate_settings_storage_unavailable` and `applied: false`. If rename succeeds but directory sync fails, the rule is already active; the response is `503` with `upload_rate_settings_durability_unconfirmed` and `applied: true`. Reread settings rather than assuming every failed response means no change. Scheduling and startup semantics are in the [usage guide](usage.en.md).

## Statistics and errors

`GET /api/v1/stats` returns session-wide cache and transfer counters. Cache `used_bytes` / `capacity_bytes` describe verified resident pieces and the configured cache cap, not process RSS. Transfer `downloaded_bytes` is useful payload; `uploaded_bytes` is actual data payload sent. Counters exclude protocol overhead and reset when the backend session restarts, not when a browser refreshes or a task is deleted.

Task states and per-task handle lifetimes are explained in the [usage guide](usage.en.md).

| HTTP status | Typical cause |
| --- | --- |
| `400` | Invalid JSON/multipart data, content type, magnet, path, or setting |
| `401` | Missing, malformed, expired, or revoked token; invalid login credentials |
| `403` | Canonically formatted but mismatched stream signature |
| `404` | Unknown task/operation/category; unavailable login/logout when auth is disabled; disabled stream, malformed stream path/signature format, or unknown content |
| `405` | Unsupported method; enabled streams accept only GET/HEAD |
| `409` | Deletion or visible-namespace conflict |
| `413` | Upload body exceeds its limit; login has a fixed 8 KiB limit |
| `415` | Unsupported request media type or subtitle name/format |
| `416` | Unsatisfiable stream Range |
| `500` | Unclassified internal error |
| `503` | Unavailable subtitle/settings storage, unconfirmed settings durability, or stream service/Session shutdown |
| `507` | Insufficient subtitle storage space or quota |

Unknown routes and methods can produce router `404`/`405` responses rather than a SPA page or business JSON. Errors do not expose host storage paths.
