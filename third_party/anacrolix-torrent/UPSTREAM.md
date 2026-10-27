# Upstream provenance

- Module: `github.com/anacrolix/torrent`
- Source replacement: `github.com/yakumioto/torrent v1.61.0-bep27.3`
- Commit: `3f240da1eade9026256bae662a4dbcfa2d090e05`
- Tag object: `49e27654a3ddc97864949fd97c2dcf58f1a5752e`
- Module sum: `h1:vjB/4FVkJ4XG7jEzQmWV9FzeW/np4f6rPM6zSlOp7Sw=`
- License: MPL-2.0, preserved in `LICENSE`.

The source is copied from the pinned Go module archive without changing the existing BEP 27 DHT/PEX isolation, `internal/mytimer.Timer.When` read locking, or closed peer-request cleanup fixes. This is not a newly published fork.

## Additional local patch

- `webseed-peer.go`: reader/completion demand calls the existing client-level `scheduleImmediateWebseedRequestUpdate`; closed clients reject new notifications.
- `webseed-requesting.go`: the timer callback takes the Client lock before checking closed state or changing its coalesced reason, preventing a raced shutdown from rearming the timer and synchronizing reason access.
- `webseed-wakeup_test.go`: demand wakeup, coalescing, safety-poll rearming, and closed-client regression tests.

The five-second safety polling interval, network timeouts, host concurrency and retry/backoff constants are unchanged. The patch is justified by the 16MiB+123 / 8MiB-cache cold-read regression: after admitted-prefix and storage-capacity fixes, eligible requests still started at roughly 5s and 10s while the unchanged 12s read gate failed.

Remove this local replacement only when an upstream release contains all retained fixes and the added demand-wakeup behavior, and passes the Session cold-read, reader/request-strategy, and complete Docker SMB regression gates.
