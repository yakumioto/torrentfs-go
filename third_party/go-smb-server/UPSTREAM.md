# Upstream provenance

- Module: `github.com/sonroyaalmerol/go-smb-server`
- Version: `v0.2.0`
- Commit: `8e742d318cb3ad18006819f0f3ca09b62835530f`
- Module sum: `h1:QJrFDuw8vml6eErSlq4wyZVa3d7UJfUd5K8j01cj6aI=`
- License: MIT, preserved in `LICENSE`.
- Source copied from the Go module archive; upstream's prebuilt `encryption.test` is excluded.

## Local compatibility changes

- `smb/server/server.go`: public `Serve(ctx, net.Listener)` owns its listener, cancels and closes active connections, joins their handlers, and releases its cancellation watcher on every return. `Shutdown` drains the active serve and prevents a racing late start.
- `smb/server/{server,dispatch}.go`: directory snapshots, saved search patterns and cursors survive pagination. Restart/reopen refreshes the snapshot, single-entry requests advance once, and undersized buffers fail without losing entries. Encoded sizes include UTF-16 and alignment.
- `smb/server/dispatch.go`, `smb/vfs/vfs.go`: forward CREATE access/options to the backend; bound READ length and signed offset before allocation.
- `smb/server/queryinfo.go`, `smb/vfs/vfs.go`: forward delete disposition to `SetInfoer` before changing delete-pending state, allowing read-only handles to reject it explicitly.
- `smb/wire/fscc.go`: encode/decode UTF-16 surrogate pairs so non-BMP torrent names remain addressable.
- `smb/wire/fileops.go`: emit all 100 fixed bytes of `FileAllInformation`; real clients otherwise reject handle Stat. `smb/server/dispatch.go` also preserves backend file attributes (including read-only).
- `smb/server/compatibility_test.go`: regression coverage for pagination, snapshot/restart, single entries, small buffers, read bounds, forwarded flags, idle/blocked-write cancellation, failed accept and shutdown/start ordering.

Remove the local replacement when an upstream release includes these behaviors and passes the repository's real-client, Session and Docker SMB tests. This copy is not a separately published fork. No upstream protocol or authentication code is otherwise replaced.
