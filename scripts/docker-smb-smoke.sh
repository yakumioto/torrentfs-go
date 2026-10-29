#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly IMAGE="${TORRENTFS_SMOKE_IMAGE:-torrentfs-smb-smoke:${BASHPID}}"
readonly CLIENT_IMAGE="${TORRENTFS_SMOKE_CLIENT_IMAGE:-torrentfs-smb-client:${BASHPID}}"
readonly NETWORK="torrentfs-smb-network-${BASHPID}"
readonly APP_PREFIX="torrentfs-smb-app-${BASHPID}"
readonly CLIENT_CREDENTIALS_NAME="smb-credentials"
readonly DOCKER_OP_TIMEOUT=60
readonly DOCKER_BUILD_TIMEOUT=1800
readonly PROBE_TIMEOUT=10
readonly START_TIMEOUT=60
# Client operations share one bound. The payload is deliberately larger than the
# configured cache so reads have to fetch pieces, but small enough that a
# web-seeded download finishes comfortably inside this bound on a slow runner.
readonly CLIENT_TIMEOUT=180
readonly CLIENT_SMB_TIMEOUT=120
readonly SMOKE_UID="${TORRENTFS_SMOKE_UID:-99}"
readonly SMOKE_GID="${TORRENTFS_SMOKE_GID:-100}"
readonly FILE_SIZE=$((64 * 1024 * 1024 + 123))
readonly PIECE_LENGTH=$((1 * 1024 * 1024))
# Smaller than the payload and large enough for the reader's readahead window;
# the full read still evicts the early pieces before the cold-cache seek below.
readonly CACHE_BYTES=$((8 * 1024 * 1024))
readonly RANDOM_OFFSET=$((40 * 1024 * 1024 + 12345))
readonly RANDOM_LENGTH=8192
# Kernel-level "this host cannot do CIFS at all" failures, as opposed to a real
# regression in the mount or the data it returns.
readonly CIFS_UNAVAILABLE_PATTERN='(unknown filesystem type|cifs filesystem not supported by the system|No such device|modprobe.*(not found|not permitted)|mount error\(1\): Operation not permitted)'

work_dir=''
webseed_pid=''
network_created=0
image_built=0
client_image_built=0
cifs_available=0
app_names=()

fail() {
	printf 'docker SMB smoke: %s\n' "$*" >&2
	exit 1
}

bounded() {
	local seconds="$1" description="$2" status
	shift 2
	if timeout --foreground "$seconds" "$@"; then
		return 0
	else
		status=$?
	fi
	printf 'docker SMB smoke: %s failed or exceeded %ss (status=%s)\n' "$description" "$seconds" "$status" >&2
	return "$status"
}

probe() {
	local description="$1"
	shift
	bounded "$PROBE_TIMEOUT" "$description" "$@"
}

logs() {
	probe "read logs for $1" docker logs "$1" 2>&1 || true
}

all_process_identities() {
	local container="$1" process_name="$2"
	probe "read $process_name credentials" docker exec "$container" /bin/sh -c '
		wanted="$1"
		found=0
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
			found=1
		done
		[ "$found" -eq 1 ]
	' sh "$process_name"
}

assert_process_identities() {
	local container="$1" process_name="$2" output pid uid gid count=0
	output="$(all_process_identities "$container" "$process_name")" || fail "$container has no $process_name process"
	while IFS=' ' read -r pid uid gid; do
		[[ -n "$pid" ]] || continue
		[[ "$uid" == "$SMOKE_UID:$SMOKE_UID:$SMOKE_UID:$SMOKE_UID" ]] || fail "unexpected $process_name UID credentials"
		[[ "$gid" == "$SMOKE_GID:$SMOKE_GID:$SMOKE_GID:$SMOKE_GID" ]] || fail "unexpected $process_name GID credentials"
		count=$((count + 1))
	done <<<"$output"
	(( count > 0 )) || fail "missing $process_name process"
}

assert_native_app() {
	local container="$1"
	assert_process_identities "$container" torrentfs
	probe 'check native process privileges and absence of FUSE/Samba' docker exec "$container" /bin/sh -eu -c '
		test ! -e /dev/fuse
		test ! -d /run/samba
		! command -v smbd >/dev/null 2>&1
		found=0
		for status in /proc/[0-9]*/status; do
			[ -r "$status" ] || continue
			grep -q "^Name:[[:space:]]*torrentfs$" "$status" || continue
			grep -q "^CapEff:[[:space:]]*0000000000000000$" "$status"
			grep -q "^CapBnd:[[:space:]]*0000000000000000$" "$status"
			grep -q "^NoNewPrivs:[[:space:]]*1$" "$status"
			found=1
		done
		test "$found" = 1
	'
}

# remove_work_dir deletes the scratch tree even when torrentfs created files as a
# different UID than the caller (the container runs as its own runtime identity,
# the runner user is not that UID), which a plain rm cannot do.
remove_work_dir() {
	local status
	[[ -n "$work_dir" ]] || return 0
	if rm -rf -- "$work_dir" 2>/dev/null; then
		return 0
	fi
	printf 'docker SMB smoke: %s is not fully removable by this user; removing it via a root container\n' "$work_dir" >&2
	if (( ! client_image_built )); then
		return 1
	fi
	status=0
	bounded 60 'remove the scratch directory as root' docker run --rm \
		--mount "type=bind,src=$work_dir,dst=/scratch" \
		--entrypoint /bin/sh "$CLIENT_IMAGE" -c 'rm -rf /scratch/* /scratch/..?* /scratch/.[!.]*' >/dev/null 2>&1 || status=$?
	if (( status != 0 )); then
		printf 'docker SMB smoke: could not remove %s (status=%s)\n' "$work_dir" "$status" >&2
		return "$status"
	fi
	rmdir -- "$work_dir" 2>/dev/null || true
}

cleanup() {
	local status=$?
	trap - EXIT INT TERM
	for app in "${app_names[@]}"; do
		bounded 30 "remove $app" docker rm -f "$app" >/dev/null 2>&1 || true
	done
	if [[ -n "$webseed_pid" ]]; then
		kill "$webseed_pid" 2>/dev/null || true
		wait "$webseed_pid" 2>/dev/null || true
	fi
	if (( network_created )); then
		bounded 30 'remove SMB network' docker network rm "$NETWORK" >/dev/null 2>&1 || true
	fi
	# The work tree goes away before the images it borrows for the root removal.
	remove_work_dir || true
	if (( client_image_built )); then
		bounded 60 'remove SMB client image' docker image rm "$CLIENT_IMAGE" >/dev/null 2>&1 || true
	fi
	if (( image_built )); then
		bounded 60 'remove SMB image' docker image rm "$IMAGE" >/dev/null 2>&1 || true
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for command_name in docker python3 sha256sum dd timeout; do
	command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/torrentfs-smb.XXXXXX")"
torrents_dir="$work_dir/torrents"
webseed_dir="$work_dir/webseed"
client_output_dir="$work_dir/client-output"
mkdir -p "$torrents_dir" "$webseed_dir" "$client_output_dir"
chmod 0777 "$torrents_dir"

python3 -I - "$webseed_dir/payload.bin" "$FILE_SIZE" "$PIECE_LENGTH" <<'PY'
import sys

path, size, piece_length = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
with open(path, "wb") as output:
    offset = 0
    while offset < size:
        length = min(piece_length, size - offset)
        output.write(bytes(((offset + index) * 31 + 7) % 251 for index in range(length)))
        offset += length
PY

# The STRM scenario alone uses a video name; keep payload.bin for the existing
# full-read, cold-read, and CIFS fixtures.
ln -- "$webseed_dir/payload.bin" "$webseed_dir/Movie.mkv"
webseed_reads="$work_dir/webseed.reads"
: >"$webseed_reads"

# The BEP 19 web seed must answer ranged GETs: anacrolix fetches one piece at a
# time, and the standard library handler ignores Range, which would make every
# piece fetch download the whole file.
python3 -I - "$webseed_dir" "$work_dir/webseed.port" "$webseed_reads" >"$work_dir/webseed.log" 2>&1 <<'PY' &
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

root, port_path, reads_path = sys.argv[1:]


class ThreadedServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class RangedHandler(BaseHTTPRequestHandler):
    # Keep-alive keeps a piece fetch from paying a fresh connection per request.
    protocol_version = "HTTP/1.1"

    def resolve(self):
        path = os.path.join(root, os.path.basename(self.path.split("?", 1)[0]))
        try:
            return path, os.path.getsize(path)
        except OSError:
            self.send_error(404)
            return None, None

    def range_bounds(self, size):
        start, end = 0, size - 1
        requested = self.headers.get("Range", "")
        if not requested.startswith("bytes="):
            return start, end, False
        spec = requested[len("bytes="):].split(",", 1)[0]
        first, _, last = spec.partition("-")
        if first:
            start = int(first)
        if last:
            end = min(int(last), size - 1)
        if start > end or start >= size:
            self.send_response(416)
            self.send_header("Content-Range", "bytes */%d" % size)
            self.end_headers()
            return None, None, True
        return start, end, True

    def serve(self, body):
        path, size = self.resolve()
        if path is None:
            return
        start, end, partial = self.range_bounds(size)
        if start is None:
            return
        if partial:
            self.send_response(206)
            self.send_header("Content-Range", "bytes %d-%d/%d" % (start, end, size))
        else:
            self.send_response(200)
        length = end - start + 1
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Content-Length", str(length))
        self.end_headers()
        if not body:
            return
        with open(reads_path, "a") as read_log:
            read_log.write(os.path.basename(path) + "\n")
        with open(path, "rb") as source:
            source.seek(start)
            remaining = length
            while remaining > 0:
                chunk = source.read(min(1 << 20, remaining))
                if not chunk:
                    break
                self.wfile.write(chunk)
                remaining -= len(chunk)

    def do_GET(self):
        self.serve(body=True)

    # Piece size probing uses HEAD; without it the client sees an unsupported
    # method and each web-seed fetch has to time out and retry.
    def do_HEAD(self):
        self.serve(body=False)

    def log_message(self, *args):
        pass


server = ThreadedServer(("0.0.0.0", 0), RangedHandler)
with open(port_path, "w") as port_file:
    port_file.write(str(server.server_port))
    port_file.flush()
server.serve_forever()
PY
webseed_pid=$!
for _ in {1..100}; do
	if [[ -s "$work_dir/webseed.port" ]]; then
		break
	fi
	sleep 0.1
done
[[ -s "$work_dir/webseed.port" ]] || fail "web seed did not start: $(sed -n '1,20p' "$work_dir/webseed.log")"
webseed_port="$(<"$work_dir/webseed.port")"

create_fixture() {
	local root="$1" source="$2" name="$3" url="$4" hash_path="$5"
	mkdir -p "$root/.metadata/state" "$root/.metadata/pending"
	chmod 0777 "$root" "$root/.metadata" "$root/.metadata/state" "$root/.metadata/pending"
	python3 -I - "$root" "$source" "$name" "$url" "$hash_path" <<'PY'
import datetime
import hashlib
import json
import os
import sys

root, source_path, name, url, hash_path = sys.argv[1:]
size = 0
pieces = []
with open(source_path, "rb") as source:
    while True:
        piece = source.read(1 << 20)
        if not piece:
            break
        size += len(piece)
        pieces.append(hashlib.sha1(piece).digest())

def bencode(value):
    if isinstance(value, int):
        return b"i" + str(value).encode() + b"e"
    if isinstance(value, bytes):
        return str(len(value)).encode() + b":" + value
    if isinstance(value, dict):
        return b"d" + b"".join(bencode(key) + bencode(value[key]) for key in sorted(value)) + b"e"
    raise TypeError(type(value))

info = {b"length": size, b"name": name.encode(), b"piece length": 1 << 20, b"pieces": b"".join(pieces)}
infohash = hashlib.sha1(bencode(info)).hexdigest()
with open(os.path.join(root, infohash + ".torrent"), "wb") as output:
    output.write(bencode({b"info": info, b"url-list": url.encode()}))
with open(hash_path, "w") as output:
    output.write(infohash)
with open(os.path.join(root, ".metadata", "layout_version"), "w") as output:
    output.write("2\n")
now = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
with open(os.path.join(root, ".metadata", "state", infohash + ".json"), "w") as output:
    json.dump({"id": infohash, "info_hash": infohash, "name": name, "state": "ready", "created_at": now, "updated_at": now}, output)
PY
}
# This metadata-only torrent exercises already visited multi-level directories.
# No payload read is needed to publish, stat, or download its managed subtitles.
create_subtitle_fixture() {
	local root="$1" hash_path="$2"
	python3 -I - "$root" "$hash_path" <<'PY'
import datetime
import hashlib
import json
import os
import sys

root, hash_path = sys.argv[1:]


def bencode(value):
    if isinstance(value, int):
        return b"i" + str(value).encode() + b"e"
    if isinstance(value, bytes):
        return str(len(value)).encode() + b":" + value
    if isinstance(value, list):
        return b"l" + b"".join(bencode(item) for item in value) + b"e"
    if isinstance(value, dict):
        return b"d" + b"".join(bencode(key) + bencode(value[key]) for key in sorted(value)) + b"e"
    raise TypeError(type(value))


paths = [b"top.mkv", b"Season 1/deep/Movie.mkv", b"notes.txt"]
content = b"subtitle fixture payload\n"
info = {
    b"name": b"Collection",
    b"files": [{b"length": len(content), b"path": path.split(b"/")} for path in paths],
    b"piece length": 1 << 20,
    b"pieces": hashlib.sha1(content * len(paths)).digest(),
}
infohash = hashlib.sha1(bencode(info)).hexdigest()
with open(os.path.join(root, infohash + ".torrent"), "wb") as output:
    output.write(bencode({b"info": info}))
now = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
with open(os.path.join(root, ".metadata", "state", infohash + ".json"), "w") as output:
    json.dump({"id": infohash, "info_hash": infohash, "name": "Collection", "state": "ready", "created_at": now, "updated_at": now}, output)
with open(hash_path, "w") as output:
    output.write(infohash)
PY
}

create_fixture "$torrents_dir" "$webseed_dir/payload.bin" payload.bin \
	"http://host.docker.internal:$webseed_port/payload.bin" "$work_dir/infohash"
torrent_hash="$(<"$work_dir/infohash")"
canonical_torrent="$torrents_dir/$torrent_hash.torrent"
cp -- "$canonical_torrent" "$torrents_dir/manual-copy.torrent"

if [[ -n "${TORRENTFS_SMOKE_IMAGE:-}" ]]; then
	probe 'inspect prebuilt application image' docker image inspect "$IMAGE" >/dev/null
else
	printf 'docker SMB smoke: building native application image for %s:%s\n' "$SMOKE_UID" "$SMOKE_GID"
	bounded "$DOCKER_BUILD_TIMEOUT" 'build application image' docker build --tag "$IMAGE" "$ROOT_DIR"
	image_built=1
fi
if [[ -n "${TORRENTFS_SMOKE_CLIENT_IMAGE:-}" ]]; then
	probe 'inspect prebuilt client image' docker image inspect "$CLIENT_IMAGE" >/dev/null
else
	printf 'docker SMB smoke: building SMB client image\n'
	bounded "$DOCKER_BUILD_TIMEOUT" 'build SMB client image' docker build --tag "$CLIENT_IMAGE" - <<'EOF'
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends cifs-utils smbclient ca-certificates curl && rm -rf /var/lib/apt/lists/*
EOF
	client_image_built=1
fi
bounded 30 'create SMB network' docker network create "$NETWORK" >/dev/null
network_created=1

smb_user=torrentfs
smb_password="torrentfs-smoke-${BASHPID}-${RANDOM}"
credentials_file="$work_dir/$CLIENT_CREDENTIALS_NAME"
printf 'username=%s\npassword=%s\n' "$smb_user" "$smb_password" >"$credentials_file"
chmod 0400 "$credentials_file"

has_log_line() {
	grep -Fx -- "$2" <<<"$1" >/dev/null
}

start_app() {
	local app="$1" app_torrents="$2" http_auth="${3:-true}" read_timeout="${4:-30s}" strm_enabled="${5:-false}"
	local env_args=(
		--env "PUID=$SMOKE_UID"
		--env "PGID=$SMOKE_GID"
		--env TORRENTFS_FILESYSTEM_BACKEND=smb
		--env "TORRENTFS_USERNAME=$smb_user"
		--env "TORRENTFS_PASSWORD=$smb_password"
		# The cache capacity accepts only a byte quantity with an explicit unit;
		# CACHE_BYTES itself stays a plain number.
		--env "TORRENTFS_CACHE_CAPACITY=${CACHE_BYTES}B"
		--env "TORRENTFS_FILESYSTEM_READ_TIMEOUT=$read_timeout"
	)
	if [[ "$http_auth" == true ]]; then
		env_args+=(
			--env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080
			--env TORRENTFS_HTTP_AUTH_ENABLED=true
		)
	else
		env_args+=(--env TORRENTFS_HTTP_LISTEN_ADDR= --env TORRENTFS_HTTP_AUTH_ENABLED=false)
	fi
	if [[ "$strm_enabled" == true ]]; then
		env_args+=(
			--env "TORRENTFS_STRM_BASE_URL=http://$app:8080"
		)
	fi
	app_names+=("$app")
	bounded "$DOCKER_OP_TIMEOUT" "start $app" docker run --detach --name "$app" \
		--network "$NETWORK" \
		--add-host host.docker.internal:host-gateway \
		--user "$SMOKE_UID:$SMOKE_GID" \
		--cap-drop ALL \
		--security-opt no-new-privileges:true \
		--publish 127.0.0.1::1445 \
		--mount "type=bind,src=$app_torrents,dst=/torrents" \
		"${env_args[@]}" \
		"$IMAGE" >/dev/null
	local deadline=$((SECONDS + START_TIMEOUT)) state output
	while (( SECONDS < deadline )); do
		state="$(probe "inspect $app" docker inspect --format '{{.State.Status}}' "$app" 2>/dev/null || true)"
		if [[ "$state" == exited || "$state" == dead ]]; then
			output="$(logs "$app")"
			fail "$app exited before SMB readiness (state=$state): $output"
		fi
		if [[ "$state" == running ]]; then
			output="$(logs "$app")"
			if [[ "$output" == *'smb listening'* ]]; then
				return 0
			fi
		fi
		sleep 0.2
done
	fail "$app did not become ready: $(logs "$app")"
}

http_client_authenticate() {
	local app="$1"
	bounded "$CLIENT_TIMEOUT" "HTTP authentication against $app" docker run --rm --network "$NETWORK" \
		--env "APP=$app" \
		--env "USERNAME=$smb_user" \
		--env "PASSWORD=$smb_password" \
		"$CLIENT_IMAGE" sh -eu -c '
		status=0
		for _ in $(seq 1 60); do
			status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 \
				--output /dev/null --write-out "%{http_code}" "http://$APP:8080/" || true)"
			if [ "$status" = 200 ]; then
				break
			fi
			sleep 0.2
		done
		[ "$status" = 200 ]
		status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 \
			--output /dev/null --write-out "%{http_code}" "http://$APP:8080/api/v1/torrents")"
		[ "$status" = 401 ]
		status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 \
			--header "Content-Type: application/json" \
			--data "{\"password\":\"$PASSWORD\"}" \
			--output /dev/null --write-out "%{http_code}" "http://$APP:8080/api/v1/auth/login")"
		[ "$status" = 200 ]
	'
}

# Login and upload through the same authenticated API used by the Web UI. The
# token stays inside the disposable client, never in the URL or smoke output.
subtitle_upload() {
	local app="$1" torrent_id="$2" video_path="$3" filename="$4" source="$5"
	bounded "$CLIENT_TIMEOUT" "upload subtitle against $app" docker run --rm --network "$NETWORK" \
		--env "APP=$app" --env "PASSWORD=$smb_password" \
		--env "TORRENT_ID=$torrent_id" --env "VIDEO_PATH=$video_path" --env "FILENAME=$filename" \
		--mount "type=bind,src=$source,dst=/input/subtitle,readonly" \
		"$CLIENT_IMAGE" sh -eu -c '
			curl --silent --show-error --fail --connect-timeout 2 --max-time 10 \
				--header "Content-Type: application/json" --data "{\"password\":\"$PASSWORD\"}" \
				--output /tmp/login.json "http://$APP:8080/api/v1/auth/login"
			token="$(sed -n '\''s/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'\'' /tmp/login.json)"
			test -n "$token"
			status="$(curl --silent --show-error --connect-timeout 2 --max-time 20 \
				--request PUT --header "Authorization: Bearer $token" \
				--form "video_path=$VIDEO_PATH" --form "file=@/input/subtitle;filename=$FILENAME" \
				--output /tmp/upload.json --write-out "%{http_code}" \
				"http://$APP:8080/api/v1/torrents/$TORRENT_ID/subtitles")"
			case "$status" in
			200 | 201) ;;
			*) echo "subtitle upload returned HTTP $status" >&2; exit 1 ;;
			esac
		' || fail "subtitle upload failed
app logs:
$(logs "$app")
web seed log:
$(tail -20 "$work_dir/webseed.log")"
}

subtitle_smb_check() {
	local app="$1" share="$2" directory="$3" filename="$4" source="$5"
	bounded "$CLIENT_TIMEOUT" "list/stat/get subtitle against $app/$share" docker run --rm --network "$NETWORK" \
		--env "APP=$app" --env "SHARE=$share" --env "DIRECTORY=$directory" --env "FILENAME=$filename" \
		--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
		--mount "type=bind,src=$source,dst=/input/expected,readonly" \
		"$CLIENT_IMAGE" sh -eu -c '
			client() {
				smbclient -p 1445 "//$APP/$SHARE" -A /run/secrets/smb-credentials -m SMB3 \
					-t '"$CLIENT_SMB_TIMEOUT"' -c "$1"
			}
			expected_size="$(wc -c </input/expected)"
			client "cd \"$DIRECTORY\"; ls" >/tmp/subtitle-list.log 2>&1 || {
				cat /tmp/subtitle-list.log >&2; exit 1;
			}
			# These fixture leaf names have no spaces. Check only ls output, so
			# successful allinfo/get cannot hide a stale directory listing.
			awk -v name="$FILENAME" -v size="$expected_size" '\''
				$1 == name && $3 == size { found = 1 }
				END { exit !found }
			'\'' /tmp/subtitle-list.log || { cat /tmp/subtitle-list.log >&2; exit 1; }
			client "cd \"$DIRECTORY\"; allinfo \"$FILENAME\"" >/tmp/subtitle-stat.log 2>&1 || {
				cat /tmp/subtitle-stat.log >&2; exit 1;
			}
			awk -v size="$expected_size" '\''
				$1 == "stream:" && $2 == "[::$DATA]," && $3 == size && $4 == "bytes" { found = 1 }
				END { exit !found }
			'\'' /tmp/subtitle-stat.log || { cat /tmp/subtitle-stat.log >&2; exit 1; }
			client "cd \"$DIRECTORY\"; get \"$FILENAME\" /tmp/subtitle" >/tmp/subtitle-get.log 2>&1 || {
				cat /tmp/subtitle-get.log >&2; exit 1;
			}
			test "$(wc -c </tmp/subtitle)" -eq "$expected_size"
			test "$(sha256sum /tmp/subtitle | cut -d" " -f1)" = "$(sha256sum /input/expected | cut -d" " -f1)"
		' || fail "subtitle list/stat/get failed on $share
app logs:
$(logs "$app")
web seed log:
$(tail -20 "$work_dir/webseed.log")"
}

# smb_client runs one smbclient operation with the shared client bound. The
# explicit -t keeps the per-operation timeout of the client itself bounded and
# inside that bound, so a stalled server surfaces as a failure with the app log
# instead of hanging the job.
smb_share_client() {
	local app="$1" share="$2"
	shift 2
	bounded "$CLIENT_TIMEOUT" "SMB client against $app/$share" docker run --rm --network "$NETWORK" \
		--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
		"$CLIENT_IMAGE" smbclient -p 1445 "//$app/$share" \
		-A "/run/secrets/$CLIENT_CREDENTIALS_NAME" -m SMB3 -t "$CLIENT_SMB_TIMEOUT" "$@"
}

smb_client() {
	local app="$1"
	shift
	smb_share_client "$app" torrentfs "$@"
}

smb_list_shares() {
	local app="$1"
	bounded "$CLIENT_TIMEOUT" "enumerate SMB shares on $app" docker run --rm --network "$NETWORK" \
		--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
		"$CLIENT_IMAGE" smbclient -p 1445 -L "$app" -g \
		-A "/run/secrets/$CLIENT_CREDENTIALS_NAME" -m SMB3 -t "$CLIENT_SMB_TIMEOUT"
}

printf 'docker SMB smoke: starting HTTP+SMB authenticated share as %s:%s\n' "$SMOKE_UID" "$SMOKE_GID"
app_normal="${APP_PREFIX}-normal"
start_app "$app_normal" "$torrents_dir" true
assert_native_app "$app_normal"
share_enumeration="$(smb_list_shares "$app_normal" 2>&1)" || fail "share enumeration failed: $share_enumeration"
grep -Eq '^Disk\|torrentfs\|' <<<"$share_enumeration" || fail 'original share missing from srvsvc enumeration'
if grep -Eq '^Disk\|torrentfs-strm\|' <<<"$share_enumeration"; then fail 'disabled STRM share was enumerated'; fi
disabled_share_status=0
disabled_share_output="$(smb_share_client "$app_normal" torrentfs-strm -c 'ls' 2>&1)" || disabled_share_status=$?
[[ "$disabled_share_status" != 0 && "$disabled_share_output" == *NT_STATUS_BAD_NETWORK_NAME* ]] ||
	fail 'STRM-disabled startup exposed torrentfs-strm or failed for an unrelated reason'
http_client_authenticate "$app_normal"
printf 'docker SMB smoke: combined HTTP authentication and disabled STRM isolation passed\n'

published_smb_port="$(probe 'inspect published SMB port' docker port "$app_normal" 1445/tcp | awk -F: 'NF { print $NF; exit }')"
[[ -n "$published_smb_port" ]] || fail 'SMB port 1445/tcp was not published'
[[ "$(docker port "$app_normal" 137/udp 2>/dev/null || true)" == '' ]] || fail 'unexpected NetBIOS UDP port was published'
[[ "$(docker port "$app_normal" 139/tcp 2>/dev/null || true)" == '' ]] || fail 'unexpected NetBIOS TCP port was published'

listing_output=''
if ! listing_output="$(smb_client "$app_normal" -c 'ls' 2>&1)"; then
	fail "authenticated SMB directory listing failed: $listing_output\napp logs:\n$(logs "$app_normal")"
fi
[[ "$listing_output" == *'payload.bin'* ]] || fail "SMB share root did not list payload.bin: $listing_output"
wrong_credentials="$work_dir/wrong-credentials"
printf 'username=%s\npassword=definitely-wrong\n' "$smb_user" >"$wrong_credentials"
chmod 0400 "$wrong_credentials"
wrong_status=0
bounded "$CLIENT_TIMEOUT" 'SMB client with a wrong password' docker run --rm --network "$NETWORK" \
	--mount "type=bind,src=$wrong_credentials,dst=/run/secrets/wrong,readonly" \
	"$CLIENT_IMAGE" smbclient -p 1445 "//$app_normal/torrentfs" -A /run/secrets/wrong -m SMB3 \
	-t "$CLIENT_SMB_TIMEOUT" -c 'ls' >/dev/null 2>&1 || wrong_status=$?
[[ "$wrong_status" != 0 ]] || fail 'wrong SMB password was accepted'

guest_status=0
bounded "$CLIENT_TIMEOUT" 'SMB guest client' docker run --rm --network "$NETWORK" "$CLIENT_IMAGE" \
	smbclient -p 1445 "//$app_normal/torrentfs" -N -m SMB3 -t "$CLIENT_SMB_TIMEOUT" -c 'ls' >/dev/null 2>&1 || guest_status=$?
[[ "$guest_status" != 0 ]] || fail 'SMB guest access was accepted'

# The whole file is read and hashed inside the client container: the transfer
# itself stays bounded, and the copied bytes never depend on a host bind mount.
expected_hash="$(sha256sum "$webseed_dir/payload.bin" | awk '{ print $1 }')"
full_read_log="$work_dir/full-read.log"
full_read_status=0
bounded "$CLIENT_TIMEOUT" 'full SMB read' docker run --rm --network "$NETWORK" \
	--env "APP=$app_normal" \
	--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
	"$CLIENT_IMAGE" sh -eu -c '
		smbclient -p 1445 "//$APP/torrentfs" -A "/run/secrets/'"$CLIENT_CREDENTIALS_NAME"'" -m SMB3 \
			-t '"$CLIENT_SMB_TIMEOUT"' -c "get payload.bin /tmp/payload.bin" >/dev/null
		sha256sum /tmp/payload.bin | cut -d" " -f1
	' >"$work_dir/full-read.out" 2>"$full_read_log" || full_read_status=$?
full_read_hash="$(tr -d '[:space:]' <"$work_dir/full-read.out")"
if (( full_read_status != 0 )); then
	fail "full SMB read failed (status=$full_read_status)
client output:
$(sed -n '1,20p' "$full_read_log")
app logs:
$(logs "$app_normal")
web seed log:
$(tail -20 "$work_dir/webseed.log")"
fi
[[ "$full_read_hash" == "$expected_hash" ]] || fail "SMB full-read hash $full_read_hash differs from source $expected_hash"

# The container runs as its own runtime identity while the bind-mounted torrents
# directory belongs to the host user, so `.metadata` and its directories must
# stay traversable for both; a leaked restrictive umask made them 0700 and broke
# inspection and cleanup. `peer_id` is deliberately private (0600) and is not
# part of this check.
[[ -d "$torrents_dir/.metadata" ]] || fail 'torrentfs did not create .metadata in the mounted torrents directory'
for dir in "$torrents_dir/.metadata" "$torrents_dir/.metadata/state"; do
	[[ -d "$dir" ]] || continue
	[[ -r "$dir" && -x "$dir" ]] || fail "host user cannot traverse $dir created by the container identity"
done
[[ -r "$torrents_dir/.metadata/instance.lock" ]] || fail 'host user cannot read the instance lock in .metadata'
printf 'docker SMB smoke: .metadata stays traversable across the container/host UID boundary\n'

write_status=0
smb_client "$app_normal" -c 'put /etc/hosts write-probe' >/dev/null 2>&1 || write_status=$?
[[ "$write_status" != 0 ]] || fail 'SMB read-only share accepted a write'
metadata_status=0
smb_client "$app_normal" -c 'get .metadata/instance.lock /tmp/metadata-leak' >/dev/null 2>&1 || metadata_status=$?
[[ "$metadata_status" != 0 ]] || fail 'SMB share exposed /torrents/.metadata'
printf 'docker SMB smoke: authentication, listing, full read, guest denial, and write denial passed\n'

# Positional reads go through a real kernel CIFS mount when the host can provide
# one, because that is the only client path that issues true pread-style ranges
# instead of a sequential download. Hosts without the CIFS module are reported
# as such; a mount that succeeds but returns wrong bytes is never skipped.
if [[ "${TORRENTFS_SMB_SKIP_CIFS:-0}" == 1 ]]; then
	printf 'docker SMB smoke: skipping CIFS positional-read check because TORRENTFS_SMB_SKIP_CIFS=1\n'
else
	expected_range="$(dd if="$webseed_dir/payload.bin" bs=1 skip="$RANDOM_OFFSET" count="$RANDOM_LENGTH" status=none | sha256sum | awk '{ print $1 }')"
	range_output="$work_dir/range-output"
	range_status=0
	bounded 240 'CIFS positional read' docker run --rm --privileged --network "$NETWORK" \
		--env "APP_HOST=$app_normal" \
		--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
		"$CLIENT_IMAGE" sh -ceu '
		command -v mount.cifs >/dev/null || { echo "mount.cifs is missing"; exit 3; }
		mkdir -p /mnt/torrentfs-client
		mount.cifs "//$APP_HOST/torrentfs" /mnt/torrentfs-client -o "credentials=/run/secrets/smb-credentials,vers=3.0,port=1445,ro"
		trap "umount /mnt/torrentfs-client" EXIT
		dd if=/mnt/torrentfs-client/payload.bin of=/tmp/range.bin bs=1 skip='"$RANDOM_OFFSET"' count='"$RANDOM_LENGTH"' status=none
		sha256sum /tmp/range.bin
	' >"$range_output" 2>&1 || range_status=$?
	if (( range_status != 0 )); then
		if grep -Eq "$CIFS_UNAVAILABLE_PATTERN" "$range_output"; then
			printf 'docker SMB smoke: host cannot mount CIFS (%s); the SMB protocol tests still run; kernel CIFS range/concurrency validation remains a manual gate on a CIFS-capable host\n' \
				"$(head -n1 "$range_output")"
		else
			fail "CIFS positional read failed (status=$range_status): $(sed -n '1,20p' "$range_output")"
		fi
	else
		actual_range="$(awk '{ print $1 }' "$range_output")"
		[[ "$actual_range" == "$expected_range" ]] || fail "CIFS positional-read hash $actual_range differs from source $expected_range"
		cifs_available=1
		printf 'docker SMB smoke: CIFS positional read at offset %s passed\n' "$RANDOM_OFFSET"
	fi
fi

stop_normal_status=0
bounded "$DOCKER_OP_TIMEOUT" "signal $app_normal" docker kill --signal TERM "$app_normal" >/dev/null
stop_normal_status="$(bounded "$DOCKER_OP_TIMEOUT" "wait for $app_normal" docker wait "$app_normal")"
[[ "$stop_normal_status" == 0 ]] || fail "normal SMB shutdown returned $stop_normal_status"
normal_logs="$(logs "$app_normal")"
[[ "$normal_logs" != *"$smb_password"* ]] || fail 'application logs contained the SMB password'
[[ "$normal_logs" == *'stopped'* ]] || fail 'native shutdown did not report completion'
printf 'docker SMB smoke: normal SIGTERM shutdown was bounded and ordered\n'

if (( cifs_available )); then
	concurrent_torrents="$work_dir/concurrent-torrents"
	mkdir -p "$concurrent_torrents"
	chmod 0777 "$concurrent_torrents"
	python3 -I - "$concurrent_torrents" "$canonical_torrent" "$torrents_dir/.metadata/state/$torrent_hash.json" <<'PY'
import datetime
import hashlib
import json
import os
import shutil
import sys

root, payload_torrent, payload_state = sys.argv[1:]
metadata = os.path.join(root, ".metadata")
state_dir = os.path.join(metadata, "state")
for path in (metadata, state_dir, os.path.join(metadata, "pending")):
    os.makedirs(path, mode=0o777, exist_ok=True)
    os.chmod(path, 0o777)
with open(os.path.join(metadata, "layout_version"), "w") as output:
    output.write("2\n")
shutil.copyfile(payload_torrent, os.path.join(root, os.path.basename(payload_torrent)))
shutil.copyfile(payload_state, os.path.join(state_dir, os.path.basename(payload_state)))


def bencode(value):
    if isinstance(value, int):
        return b"i" + str(value).encode() + b"e"
    if isinstance(value, bytes):
        return str(len(value)).encode() + b":" + value
    if isinstance(value, list):
        return b"l" + b"".join(bencode(item) for item in value) + b"e"
    if isinstance(value, dict):
        return b"d" + b"".join(bencode(key) + bencode(value[key]) for key in sorted(value)) + b"e"
    raise TypeError(type(value))


piece_length = 1 << 20
files, pieces = [], []
for index in range(16):
    files.append({b"length": piece_length, b"path": [("video-%02d.bin" % index).encode()]})
    pieces.append(hashlib.sha1(bytes([index + 1]) * piece_length).digest())
info = {b"name": b"unavailable", b"files": files, b"piece length": piece_length, b"pieces": b"".join(pieces)}
info_bytes = bencode(info)
infohash = hashlib.sha1(info_bytes).hexdigest()
with open(os.path.join(root, infohash + ".torrent"), "wb") as output:
    output.write(bencode({b"info": info}))
now = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
with open(os.path.join(state_dir, infohash + ".json"), "w") as output:
    json.dump({"id": infohash, "info_hash": infohash, "name": "unavailable", "state": "ready", "created_at": now, "updated_at": now}, output)
PY
	app_concurrent="${APP_PREFIX}-concurrent"
	# Keep server and client slow-read deadlines above the 30-second healthy-read bound.
	start_app "$app_concurrent" "$concurrent_torrents" true 45s
	expected_prefix="$(dd if="$webseed_dir/payload.bin" bs=64K count=4096 iflag=count_bytes status=none | sha256sum | awk '{ print $1 }')"
	client_concurrent="${APP_PREFIX}-concurrent-client"
	app_names+=("$client_concurrent")
	bounded "$DOCKER_OP_TIMEOUT" 'start concurrent CIFS client' docker run --detach --privileged \
		--name "$client_concurrent" --network "$NETWORK" \
		--env "APP_HOST=$app_concurrent" \
		--env "EXPECTED_PREFIX=$expected_prefix" \
		--env "EXPECTED_RANGE=$expected_range" \
		--env "RANDOM_OFFSET=$RANDOM_OFFSET" \
		--env "RANDOM_LENGTH=$RANDOM_LENGTH" \
		--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
		"$CLIENT_IMAGE" sh -ceu '
		mkdir -p /mnt/torrentfs-client
		mount.cifs "//$APP_HOST/torrentfs" /mnt/torrentfs-client -o "credentials=/run/secrets/smb-credentials,vers=3.0,port=1445,ro"
		trap "umount -l /mnt/torrentfs-client >/dev/null 2>&1 || true" EXIT
		if ! timeout --kill-after=2 30 dd if=/mnt/torrentfs-client/payload.bin of=/tmp/prefix \
			bs=64K count=4096 iflag=count_bytes status=none; then
			printf "FAIL concurrent healthy prefix read\n"
			exit 1
		fi
		[ "$(sha256sum /tmp/prefix | cut -d" " -f1)" = "$EXPECTED_PREFIX" ] || {
			printf "FAIL concurrent healthy prefix hash\n"
			exit 1
		}
		printf "CONTROL concurrent healthy prefix passed\n"
		pids=""
		# Client cancellation must not release stalled reads before the healthy-read deadline.
		for index in $(seq 0 15); do
			name="$(printf "video-%02d.bin" "$index")"
			timeout --kill-after=2 60 dd if="/mnt/torrentfs-client/unavailable/$name" of=/dev/null \
				bs=64K count=4096 iflag=count_bytes status=none >"/tmp/missing-$index.log" 2>&1 &
			pids="$pids $!"
		done
		sleep 0.25
		for pid in $pids; do
			kill -0 "$pid" 2>/dev/null || {
				printf "FAIL missing video read ended before the concurrent probe\n"
				exit 1
			}
		done
		printf "CONTROL 16 missing video reads pending\n"
		if ! timeout --kill-after=2 30 dd if=/mnt/torrentfs-client/payload.bin of=/tmp/range \
			bs=64K skip="$RANDOM_OFFSET" count="$RANDOM_LENGTH" iflag=skip_bytes,count_bytes status=none; then
			printf "FAIL healthy cold read with missing reads pending failed or exceeded 30s\n"
			exit 1
		fi
		[ "$(sha256sum /tmp/range | cut -d" " -f1)" = "$EXPECTED_RANGE" ] || {
			printf "FAIL healthy cold read with missing reads pending returned wrong data\n"
			exit 1
		}
		printf "PASS same-connection concurrent cold read\n"
	' >/dev/null
	concurrent_deadline=$((SECONDS + 90))
	concurrent_passed=0
	concurrent_output=''
	while (( SECONDS < concurrent_deadline )); do
		concurrent_output="$(logs "$client_concurrent")"
		if [[ "$concurrent_output" == *'FAIL '* ]]; then
			fail "same-connection concurrent read failed: $concurrent_output
app logs:
$(logs "$app_concurrent")"
		fi
		if [[ "$concurrent_output" == *'PASS same-connection concurrent cold read'* ]]; then
			concurrent_passed=1
			break
		fi
		concurrent_state="$(probe 'inspect concurrent client' docker inspect --format '{{.State.Status}}' "$client_concurrent")"
		if [[ "$concurrent_state" == exited || "$concurrent_state" == dead ]]; then
			fail "concurrent CIFS client exited without a successful read: $concurrent_output"
		fi
		sleep 0.2
	done
	(( concurrent_passed )) || fail "same-connection concurrent read exceeded 90s: $concurrent_output"
	printf 'docker SMB smoke: same-connection cold read passed with 16 missing videos pending\n'
	bounded "$DOCKER_OP_TIMEOUT" 'stop concurrent app' docker kill --signal TERM "$app_concurrent" >/dev/null
	concurrent_status="$(bounded "$DOCKER_OP_TIMEOUT" 'wait for concurrent app' docker wait "$app_concurrent")"
	[[ "$concurrent_status" == 0 ]] || fail "concurrent SMB shutdown returned $concurrent_status"
	bounded 30 'remove concurrent client' docker rm -f "$client_concurrent" >/dev/null
else
	printf 'docker SMB smoke: same-connection concurrent-read check not run (CIFS unavailable or explicitly skipped)\n'
fi

printf 'docker SMB smoke: starting isolated original-file and STRM shares\n'
strm_torrents="$work_dir/strm-torrents"
create_fixture "$strm_torrents" "$webseed_dir/Movie.mkv" Movie.mkv \
	"http://host.docker.internal:$webseed_port/Movie.mkv" "$work_dir/strm-infohash"
create_subtitle_fixture "$strm_torrents" "$work_dir/subtitle-infohash"
subtitle_hash="$(<"$work_dir/subtitle-infohash")"
app_strm="${APP_PREFIX}-strm"
start_app "$app_strm" "$strm_torrents" true 30s true
assert_native_app "$app_strm"
share_enumeration="$(smb_list_shares "$app_strm" 2>&1)" || fail "dual-share enumeration failed: $share_enumeration"
for share in torrentfs torrentfs-strm; do
	grep -Eq "^Disk\\|$share\\|" <<<"$share_enumeration" || fail "$share missing from srvsvc enumeration"
done
strm_original_listing="$(smb_client "$app_strm" -c 'ls' 2>&1)"
[[ "$strm_original_listing" == *Movie.mkv* ]] || fail 'original share lost the video in STRM-enabled mode'
strm_listing="$(smb_share_client "$app_strm" torrentfs-strm -c 'ls' 2>&1)"
[[ "$strm_listing" == *Movie.strm* ]] || fail 'STRM share did not list Movie.strm'
bounded "$CLIENT_TIMEOUT" 'read STRM over SMB and request HTTP HEAD without payload' docker run --rm --network "$NETWORK" \
	--env "APP=$app_strm" --env "FILE_SIZE=$FILE_SIZE" \
	--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
	--mount "type=bind,src=$client_output_dir,dst=/output" \
	"$CLIENT_IMAGE" sh -eu -c '
		smbclient -p 1445 "//$APP/torrentfs-strm" -A /run/secrets/smb-credentials -m SMB3 \
			-t '"$CLIENT_SMB_TIMEOUT"' -c "get Movie.strm /output/Movie.strm" >/dev/null
		test "$(wc -l </output/Movie.strm)" -eq 1
		test "$(wc -c </output/Movie.strm)" -lt 4096
		url="$(cat /output/Movie.strm)"
		case "$url" in
		"http://$APP:8080/stream/"*) ;;
		*) echo "STRM did not contain the configured player-reachable stream URL" >&2; exit 1 ;;
		esac
		status="$(curl --silent --show-error --head --connect-timeout 2 --max-time 10 \
			--dump-header /tmp/head.headers --output /dev/null --write-out "%{http_code}" "$url")"
		test "$status" = 200
		tr -d "\r" </tmp/head.headers | grep -Fxi "Content-Length: $FILE_SIZE" >/dev/null
	'
if grep -Fx Movie.mkv "$webseed_reads" >/dev/null; then
	fail 'STRM listing/open/read or HTTP HEAD fetched video payload'
fi
# Readdir must happen before publication: same StableAttr reuses the old inode
# even after this client disconnects. Exercise top and deep directories in both
# shares, then require fresh listings, live metadata, and exact subtitle bytes.
for share in torrentfs torrentfs-strm; do
	visited_listing="$(smb_share_client "$app_strm" "$share" \
		-c 'cd Collection; ls; cd "Season 1"; ls; cd deep; ls' 2>&1)" ||
		fail "could not visit subtitle directories on $share: $visited_listing
app logs:
$(logs "$app_strm")"
	[[ "$visited_listing" == *'notes.txt'* && "$visited_listing" == *'Movie.'* ]] ||
		fail "multi-file subtitle fixture was not listed on $share: $visited_listing"
done
printf 'top subtitle\n' >"$work_dir/top.srt"
subtitle_upload "$app_strm" "$subtitle_hash" top.mkv top.srt "$work_dir/top.srt"
for share in torrentfs torrentfs-strm; do
	subtitle_smb_check "$app_strm" "$share" Collection top.srt "$work_dir/top.srt"
done
for version in initial same-length longer; do
	case "$version" in
	initial) printf 'old subtitle\n' >"$work_dir/Movie.srt" ;;
	same-length) printf 'new subtitle\n' >"$work_dir/Movie.srt" ;;
	longer) printf 'a much longer replacement subtitle\nsecond subtitle line\n' >"$work_dir/Movie.srt" ;;
	esac
	subtitle_upload "$app_strm" "$subtitle_hash" 'Season 1/deep/Movie.mkv' Movie.srt "$work_dir/Movie.srt"
	for share in torrentfs torrentfs-strm; do
		subtitle_smb_check "$app_strm" "$share" 'Collection/Season 1/deep' Movie.srt "$work_dir/Movie.srt"
	done
done
printf 'docker SMB smoke: visited multi-file directories refresh uploaded and replaced subtitles on both shares\n'

strm_expected_range="$(dd if="$webseed_dir/Movie.mkv" bs=1 skip="$RANDOM_OFFSET" count="$RANDOM_LENGTH" status=none | sha256sum | awk '{ print $1 }')"
bounded "$CLIENT_TIMEOUT" 'signed HTTP Range and tampered-signature denial' docker run --rm --network "$NETWORK" \
	--env "APP=$app_strm" --env "EXPECTED_RANGE=$strm_expected_range" \
	--env "RANDOM_OFFSET=$RANDOM_OFFSET" --env "RANDOM_LENGTH=$RANDOM_LENGTH" --env "FILE_SIZE=$FILE_SIZE" \
	--mount "type=bind,src=$client_output_dir,dst=/input,readonly" \
	"$CLIENT_IMAGE" sh -eu -c '
		url="$(cat /input/Movie.strm)"
		end=$((RANDOM_OFFSET + RANDOM_LENGTH - 1))
		status="$(curl --silent --show-error --connect-timeout 2 --max-time 60 \
			--header "Range: bytes=$RANDOM_OFFSET-$end" --dump-header /tmp/range.headers \
			--output /tmp/range --write-out "%{http_code}" "$url")"
		test "$status" = 206
		tr -d "\r" </tmp/range.headers | grep -Fxi "Content-Range: bytes $RANDOM_OFFSET-$end/$FILE_SIZE" >/dev/null
		test "$(wc -c </tmp/range)" -eq "$RANDOM_LENGTH"
		test "$(sha256sum /tmp/range | cut -d" " -f1)" = "$EXPECTED_RANGE"
		location="${url#http://$APP:8080/stream/}"
		infohash="${location%%/*}"
		location="${location#*/}"
		signature="${location%%/*}"
		path="${location#*/}"
		case "$signature" in
		0*) signature="1${signature#?}" ;;
		*) signature="0${signature#?}" ;;
		esac
		status="$(curl --silent --show-error --connect-timeout 2 --max-time 10 \
			--output /dev/null --write-out "%{http_code}" \
			"http://$APP:8080/stream/$infohash/$signature/$path")"
		test "$status" = 403
	'
for share in torrentfs torrentfs-strm; do
	write_status=0
	smb_share_client "$app_strm" "$share" -c 'put /etc/hosts write-probe' >/dev/null 2>&1 || write_status=$?
	[[ "$write_status" != 0 ]] || fail "STRM-enabled $share accepted a write"
	metadata_status=0
	smb_share_client "$app_strm" "$share" -c 'get .metadata/instance.lock /tmp/metadata-leak' >/dev/null 2>&1 || metadata_status=$?
	[[ "$metadata_status" != 0 ]] || fail "STRM-enabled $share exposed session metadata"
done
strm_guest_status=0
bounded "$CLIENT_TIMEOUT" 'STRM guest client' docker run --rm --network "$NETWORK" "$CLIENT_IMAGE" \
	smbclient -p 1445 "//$app_strm/torrentfs-strm" -N -m SMB3 -t "$CLIENT_SMB_TIMEOUT" -c 'ls' >/dev/null 2>&1 || strm_guest_status=$?
[[ "$strm_guest_status" != 0 ]] || fail 'STRM share accepted guest access'
strm_wrong_status=0
bounded "$CLIENT_TIMEOUT" 'STRM client with a wrong password' docker run --rm --network "$NETWORK" \
	--mount "type=bind,src=$wrong_credentials,dst=/run/secrets/wrong,readonly" \
	"$CLIENT_IMAGE" smbclient -p 1445 "//$app_strm/torrentfs-strm" -A /run/secrets/wrong -m SMB3 \
	-t "$CLIENT_SMB_TIMEOUT" -c 'ls' >/dev/null 2>&1 || strm_wrong_status=$?
[[ "$strm_wrong_status" != 0 ]] || fail 'STRM share accepted a wrong password'
bounded "$DOCKER_OP_TIMEOUT" "signal $app_strm" docker kill --signal TERM "$app_strm" >/dev/null
strm_stop_status="$(bounded "$DOCKER_OP_TIMEOUT" "wait for $app_strm" docker wait "$app_strm")"
[[ "$strm_stop_status" == 0 ]] || fail "dual-share shutdown returned $strm_stop_status"
strm_logs="$(logs "$app_strm")"
[[ "$strm_logs" == *'stopped'* ]] || fail 'dual-share native shutdown did not complete'
strm_url="$(cat "$client_output_dir/Movie.strm")"
strm_location="${strm_url#http://$app_strm:8080/stream/}"
strm_location="${strm_location#*/}"
strm_signature="${strm_location%%/*}"
[[ "$strm_logs" != *"$smb_password"* && "$strm_logs" != *"$strm_url"* && "$strm_logs" != *"$strm_signature"* ]] ||
	fail 'STRM-enabled application logs exposed credentials or a signed stream URL'
printf 'docker SMB smoke: dual read-only shares, no-payload HEAD, Range 206/hash, signature 403, and bounded shutdown passed\n'

printf 'docker SMB smoke: starting SMB-only share with the HTTP listener disabled\n'
smb_only_torrents="$work_dir/smb-only-torrents"
mkdir -p "$smb_only_torrents"
chmod 0777 "$smb_only_torrents"
app_smb_only="${APP_PREFIX}-smb-only"
start_app "$app_smb_only" "$smb_only_torrents" false
smb_client "$app_smb_only" -c 'ls' >/dev/null
smb_only_status=0
bounded "$DOCKER_OP_TIMEOUT" "stop $app_smb_only" docker kill --signal TERM "$app_smb_only" >/dev/null || smb_only_status=$?
[[ "$smb_only_status" == 0 ]] || fail "SMB-only shutdown signal failed with status $smb_only_status"
smb_only_status="$(bounded "$DOCKER_OP_TIMEOUT" "wait for $app_smb_only" docker wait "$app_smb_only")"
[[ "$smb_only_status" == 0 ]] || fail "SMB-only shutdown returned $smb_only_status"
smb_only_logs="$(logs "$app_smb_only")"
[[ "$smb_only_logs" != *"$smb_password"* ]] || fail 'SMB-only application logs contained the SMB password'
printf 'docker SMB smoke: SMB-only startup and authentication passed\n'

fault_torrents="$work_dir/fault-torrents"
mkdir -p "$fault_torrents"
# Writable by the container identity, which need not share the host UID.
chmod 0777 "$fault_torrents"
cp -- "$canonical_torrent" "$fault_torrents/$torrent_hash.torrent"
mkdir -p "$fault_torrents/.metadata/state" "$fault_torrents/.metadata/pending"
chmod 0777 "$fault_torrents/.metadata" "$fault_torrents/.metadata/state" "$fault_torrents/.metadata/pending"
cp -- "$torrents_dir/.metadata/layout_version" "$fault_torrents/.metadata/layout_version"
cp -- "$torrents_dir/.metadata/state/$torrent_hash.json" "$fault_torrents/.metadata/state/$torrent_hash.json"
app_torrent_fault="${APP_PREFIX}-torrentfs-fault"
start_app "$app_torrent_fault" "$fault_torrents"
torrentfs_pid="$(all_process_identities "$app_torrent_fault" torrentfs | awk 'NR == 1 { print $1 }')"
[[ "$torrentfs_pid" =~ ^[1-9][0-9]*$ ]] || fail 'could not find native process for fault injection'
bounded 30 'terminate native torrentfs in fault scenario' docker exec "$app_torrent_fault" /bin/sh -c 'kill -KILL "$1"' sh "$torrentfs_pid"
torrent_fault_status="$(bounded "$DOCKER_OP_TIMEOUT" "wait for torrentfs fault" docker wait "$app_torrent_fault")"
[[ "$torrent_fault_status" != 0 ]] || fail 'abrupt native process death was reported as clean'
printf 'docker SMB smoke: abrupt native process death propagated as non-zero container exit\n'

printf 'docker SMB smoke: all checks passed\n'
