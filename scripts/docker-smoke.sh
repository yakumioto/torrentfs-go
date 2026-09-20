#!/usr/bin/env bash
set -Eeuo pipefail

readonly SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly FIXTURE_DIR="$ROOT_DIR/examples/docker"
readonly FIXTURE_TORRENT="$FIXTURE_DIR/example.torrent"
readonly FIXTURE_PAYLOAD="$FIXTURE_DIR/data/payload.txt"
# Info hash of examples/docker/example.torrent. Pieces live only in the
# in-memory cache now, so the fixture cannot be preloaded on disk. Instead the
# data scenarios are fed by a BEP 19 web seed: the fixture payload is served
# over plain HTTP and the torrent handed to those scenarios carries a url-list
# pointing at it. anacrolix downloads the piece over HTTP and writes it into the
# cache through the same storage path a BitTorrent peer would. Adding url-list
# is a top-level key, so the info dict and therefore this hash are unchanged.
readonly FIXTURE_INFO_HASH="dd45d0108ac27c3c0fb1d7b28fd2b313c753bc9b"
readonly MOUNTED_PAYLOAD="/mnt/payload.txt"
readonly LEGACY_STATS_MARKER="/torrents/.stats/leftover.txt"
readonly IMAGE="torrentfs-mio17-smoke:${BASHPID}"
readonly CONTAINER="torrentfs-mio17-${BASHPID}"
readonly FILE_INPUT_CONTAINER="torrentfs-mio17-file-input-${BASHPID}"
readonly MISSING_DIR_CONTAINER="torrentfs-mio17-missing-dir-${BASHPID}"
readonly HOST_OBSERVER_CONTAINER="torrentfs-mio17-host-observer-${BASHPID}"
readonly BLOCKED_READ_CONTAINER="torrentfs-mio17-blocked-${BASHPID}"
readonly PEER_DAEMON_CONTAINER="torrentfs-mio17-peer-ns-${BASHPID}"
readonly PEER_HOLDER_CONTAINER="torrentfs-mio17-peer-holder-${BASHPID}"

# Every Docker call is bounded: a wedged daemon or an unmount that waits forever
# must fail the smoke instead of hanging it. The positive scenario's bound is
# meaningful because the propagation peer this script creates itself (the host
# observer container) is reclaimed before SIGTERM: a mount namespace still
# holding a propagated copy would otherwise keep Server.Unmount waiting past any
# bound.
#
# DOCKER_OP_TIMEOUT covers operations that may legitimately take a while
# (starting or stopping a container). PROBE_TIMEOUT covers the short queries —
# container state, an in-container test, a fixture hash read — including every
# readiness poll, which must come back as a bounded "not ready yet" instead of
# hanging. DOCKER_BUILD_TIMEOUT is deliberately generous: it exists to catch a
# wedged daemon during an image build, not to bound how long a cold build takes.
readonly DOCKER_OP_TIMEOUT=60
readonly PROBE_TIMEOUT=10
readonly DOCKER_BUILD_TIMEOUT=1800

# The anacrolix reader errors that a cancellation storm produces. Playback
# (before shutdown) must never emit them; teardown cancellations are counted
# separately and are not merged into the runtime figure.
readonly READER_CANCEL_PATTERN='msg="(initial read failed|read failed after reader reset|read failed after completion resync)" err="context canceled"'
readonly UNMOUNT_BUSY_PATTERN='failed to unmount .*Device or resource busy'

# The peer-namespace scenario measures the bounded unmount stage against its
# configured deadline. UNMOUNT_TIMEOUT_SECONDS mirrors defaultUnmountTimeout in
# cmd/torrentfs/main.go and UNMOUNT_STOP_MARGIN covers the rest of the shutdown
# sequence; keep their sum below DOCKER_OP_TIMEOUT so "stopped late" and "never
# stopped" stay distinguishable. UNMOUNT_TIMEOUT_DIAGNOSTIC is the stable
# fragment of the diagnostic the daemon must print before exiting non-zero.
readonly UNMOUNT_TIMEOUT_SECONDS=30
readonly UNMOUNT_STOP_MARGIN=20
readonly UNMOUNT_TIMEOUT_DIAGNOSTIC='unmount did not return within'

TMP_DIR=""
WEBSEED_PID=""
IMAGE_TAGGED=0
POSITIVE_STARTED=0
HOST_OBSERVER_STARTED=0
FILE_INPUT_CREATED=0
MISSING_DIR_CREATED=0
# Background readers of the blocked-read scenario, reclaimed on every path.
BLOCKED_READ_PIDS=()

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

# probe runs one short Docker query under PROBE_TIMEOUT. An unresponsive daemon
# has to surface here as a bounded failure that the enclosing loop or assertion
# reports with its own diagnostics; it must never stall the script.
probe() {
	local description="$1"
	shift
	bounded "$PROBE_TIMEOUT" "$description" "$@"
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

# build_seeded_torrent writes a copy of src with a top-level url-list, so the
# torrent keeps its info hash but gains a web seed. bencode keys are sorted, and
# "info" sorts before "url-list", so appending before the closing "e" keeps the
# dictionary canonical.
build_seeded_torrent() {
	local src="$1" dst="$2" url="$3"
	{ head -c -1 -- "$src"; printf '8:url-list%d:%se' "${#url}" "$url"; } > "$dst"
}

# webseed_fetch fetches a path under the running web seed and prints it, so
# readiness and the served bytes are checked through the same HTTP path
# anacrolix will use.
webseed_fetch() {
	local path="$1"
	python3 -c 'import sys, urllib.request; sys.stdout.write(urllib.request.urlopen(sys.argv[1], timeout=5).read().decode())' "http://127.0.0.1:${WEBSEED_PORT}/${path}"
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

	# Reclaim any outstanding blocked-read reader, success or failure path.
	if (( ${#BLOCKED_READ_PIDS[@]} )); then
		for blocked_pid in "${BLOCKED_READ_PIDS[@]}"; do
			kill "$blocked_pid" 2>/dev/null || true
		done
	fi

	# Propagation peers go first. A namespace that still holds a copy of a FUSE
	# mount keeps that daemon's Server.Unmount blocked, so waiting on the daemon
	# before reclaiming its peers would spend the whole docker-wait bound on a
	# state that removing the peer resolves at once. Removing the peer first is
	# what keeps this teardown bounded by reality and not just by the timeout.
	for container in "$PEER_HOLDER_CONTAINER" "$HOST_OBSERVER_CONTAINER"; do
		[[ -n "$container" ]] || continue
		bounded 30 "docker rm -f $container" docker rm -f "$container" >/dev/null 2>&1 || true
	done

	# Every teardown step is bounded. Forced kill/remove is a last resort for
	# residue; it never changes the recorded exit status, so a forced cleanup
	# cannot turn a failed run green.
	for container in "$CONTAINER" "$PEER_DAEMON_CONTAINER" "$BLOCKED_READ_CONTAINER" \
		"$FILE_INPUT_CONTAINER" "$MISSING_DIR_CONTAINER"; do
		[[ -n "$container" ]] || continue
		bounded 30 "docker kill $container" docker kill --signal TERM "$container" >/dev/null 2>&1 || true
		bounded 30 "docker wait $container" docker wait "$container" >/dev/null 2>&1 || true
		bounded 30 "docker kill -9 $container" docker kill --signal KILL "$container" >/dev/null 2>&1 || true
		bounded 30 "docker rm -f $container" docker rm -f "$container" >/dev/null 2>&1 || true
	done
	if [[ -n "$WEBSEED_PID" ]]; then
		kill "$WEBSEED_PID" 2>/dev/null || true
		wait "$WEBSEED_PID" 2>/dev/null || true
		WEBSEED_PID=""
	fi
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

for command_name in docker findmnt sha256sum timeout python3; do
	command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done
[[ -e /dev/fuse ]] || fail "FUSE prerequisite missing: /dev/fuse is not available"
probe "docker info" docker info >/dev/null 2>&1 || fail "Docker daemon is unavailable"
[[ -f "$FIXTURE_TORRENT" ]] || fail "fixture is missing: $FIXTURE_TORRENT"
[[ -f "$FIXTURE_PAYLOAD" ]] || fail "fixture payload is missing: $FIXTURE_PAYLOAD"

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/torrentfs-mio17.XXXXXX")"
TORRENT_HOST_DIR="$TMP_DIR/torrents"
MOUNT_HOST_DIR="$TMP_DIR/mnt"
NEGATIVE_MOUNT_DIR="$TMP_DIR/negative-mnt"
mkdir -p "$TORRENT_HOST_DIR" "$MOUNT_HOST_DIR" "$NEGATIVE_MOUNT_DIR"
SEEDED_TORRENT_HOST_DIR="$TMP_DIR/seeded-torrents"
WEBSEED_DIR="$TMP_DIR/webseed"
mkdir -p "$SEEDED_TORRENT_HOST_DIR" "$WEBSEED_DIR"
HOST_PROPAGATION="$(findmnt -T "$MOUNT_HOST_DIR" -n -o PROPAGATION 2>/dev/null || true)"
[[ "$HOST_PROPAGATION" == *shared* ]] || \
	fail "host mount containing $MOUNT_HOST_DIR must use shared propagation (current: ${HOST_PROPAGATION:-unknown})"
# The plain fixture (no web seed) feeds the scenarios that need a torrent with
# no reachable data source: blocked-read and the single-file input check.
cp -- "$FIXTURE_TORRENT" "$TORRENT_HOST_DIR/example.torrent"
EXPECTED_HASH="$(sha256sum "$FIXTURE_PAYLOAD")"
EXPECTED_HASH="${EXPECTED_HASH%% *}"

# Serve the fixture payload and hand the data scenarios a web-seeded torrent.
cp -- "$FIXTURE_PAYLOAD" "$WEBSEED_DIR/payload.txt"
# The positive scenario mounts the seeded torrents directory, so the legacy
# .stats marker it asserts on must exist there too.
mkdir -p "$SEEDED_TORRENT_HOST_DIR/.stats"
printf 'legacy\n' > "$SEEDED_TORRENT_HOST_DIR/.stats/leftover.txt"
WEBSEED_PORT="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
[[ -n "$WEBSEED_PORT" ]] || fail "could not allocate a web seed port"
python3 -m http.server "$WEBSEED_PORT" --bind 127.0.0.1 --directory "$WEBSEED_DIR" \
	>"$TMP_DIR/webseed.log" 2>&1 &
WEBSEED_PID=$!
webseed_deadline=$((SECONDS + 15))
while ((SECONDS < webseed_deadline)); do
	if [[ "$(webseed_fetch payload.txt 2>/dev/null || true)" == "$(cat "$WEBSEED_DIR/payload.txt")" ]]; then
		break
	fi
	sleep 0.1
done
[[ "$(webseed_fetch payload.txt 2>/dev/null || true)" == "$(cat "$WEBSEED_DIR/payload.txt")" ]] || \
	fail "web seed never served $WEBSEED_DIR/payload.txt (see $TMP_DIR/webseed.log)"
build_seeded_torrent "$FIXTURE_TORRENT" "$SEEDED_TORRENT_HOST_DIR/example.torrent" "http://127.0.0.1:$WEBSEED_PORT/"
printf 'docker smoke: web seed serving the fixture on 127.0.0.1:%s\n' "$WEBSEED_PORT"

# A legacy empty .stats directory must survive startup untouched: the new
# layout neither creates nor removes it.
mkdir -p "$TORRENT_HOST_DIR/.stats"
printf 'legacy\n' > "$TORRENT_HOST_DIR/.stats/leftover.txt"

printf 'docker smoke: building %s\n' "$IMAGE"
IMAGE_TAGGED=1
if ! bounded "$DOCKER_BUILD_TIMEOUT" "docker build" docker build --tag "$IMAGE" "$ROOT_DIR"; then
	fail "docker build failed"
fi

printf 'docker smoke: starting real FUSE mount\n'
if ! bounded "$DOCKER_OP_TIMEOUT" "start the FUSE container" docker run --detach --name "$CONTAINER" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--network host \
	--env TORRENTFS_HTTP_LISTEN_ADDR= \
	--mount "type=bind,src=$SEEDED_TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$MOUNT_HOST_DIR,dst=/mnt,bind-propagation=rshared" \
	"$IMAGE" -mountpoint /mnt /torrents >/dev/null; then
	fail "could not start the FUSE container; check /dev/fuse, SYS_ADMIN, and AppArmor permissions"
fi
POSITIVE_STARTED=1
MNT_PROPAGATION="$(probe "read /mnt propagation" docker inspect --format '{{range .Mounts}}{{if eq .Destination "/mnt"}}{{.Propagation}}{{end}}{{end}}' "$CONTAINER" 2>/dev/null || true)"
[[ "$MNT_PROPAGATION" == "rshared" ]] || \
	fail "container /mnt mount propagation is ${MNT_PROPAGATION:-unknown}, expected rshared"
printf 'docker smoke: /mnt bind propagation verified (rshared)\n'

if ! bounded "$DOCKER_OP_TIMEOUT" "start the host mount observer" docker run --detach --name "$HOST_OBSERVER_CONTAINER" \
	--entrypoint /bin/sh \
	--mount "type=bind,src=$MOUNT_HOST_DIR,dst=/host-mnt,bind-propagation=rslave" \
	"$IMAGE" -c 'sleep 300' >/dev/null; then
	fail "could not start the host mount observer"
fi
HOST_OBSERVER_STARTED=1

mount_deadline=$((SECONDS + 30))
while ((SECONDS < mount_deadline)); do
	state="$(probe "check FUSE container state" docker inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null || true)"
	if [[ "$state" == "exited" || "$state" == "dead" ]]; then
		logs="$(container_logs "$CONTAINER")"
		fail "FUSE container exited before exposing the fixture (state=$state):$'\n'$logs"
	fi
	if [[ "$state" == "running" ]] \
		&& probe "expose fixture in the FUSE container" docker exec "$CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1 \
		&& probe "expose fixture in the propagation peer" docker exec "$HOST_OBSERVER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done
if ! probe "expose fixture in the FUSE container" docker exec "$CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
	logs="$(container_logs "$CONTAINER")"
	fail "timed out waiting for $MOUNTED_PAYLOAD:$'\n'$logs"
fi
if ! probe "expose fixture in the propagation peer" docker exec "$HOST_OBSERVER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
	fail "timed out waiting for $MOUNT_HOST_DIR/payload.txt"
fi

ACTUAL_HASH="$(probe "read the fixture through the FUSE mount" docker exec "$CONTAINER" sha256sum "$MOUNTED_PAYLOAD")" || \
	fail "could not read the fixture through the FUSE mount"
ACTUAL_HASH="${ACTUAL_HASH%% *}"
[[ "$ACTUAL_HASH" == "$EXPECTED_HASH" ]] || \
	fail "mounted payload hash $ACTUAL_HASH does not match fixture hash $EXPECTED_HASH"
HOST_ACTUAL_HASH="$(probe "read the fixture through the propagated host mount" docker exec "$HOST_OBSERVER_CONTAINER" sha256sum /host-mnt/payload.txt)" || \
	fail "could not read the fixture through the propagated host mount"
HOST_ACTUAL_HASH="${HOST_ACTUAL_HASH%% *}"
[[ "$HOST_ACTUAL_HASH" == "$EXPECTED_HASH" ]] || \
	fail "host mounted payload hash $HOST_ACTUAL_HASH does not match fixture hash $EXPECTED_HASH"
printf 'docker smoke: mounted payload verified in container and host (%s)\n' "$ACTUAL_HASH"

# The mount exposes torrent data only: the former control directories must be
# absent, and the legacy .stats directory must be untouched.
for control_path in /mnt/metadata /mnt/stats; do
	if probe "check for $control_path" docker exec "$CONTAINER" test -e "$control_path" >/dev/null 2>&1; then
		fail "$control_path exists in the mount; the data mount must expose data only"
	fi
done
for control_path in /host-mnt/metadata /host-mnt/stats; do
	if probe "check for $control_path" docker exec "$HOST_OBSERVER_CONTAINER" test -e "$control_path" >/dev/null 2>&1; then
		fail "$control_path exists in the propagated mount; the data mount must expose data only"
	fi
done
if ! probe "check the legacy .stats marker" docker exec "$CONTAINER" test -f "$LEGACY_STATS_MARKER" >/dev/null 2>&1; then
	fail "legacy $LEGACY_STATS_MARKER was removed or rewritten by startup"
fi
printf 'docker smoke: control paths absent and legacy .stats preserved\n'

HOST_WRITE_PROBE="/host-mnt/.write-probe"
if probe "reject a host write" docker exec "$HOST_OBSERVER_CONTAINER" sh -c 'printf "write must fail\\n" > /host-mnt/.write-probe' >/dev/null 2>&1; then
	fail "host write to the FUSE mount unexpectedly succeeded"
fi
if probe "check the write probe" docker exec "$HOST_OBSERVER_CONTAINER" test -e "$HOST_WRITE_PROBE" >/dev/null 2>&1; then
	fail "host write probe was created in the read-only FUSE mount"
fi
HOST_AFTER_WRITE_HASH="$(probe "re-read the host payload" docker exec "$HOST_OBSERVER_CONTAINER" sha256sum /host-mnt/payload.txt)" || \
	fail "could not re-read the host payload after the write probe"
HOST_AFTER_WRITE_HASH="${HOST_AFTER_WRITE_HASH%% *}"
[[ "$HOST_AFTER_WRITE_HASH" == "$EXPECTED_HASH" ]] || \
	fail "host payload changed after rejected write: $HOST_AFTER_WRITE_HASH"
printf 'docker smoke: propagated host mount is read-only\n'

# Reclaim the observer before SIGTERM. It is a propagation peer this script
# created itself, and while its mount namespace holds a copy of the FUSE mount
# the daemon's Server.Unmount waits for that namespace to release it. Every
# cross-namespace check it exists for has already run above; the authoritative
# "the host propagated mount is gone" assertion below is host-side and does not
# go through the observer.
if ! bounded 30 "remove the host mount observer" docker rm -f "$HOST_OBSERVER_CONTAINER" >/dev/null; then
	fail "could not remove the host mount observer before shutdown"
fi
HOST_OBSERVER_STARTED=0
printf 'docker smoke: observer reclaimed before shutdown so it cannot act as a propagation peer\n'

if ! bounded 30 "signal the FUSE container" docker kill --signal TERM "$CONTAINER" >/dev/null; then
	fail "could not send SIGTERM to the FUSE container"
fi
if ! STOP_STATUS="$(bounded "$DOCKER_OP_TIMEOUT" "wait for the FUSE container" docker wait "$CONTAINER")"; then
	collect_unmount_diagnostics "$CONTAINER" "$MOUNT_HOST_DIR"
	fail "FUSE container did not stop within ${DOCKER_OP_TIMEOUT}s after SIGTERM; another mount namespace may still hold a propagated copy of $MOUNT_HOST_DIR"
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
POSITIVE_STARTED=0
printf 'docker smoke: FUSE container stopped and host mount disappeared cleanly\n'

# Blocked-read shutdown scenario: no payload is preloaded and no peer exists,
# so reads through the container's own mount block on a missing piece. Two
# overlapping logical readers are left outstanding and a SIGTERM must then
# still stop the process cleanly, without the daemon-owned unmount failure the
# unmount-first order produced, releasing both readers.
#
# Scope note (measured, not assumed): the fixture is a single 31-byte file, so
# the whole file lives in one page and the kernel collapses the two same-page
# reads into one in-flight FUSE request before the daemon sees them. An
# instrumented pre-fix build logged exactly one loader entry and zero
# cancellations for two overlapping readers, so this layer cannot discriminate
# the cancellation fix. Runtime cancellation evidence therefore comes from the
# Go/FUSE suite (TestFuseIncompleteOverlapReadsShareOneLoader, which reads two
# pieces in different pages and asserts a zero playback-phase cancellation
# count); this scenario covers the shutdown and EBUSY half.
printf 'docker smoke: starting blocked-read shutdown scenario\n'
BLOCKED_MOUNT_DIR="$TMP_DIR/blocked-mnt"
mkdir -p "$BLOCKED_MOUNT_DIR"
if ! bounded "$DOCKER_OP_TIMEOUT" "start the blocked-read container" docker run --detach --name "$BLOCKED_READ_CONTAINER" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--mount "type=bind,src=$TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$BLOCKED_MOUNT_DIR,dst=/mnt,bind-propagation=rshared" \
	"$IMAGE" -mountpoint /mnt /torrents >/dev/null; then
	fail "could not start the blocked-read container"
fi
BLOCKED_PROPAGATION="$(probe "read /mnt propagation" docker inspect --format '{{range .Mounts}}{{if eq .Destination "/mnt"}}{{.Propagation}}{{end}}{{end}}' "$BLOCKED_READ_CONTAINER" 2>/dev/null || true)"
[[ "$BLOCKED_PROPAGATION" == "rshared" ]] || \
	fail "blocked-read /mnt propagation is ${BLOCKED_PROPAGATION:-unknown}, expected rshared"

blocked_deadline=$((SECONDS + 30))
while ((SECONDS < blocked_deadline)); do
	if probe "expose fixture in the blocked-read container" docker exec "$BLOCKED_READ_CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
		break
	fi
	# Only a terminal state is a failure. A probe that times out reports an
	# empty state, and that must stay "not ready yet" so the loop's own deadline
	# reports it instead of this check misdiagnosing an unresponsive daemon as a
	# container that exited.
	blocked_state="$(probe "check blocked-read container state" docker inspect --format '{{.State.Status}}' "$BLOCKED_READ_CONTAINER" 2>/dev/null || true)"
	if [[ "$blocked_state" == "exited" || "$blocked_state" == "dead" ]]; then
		fail "blocked-read container exited before exposing the fixture path"
	fi
	sleep 0.2
done
if ! probe "expose fixture in the blocked-read container" docker exec "$BLOCKED_READ_CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
	collect_unmount_diagnostics "$BLOCKED_READ_CONTAINER" "$BLOCKED_MOUNT_DIR"
	fail "blocked-read container never exposed $MOUNTED_PAYLOAD"
fi

# Issue two overlapping reads that must both block on the same missing piece.
# They read different, non-overlapping offsets of the 31-byte fixture, so both
# stay inside the file and inside one piece; a read past EOF would return
# immediately and prove nothing. Both are bounded and reclaimable.
BLOCKED_READ_PIDS=()
bounded 60 "first blocked read" docker exec "$BLOCKED_READ_CONTAINER" \
	dd if="$MOUNTED_PAYLOAD" of=/dev/null bs=8 skip=0 count=1 >/dev/null 2>&1 &
BLOCKED_READ_PIDS+=("$!")
# Let the first request reach the loader before the overlapping one arrives.
sleep 1
bounded 60 "second blocked read" docker exec "$BLOCKED_READ_CONTAINER" \
	dd if="$MOUNTED_PAYLOAD" of=/dev/null bs=8 skip=1 count=1 >/dev/null 2>&1 &
BLOCKED_READ_PIDS+=("$!")
sleep 2

# Scenario validity: the readers must still be waiting on the missing piece,
# otherwise the shutdown below would be exercised with nothing outstanding.
# This is not the cancellation discriminator (see the scope note above).
for index in "${!BLOCKED_READ_PIDS[@]}"; do
	read_pid="${BLOCKED_READ_PIDS[$index]}"
	if ! kill -0 "$read_pid" 2>/dev/null; then
		wait "$read_pid" 2>/dev/null || true
		fail "reader $((index + 1)) returned before SIGTERM instead of waiting on the missing piece; the scenario needs an outstanding read"
	fi
done
printf 'docker smoke: %d readers are outstanding on the missing piece\n' "${#BLOCKED_READ_PIDS[@]}"

# The running phase must be free of reader cancellation errors. On this fixture
# the pre-fix build also reports zero here, so this is a health check on the
# running phase, not proof of the fix; the pre-fix failure this scenario
# produces is the shutdown half asserted below.
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

# Reclaim both outstanding readers and the container.
for read_pid in "${BLOCKED_READ_PIDS[@]}"; do
	if kill -0 "$read_pid" 2>/dev/null; then
		kill "$read_pid" 2>/dev/null || true
	fi
	wait "$read_pid" 2>/dev/null || true
done
BLOCKED_READ_PIDS=()
if ! bounded 30 "remove the blocked-read container" docker rm -f "$BLOCKED_READ_CONTAINER" >/dev/null; then
	fail "could not remove the blocked-read container"
fi
printf 'docker smoke: blocked-read shutdown completed without a daemon-owned unmount failure\n'

# Peer mount namespace scenario: a second mount namespace holds a propagated
# copy of the FUSE mount, so the FUSE connection never reaches ENODEV and
# Server.Unmount parks in its event-loop Wait. This is a different cause from
# both gaps above and it must not produce an EBUSY line: the unmount of the
# daemon's own copy succeeds, only the connection outlives it. The daemon has to
# give up at its own deadline, say why, and exit non-zero instead of hanging
# until the peer goes away. Needs rootful Docker, /dev/fuse, SYS_ADMIN, and bind
# propagation, so it runs here and not in the CI gate.
printf 'docker smoke: starting peer mount namespace shutdown scenario\n'
PEER_MOUNT_DIR="$TMP_DIR/peer-mnt"
mkdir -p "$PEER_MOUNT_DIR"
# The peer daemon is fed by the web seed so a read cannot block on a missing
# piece; the outstanding-request cause belongs to the blocked-read scenario,
# not this one.
if ! bounded "$DOCKER_OP_TIMEOUT" "start the peer-namespace container" docker run --detach --name "$PEER_DAEMON_CONTAINER" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--network host \
	--env TORRENTFS_HTTP_LISTEN_ADDR= \
	--mount "type=bind,src=$SEEDED_TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$PEER_MOUNT_DIR,dst=/mnt,bind-propagation=rshared" \
	"$IMAGE" -mountpoint /mnt /torrents >/dev/null; then
	fail "could not start the peer-namespace container"
fi
PEER_PROPAGATION="$(probe "read /mnt propagation" docker inspect --format '{{range .Mounts}}{{if eq .Destination "/mnt"}}{{.Propagation}}{{end}}{{end}}' "$PEER_DAEMON_CONTAINER" 2>/dev/null || true)"
[[ "$PEER_PROPAGATION" == "rshared" ]] || \
	fail "peer-namespace /mnt propagation is ${PEER_PROPAGATION:-unknown}, expected rshared"

if ! bounded "$DOCKER_OP_TIMEOUT" "start the peer mount namespace holder" docker run --detach --name "$PEER_HOLDER_CONTAINER" \
	--entrypoint /bin/sh \
	--mount "type=bind,src=$PEER_MOUNT_DIR,dst=/host-mnt,bind-propagation=rslave" \
	"$IMAGE" -c 'sleep 300' >/dev/null; then
	fail "could not start the peer mount namespace holder"
fi

peer_mount_deadline=$((SECONDS + 30))
while ((SECONDS < peer_mount_deadline)); do
	peer_state="$(probe "check peer-namespace container state" docker inspect --format '{{.State.Status}}' "$PEER_DAEMON_CONTAINER" 2>/dev/null || true)"
	if [[ "$peer_state" == "exited" || "$peer_state" == "dead" ]]; then
		collect_unmount_diagnostics "$PEER_DAEMON_CONTAINER" "$PEER_MOUNT_DIR"
		fail "peer-namespace container exited before exposing the fixture (state=$peer_state)"
	fi
	if probe "expose fixture in the peer mount namespace" docker exec "$PEER_HOLDER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done
# Scenario validity: without a real copy in the peer namespace the shutdown
# below would be exercised against nothing.
if ! probe "expose fixture in the peer mount namespace" docker exec "$PEER_HOLDER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
	collect_unmount_diagnostics "$PEER_DAEMON_CONTAINER" "$PEER_MOUNT_DIR"
	fail "the peer mount namespace never received a propagated copy of $PEER_MOUNT_DIR/payload.txt"
fi
PEER_HASH="$(probe "read through the peer mount namespace" \
	docker exec "$PEER_HOLDER_CONTAINER" sha256sum /host-mnt/payload.txt)" || \
	fail "could not read the fixture through the peer mount namespace"
PEER_HASH="${PEER_HASH%% *}"
[[ "$PEER_HASH" == "$EXPECTED_HASH" ]] || \
	fail "peer namespace payload hash $PEER_HASH does not match fixture hash $EXPECTED_HASH"
printf 'docker smoke: peer mount namespace holds a propagated read-only copy of the FUSE mount (%s)\n' "$PEER_HASH"

PEER_SHUTDOWN_START=$SECONDS
if ! bounded 30 "signal the peer-namespace container" docker kill --signal TERM "$PEER_DAEMON_CONTAINER" >/dev/null; then
	fail "could not send SIGTERM to the peer-namespace container"
fi
if ! PEER_STOP_STATUS="$(bounded "$DOCKER_OP_TIMEOUT" "wait for the peer-namespace container" docker wait "$PEER_DAEMON_CONTAINER")"; then
	collect_unmount_diagnostics "$PEER_DAEMON_CONTAINER" "$PEER_MOUNT_DIR"
	fail "the peer-namespace container did not stop within ${DOCKER_OP_TIMEOUT}s after SIGTERM; the bounded unmount stage did not take effect"
fi
PEER_SHUTDOWN_SECONDS=$((SECONDS - PEER_SHUTDOWN_START))
PEER_STOP_BOUND=$((UNMOUNT_TIMEOUT_SECONDS + UNMOUNT_STOP_MARGIN))
(( PEER_SHUTDOWN_SECONDS <= PEER_STOP_BOUND )) || \
	fail "the peer-namespace container stopped after ${PEER_SHUTDOWN_SECONDS}s, want at most ${PEER_STOP_BOUND}s (unmount deadline ${UNMOUNT_TIMEOUT_SECONDS}s plus margin)"
# The deadline is reported as a runtime failure: exiting 0 would tell an
# orchestrator the mount was released when the peer still holds a copy.
[[ "$PEER_STOP_STATUS" == "1" ]] || \
	fail "the peer-namespace container exited with $PEER_STOP_STATUS, want 1 (the unmount deadline must be reported as a runtime failure)"
PEER_TEARDOWN_LOGS="$(container_logs "$PEER_DAEMON_CONTAINER")"
[[ "$PEER_TEARDOWN_LOGS" == *"$UNMOUNT_TIMEOUT_DIAGNOSTIC"* ]] || \
	fail "the peer-namespace shutdown omitted the unmount timeout diagnostic:$'\n'$PEER_TEARDOWN_LOGS"
if [[ -n "$(unmount_busy_lines "$PEER_TEARDOWN_LOGS")" ]]; then
	collect_unmount_diagnostics "$PEER_DAEMON_CONTAINER" "$PEER_MOUNT_DIR"
	fail "the peer-namespace shutdown reported a daemon-owned unmount failure; this scenario must reproduce the peer-namespace cause and not the outstanding-request one:$'\n'$PEER_TEARDOWN_LOGS"
fi
printf 'docker smoke: peer-namespace shutdown stopped in %ss with exit code %s and a timeout diagnostic\n' \
	"$PEER_SHUTDOWN_SECONDS" "$PEER_STOP_STATUS"

if ! bounded 30 "remove the peer mount namespace holder" docker rm -f "$PEER_HOLDER_CONTAINER" >/dev/null; then
	fail "could not remove the peer mount namespace holder"
fi
peer_release_deadline=$((SECONDS + 30))
while ((SECONDS < peer_release_deadline)); do
	if host_mount_released "$PEER_MOUNT_DIR"; then
		break
	fi
	sleep 0.2
done
if ! host_mount_released "$PEER_MOUNT_DIR"; then
	collect_unmount_diagnostics "$PEER_DAEMON_CONTAINER" "$PEER_MOUNT_DIR"
	fail "the propagated host mount under $PEER_MOUNT_DIR outlived the peer namespace and the stopped daemon"
fi
if ! bounded 30 "remove the peer-namespace container" docker rm -f "$PEER_DAEMON_CONTAINER" >/dev/null; then
	fail "could not remove the peer-namespace container"
fi
printf 'docker smoke: peer namespace reclamation released the propagated host mount\n'

printf 'docker smoke: checking rejected single-file input\n'
FILE_INPUT_CREATED=1
NEGATIVE_STATUS=0
NEGATIVE_OUTPUT=""
if NEGATIVE_OUTPUT="$(bounded 15 "run the single-file input check" docker run --name "$FILE_INPUT_CONTAINER" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--mount "type=bind,src=$TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$NEGATIVE_MOUNT_DIR,dst=/mnt" \
	"$IMAGE" -mountpoint /mnt /torrents/example.torrent 2>&1)"; then
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
if NEGATIVE_OUTPUT="$(bounded 15 "run the missing-directory check" docker run --name "$MISSING_DIR_CONTAINER" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--mount "type=bind,src=$TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$NEGATIVE_MOUNT_DIR,dst=/mnt" \
	"$IMAGE" -mountpoint /mnt /torrents/missing 2>&1)"; then
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
