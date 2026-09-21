#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly IMAGE="torrentfs-smb-smoke:${BASHPID}"
readonly CLIENT_IMAGE="torrentfs-smb-client:${BASHPID}"
readonly NETWORK="torrentfs-smb-network-${BASHPID}"
readonly APP_PREFIX="torrentfs-smb-app-${BASHPID}"
readonly CLIENT_CREDENTIALS_NAME="smb-credentials"
readonly DOCKER_OP_TIMEOUT=60
readonly DOCKER_BUILD_TIMEOUT=1800
readonly PROBE_TIMEOUT=10
readonly START_TIMEOUT=60
readonly FILE_SIZE=$((64 * 1024 * 1024 + 123))
readonly PIECE_LENGTH=$((1 * 1024 * 1024))
readonly RANDOM_OFFSET=$((40 * 1024 * 1024 + 12345))
readonly RANDOM_LENGTH=8192

work_dir=''
webseed_pid=''
network_created=0
image_built=0
client_image_built=0
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
	if (( client_image_built )); then
		bounded 60 'remove SMB client image' docker image rm "$CLIENT_IMAGE" >/dev/null 2>&1 || true
	fi
	if (( image_built )); then
		bounded 60 'remove SMB image' docker image rm "$IMAGE" >/dev/null 2>&1 || true
	fi
	if [[ -n "$work_dir" ]]; then
		rm -rf -- "$work_dir"
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for command_name in docker python3 sha256sum dd timeout findmnt; do
	command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done
[[ -c /dev/fuse ]] || fail '/dev/fuse is required; run this smoke with --device /dev/fuse'

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/torrentfs-smb.XXXXXX")"
torrents_dir="$work_dir/torrents"
webseed_dir="$work_dir/webseed"
client_output_dir="$work_dir/client-output"
mkdir -p "$torrents_dir" "$webseed_dir" "$client_output_dir"
chmod 0777 "$torrents_dir"

python3 - "$webseed_dir/payload.bin" <<'PY'
import sys

path = sys.argv[1]
size = 64 * 1024 * 1024 + 123
chunk_size = 1 << 20
with open(path, "wb") as output:
    offset = 0
    while offset < size:
        length = min(chunk_size, size - offset)
        output.write(bytes(((offset + index) * 31 + 7) % 251 for index in range(length)))
        offset += length
PY

# The BEP 19 web seed must answer ranged GETs: anacrolix fetches one piece at a
# time, and the standard library handler ignores Range, which would make every
# piece fetch download the whole file.
python3 - "$webseed_dir" "$work_dir/webseed.port" >"$work_dir/webseed.log" 2>&1 <<'PY' &
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

root, port_path = sys.argv[1], sys.argv[2]


class RangedHandler(BaseHTTPRequestHandler):
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


server = ThreadingHTTPServer(("0.0.0.0", 0), RangedHandler)
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

python3 - "$torrents_dir/payload.torrent" "$webseed_dir/payload.bin" "http://host.docker.internal:$webseed_port/payload.bin" <<'PY'
import hashlib
import sys

output_path, source_path, url = sys.argv[1], sys.argv[2], sys.argv[3].encode()
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

info = {b"length": size, b"name": b"payload.bin", b"piece length": 1 << 20, b"pieces": b"".join(pieces)}
with open(output_path, "wb") as output:
    output.write(bencode({b"info": info, b"url-list": url}))
PY

printf 'docker SMB smoke: building application image\n'
bounded "$DOCKER_BUILD_TIMEOUT" 'build application image' docker build --tag "$IMAGE" "$ROOT_DIR"
image_built=1
printf 'docker SMB smoke: building SMB client image\n'
docker build --tag "$CLIENT_IMAGE" - <<'EOF'
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends cifs-utils smbclient ca-certificates && rm -rf /var/lib/apt/lists/*
EOF
client_image_built=1
bounded 30 'create SMB network' docker network create "$NETWORK" >/dev/null
network_created=1

smb_user="$(docker run --rm --entrypoint /bin/sh "$IMAGE" -c "awk -F= '\$1 == \"user\" { print \$2 }' /etc/torrentfs/runtime-identity")"
[[ -n "$smb_user" ]] || fail 'could not determine runtime SMB username'
smb_password="torrentfs-smoke-${BASHPID}-${RANDOM}"
password_file="$work_dir/password"
printf '%s\n' "$smb_password" >"$password_file"
chmod 0400 "$password_file"
credentials_file="$work_dir/$CLIENT_CREDENTIALS_NAME"
printf 'username=%s\npassword=%s\n' "$smb_user" "$smb_password" >"$credentials_file"
chmod 0400 "$credentials_file"

start_app() {
	local app="$1" app_torrents="$2"
	app_names+=("$app")
	bounded "$DOCKER_OP_TIMEOUT" "start $app" docker run --detach --name "$app" \
		--network "$NETWORK" \
		--add-host host.docker.internal:host-gateway \
		--device /dev/fuse \
		--cap-add SYS_ADMIN \
		--cap-add NET_BIND_SERVICE \
		--security-opt apparmor=unconfined \
		--publish 127.0.0.1::445 \
		--mount "type=bind,src=$app_torrents,dst=/torrents" \
		--mount "type=bind,src=$password_file,dst=/run/secrets/smb-password,readonly" \
		--env TORRENTFS_SMB_ENABLED=true \
		--env TORRENTFS_SMB_PASSWORD_FILE=/run/secrets/smb-password \
		--env TORRENTFS_CACHE_CAPACITY_BYTES=8388608 \
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
			if [[ "$output" == *'fuse ready: /mnt/torrentfs'* && "$output" == *'starting smbd on TCP 445'* ]]; then
				printf '%s\n' "$output" | grep -q 'fuse ready: /mnt/torrentfs' || fail "$app did not log FUSE readiness"
				return 0
			fi
		fi
		sleep 0.2
done
	fail "$app did not become ready: $(logs "$app")"
}

smb_client() {
	local app="$1"
	shift
	bounded 60 "SMB client against $app" docker run --rm --network "$NETWORK" \
		--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
		"$CLIENT_IMAGE" smbclient "//$app/torrentfs" \
		-A "/run/secrets/$CLIENT_CREDENTIALS_NAME" -m SMB3 "$@"
}

printf 'docker SMB smoke: starting authenticated share\n'
app_normal="${APP_PREFIX}-normal"
start_app "$app_normal" "$torrents_dir"

published_smb_port="$(probe 'inspect published SMB port' docker port "$app_normal" 445/tcp | awk -F: 'NF { print $NF; exit }')"
[[ -n "$published_smb_port" ]] || fail 'SMB port 445/tcp was not published'
[[ "$(docker port "$app_normal" 137/udp 2>/dev/null || true)" == '' ]] || fail 'unexpected NetBIOS UDP port was published'
[[ "$(docker port "$app_normal" 139/tcp 2>/dev/null || true)" == '' ]] || fail 'unexpected NetBIOS TCP port was published'

listing_output=''
if ! listing_output="$(smb_client "$app_normal" -c 'ls' 2>&1)"; then
	fail "authenticated SMB directory listing failed: $listing_output\napp logs:\n$(logs "$app_normal")"
fi
wrong_credentials="$work_dir/wrong-credentials"
printf 'username=%s\npassword=definitely-wrong\n' "$smb_user" >"$wrong_credentials"
chmod 0400 "$wrong_credentials"
wrong_status=0
timeout 60 docker run --rm --network "$NETWORK" \
	--mount "type=bind,src=$wrong_credentials,dst=/run/secrets/wrong,readonly" \
	"$CLIENT_IMAGE" smbclient "//$app_normal/torrentfs" -A /run/secrets/wrong -m SMB3 -c 'ls' >/dev/null 2>&1 || wrong_status=$?
[[ "$wrong_status" != 0 ]] || fail 'wrong SMB password was accepted'

guest_status=0
timeout 60 docker run --rm --network "$NETWORK" "$CLIENT_IMAGE" \
	smbclient "//$app_normal/torrentfs" -N -m SMB3 -c 'ls' >/dev/null 2>&1 || guest_status=$?
[[ "$guest_status" != 0 ]] || fail 'SMB guest access was accepted'

# The whole file is read and hashed inside the client container: the transfer
# itself stays bounded, and the copied bytes never depend on a host bind mount.
expected_hash="$(sha256sum "$webseed_dir/payload.bin" | awk '{ print $1 }')"
full_read_hash="$(bounded 300 'full SMB read' docker run --rm --network "$NETWORK" \
	--env "APP=$app_normal" \
	--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
	"$CLIENT_IMAGE" sh -eu -c '
		smbclient "//$APP/torrentfs" -A "/run/secrets/'"$CLIENT_CREDENTIALS_NAME"'" -m SMB3 \
			-c "get payload.bin /tmp/payload.bin" >/dev/null
		sha256sum /tmp/payload.bin | cut -d" " -f1
	')"
[[ "$full_read_hash" == "$expected_hash" ]] || fail "SMB full-read hash $full_read_hash differs from source $expected_hash"

write_status=0
smb_client "$app_normal" -c 'put /etc/hosts write-probe' >/dev/null 2>&1 || write_status=$?
[[ "$write_status" != 0 ]] || fail 'SMB read-only share accepted a write'
metadata_status=0
smb_client "$app_normal" -c 'ls .metadata' >/dev/null 2>&1 || metadata_status=$?
[[ "$metadata_status" != 0 ]] || fail 'SMB share exposed /torrents/.metadata'
printf 'docker SMB smoke: authentication, listing, full read, guest denial, and write denial passed\n'

if [[ "${TORRENTFS_SMB_SKIP_CIFS:-0}" == 1 ]]; then
	printf 'docker SMB smoke: skipping CIFS positional-read check because TORRENTFS_SMB_SKIP_CIFS=1\n'
else
	expected_range="$(dd if="$webseed_dir/payload.bin" bs=1 skip="$RANDOM_OFFSET" count="$RANDOM_LENGTH" status=none | sha256sum | awk '{ print $1 }')"
	range_output="$work_dir/range-output"
	range_status=0
	timeout 120 docker run --rm --privileged --network "$NETWORK" \
		--env "APP_HOST=$app_normal" \
		--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
		"$CLIENT_IMAGE" sh -ceu '
		mkdir -p /mnt/share
		mount.cifs "//$APP_HOST/torrentfs" /mnt/share -o "credentials=/run/secrets/smb-credentials,vers=3.0,ro"
		trap "umount /mnt/share" EXIT
		dd if=/mnt/share/payload.bin bs=1 skip='"$RANDOM_OFFSET"' count='"$RANDOM_LENGTH"' status=none | sha256sum
	' >"$range_output" 2>&1 || range_status=$?
	[[ "$range_status" == 0 ]] || fail "CIFS positional read failed; run the rootful manual check or set TORRENTFS_SMB_SKIP_CIFS=1 for basic CI: $(sed -n '1,20p' "$range_output")"
	actual_range="$(awk '{ print $1 }' "$range_output")"
	[[ "$actual_range" == "$expected_range" ]] || fail "CIFS positional-read hash $actual_range differs from source $expected_range"
	printf 'docker SMB smoke: CIFS positional read at offset %s passed\n' "$RANDOM_OFFSET"
fi

stop_normal_status=0
bounded "$DOCKER_OP_TIMEOUT" "signal $app_normal" docker kill --signal TERM "$app_normal" >/dev/null
stop_normal_status="$(bounded "$DOCKER_OP_TIMEOUT" "wait for $app_normal" docker wait "$app_normal")"
[[ "$stop_normal_status" == 0 ]] || fail "normal SMB shutdown returned $stop_normal_status"
normal_logs="$(logs "$app_normal")"
smbd_line="$(printf '%s\n' "$normal_logs" | grep -n 'smbd stopped' | head -n1 | cut -d: -f1 || true)"
torrent_line="$(printf '%s\n' "$normal_logs" | grep -n 'torrentfs stopping' | head -n1 | cut -d: -f1 || true)"
[[ -n "$smbd_line" && -n "$torrent_line" && "$smbd_line" -lt "$torrent_line" ]] ||
	fail "normal shutdown order was not smbd before torrentfs: $normal_logs"
printf 'docker SMB smoke: normal SIGTERM shutdown was bounded and ordered\n'

fault_torrents="$work_dir/fault-torrents"
mkdir -p "$fault_torrents"
cp -- "$torrents_dir/payload.torrent" "$fault_torrents/payload.torrent"
app_smbd_fault="${APP_PREFIX}-smbd-fault"
start_app "$app_smbd_fault" "$fault_torrents"
bounded 30 'terminate smbd in fault scenario' docker exec "$app_smbd_fault" /bin/sh -c \
	'kill -TERM "$(cat /run/samba/smbd.pid)"'
smbd_fault_status="$(bounded "$DOCKER_OP_TIMEOUT" "wait for smbd fault" docker wait "$app_smbd_fault")"
[[ "$smbd_fault_status" != 0 ]] || fail 'unexpected smbd exit was reported as clean'
printf 'docker SMB smoke: smbd failure propagated as non-zero container exit\n'

app_torrent_fault="${APP_PREFIX}-torrentfs-fault"
start_app "$app_torrent_fault" "$fault_torrents"
bounded 30 'terminate torrentfs in fault scenario' docker exec "$app_torrent_fault" /bin/sh -c \
	'kill -TERM "$(cat /run/torrentfs.pid)"'
torrent_fault_status="$(bounded "$DOCKER_OP_TIMEOUT" "wait for torrentfs fault" docker wait "$app_torrent_fault")"
[[ "$torrent_fault_status" != 0 ]] || fail 'unexpected torrentfs exit was reported as clean'
printf 'docker SMB smoke: torrentfs failure propagated as non-zero container exit\n'

printf 'docker SMB smoke: all checks passed\n'
