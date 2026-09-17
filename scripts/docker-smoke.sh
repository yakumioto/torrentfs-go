#!/usr/bin/env bash
set -Eeuo pipefail

readonly SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly FIXTURE_DIR="$ROOT_DIR/examples/docker"
readonly FIXTURE_TORRENT="$FIXTURE_DIR/example.torrent"
readonly FIXTURE_PAYLOAD="$FIXTURE_DIR/data/payload.txt"
# Info hash of examples/docker/example.torrent. The session storage reads
# payload data from <payload_dir>/<info-hash>/<name>, so the fixture payload
# must be preloaded at that path for the mount to serve real bytes.
readonly FIXTURE_INFO_HASH="dd45d0108ac27c3c0fb1d7b28fd2b313c753bc9b"
readonly MOUNTED_PAYLOAD="/mnt/payload.txt"
readonly LEGACY_STATS_MARKER="/torrents/.stats/leftover.txt"
readonly IMAGE="torrentfs-mio17-smoke:${BASHPID}"
readonly CONTAINER="torrentfs-mio17-${BASHPID}"
readonly FILE_INPUT_CONTAINER="torrentfs-mio17-file-input-${BASHPID}"
readonly MISSING_DIR_CONTAINER="torrentfs-mio17-missing-dir-${BASHPID}"
readonly HOST_OBSERVER_CONTAINER="torrentfs-mio17-host-observer-${BASHPID}"
readonly BLOCKED_READ_CONTAINER="torrentfs-mio17-blocked-${BASHPID}"

# Every Docker call that can hang is bounded: a wedged daemon or an unmount
# that waits forever must fail the smoke instead of hanging it.
readonly DOCKER_OP_TIMEOUT=60

# The anacrolix reader errors that a cancellation storm produces. Playback
# (before shutdown) must never emit them; teardown cancellations are counted
# separately and are not merged into the runtime figure.
readonly READER_CANCEL_PATTERN='msg="(initial read failed|read failed after reader reset|read failed after completion resync)" err="context canceled"'
readonly UNMOUNT_BUSY_PATTERN='failed to unmount .*Device or resource busy'

TMP_DIR=""
IMAGE_TAGGED=0
POSITIVE_STARTED=0
HOST_OBSERVER_STARTED=0
FILE_INPUT_CREATED=0
MISSING_DIR_CREATED=0

fail() {
	printf 'docker smoke: %s\n' "$*" >&2
	exit 1
}

# bounded runs a potentially blocking Docker operation under a timeout.
bounded() {
	local seconds="$1" description="$2"
	shift 2
	timeout --foreground "$seconds" "$@"
}

# container_logs prints a container's logs, bounded so an unresponsive daemon
# cannot stall diagnostics or teardown.
container_logs() {
	bounded 30 "docker logs $1" docker logs "$1" 2>&1 || true
}

# reader_cancel_count counts runtime/playback anacrolix reader cancellation
# errors in a log snapshot.
reader_cancel_count() {
	printf '%s\n' "$1" | grep -Ec "$READER_CANCEL_PATTERN" || true
}

# unmount_busy_lines prints any daemon-owned unmount failure lines.
unmount_busy_lines() {
	printf '%s\n' "$1" | grep -E "$UNMOUNT_BUSY_PATTERN" || true
}

# collect_unmount_diagnostics records the evidence needed to separate a
# daemon-owned outstanding request from a host fd/cwd holder. Missing tools or
# missing permission mark the diagnostics as incomplete; they are never
# evidence that no holder exists.
collect_unmount_diagnostics() {
	local container="$1" mount="$2" pid
	printf 'docker smoke: collecting unmount diagnostics for %s\n' "$mount" >&2
	bounded 20 "findmnt" findmnt -T "$mount" -o TARGET,SOURCE,FSTYPE,PROPAGATION,OPTIONS >&2 || \
		printf 'docker smoke: findmnt could not inspect %s\n' "$mount" >&2
	if command -v fuser >/dev/null 2>&1; then
		if ! bounded 20 "fuser" fuser -vm "$mount" >&2 2>&1; then
			printf 'docker smoke: fuser could not list holders for %s (permission or no holder)\n' "$mount" >&2
		fi
	else
		printf 'docker smoke: fuser is unavailable; holder diagnostics are incomplete, not empty\n' >&2
	fi
	if command -v lsof >/dev/null 2>&1; then
		if ! bounded 20 "lsof" lsof -- "$mount" >&2 2>&1; then
			printf 'docker smoke: lsof could not list holders for %s (permission or no holder)\n' "$mount" >&2
		fi
	else
		printf 'docker smoke: lsof is unavailable; holder diagnostics are incomplete, not empty\n' >&2
	fi
	if [[ -n "$container" ]]; then
		printf 'docker smoke: logs for %s:\n%s\n' "$container" "$(container_logs "$container")" >&2
	fi
	pid="$(pgrep -f "torrentfs" 2>/dev/null || true)"
	if [[ -n "$pid" ]]; then
		printf 'docker smoke: host torrentfs processes: %s\n' "$pid" >&2
	fi
}

# host_mount_released reports whether the propagated FUSE mount under mount is
# gone and its directory is empty again.
host_mount_released() {
	local mount="$1" fstype entry
	fstype="$(findmnt -T "$mount" -n -o FSTYPE 2>/dev/null || true)"
	entry="$(find "$mount" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null || true)"
	[[ "$fstype" != fuse.* && -z "$entry" ]]
}

cleanup() {
	local status=$?
	trap - EXIT INT TERM

	# Every teardown step is bounded. Forced kill/remove is a last resort for
	# residue; it never changes the recorded exit status, so a forced cleanup
	# cannot turn a failed run green.
	for container in "$CONTAINER" "$BLOCKED_READ_CONTAINER" "$HOST_OBSERVER_CONTAINER" \
		"$FILE_INPUT_CONTAINER" "$MISSING_DIR_CONTAINER"; do
		[[ -n "$container" ]] || continue
		bounded 30 "docker kill $container" docker kill --signal TERM "$container" >/dev/null 2>&1 || true
		bounded 30 "docker wait $container" docker wait "$container" >/dev/null 2>&1 || true
		bounded 30 "docker kill -9 $container" docker kill --signal KILL "$container" >/dev/null 2>&1 || true
		bounded 30 "docker rm -f $container" docker rm -f "$container" >/dev/null 2>&1 || true
	done
	if ((IMAGE_TAGGED)); then
		bounded 30 "docker image rm" docker image rm "$IMAGE" >/dev/null 2>&1 || true
	fi
	if [[ -n "$TMP_DIR" ]]; then
		rm -rf -- "$TMP_DIR" 2>/dev/null || true
	fi

	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for command_name in docker findmnt sha256sum timeout; do
	command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done
[[ -e /dev/fuse ]] || fail "FUSE prerequisite missing: /dev/fuse is not available"
docker info >/dev/null 2>&1 || fail "Docker daemon is unavailable"
[[ -f "$FIXTURE_TORRENT" ]] || fail "fixture is missing: $FIXTURE_TORRENT"
[[ -f "$FIXTURE_PAYLOAD" ]] || fail "fixture payload is missing: $FIXTURE_PAYLOAD"

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/torrentfs-mio17.XXXXXX")"
TORRENT_HOST_DIR="$TMP_DIR/torrents"
DATA_HOST_DIR="$TMP_DIR/data"
MOUNT_HOST_DIR="$TMP_DIR/mnt"
NEGATIVE_DATA_DIR="$TMP_DIR/negative-data"
NEGATIVE_MOUNT_DIR="$TMP_DIR/negative-mnt"
mkdir -p "$TORRENT_HOST_DIR" "$DATA_HOST_DIR" "$MOUNT_HOST_DIR" \
	"$NEGATIVE_DATA_DIR" "$NEGATIVE_MOUNT_DIR"
HOST_PROPAGATION="$(findmnt -T "$MOUNT_HOST_DIR" -n -o PROPAGATION 2>/dev/null || true)"
[[ "$HOST_PROPAGATION" == *shared* ]] || \
	fail "host mount containing $MOUNT_HOST_DIR must use shared propagation (current: ${HOST_PROPAGATION:-unknown})"
cp -- "$FIXTURE_TORRENT" "$TORRENT_HOST_DIR/example.torrent"
mkdir -p "$DATA_HOST_DIR/payload/$FIXTURE_INFO_HASH"
cp -- "$FIXTURE_PAYLOAD" "$DATA_HOST_DIR/payload/$FIXTURE_INFO_HASH/payload.txt"
EXPECTED_HASH="$(sha256sum "$DATA_HOST_DIR/payload/$FIXTURE_INFO_HASH/payload.txt")"
EXPECTED_HASH="${EXPECTED_HASH%% *}"

# A legacy empty .stats directory must survive startup untouched: the new
# layout neither creates nor removes it.
mkdir -p "$TORRENT_HOST_DIR/.stats"
printf 'legacy\n' > "$TORRENT_HOST_DIR/.stats/leftover.txt"

printf 'docker smoke: building %s\n' "$IMAGE"
IMAGE_TAGGED=1
if ! docker build --tag "$IMAGE" "$ROOT_DIR"; then
	fail "docker build failed"
fi

printf 'docker smoke: starting real FUSE mount\n'
if ! docker run --detach --name "$CONTAINER" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--mount "type=bind,src=$DATA_HOST_DIR,dst=/data" \
	--mount "type=bind,src=$TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$MOUNT_HOST_DIR,dst=/mnt,bind-propagation=rshared" \
	"$IMAGE" -mountpoint /mnt -data-dir /data /torrents >/dev/null; then
	fail "could not start the FUSE container; check /dev/fuse, SYS_ADMIN, and AppArmor permissions"
fi
POSITIVE_STARTED=1
MNT_PROPAGATION="$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/mnt"}}{{.Propagation}}{{end}}{{end}}' "$CONTAINER" 2>/dev/null || true)"
[[ "$MNT_PROPAGATION" == "rshared" ]] || \
	fail "container /mnt mount propagation is ${MNT_PROPAGATION:-unknown}, expected rshared"
printf 'docker smoke: /mnt bind propagation verified (rshared)\n'

if ! docker run --detach --name "$HOST_OBSERVER_CONTAINER" \
	--entrypoint /bin/sh \
	--mount "type=bind,src=$MOUNT_HOST_DIR,dst=/host-mnt,bind-propagation=rslave" \
	"$IMAGE" -c 'sleep 300' >/dev/null; then
	fail "could not start the host mount observer"
fi
HOST_OBSERVER_STARTED=1

mount_deadline=$((SECONDS + 30))
while ((SECONDS < mount_deadline)); do
	state="$(docker inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null || true)"
	if [[ "$state" == "exited" || "$state" == "dead" ]]; then
		logs="$(docker logs "$CONTAINER" 2>&1 || true)"
		fail "FUSE container exited before exposing the fixture (state=$state):$'\n'$logs"
	fi
	if [[ "$state" == "running" ]] \
		&& docker exec "$CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1 \
		&& docker exec "$HOST_OBSERVER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done
if ! docker exec "$CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
	logs="$(docker logs "$CONTAINER" 2>&1 || true)"
	fail "timed out waiting for $MOUNTED_PAYLOAD:$'\n'$logs"
fi
if ! docker exec "$HOST_OBSERVER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
	fail "timed out waiting for $MOUNT_HOST_DIR/payload.txt"
fi

ACTUAL_HASH="$(docker exec "$CONTAINER" sha256sum "$MOUNTED_PAYLOAD")" || \
	fail "could not read the fixture through the FUSE mount"
ACTUAL_HASH="${ACTUAL_HASH%% *}"
[[ "$ACTUAL_HASH" == "$EXPECTED_HASH" ]] || \
	fail "mounted payload hash $ACTUAL_HASH does not match fixture hash $EXPECTED_HASH"
HOST_ACTUAL_HASH="$(docker exec "$HOST_OBSERVER_CONTAINER" sha256sum /host-mnt/payload.txt)" || \
	fail "could not read the fixture through the propagated host mount"
HOST_ACTUAL_HASH="${HOST_ACTUAL_HASH%% *}"
[[ "$HOST_ACTUAL_HASH" == "$EXPECTED_HASH" ]] || \
	fail "host mounted payload hash $HOST_ACTUAL_HASH does not match fixture hash $EXPECTED_HASH"
printf 'docker smoke: mounted payload verified in container and host (%s)\n' "$ACTUAL_HASH"

# The mount exposes torrent data only: the former control directories must be
# absent, and the legacy .stats directory must be untouched.
for control_path in /mnt/metadata /mnt/stats; do
	if docker exec "$CONTAINER" test -e "$control_path" >/dev/null 2>&1; then
		fail "$control_path exists in the mount; the data mount must expose data only"
	fi
done
for control_path in /host-mnt/metadata /host-mnt/stats; do
	if docker exec "$HOST_OBSERVER_CONTAINER" test -e "$control_path" >/dev/null 2>&1; then
		fail "$control_path exists in the propagated mount; the data mount must expose data only"
	fi
done
if ! docker exec "$CONTAINER" test -f "$LEGACY_STATS_MARKER" >/dev/null 2>&1; then
	fail "legacy $LEGACY_STATS_MARKER was removed or rewritten by startup"
fi
printf 'docker smoke: control paths absent and legacy .stats preserved\n'

HOST_WRITE_PROBE="/host-mnt/.write-probe"
if docker exec "$HOST_OBSERVER_CONTAINER" sh -c 'printf "write must fail\\n" > /host-mnt/.write-probe' >/dev/null 2>&1; then
	fail "host write to the FUSE mount unexpectedly succeeded"
fi
if docker exec "$HOST_OBSERVER_CONTAINER" test -e "$HOST_WRITE_PROBE" >/dev/null 2>&1; then
	fail "host write probe was created in the read-only FUSE mount"
fi
HOST_AFTER_WRITE_HASH="$(docker exec "$HOST_OBSERVER_CONTAINER" sha256sum /host-mnt/payload.txt)" || \
	fail "could not re-read the host payload after the write probe"
HOST_AFTER_WRITE_HASH="${HOST_AFTER_WRITE_HASH%% *}"
[[ "$HOST_AFTER_WRITE_HASH" == "$EXPECTED_HASH" ]] || \
	fail "host payload changed after rejected write: $HOST_AFTER_WRITE_HASH"
printf 'docker smoke: propagated host mount is read-only\n'

if ! bounded 30 "signal the FUSE container" docker kill --signal TERM "$CONTAINER" >/dev/null; then
	fail "could not send SIGTERM to the FUSE container"
fi
if ! STOP_STATUS="$(bounded "$DOCKER_OP_TIMEOUT" "wait for the FUSE container" docker wait "$CONTAINER")"; then
	collect_unmount_diagnostics "$CONTAINER" "$MOUNT_HOST_DIR"
	fail "FUSE container did not stop within ${DOCKER_OP_TIMEOUT}s after SIGTERM"
fi
[[ "$STOP_STATUS" == "0" ]] || fail "FUSE container stopped with exit code $STOP_STATUS"

POSITIVE_TEARDOWN_LOGS="$(container_logs "$CONTAINER")"
if [[ -n "$(unmount_busy_lines "$POSITIVE_TEARDOWN_LOGS")" ]]; then
	collect_unmount_diagnostics "$CONTAINER" "$MOUNT_HOST_DIR"
	fail "shutdown reported a daemon-owned unmount failure:$'\n'$POSITIVE_TEARDOWN_LOGS"
fi

unmount_deadline=$((SECONDS + 30))
while ((SECONDS < unmount_deadline)); do
	if host_mount_released "$MOUNT_HOST_DIR"; then
		break
	fi
	sleep 0.2
done
if ! host_mount_released "$MOUNT_HOST_DIR"; then
	collect_unmount_diagnostics "$CONTAINER" "$MOUNT_HOST_DIR"
	fail "propagated host mount did not disappear after the container stopped"
fi
if docker exec "$HOST_OBSERVER_CONTAINER" test -e /host-mnt/payload.txt >/dev/null 2>&1; then
	fail "host observer still sees the propagated FUSE payload after shutdown"
fi
if ! bounded 30 "remove the host mount observer" docker rm -f "$HOST_OBSERVER_CONTAINER" >/dev/null; then
	fail "could not remove the host mount observer"
fi
HOST_OBSERVER_STARTED=0
POSITIVE_STARTED=0
printf 'docker smoke: FUSE container stopped and host mount disappeared cleanly\n'

# Blocked-read shutdown scenario: no payload is preloaded and no peer exists,
# so a read through the container's own mount blocks on a missing piece. A
# SIGTERM must then still stop the process cleanly, without the daemon-owned
# unmount failure the unmount-first order produced.
printf 'docker smoke: starting blocked-read shutdown scenario\n'
BLOCKED_DATA_DIR="$TMP_DIR/blocked-data"
BLOCKED_MOUNT_DIR="$TMP_DIR/blocked-mnt"
mkdir -p "$BLOCKED_DATA_DIR" "$BLOCKED_MOUNT_DIR"
if ! docker run --detach --name "$BLOCKED_READ_CONTAINER" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--mount "type=bind,src=$BLOCKED_DATA_DIR,dst=/data" \
	--mount "type=bind,src=$TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$BLOCKED_MOUNT_DIR,dst=/mnt,bind-propagation=rshared" \
	"$IMAGE" -mountpoint /mnt -data-dir /data /torrents >/dev/null; then
	fail "could not start the blocked-read container"
fi
BLOCKED_PROPAGATION="$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/mnt"}}{{.Propagation}}{{end}}{{end}}' "$BLOCKED_READ_CONTAINER" 2>/dev/null || true)"
[[ "$BLOCKED_PROPAGATION" == "rshared" ]] || \
	fail "blocked-read /mnt propagation is ${BLOCKED_PROPAGATION:-unknown}, expected rshared"

blocked_deadline=$((SECONDS + 30))
while ((SECONDS < blocked_deadline)); do
	if docker exec "$BLOCKED_READ_CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
		break
	fi
	if [[ "$(docker inspect --format '{{.State.Status}}' "$BLOCKED_READ_CONTAINER" 2>/dev/null || true)" != "running" ]]; then
		fail "blocked-read container exited before exposing the fixture path"
	fi
	sleep 0.2
done
if ! docker exec "$BLOCKED_READ_CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
	collect_unmount_diagnostics "$BLOCKED_READ_CONTAINER" "$BLOCKED_MOUNT_DIR"
	fail "blocked-read container never exposed $MOUNTED_PAYLOAD"
fi

# Issue the read that must block on the missing piece. It is bounded and
# reclaimable, so a failure path cannot leave it running forever.
bounded 60 "blocked read" docker exec "$BLOCKED_READ_CONTAINER" \
	dd if="$MOUNTED_PAYLOAD" of=/dev/null bs=4096 count=1 >/dev/null 2>&1 &
BLOCKED_READ_PID=$!
sleep 2
if ! kill -0 "$BLOCKED_READ_PID" 2>/dev/null; then
	fail "the read returned instead of waiting on the missing piece; the scenario needs an outstanding read"
fi
printf 'docker smoke: a read is outstanding on the missing piece\n'

# Playback snapshot: while the read is outstanding, the reader must not be
# producing cancellation errors. This is the runtime half of the regression.
BLOCKED_PLAYBACK_LOGS="$(container_logs "$BLOCKED_READ_CONTAINER")"
PLAYBACK_CANCELS="$(reader_cancel_count "$BLOCKED_PLAYBACK_LOGS")"
[[ "$PLAYBACK_CANCELS" == "0" ]] || \
	fail "runtime produced $PLAYBACK_CANCELS anacrolix reader cancellation errors before shutdown:$'\n'$BLOCKED_PLAYBACK_LOGS"
printf 'docker smoke: runtime reader cancellation errors = %s\n' "$PLAYBACK_CANCELS"

if ! bounded 30 "signal the blocked-read container" docker kill --signal TERM "$BLOCKED_READ_CONTAINER" >/dev/null; then
	fail "could not send SIGTERM to the blocked-read container"
fi
if ! BLOCKED_STOP_STATUS="$(bounded "$DOCKER_OP_TIMEOUT" "wait for the blocked-read container" docker wait "$BLOCKED_READ_CONTAINER")"; then
	collect_unmount_diagnostics "$BLOCKED_READ_CONTAINER" "$BLOCKED_MOUNT_DIR"
	fail "blocked-read container did not stop within ${DOCKER_OP_TIMEOUT}s after SIGTERM"
fi
[[ "$BLOCKED_STOP_STATUS" == "0" ]] || \
	fail "blocked-read container stopped with exit code $BLOCKED_STOP_STATUS"

# Teardown cancellations are expected now and are recorded separately; they
# must never be merged into the playback figure above.
BLOCKED_TEARDOWN_LOGS="$(container_logs "$BLOCKED_READ_CONTAINER")"
printf 'docker smoke: blocked-read teardown reader cancellation errors = %s\n' \
	"$(reader_cancel_count "$BLOCKED_TEARDOWN_LOGS")"
if [[ -n "$(unmount_busy_lines "$BLOCKED_TEARDOWN_LOGS")" ]]; then
	collect_unmount_diagnostics "$BLOCKED_READ_CONTAINER" "$BLOCKED_MOUNT_DIR"
	fail "blocked-read shutdown reported a daemon-owned unmount failure:$'\n'$BLOCKED_TEARDOWN_LOGS"
fi

blocked_unmount_deadline=$((SECONDS + 30))
while ((SECONDS < blocked_unmount_deadline)); do
	if host_mount_released "$BLOCKED_MOUNT_DIR"; then
		break
	fi
	sleep 0.2
done
if ! host_mount_released "$BLOCKED_MOUNT_DIR"; then
	collect_unmount_diagnostics "$BLOCKED_READ_CONTAINER" "$BLOCKED_MOUNT_DIR"
	fail "blocked-read propagated host mount did not disappear after shutdown"
fi

# Reclaim the outstanding reader and the container.
if kill -0 "$BLOCKED_READ_PID" 2>/dev/null; then
	kill "$BLOCKED_READ_PID" 2>/dev/null || true
fi
wait "$BLOCKED_READ_PID" 2>/dev/null || true
if ! bounded 30 "remove the blocked-read container" docker rm -f "$BLOCKED_READ_CONTAINER" >/dev/null; then
	fail "could not remove the blocked-read container"
fi
printf 'docker smoke: blocked-read shutdown completed without a daemon-owned unmount failure\n'

printf 'docker smoke: checking rejected single-file input\n'
FILE_INPUT_CREATED=1
NEGATIVE_STATUS=0
NEGATIVE_OUTPUT=""
if NEGATIVE_OUTPUT="$(timeout --foreground 15s docker run --name "$FILE_INPUT_CONTAINER" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--mount "type=bind,src=$NEGATIVE_DATA_DIR,dst=/data" \
	--mount "type=bind,src=$TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$NEGATIVE_MOUNT_DIR,dst=/mnt" \
	"$IMAGE" -mountpoint /mnt -data-dir /data /torrents/example.torrent 2>&1)"; then
	NEGATIVE_STATUS=0
else
	NEGATIVE_STATUS=$?
fi
[[ "$NEGATIVE_STATUS" == "2" ]] || \
	fail "single-file input exited with $NEGATIVE_STATUS instead of 2:$'\n'$NEGATIVE_OUTPUT"
[[ "$NEGATIVE_OUTPUT" == *"/torrents/example.torrent"* ]] || \
	fail "single-file error omitted the container path:$'\n'$NEGATIVE_OUTPUT"
[[ "$NEGATIVE_OUTPUT" == *"existing directory"* ]] || \
	fail "single-file error did not require a directory:$'\n'$NEGATIVE_OUTPUT"
printf 'docker smoke: single-file input rejected accurately\n'

printf 'docker smoke: checking missing torrent directory failure\n'
MISSING_DIR_CREATED=1
NEGATIVE_STATUS=0
NEGATIVE_OUTPUT=""
if NEGATIVE_OUTPUT="$(timeout --foreground 15s docker run --name "$MISSING_DIR_CONTAINER" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--mount "type=bind,src=$NEGATIVE_DATA_DIR,dst=/data" \
	--mount "type=bind,src=$TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$NEGATIVE_MOUNT_DIR,dst=/mnt" \
	"$IMAGE" -mountpoint /mnt -data-dir /data /torrents/missing 2>&1)"; then
	NEGATIVE_STATUS=0
else
	NEGATIVE_STATUS=$?
fi
[[ "$NEGATIVE_STATUS" == "2" ]] || \
	fail "missing directory exited with $NEGATIVE_STATUS instead of 2:$'\n'$NEGATIVE_OUTPUT"
[[ "$NEGATIVE_OUTPUT" == *"/torrents/missing"* ]] || \
	fail "missing-directory error omitted the container path:$'\n'$NEGATIVE_OUTPUT"
[[ "$NEGATIVE_OUTPUT" == *"existing directory"* ]] || \
	fail "missing-directory error did not require a directory:$'\n'$NEGATIVE_OUTPUT"
[[ "$NEGATIVE_OUTPUT" == *"no such file or directory"* ]] || \
	fail "missing-directory error omitted the ENOENT text:$'\n'$NEGATIVE_OUTPUT"
printf 'docker smoke: missing directory rejected accurately\n'

printf 'docker smoke: all checks passed\n'
