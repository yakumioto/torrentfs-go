#!/usr/bin/env bash
set -Eeuo pipefail

readonly SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly FIXTURE_DIR="$ROOT_DIR/examples/docker"
readonly FIXTURE_TORRENT="$FIXTURE_DIR/example.torrent"
readonly FIXTURE_PAYLOAD="$FIXTURE_DIR/data/payload.txt"
readonly MOUNTED_PAYLOAD="/mnt/payload.txt"
readonly IMAGE="torrentfs-mio17-smoke:${BASHPID}"
readonly CONTAINER="torrentfs-mio17-${BASHPID}"
readonly FILE_INPUT_CONTAINER="torrentfs-mio17-file-input-${BASHPID}"
readonly MISSING_DIR_CONTAINER="torrentfs-mio17-missing-dir-${BASHPID}"

TMP_DIR=""
IMAGE_TAGGED=0
POSITIVE_STARTED=0
FILE_INPUT_CREATED=0
MISSING_DIR_CREATED=0

fail() {
	printf 'docker smoke: %s\n' "$*" >&2
	exit 1
}

cleanup() {
	local status=$?
	trap - EXIT INT TERM

	if ((POSITIVE_STARTED)); then
		docker kill --signal TERM "$CONTAINER" >/dev/null 2>&1 || true
		docker wait "$CONTAINER" >/dev/null 2>&1 || true
	fi
	if [[ -n "$CONTAINER" ]]; then
		docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
	fi
	if ((FILE_INPUT_CREATED)); then
		docker rm -f "$FILE_INPUT_CONTAINER" >/dev/null 2>&1 || true
	fi
	if ((MISSING_DIR_CREATED)); then
		docker rm -f "$MISSING_DIR_CONTAINER" >/dev/null 2>&1 || true
	fi
	if ((IMAGE_TAGGED)); then
		docker image rm "$IMAGE" >/dev/null 2>&1 || true
	fi
	if [[ -n "$TMP_DIR" ]]; then
		rm -rf -- "$TMP_DIR"
	fi

	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for command_name in docker sha256sum timeout; do
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
cp -- "$FIXTURE_TORRENT" "$TORRENT_HOST_DIR/example.torrent"
cp -- "$FIXTURE_PAYLOAD" "$DATA_HOST_DIR/payload.txt"
EXPECTED_HASH="$(sha256sum "$DATA_HOST_DIR/payload.txt")"
EXPECTED_HASH="${EXPECTED_HASH%% *}"

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
	--mount "type=bind,src=$MOUNT_HOST_DIR,dst=/mnt" \
	"$IMAGE" -mountpoint /mnt -data-dir /data /torrents >/dev/null; then
	fail "could not start the FUSE container; check /dev/fuse, SYS_ADMIN, and AppArmor permissions"
fi
POSITIVE_STARTED=1

mount_deadline=$((SECONDS + 30))
while ((SECONDS < mount_deadline)); do
	state="$(docker inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null || true)"
	if [[ "$state" == "exited" || "$state" == "dead" ]]; then
		logs="$(docker logs "$CONTAINER" 2>&1 || true)"
		fail "FUSE container exited before exposing the fixture (state=$state):$'\n'$logs"
	fi
	if [[ "$state" == "running" ]] && docker exec "$CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done
if ! docker exec "$CONTAINER" test -f "$MOUNTED_PAYLOAD" >/dev/null 2>&1; then
	logs="$(docker logs "$CONTAINER" 2>&1 || true)"
	fail "timed out waiting for $MOUNTED_PAYLOAD:$'\n'$logs"
fi

ACTUAL_HASH="$(docker exec "$CONTAINER" sha256sum "$MOUNTED_PAYLOAD")" || \
	fail "could not read the fixture through the FUSE mount"
ACTUAL_HASH="${ACTUAL_HASH%% *}"
[[ "$ACTUAL_HASH" == "$EXPECTED_HASH" ]] || \
	fail "mounted payload hash $ACTUAL_HASH does not match fixture hash $EXPECTED_HASH"
printf 'docker smoke: mounted payload verified (%s)\n' "$ACTUAL_HASH"

if ! docker kill --signal TERM "$CONTAINER" >/dev/null; then
	fail "could not send SIGTERM to the FUSE container"
fi
if ! STOP_STATUS="$(docker wait "$CONTAINER")"; then
	fail "could not wait for the FUSE container to stop"
fi
[[ "$STOP_STATUS" == "0" ]] || fail "FUSE container stopped with exit code $STOP_STATUS"
POSITIVE_STARTED=0
printf 'docker smoke: FUSE container stopped cleanly\n'

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
