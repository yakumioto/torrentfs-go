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
readonly SUBTITLE_CONTAINER="torrentfs-mio17-subtitle-${BASHPID}"
readonly SUBTITLE_OBSERVER_CONTAINER="torrentfs-mio17-subtitle-observer-${BASHPID}"
# The bcrypt hash of "password", matching the API tests. The subtitle scenario
# needs authentication enabled, and auth credentials are TOML-only.
readonly SUBTITLE_PASSWORD_HASH='$2a$10$N9qo8uLOickgx2ZMRZoMye8fOsiTWZqYtkxvXkKm8BMzjT7t/vIdq'
readonly SUBTITLE_VIDEO_NAME="video.mkv"
readonly SUBTITLE_VIDEO_STEM="video"
readonly PEER_HOLDER_CONTAINER="torrentfs-mio17-peer-holder-${BASHPID}"
readonly HOST_UID="$(id -u)"
readonly HOST_GID="$(id -g)"
if (( HOST_UID == 0 || HOST_GID == 0 )); then
	readonly RUNTIME_UID=1500
	readonly RUNTIME_GID=1500
else
	readonly RUNTIME_UID="$HOST_UID"
	readonly RUNTIME_GID="$HOST_GID"
fi

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

(( RUNTIME_UID != 0 && RUNTIME_GID != 0 )) || fail "docker smoke requires non-root runtime UID/GID"

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

process_identity() {
	local container="$1" process_name="$2"
	probe "read $process_name credentials" docker exec "$container" /bin/sh -c '
		wanted="$1"
		for status in /proc/[0-9]*/status; do
			[ -r "$status" ] || continue
			name="$(grep "^Name:" "$status" | cut -d: -f2 | tr -d "[:space:]")"
			[ "$name" = "$wanted" ] || continue
			uid_line="$(grep "^Uid:" "$status")"
			gid_line="$(grep "^Gid:" "$status")"
			uid_line="${uid_line#Uid:}"
			gid_line="${gid_line#Gid:}"
			set -- $uid_line
			uid="$1:$2:$3:$4"
			set -- $gid_line
			gid="$1:$2:$3:$4"
			pid="${status#/proc/}"
			pid="${pid%/status}"
			printf "%s %s %s\\n" "$pid" "$uid" "$gid"
			exit 0
		done
		exit 1
	' sh "$process_name"
}

wait_process_identity() {
	local container="$1" process_name="$2" output
	for _ in {1..60}; do
		if output="$(process_identity "$container" "$process_name" 2>/dev/null)"; then
			printf '%s\n' "$output"
			return 0
		fi
		sleep 0.2
	done
	return 1
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

write_registry_fixture() {
	local torrents_dir="$1" now
	now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	mkdir -p "$torrents_dir/.metadata/state" "$torrents_dir/.metadata/pending"
	printf '2\n' > "$torrents_dir/.metadata/layout_version"
	cat >"$torrents_dir/.metadata/state/$FIXTURE_INFO_HASH.json" <<EOF
{"id":"$FIXTURE_INFO_HASH","info_hash":"$FIXTURE_INFO_HASH","name":"payload.txt","state":"ready","created_at":"$now","updated_at":"$now"}
EOF
}

# write_registry_fixture_for registers one ready task whose canonical metainfo
# already sits in torrents_dir. The subtitle scenario needs its own info hash
# and payload name, so the fixture is not hard-coded to the shared one.
write_registry_fixture_for() {
	local torrents_dir="$1" info_hash="$2" name="$3" now
	now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	mkdir -p "$torrents_dir/.metadata/state" "$torrents_dir/.metadata/pending"
	printf '2\n' > "$torrents_dir/.metadata/layout_version"
	cat >"$torrents_dir/.metadata/state/$info_hash.json" <<EOF
{"id":"$info_hash","info_hash":"$info_hash","name":"$name","state":"ready","created_at":"$now","updated_at":"$now"}
EOF
}

# webseed_fetch fetches a path under the running web seed and prints it, so
# readiness and the served bytes are checked through the same HTTP path
# anacrolix will use.
webseed_fetch() {
	local path="$1"
	python3 -c 'import sys, urllib.request; sys.stdout.write(urllib.request.urlopen(sys.argv[1], timeout=5).read().decode())' "http://127.0.0.1:${WEBSEED_PORT}/${path}"
}

# build_video_torrent writes a deterministic single-file .torrent whose payload
# is named with a supported video extension, and prints its info hash. The
# subtitle overlay scenario needs no payload bytes: it proves where a managed
# subtitle lands and how it is cleaned up, not how pieces are read.
build_video_torrent() {
	local dst="$1" name="$2"
	python3 - "$dst" "$name" <<'PY'
import hashlib, sys

def bencode(value):
    if isinstance(value, int):
        return b"i%de" % value
    if isinstance(value, (bytes, bytearray)):
        return b"%d:%s" % (len(value), bytes(value))
    if isinstance(value, str):
        return bencode(value.encode())
    if isinstance(value, list):
        return b"l" + b"".join(bencode(item) for item in value) + b"e"
    if isinstance(value, dict):
        out = b"d"
        for key in sorted(value):
            out += bencode(key) + bencode(value[key])
        return out + b"e"
    raise TypeError(type(value))

dst, name = sys.argv[1], sys.argv[2]
data = b"torrentfs subtitle overlay fixture\n"
info = {
    "length": len(data),
    "name": name,
    "piece length": 16384,
    "pieces": hashlib.sha1(data).digest(),
}
with open(dst, "wb") as handle:
    handle.write(bencode({"info": info}))
sys.stdout.write(hashlib.sha1(bencode(info)).hexdigest())
PY
}

# subtitle_auth simulates the API calls the Web UI makes: log in, then use the
# bearer token. Every call is bounded and fails the run on a non-2xx status.
subtitle_login() {
	local base="$1"
	curl --silent --show-error --fail --max-time "$PROBE_TIMEOUT" \
		--request POST "$base/api/v1/auth/login" \
		--header 'Content-Type: application/json' \
		--data '{"username":"alice","password":"password"}' |
		python3 -c 'import json, sys; print(json.load(sys.stdin)["token"])'
}

subtitle_upload() {
	local base="$1" token="$2" torrent_id="$3" video_path="$4" file="$5" response code
	response="$(curl --silent --show-error --max-time "$PROBE_TIMEOUT" --write-out '\n%{http_code}' \
		--request PUT "$base/api/v1/torrents/$torrent_id/subtitles" \
		--header "Authorization: Bearer $token" \
		--form "video_path=$video_path" \
		--form "file=@$file")" || return 1
	code="${response##*$'\n'}"
	SUBTITLE_UPLOAD_BODY="${response%$'\n'*}"
	case "$code" in
	200 | 201) return 0 ;;
	*)
		printf 'subtitle upload returned HTTP %s: %s\n' "$code" "$SUBTITLE_UPLOAD_BODY" >&2
		return 1
		;;
	esac
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
		"$FILE_INPUT_CONTAINER" "$MISSING_DIR_CONTAINER" "$SUBTITLE_CONTAINER" \
		"$SUBTITLE_OBSERVER_CONTAINER"; do
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

for command_name in docker findmnt sha256sum timeout python3 curl; do
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
chmod 0777 "$TORRENT_HOST_DIR" "$SEEDED_TORRENT_HOST_DIR" "$MOUNT_HOST_DIR" "$NEGATIVE_MOUNT_DIR"
HOST_PROPAGATION="$(findmnt -T "$MOUNT_HOST_DIR" -n -o PROPAGATION 2>/dev/null || true)"
[[ "$HOST_PROPAGATION" == *shared* ]] || \
	fail "host mount containing $MOUNT_HOST_DIR must use shared propagation (current: ${HOST_PROPAGATION:-unknown})"
# Registry fixtures feed the FUSE-only scenarios. Root files with arbitrary
# names are deliberate orphans and must be ignored by registry-only startup.
cp -- "$FIXTURE_TORRENT" "$TORRENT_HOST_DIR/$FIXTURE_INFO_HASH.torrent"
cp -- "$FIXTURE_TORRENT" "$TORRENT_HOST_DIR/example.torrent"
write_registry_fixture "$TORRENT_HOST_DIR"
EXPECTED_HASH="$(sha256sum "$FIXTURE_PAYLOAD")"
EXPECTED_HASH="${EXPECTED_HASH%% *}"

# Serve the fixture payload and hand the data scenarios a web-seeded torrent.
cp -- "$FIXTURE_PAYLOAD" "$WEBSEED_DIR/payload.txt"
# The positive scenario mounts the seeded torrents directory, so the legacy
# .stats marker it asserts on must exist there too.
mkdir -p "$SEEDED_TORRENT_HOST_DIR/.stats"
printf 'legacy\n' > "$SEEDED_TORRENT_HOST_DIR/.stats/leftover.txt"
write_registry_fixture "$SEEDED_TORRENT_HOST_DIR"
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
build_seeded_torrent "$FIXTURE_TORRENT" "$SEEDED_TORRENT_HOST_DIR/$FIXTURE_INFO_HASH.torrent" "http://127.0.0.1:$WEBSEED_PORT/"
printf 'docker smoke: web seed serving the fixture on 127.0.0.1:%s\n' "$WEBSEED_PORT"

# A legacy empty .stats directory must survive startup untouched: the new
# layout neither creates nor removes it.
mkdir -p "$TORRENT_HOST_DIR/.stats"
printf 'legacy\n' > "$TORRENT_HOST_DIR/.stats/leftover.txt"

printf 'docker smoke: building %s\n' "$IMAGE"
IMAGE_TAGGED=1
if ! bounded "$DOCKER_BUILD_TIMEOUT" "docker build" docker build \
	--tag "$IMAGE" "$ROOT_DIR"; then
	fail "docker build failed"
fi

printf 'docker smoke: starting runtime-user host mount observer before FUSE\n'
if ! bounded "$DOCKER_OP_TIMEOUT" "start the runtime-user host mount observer" docker run --detach --name "$HOST_OBSERVER_CONTAINER" \
	--user "$RUNTIME_UID:$RUNTIME_GID" \
	--entrypoint /bin/sh \
	--mount "type=bind,src=$MOUNT_HOST_DIR,dst=/host-mnt,bind-propagation=rslave" \
	"$IMAGE" -c 'sleep 300' >/dev/null; then
	fail "could not start the host mount observer"
fi
HOST_OBSERVER_STARTED=1

printf 'docker smoke: starting runtime-configured real FUSE mount (uid=%s gid=%s)\n' "$RUNTIME_UID" "$RUNTIME_GID"
if ! bounded "$DOCKER_OP_TIMEOUT" "start the runtime-configured FUSE container" docker run --detach --name "$CONTAINER" \
	--env "PUID=$RUNTIME_UID" \
	--env "PGID=$RUNTIME_GID" \
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

PROCESS_CREDENTIALS="$(wait_process_identity "$CONTAINER" torrentfs)" || \
	fail "could not read the FUSE torrentfs process credentials"
IFS=' ' read -r TORRENTFS_PID TORRENTFS_UIDS TORRENTFS_GIDS <<<"$PROCESS_CREDENTIALS"
[[ "$TORRENTFS_UIDS" == "$RUNTIME_UID:$RUNTIME_UID:$RUNTIME_UID:$RUNTIME_UID" ]] || \
	fail "FUSE torrentfs UID credentials are $TORRENTFS_UIDS, expected $RUNTIME_UID in every field"
[[ "$TORRENTFS_GIDS" == "$RUNTIME_GID:$RUNTIME_GID:$RUNTIME_GID:$RUNTIME_GID" ]] || \
	fail "FUSE torrentfs GID credentials are $TORRENTFS_GIDS, expected $RUNTIME_GID in every field"
printf 'docker smoke: torrentfs process %s runs as %s:%s\n' "$TORRENTFS_PID" "$RUNTIME_UID" "$RUNTIME_GID"

mount_deadline=$((SECONDS + 30))
while ((SECONDS < mount_deadline)); do
	state="$(probe "check FUSE container state" docker inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null || true)"
	if [[ "$state" == "exited" || "$state" == "dead" ]]; then
		logs="$(container_logs "$CONTAINER")"
		fail "FUSE container exited before exposing the fixture (state=$state):$'\n'$logs"
	fi
	if [[ "$state" == "running" ]] \
		&& probe "expose fixture in the FUSE container" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1 \
		&& probe "expose fixture in the propagation peer" docker exec "$HOST_OBSERVER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done
if ! probe "expose fixture in the FUSE container" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
	logs="$(container_logs "$CONTAINER")"
	fail "timed out waiting for $MOUNTED_PAYLOAD:$'\n'$logs"
fi
if ! probe "expose fixture in the propagation peer" docker exec "$HOST_OBSERVER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
	fail "timed out waiting for $MOUNT_HOST_DIR/payload.txt"
fi

ACTUAL_HASH="$(probe "read the fixture through the FUSE mount" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$CONTAINER" sha256sum "$MOUNTED_PAYLOAD")" || \
	fail "could not read the fixture through the FUSE mount"
ACTUAL_HASH="${ACTUAL_HASH%% *}"
[[ "$ACTUAL_HASH" == "$EXPECTED_HASH" ]] || \
	fail "mounted payload hash $ACTUAL_HASH does not match fixture hash $EXPECTED_HASH"
HOST_ACTUAL_HASH="$(probe "read the fixture through the propagated host mount" docker exec "$HOST_OBSERVER_CONTAINER" sha256sum /host-mnt/payload.txt)" || \
	fail "could not read the fixture through the propagated host mount"
HOST_ACTUAL_HASH="${HOST_ACTUAL_HASH%% *}"
[[ "$HOST_ACTUAL_HASH" == "$EXPECTED_HASH" ]] || \
	fail "host mounted payload hash $HOST_ACTUAL_HASH does not match fixture hash $EXPECTED_HASH"
HOST_DIRECT_HASH="$(sha256sum "$MOUNT_HOST_DIR/payload.txt")" || \
	fail "host UID $HOST_UID could not read the propagated payload directly"
HOST_DIRECT_HASH="${HOST_DIRECT_HASH%% *}"
[[ "$HOST_DIRECT_HASH" == "$EXPECTED_HASH" ]] || \
	fail "direct host payload hash $HOST_DIRECT_HASH does not match fixture hash $EXPECTED_HASH"
HOST_STAT="$(stat -c '%u:%g %a %n' "$MOUNT_HOST_DIR/payload.txt")" || \
	fail "could not stat the propagated payload"
EXPECTED_STAT="$RUNTIME_UID:$RUNTIME_GID 444 $MOUNT_HOST_DIR/payload.txt"
[[ "$HOST_STAT" == "$EXPECTED_STAT" ]] || \
	fail "mounted payload stat is '$HOST_STAT', expected '$EXPECTED_STAT'"
printf 'docker smoke: mounted payload verified in container and host (%s), ownership %s:%s mode 444\n' \
	"$ACTUAL_HASH" "$RUNTIME_UID" "$RUNTIME_GID"

# The mount exposes torrent data only: the former control directories must be
# absent, and the legacy .stats directory must be untouched.
for control_path in /mnt/metadata /mnt/stats; do
	if probe "check for $control_path" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$CONTAINER" test -e "$control_path" >/dev/null 2>&1; then
		fail "$control_path exists in the mount; the data mount must expose data only"
	fi
done
for control_path in /host-mnt/metadata /host-mnt/stats; do
	if probe "check for $control_path" docker exec "$HOST_OBSERVER_CONTAINER" test -e "$control_path" >/dev/null 2>&1; then
		fail "$control_path exists in the propagated mount; the data mount must expose data only"
	fi
done
if ! probe "check the legacy .stats marker" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$CONTAINER" test -f "$LEGACY_STATS_MARKER" >/dev/null 2>&1; then
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
# Go/FUSE suite (TestFuseIncompleteOverlapReadsShareOneFetcher, which reads two
# pieces in different pages and asserts a zero playback-phase cancellation
# count); this scenario covers the shutdown and EBUSY half.
printf 'docker smoke: starting blocked-read shutdown scenario\n'
BLOCKED_MOUNT_DIR="$TMP_DIR/blocked-mnt"
mkdir -p "$BLOCKED_MOUNT_DIR"
if ! bounded "$DOCKER_OP_TIMEOUT" "start the blocked-read container" docker run --detach --name "$BLOCKED_READ_CONTAINER" \
	--env "PUID=$RUNTIME_UID" \
	--env "PGID=$RUNTIME_GID" \
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
	if probe "expose fixture in the blocked-read container" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$BLOCKED_READ_CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
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
if ! probe "expose fixture in the blocked-read container" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$BLOCKED_READ_CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
	collect_unmount_diagnostics "$BLOCKED_READ_CONTAINER" "$BLOCKED_MOUNT_DIR"
	fail "blocked-read container never exposed $MOUNTED_PAYLOAD"
fi

# Issue two overlapping reads that must both block on the same missing piece.
# They read different, non-overlapping offsets of the 31-byte fixture, so both
# stay inside the file and inside one piece; a read past EOF would return
# immediately and prove nothing. Both are bounded and reclaimable.
BLOCKED_READ_PIDS=()
bounded 60 "first blocked read" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$BLOCKED_READ_CONTAINER" \
	dd if="$MOUNTED_PAYLOAD" of=/dev/null bs=8 skip=0 count=1 >/dev/null 2>&1 &
BLOCKED_READ_PIDS+=("$!")
# Let the first request reach the loader before the overlapping one arrives.
sleep 1
bounded 60 "second blocked read" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$BLOCKED_READ_CONTAINER" \
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
# Start the holder before the daemon mounts FUSE. Its pre-existing bind mount
# receives the later rshared propagation without Docker needing to inspect a
# non-root FUSE mount while creating the holder container. A private bind made
# after propagation keeps the FUSE connection alive when the daemon unmounts.
if ! bounded "$DOCKER_OP_TIMEOUT" "start the peer mount namespace holder" docker run --detach --name "$PEER_HOLDER_CONTAINER" \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--entrypoint /bin/sh \
	--mount "type=bind,src=$PEER_MOUNT_DIR,dst=/host-mnt,bind-propagation=rslave" \
	"$IMAGE" -c 'mkdir -p /held; sleep 300' >/dev/null; then
	fail "could not start the peer mount namespace holder"
fi

# The peer daemon is fed by the web seed so a read cannot block on a missing
# piece; the outstanding-request cause belongs to the blocked-read scenario,
# not this one.
if ! bounded "$DOCKER_OP_TIMEOUT" "start the peer-namespace container" docker run --detach --name "$PEER_DAEMON_CONTAINER" \
	--env "PUID=$RUNTIME_UID" \
	--env "PGID=$RUNTIME_GID" \
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
peer_mount_deadline=$((SECONDS + 30))
while ((SECONDS < peer_mount_deadline)); do
	peer_state="$(probe "check peer-namespace container state" docker inspect --format '{{.State.Status}}' "$PEER_DAEMON_CONTAINER" 2>/dev/null || true)"
	if [[ "$peer_state" == "exited" || "$peer_state" == "dead" ]]; then
		collect_unmount_diagnostics "$PEER_DAEMON_CONTAINER" "$PEER_MOUNT_DIR"
		fail "peer-namespace container exited before exposing the fixture (state=$peer_state)"
	fi
	if probe "expose fixture in the peer mount namespace" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$PEER_HOLDER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done
# Scenario validity: without a real copy in the peer namespace the shutdown
# below would be exercised against nothing.
if ! probe "expose fixture in the peer mount namespace" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$PEER_HOLDER_CONTAINER" test -f /host-mnt/payload.txt >/dev/null 2>&1; then
	collect_unmount_diagnostics "$PEER_DAEMON_CONTAINER" "$PEER_MOUNT_DIR"
	fail "the peer mount namespace never received a propagated copy of $PEER_MOUNT_DIR/payload.txt"
fi
if ! bounded 30 'create private peer bind' docker exec "$PEER_HOLDER_CONTAINER" /bin/sh -c \
	'mount --bind /host-mnt /held && mount --make-private /held' >/dev/null; then
	fail 'could not create a private peer bind mount'
fi
held_mount_deadline=$((SECONDS + 30))
while ((SECONDS < held_mount_deadline)); do
	if probe "expose private peer bind" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$PEER_HOLDER_CONTAINER" test -f /held/payload.txt >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done
if ! probe "expose private peer bind" docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$PEER_HOLDER_CONTAINER" test -f /held/payload.txt >/dev/null 2>&1; then
	collect_unmount_diagnostics "$PEER_DAEMON_CONTAINER" "$PEER_MOUNT_DIR"
	fail "the private peer bind did not expose $PEER_MOUNT_DIR/payload.txt"
fi
PEER_HASH="$(probe "read through the peer mount namespace" \
	docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$PEER_HOLDER_CONTAINER" sha256sum /held/payload.txt)" || \
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
	--env "PUID=$RUNTIME_UID" \
	--env "PGID=$RUNTIME_GID" \
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
		--env "PUID=$RUNTIME_UID" \
		--env "PGID=$RUNTIME_GID" \
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

# ---------------------------------------------------------------------------
# Managed subtitle overlay.
#
# A subtitle must reach the mount through the authenticated API only. This
# scenario proves the whole chain in real Docker: a direct `docker cp` into the
# FUSE mount fails, the API upload becomes visible and readable in the
# container mount, in a propagation observer, and on the host; a same-name
# replacement is served by a later open; DELETE removes payload and subtitle
# together; and an injected cleanup fault lands in delete_failed with a stable
# error_code before a retry with the same operation id succeeds.
printf 'docker smoke: checking managed subtitle overlay\n'
SUBTITLE_TORRENT_HOST_DIR="$TMP_DIR/subtitle-torrents"
SUBTITLE_MOUNT_DIR="$TMP_DIR/subtitle-mnt"
mkdir -p "$SUBTITLE_TORRENT_HOST_DIR" "$SUBTITLE_MOUNT_DIR"
chmod 0777 "$SUBTITLE_TORRENT_HOST_DIR" "$SUBTITLE_MOUNT_DIR"

SUBTITLE_INFO_HASH="$(build_video_torrent "$SUBTITLE_TORRENT_HOST_DIR/$SUBTITLE_VIDEO_NAME.torrent" "$SUBTITLE_VIDEO_NAME")" || \
	fail "could not build the subtitle fixture torrent"
[[ "$SUBTITLE_INFO_HASH" =~ ^[0-9a-f]{40}$ ]] || \
	fail "subtitle fixture info hash $SUBTITLE_INFO_HASH is not a 40-character hex digest"
mv -- "$SUBTITLE_TORRENT_HOST_DIR/$SUBTITLE_VIDEO_NAME.torrent" "$SUBTITLE_TORRENT_HOST_DIR/$SUBTITLE_INFO_HASH.torrent"
write_registry_fixture_for "$SUBTITLE_TORRENT_HOST_DIR" "$SUBTITLE_INFO_HASH" "$SUBTITLE_VIDEO_NAME"

# The uploaded part's filename is what the server matches against the video
# stem, so the fixture files carry the matching basename. The forged one does
# not, and proves the mismatch rejection.
SUBTITLE_FIRST_FILE="$TMP_DIR/subtitles/first/$SUBTITLE_VIDEO_STEM.srt"
SUBTITLE_SECOND_FILE="$TMP_DIR/subtitles/second/$SUBTITLE_VIDEO_STEM.srt"
SUBTITLE_FORGED_FILE="$TMP_DIR/subtitles/forged/forged.srt"
mkdir -p "$(dirname "$SUBTITLE_FIRST_FILE")" "$(dirname "$SUBTITLE_SECOND_FILE")" "$(dirname "$SUBTITLE_FORGED_FILE")"
printf 'original subtitle\n' > "$SUBTITLE_FIRST_FILE"
printf 'replaced subtitle\n' > "$SUBTITLE_SECOND_FILE"
printf 'forged subtitle\n' > "$SUBTITLE_FORGED_FILE"
FIRST_SHA="$(sha256sum "$SUBTITLE_FIRST_FILE")"; FIRST_SHA="${FIRST_SHA%% *}"
SECOND_SHA="$(sha256sum "$SUBTITLE_SECOND_FILE")"; SECOND_SHA="${SECOND_SHA%% *}"
(( ${#FIRST_SHA} == 64 && ${#SECOND_SHA} == 64 )) || fail "could not hash the subtitle fixtures"

SUBTITLE_PORT="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
[[ -n "$SUBTITLE_PORT" ]] || fail "could not allocate a subtitle API port"
SUBTITLE_BASE="http://127.0.0.1:$SUBTITLE_PORT"
mkdir -p "$TMP_DIR/subtitle-config"
cat >"$TMP_DIR/subtitle-config/torrentfs.toml" <<EOF
[http]
listen_addr = "127.0.0.1:$SUBTITLE_PORT"

[http.auth]
enabled = true
username = "alice"
password_hash = "$SUBTITLE_PASSWORD_HASH"
token_ttl = "10m"
EOF
chmod 0644 "$TMP_DIR/subtitle-config/torrentfs.toml"

if ! bounded "$DOCKER_OP_TIMEOUT" "start the propagation observer for the subtitle mount" docker run --detach --name "$SUBTITLE_OBSERVER_CONTAINER" \
	--user "$RUNTIME_UID:$RUNTIME_GID" \
	--entrypoint /bin/sh \
	--mount "type=bind,src=$SUBTITLE_MOUNT_DIR,dst=/host-mnt,bind-propagation=rslave" \
	"$IMAGE" -c 'sleep 600' >/dev/null; then
	fail "could not start the subtitle propagation observer"
fi

if ! bounded "$DOCKER_OP_TIMEOUT" "start the managed subtitle container" docker run --detach --name "$SUBTITLE_CONTAINER" \
	--env "PUID=$RUNTIME_UID" \
	--env "PGID=$RUNTIME_GID" \
	--device /dev/fuse \
	--cap-add SYS_ADMIN \
	--security-opt apparmor=unconfined \
	--network host \
	--mount "type=bind,src=$SUBTITLE_TORRENT_HOST_DIR,dst=/torrents" \
	--mount "type=bind,src=$SUBTITLE_MOUNT_DIR,dst=/mnt,bind-propagation=rshared" \
	--mount "type=bind,src=$TMP_DIR/subtitle-config/torrentfs.toml,dst=/etc/torrentfs/subtitle.toml,readonly" \
	"$IMAGE" -config /etc/torrentfs/subtitle.toml -mountpoint /mnt /torrents >/dev/null; then
	fail "could not start the managed subtitle container"
fi

subtitle_ready_deadline=$((SECONDS + 60))
while ((SECONDS < subtitle_ready_deadline)); do
	if probe "read the fixture through the subtitle mount" \
		docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" test -f "/mnt/$SUBTITLE_VIDEO_NAME" >/dev/null 2>&1 \
		&& probe "reach the subtitle API" curl --silent --show-error --fail --max-time "$PROBE_TIMEOUT" "$SUBTITLE_BASE/" >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done
if ! probe "read the fixture through the subtitle mount" \
	docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" test -f "/mnt/$SUBTITLE_VIDEO_NAME" >/dev/null 2>&1; then
	fail "the subtitle container never exposed /mnt/$SUBTITLE_VIDEO_NAME"
fi
if ! probe "reach the subtitle API" curl --silent --show-error --fail --max-time "$PROBE_TIMEOUT" "$SUBTITLE_BASE/" >/dev/null 2>&1; then
	fail "the subtitle API on $SUBTITLE_BASE was never reachable"
fi

# An unauthenticated upload must be refused: the API is the only writer, and it
# is not anonymous.
UNAUTH_STATUS="$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time "$PROBE_TIMEOUT" \
	--request PUT "$SUBTITLE_BASE/api/v1/torrents/$SUBTITLE_INFO_HASH/subtitles" \
	--form "video_path=$SUBTITLE_VIDEO_NAME" --form "file=@$SUBTITLE_FIRST_FILE" || true)"
[[ "$UNAUTH_STATUS" == "401" ]] || \
	fail "unauthenticated subtitle upload returned $UNAUTH_STATUS, want 401"

# A direct copy into the FUSE mount is the workflow this feature replaces: it
# must fail and leave nothing behind.
if bounded 30 "docker cp a subtitle into the FUSE mount" docker cp "$SUBTITLE_FIRST_FILE" "$SUBTITLE_CONTAINER:/mnt/direct.srt" >/dev/null 2>&1; then
	DIRECT_COPY_STATUS=0
else
	DIRECT_COPY_STATUS=$?
fi
[[ "$DIRECT_COPY_STATUS" != "0" ]] || \
	fail "docker cp into the FUSE mount succeeded, but the mount must be read-only"
if probe "verify docker cp left nothing" \
	docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" test -e /mnt/direct.srt >/dev/null 2>&1; then
	fail "docker cp into the FUSE mount left /mnt/direct.srt behind"
fi
printf 'docker smoke: direct docker cp into the FUSE mount failed as expected\n'

SUBTITLE_TOKEN="$(subtitle_login "$SUBTITLE_BASE")" || fail "subtitle API login failed"
[[ -n "$SUBTITLE_TOKEN" ]] || fail "subtitle API login returned an empty token"

# A mismatched name is rejected with the stable code the UI branches on.
MISMATCH_BODY="$(curl --silent --show-error --max-time "$PROBE_TIMEOUT" \
	--request PUT "$SUBTITLE_BASE/api/v1/torrents/$SUBTITLE_INFO_HASH/subtitles" \
	--header "Authorization: Bearer $SUBTITLE_TOKEN" \
	--form "video_path=$SUBTITLE_VIDEO_NAME" --form "file=@$SUBTITLE_FORGED_FILE" 2>&1 || true)"
[[ "$MISMATCH_BODY" == *"subtitle_name_mismatch"* ]] || \
	fail "mismatched subtitle name was not rejected with subtitle_name_mismatch: $MISMATCH_BODY"

subtitle_upload "$SUBTITLE_BASE" "$SUBTITLE_TOKEN" "$SUBTITLE_INFO_HASH" "$SUBTITLE_VIDEO_NAME" "$SUBTITLE_FIRST_FILE" >/dev/null || \
	fail "subtitle upload failed"
SUBTITLE_PATH="video.srt"

# Three views of the same file: container mount, propagation observer, host.
if ! probe "read the subtitle in the container mount" \
	docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" sha256sum "/mnt/$SUBTITLE_PATH" >/dev/null 2>&1; then
	fail "the uploaded subtitle is missing from the container mount"
fi
CONTAINER_SUBTITLE_SHA="$(probe "hash the subtitle in the container mount" \
	docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" sha256sum "/mnt/$SUBTITLE_PATH")" || \
	fail "could not hash the subtitle inside the container"
CONTAINER_SUBTITLE_SHA="${CONTAINER_SUBTITLE_SHA%% *}"
[[ "$CONTAINER_SUBTITLE_SHA" == "$FIRST_SHA" ]] || \
	fail "container subtitle hash $CONTAINER_SUBTITLE_SHA does not match the uploaded $FIRST_SHA"

OBSERVER_SUBTITLE_SHA=""
subtitle_observe_deadline=$((SECONDS + 30))
while ((SECONDS < subtitle_observe_deadline)); do
	if OBSERVER_SUBTITLE_SHA="$(probe "hash the subtitle through the propagation observer" \
		docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_OBSERVER_CONTAINER" sha256sum "/host-mnt/$SUBTITLE_PATH" 2>/dev/null)"; then
		break
	fi
	OBSERVER_SUBTITLE_SHA=""
	sleep 0.2
done
OBSERVER_SUBTITLE_SHA="${OBSERVER_SUBTITLE_SHA%% *}"
[[ "$OBSERVER_SUBTITLE_SHA" == "$FIRST_SHA" ]] || \
	fail "propagation observer subtitle hash ${OBSERVER_SUBTITLE_SHA:-missing} does not match the uploaded $FIRST_SHA"

HOST_SUBTITLE_SHA="$(sha256sum "$SUBTITLE_MOUNT_DIR/$SUBTITLE_PATH" 2>/dev/null || true)"
HOST_SUBTITLE_SHA="${HOST_SUBTITLE_SHA%% *}"
[[ "$HOST_SUBTITLE_SHA" == "$FIRST_SHA" ]] || \
	fail "host subtitle hash ${HOST_SUBTITLE_SHA:-missing} does not match the uploaded $FIRST_SHA"
printf 'docker smoke: managed subtitle visible with matching hash in container, observer, and host views\n'

# The sidecar storage layout is what makes the overlay durable; it must not sit
# in the mount or in the torrents directory itself.
if ! probe "verify the subtitle store layout" \
	docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" \
	test -f "/torrents/.metadata/subtitles/$SUBTITLE_INFO_HASH/$SUBTITLE_PATH" >/dev/null 2>&1; then
	fail "the managed subtitle is missing from /torrents/.metadata/subtitles/$SUBTITLE_INFO_HASH"
fi

# A same-name replacement must be served by a later open, so the new bytes are
# read rather than a cached copy of the old ones.
subtitle_upload "$SUBTITLE_BASE" "$SUBTITLE_TOKEN" "$SUBTITLE_INFO_HASH" "$SUBTITLE_VIDEO_NAME" "$SUBTITLE_SECOND_FILE" >/dev/null || \
	fail "subtitle replacement failed"
REPLACED_SHA="$(sha256sum "$SUBTITLE_MOUNT_DIR/$SUBTITLE_PATH" 2>/dev/null || true)"
REPLACED_SHA="${REPLACED_SHA%% *}"
[[ "$REPLACED_SHA" == "$SECOND_SHA" ]] || \
	fail "replacement hash ${REPLACED_SHA:-missing} does not match the new upload $SECOND_SHA"
CONTAINER_REPLACED_SHA="$(probe "hash the replacement in the container mount" \
	docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" sha256sum "/mnt/$SUBTITLE_PATH")" || \
	fail "could not hash the replacement inside the container"
CONTAINER_REPLACED_SHA="${CONTAINER_REPLACED_SHA%% *}"
[[ "$CONTAINER_REPLACED_SHA" == "$SECOND_SHA" ]] || \
	fail "container replacement hash $CONTAINER_REPLACED_SHA does not match the new upload $SECOND_SHA"
printf 'docker smoke: subtitle replacement is served by a later open in every view\n'

# Direct mutation through the mount stays refused even for the new node.
if probe "write the subtitle through the mount" \
	docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" \
	sh -c "echo forged > /mnt/$SUBTITLE_PATH" >/dev/null 2>&1; then
	fail "writing a managed subtitle through the FUSE mount succeeded"
fi
AFTER_WRITE_SHA="$(sha256sum "$SUBTITLE_MOUNT_DIR/$SUBTITLE_PATH" 2>/dev/null || true)"
AFTER_WRITE_SHA="${AFTER_WRITE_SHA%% *}"
[[ "$AFTER_WRITE_SHA" == "$SECOND_SHA" ]] || \
	fail "a refused write changed the subtitle content"
printf 'docker smoke: writing a managed subtitle through the mount is refused\n'

# An injected cleanup fault must land in delete_failed with the stable code, and
# the task must stay hidden until a retry succeeds with the same operation id.
if ! bounded 30 "make the subtitle store unwritable" docker exec --user 0 "$SUBTITLE_CONTAINER" \
	chmod 0500 "/torrents/.metadata/subtitles" >/dev/null; then
	fail "could not make the subtitle store unwritable"
fi
FAULTED_OP="$(curl --silent --show-error --fail --max-time "$PROBE_TIMEOUT" \
	--request DELETE "$SUBTITLE_BASE/api/v1/torrents/$SUBTITLE_INFO_HASH" \
	--header "Authorization: Bearer $SUBTITLE_TOKEN" |
	python3 -c 'import json, sys; print(json.load(sys.stdin)["operation_id"])')" || \
	fail "could not start the faulted deletion"
[[ -n "$FAULTED_OP" ]] || fail "the faulted deletion returned no operation id"

subtitle_fault_deadline=$((SECONDS + 60))
FAULTED_STATE=""
FAULTED_CODE=""
while ((SECONDS < subtitle_fault_deadline)); do
	FAULTED_JSON="$(curl --silent --show-error --fail --max-time "$PROBE_TIMEOUT" \
		"$SUBTITLE_BASE/api/v1/operations/$FAULTED_OP" \
		--header "Authorization: Bearer $SUBTITLE_TOKEN" || true)"
	FAULTED_STATE="$(printf '%s' "$FAULTED_JSON" | python3 -c 'import json, sys; print(json.load(sys.stdin).get("state", ""))' 2>/dev/null || true)"
	FAULTED_CODE="$(printf '%s' "$FAULTED_JSON" | python3 -c 'import json, sys; print(json.load(sys.stdin).get("error_code", ""))' 2>/dev/null || true)"
	if [[ "$FAULTED_STATE" != "deleting" ]]; then
		break
	fi
	sleep 0.2
done
[[ "$FAULTED_STATE" == "delete_failed" ]] || \
	fail "faulted deletion state = ${FAULTED_STATE:-unknown}, want delete_failed"
[[ "$FAULTED_CODE" == "subtitle_cleanup_failed" ]] || \
	fail "faulted deletion error_code = ${FAULTED_CODE:-missing}, want subtitle_cleanup_failed"
if probe "verify the faulted task stays hidden" \
	docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" test -e "/mnt/$SUBTITLE_PATH" >/dev/null 2>&1; then
	fail "a delete_failed task still exposed its subtitle in the mount"
fi
printf 'docker smoke: subtitle cleanup failure reported delete_failed with subtitle_cleanup_failed\n'

if ! bounded 30 "restore the subtitle store permissions" docker exec --user 0 "$SUBTITLE_CONTAINER" \
	chmod 0700 "/torrents/.metadata/subtitles" >/dev/null; then
	fail "could not restore the subtitle store permissions"
fi
RETRY_OP="$(curl --silent --show-error --fail --max-time "$PROBE_TIMEOUT" \
	--request DELETE "$SUBTITLE_BASE/api/v1/torrents/$SUBTITLE_INFO_HASH" \
	--header "Authorization: Bearer $SUBTITLE_TOKEN" |
	python3 -c 'import json, sys; print(json.load(sys.stdin)["operation_id"])')" || \
	fail "could not retry the deletion"
[[ "$RETRY_OP" == "$FAULTED_OP" ]] || \
	fail "retry reused operation $RETRY_OP instead of the original $FAULTED_OP"

subtitle_retry_deadline=$((SECONDS + 60))
RETRY_STATE=""
while ((SECONDS < subtitle_retry_deadline)); do
	RETRY_STATE="$(curl --silent --show-error --fail --max-time "$PROBE_TIMEOUT" \
		"$SUBTITLE_BASE/api/v1/operations/$RETRY_OP" \
		--header "Authorization: Bearer $SUBTITLE_TOKEN" |
		python3 -c 'import json, sys; print(json.load(sys.stdin).get("state", ""))' 2>/dev/null || true)"
	if [[ "$RETRY_STATE" != "deleting" ]]; then
		break
	fi
	sleep 0.2
done
[[ "$RETRY_STATE" == "deleted" ]] || \
	fail "retried deletion state = ${RETRY_STATE:-unknown}, want deleted"

# The deletion must take the payload, the subtitle, and the sidecar store with
# it — in the container mount and on the host.
for name in "$SUBTITLE_PATH" "$SUBTITLE_VIDEO_NAME"; do
	if probe "verify $name is gone from the container mount" \
		docker exec --user "$RUNTIME_UID:$RUNTIME_GID" "$SUBTITLE_CONTAINER" test -e "/mnt/$name" >/dev/null 2>&1; then
		fail "$name survived the deletion in the container mount"
	fi
	if [[ -e "$SUBTITLE_MOUNT_DIR/$name" ]]; then
		fail "$name survived the deletion on the host mount"
	fi
done
if [[ -e "$SUBTITLE_TORRENT_HOST_DIR/.metadata/subtitles/$SUBTITLE_INFO_HASH" ]]; then
	fail "the subtitle store survived the deletion"
fi
printf 'docker smoke: deleting the torrent removed payload, subtitle, and subtitle store\n'

if ! bounded 30 "remove the managed subtitle container" docker rm -f "$SUBTITLE_CONTAINER" >/dev/null; then
	fail "could not remove the managed subtitle container"
fi
if ! bounded 30 "remove the subtitle propagation observer" docker rm -f "$SUBTITLE_OBSERVER_CONTAINER" >/dev/null; then
	fail "could not remove the subtitle propagation observer"
fi
printf 'docker smoke: managed subtitle overlay checks passed\n'

printf 'docker smoke: all checks passed\n'
