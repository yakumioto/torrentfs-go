#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly IMAGE="torrentfs-mio93-smb-smoke:${BASHPID}"
readonly CLIENT_IMAGE="torrentfs-mio93-smb-client:${BASHPID}"
readonly NETWORK="torrentfs-mio93-smb-network-${BASHPID}"
readonly APP_PREFIX="torrentfs-mio93-smb-app-${BASHPID}"
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
readonly CIFS_UNAVAILABLE_PATTERN='(unknown filesystem type|No such device|modprobe|Operation not permitted)'

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
	local seconds="$1" description="$2" status arg has_name=0
	shift 2
	if [[ "${1:-}" == docker && "${2:-}" == run ]]; then
		shift 2
		for arg in "$@"; do [[ "$arg" != --name ]] || has_name=1; done
		if (( has_name )); then
			set -- docker run --label "torrentfs.smoke=$APP_PREFIX" "$@"
		else
			set -- docker run --label "torrentfs.smoke=$APP_PREFIX" \
				--name "$APP_PREFIX-client-${BASHPID}-${RANDOM}" "$@"
		fi
	fi
	if timeout --foreground --kill-after=5 "$seconds" "$@"; then
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
	local container="$1" process_name="$2" output pid uid gid count=0 service_pid=''
	output="$(all_process_identities "$container" "$process_name")" || fail "$container has no $process_name process"
	while IFS=' ' read -r pid uid gid; do
		[[ -n "$pid" ]] || continue
		[[ "$uid" == "$SMOKE_UID:$SMOKE_UID:$SMOKE_UID:$SMOKE_UID" ]] || fail "$container daemon UIDs are $uid"
		[[ "$gid" == "$SMOKE_GID:$SMOKE_GID:$SMOKE_GID:$SMOKE_GID" ]] || fail "$container daemon GIDs are $gid"
		service_pid="$pid"
		count=$((count + 1))
	done <<<"$output"
	[[ "$count" == 1 ]] || fail "$container must have exactly one torrentfs process"
	[[ "$(probe 'inspect tini child' docker exec "$container" cat /proc/1/task/1/children)" == "$service_pid " ]] ||
		fail "$container has a shell supervisor instead of a direct torrentfs child"
	printf 'docker SMB smoke: single Go PID %s runs as %s:%s\n' "$service_pid" "$SMOKE_UID" "$SMOKE_GID"
}

assert_unprivileged_smb() {
	local container="$1" contract
	contract="$(probe 'inspect application security contract' docker inspect --format \
		'{{.HostConfig.Privileged}} {{json .HostConfig.Devices}} {{json .HostConfig.CapAdd}} {{json .HostConfig.SecurityOpt}}' "$container")"
	[[ "$contract" == 'false null null null' || "$contract" == 'false [] null null' ]] ||
		fail "$container requested devices, extra capabilities, or relaxed security: $contract"
	if all_process_identities "$container" smbd >/dev/null 2>&1; then fail "$container unexpectedly runs smbd"; fi
	probe 'check no FUSE device, mount, or Samba state' docker exec "$container" /bin/sh -eu -c '
		test ! -e /dev/fuse
		! grep -q " - fuse" /proc/self/mountinfo
		for path in /run/samba /var/lib/samba /var/cache/samba /var/log/samba; do test ! -e "$path"; done
		for tool in smbd smbpasswd testparm; do
			if command -v "$tool" >/dev/null 2>&1; then exit 1; fi
		done
	' || fail "$container retained FUSE or Samba resources"
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
		if [[ -n "$work_dir" ]]; then docker logs "$app" >"$work_dir/$app.log" 2>&1 || true; fi
		bounded 30 "remove $app" docker rm -f "$app" >/dev/null 2>&1 || true
	done
	# A timed-out docker client may leave its container running. Remove only this
	# run's labelled containers, including clients started inside substitutions.
	while IFS= read -r app; do
		[[ -z "$app" ]] || bounded 30 'remove owned client' docker rm -f "$app" >/dev/null 2>&1 || true
	done < <(docker ps -aq --filter "label=torrentfs.smoke=$APP_PREFIX")
	if [[ -n "$webseed_pid" ]]; then
		kill "$webseed_pid" 2>/dev/null || true
		wait "$webseed_pid" 2>/dev/null || true
	fi
	if (( network_created )); then
		bounded 30 'remove SMB network' docker network rm "$NETWORK" >/dev/null 2>&1 || true
	fi
	# Retain failed protocol/build evidence on request, but never live resources.
	if [[ "${KEEP_SMOKE_ARTIFACTS:-0}" == 1 ]]; then
		printf 'docker SMB smoke: artifacts retained at %s\n' "$work_dir" >&2
	else
		remove_work_dir || true
	fi
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

printf 'docker SMB smoke: building generic application image for %s:%s\n' "$SMOKE_UID" "$SMOKE_GID"
bounded "$DOCKER_BUILD_TIMEOUT" 'build application image' docker build --tag "$IMAGE" "$ROOT_DIR" >"$work_dir/app-build.log" 2>&1 || {
	cat "$work_dir/app-build.log" >&2
	fail 'application image build failed'
}
image_built=1
printf 'docker SMB smoke: building independent smbclient/go-smb2 client image\n'
mkdir -p "$work_dir/client-src"
cat >"$work_dir/client-src/go.mod" <<'EOF'
module torrentfs-smoke-client

go 1.27

require github.com/hirochachacha/go-smb2 v1.1.0
EOF
cat >"$work_dir/client-src/main.go" <<'EOF'
package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/hirochachacha/go-smb2"
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func integer(name string) int64 {
	n, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	must(err)
	return n
}

func denied(name string, err error) {
	if err == nil {
		log.Fatalf("read-only share accepted %s", name)
	}
	var response *smb2.ResponseError
	if !os.IsPermission(err) && !(errors.As(err, &response) && (response.Code == 0xc0000022 || response.Code == 0xc00000a2)) {
		log.Fatalf("%s failed for a non-permission reason: %v", name, err)
	}
	fmt.Printf("DENIED %s\n", name)
}

func checkRange(file *smb2.File) {
	size := integer("FILE_SIZE")
	info, err := file.Stat()
	must(err)
	if info.Size() != size {
		log.Fatalf("file size %d, want %d", info.Size(), size)
	}
	offset, length := integer("RANDOM_OFFSET"), integer("RANDOM_LENGTH")
	buf := make([]byte, length)
	n, err := file.ReadAt(buf, offset)
	must(err)
	if n != len(buf) || fmt.Sprintf("%x", sha256.Sum256(buf)) != os.Getenv("EXPECTED_RANGE") {
		log.Fatal("large-offset ReadAt hash mismatch")
	}
	position, err := file.Seek(offset, io.SeekStart)
	must(err)
	if position != offset {
		log.Fatal("Seek returned the wrong offset")
	}
	n, err = io.ReadFull(file, buf)
	must(err)
	if n != len(buf) || fmt.Sprintf("%x", sha256.Sum256(buf)) != os.Getenv("EXPECTED_RANGE") {
		log.Fatal("large-offset Seek/read hash mismatch")
	}
	n, err = file.ReadAt(buf[:1], size)
	if n != 0 || !errors.Is(err, io.EOF) {
		log.Fatalf("EOF returned %d, %v", n, err)
	}
	fmt.Printf("PASS go-smb2 ReadAt/Seek/hash/EOF offset=%d size=%d\n", offset, size)
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("expected read, readonly or concurrent")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(os.Getenv("APP"), "1445"), 5*time.Second)
	must(err)
	defer conn.Close()
	must(conn.SetDeadline(time.Now().Add(120 * time.Second)))
	dialer := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{
		User: os.Getenv("USERNAME"), Password: os.Getenv("PASSWORD"), Domain: os.Getenv("DOMAIN"),
	}}
	session, err := dialer.Dial(conn)
	must(err)
	defer session.Logoff()
	shareName := os.Getenv("SHARE")
	if shareName == "" {
		shareName = "torrentfs"
	}
	share, err := session.Mount(shareName)
	must(err)
	defer share.Umount()
	target := os.Getenv("TARGET")
	if target == "" {
		target = "payload.bin"
	}
	file, err := share.Open(target)
	must(err)
	defer file.Close()
	switch os.Args[1] {
	case "read":
		checkRange(file)
	case "readonly":
		for name, flags := range map[string]int{
			"write-open": os.O_WRONLY, "read-write-open": os.O_RDWR,
			"append-open": os.O_WRONLY | os.O_APPEND, "truncate-open": os.O_WRONLY | os.O_TRUNC,
		} {
			opened, err := share.OpenFile(target, flags, 0644)
			if opened != nil { opened.Close() }
			denied(name, err)
		}
		created, err := share.Create("write-probe")
		if created != nil { created.Close() }
		denied("create", err)
		denied("mkdir", share.Mkdir("write-probe-dir", 0755))
		denied("rename", share.Rename(target, "renamed-probe"))
		denied("remove", share.Remove(target))
		denied("truncate", file.Truncate(0))
		_, err = file.WriteAt([]byte("write-probe"), 0)
		denied("write-existing-handle", err)
		denied("set-time", share.Chtimes(target, time.Now(), time.Now()))
		if _, err := share.Stat(target); err != nil { log.Fatal("read-only probes changed original file") }
	case "concurrent":
		prefix := make([]byte, 4096)
		_, err := io.ReadFull(file, prefix)
		must(err)
		if fmt.Sprintf("%x", sha256.Sum256(prefix)) != os.Getenv("EXPECTED_PREFIX") { log.Fatal("healthy prefix hash mismatch") }
		started := make(chan struct{}, 16)
		finished := make(chan error, 16)
		for index := range 16 {
			go func() {
				missing, err := share.Open(fmt.Sprintf("unavailable/video-%02d.bin", index))
				if err != nil { finished <- err; return }
				defer missing.Close()
				started <- struct{}{}
				_, err = missing.ReadAt(make([]byte, 4096), 0)
				finished <- err
			}()
		}
		for range 16 {
			select {
			case <-started:
			case err := <-finished: log.Fatalf("missing read ended before concurrent probe: %v", err)
			case <-time.After(15*time.Second): log.Fatal("missing reads did not start")
			}
		}
		select {
		case err := <-finished: log.Fatalf("missing read ended before cold read: %v", err)
		case <-time.After(250*time.Millisecond):
		}
		checkRange(file)
		fmt.Println("PASS same-connection concurrent cold read with 16 missing videos pending")
		// Closing transport cancels the outstanding probes; no client work outlives this process.
		must(conn.Close())
	default:
		log.Fatal("unknown probe")
	}
}
EOF
cat >"$work_dir/client-src/Dockerfile" <<'EOF'
FROM golang:1.27 AS client-build
WORKDIR /src
COPY go.mod main.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -o /out/smb-probe .
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends cifs-utils smbclient ca-certificates curl && rm -rf /var/lib/apt/lists/*
COPY --from=client-build /out/smb-probe /usr/local/bin/smb-probe
EOF
bounded "$DOCKER_BUILD_TIMEOUT" 'build independent SMB client image' docker build --tag "$CLIENT_IMAGE" "$work_dir/client-src" >"$work_dir/client-build.log" 2>&1 || {
	cat "$work_dir/client-build.log" >&2
	fail 'client image build failed'
}
client_image_built=1
bounded 30 'create SMB network' docker network create "$NETWORK" >/dev/null
network_created=1

smb_user=media.viewer
smb_password="torrentfs-smoke-${BASHPID}-${RANDOM}"
credentials_file="$work_dir/$CLIENT_CREDENTIALS_NAME"
printf 'username=%s\npassword=%s\n' "$smb_user" "$smb_password" >"$credentials_file"
chmod 0400 "$credentials_file"

start_app() {
	local app="$1" app_torrents="$2" http_auth="${3:-true}" read_timeout="${4:-30s}" strm_enabled="${5:-false}"
	local env_args=(
		--env "PUID=$SMOKE_UID" --env "PGID=$SMOKE_GID"
		--env TORRENTFS_FILESYSTEM_MODE=smb
		--env TORRENTFS_SMB_LISTEN_ADDR=0.0.0.0:1445
		--env "TORRENTFS_SMB_STRM_ENABLED=$strm_enabled"
		--env "TORRENTFS_USERNAME=$smb_user" --env "TORRENTFS_PASSWORD=$smb_password"
		--env "TORRENTFS_CACHE_CAPACITY=${CACHE_BYTES}B"
		--env "TORRENTFS_MOUNT_READ_TIMEOUT=$read_timeout"
	)
	if [[ "$http_auth" == true ]]; then
		env_args+=(--env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 --env TORRENTFS_HTTP_AUTH_ENABLED=true)
	else
		env_args+=(--env TORRENTFS_HTTP_LISTEN_ADDR= --env TORRENTFS_HTTP_AUTH_ENABLED=false)
	fi
	if [[ "$strm_enabled" == true ]]; then
		env_args+=(--env "TORRENTFS_STRM_BASE_URL=http://$app:8080")
	fi
	app_names+=("$app")
	bounded "$DOCKER_OP_TIMEOUT" "start $app" docker run --detach --name "$app" \
		--network "$NETWORK" --add-host host.docker.internal:host-gateway \
		--cap-drop NET_BIND_SERVICE \
		--publish 127.0.0.1::1445 \
		--mount "type=bind,src=$app_torrents,dst=/torrents" \
		"${env_args[@]}" "$IMAGE" >/dev/null
	local deadline=$((SECONDS + START_TIMEOUT)) state
	while (( SECONDS < deadline )); do
		state="$(probe "inspect $app" docker inspect --format '{{.State.Status}}' "$app" 2>/dev/null || true)"
		if [[ "$state" == exited || "$state" == dead ]]; then
			fail "$app exited before SMB readiness (state=$state): $(logs "$app")"
		fi
		if [[ "$state" == running ]] && probe 'check in-process SMB listener' docker exec "$app" \
			/bin/bash -ec ': >/dev/tcp/127.0.0.1/1445' >/dev/null 2>&1; then
			assert_process_identities "$app" torrentfs
			assert_unprivileged_smb "$app"
			return 0
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
				smbclient "//$APP/$SHARE" -A /run/secrets/smb-credentials -m SMB3 -p 1445 \
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
		"$CLIENT_IMAGE" smbclient "//$app/$share" \
		-A "/run/secrets/$CLIENT_CREDENTIALS_NAME" -m SMB3 -p 1445 -t "$CLIENT_SMB_TIMEOUT" "$@"
}

smb_client() {
	local app="$1"
	shift
	smb_share_client "$app" torrentfs "$@"
}

go_probe() {
	local app="$1" share="$2" target="$3" mode="$4" domain="${5-WORKGROUP}"
	bounded "$CLIENT_TIMEOUT" "required go-smb2 $mode against $app/$share" docker run --rm --network "$NETWORK" \
		--env "APP=$app" --env "SHARE=$share" --env "TARGET=$target" \
		--env "USERNAME=$smb_user" --env "PASSWORD=$smb_password" --env "DOMAIN=$domain" \
		--env "FILE_SIZE=$FILE_SIZE" --env "RANDOM_OFFSET=$RANDOM_OFFSET" --env "RANDOM_LENGTH=$RANDOM_LENGTH" \
		--env "EXPECTED_RANGE=$expected_range" --env "EXPECTED_PREFIX=${expected_prefix:-}" \
		"$CLIENT_IMAGE" smb-probe "$mode" || fail "required go-smb2 $mode failed
	app logs:
	$(logs "$app")"
}

printf 'docker SMB smoke: starting HTTP+SMB authenticated share as %s:%s\n' "$SMOKE_UID" "$SMOKE_GID"
app_normal="${APP_PREFIX}-normal"
start_app "$app_normal" "$torrents_dir" true
probe 'check SMB login is independent of Unix accounts' docker exec "$app_normal" /bin/sh -c \
	'! getent passwd "$1"' sh "$smb_user" || fail 'smoke username unexpectedly exists as a Unix account'
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
	"$CLIENT_IMAGE" smbclient "//$app_normal/torrentfs" -A /run/secrets/wrong -m SMB3 -p 1445 \
	-t "$CLIENT_SMB_TIMEOUT" -c 'ls' >"$work_dir/wrong-password.log" 2>&1 || wrong_status=$?
[[ "$wrong_status" != 0 && "$wrong_status" != 124 ]] && grep -Eq 'NT_STATUS_(LOGON_FAILURE|ACCESS_DENIED)' "$work_dir/wrong-password.log" ||
	fail 'wrong SMB password did not produce an authentication denial'

guest_status=0
bounded "$CLIENT_TIMEOUT" 'SMB guest client' docker run --rm --network "$NETWORK" "$CLIENT_IMAGE" \
	smbclient "//$app_normal/torrentfs" -N -m SMB3 -p 1445 -t "$CLIENT_SMB_TIMEOUT" -c 'ls' >"$work_dir/guest.log" 2>&1 || guest_status=$?
[[ "$guest_status" != 0 && "$guest_status" != 124 ]] && grep -Eq 'NT_STATUS_(LOGON_FAILURE|ACCESS_DENIED)' "$work_dir/guest.log" ||
	fail 'SMB guest access did not produce an authentication denial'

# The whole file is read and hashed inside the client container: the transfer
# itself stays bounded, and the copied bytes never depend on a host bind mount.
expected_hash="$(sha256sum "$webseed_dir/payload.bin" | awk '{ print $1 }')"
full_read_log="$work_dir/full-read.log"
full_read_status=0
bounded "$CLIENT_TIMEOUT" 'full SMB read' docker run --rm --network "$NETWORK" \
	--env "APP=$app_normal" \
	--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
	"$CLIENT_IMAGE" sh -eu -c '
		smbclient "//$APP/torrentfs" -A "/run/secrets/'"$CLIENT_CREDENTIALS_NAME"'" -m SMB3 -p 1445 \
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
expected_range="$(dd if="$webseed_dir/payload.bin" bs=64K skip="$RANDOM_OFFSET" count="$RANDOM_LENGTH" iflag=skip_bytes,count_bytes status=none | sha256sum | awk '{ print $1 }')"
# These protocol/data gates are mandatory even when kernel CIFS is unavailable.
go_probe "$app_normal" torrentfs payload.bin read WORKGROUP
go_probe "$app_normal" torrentfs payload.bin read ''
go_probe "$app_normal" torrentfs payload.bin readonly

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
smb_client "$app_normal" -c 'ls .metadata' >/dev/null 2>&1 || metadata_status=$?
[[ "$metadata_status" != 0 ]] || fail 'SMB share exposed /torrents/.metadata'
printf 'docker SMB smoke: authentication, listing, full read, guest denial, and write denial passed\n'

# Kernel CIFS is an additional interoperability check, not a replacement for
# the mandatory go-smb2 positional reads above. Missing host support is explicit;
# a mount that succeeds but returns wrong bytes is never skipped.
if [[ "${TORRENTFS_SMB_SKIP_CIFS:-0}" == 1 ]]; then
	printf 'docker SMB smoke: supplemental kernel CIFS not tested (TORRENTFS_SMB_SKIP_CIFS=1); mandatory protocol gates are unchanged\n'
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
		dd if=/mnt/torrentfs-client/payload.bin bs=1 skip='"$RANDOM_OFFSET"' count='"$RANDOM_LENGTH"' status=none | sha256sum
	' >"$range_output" 2>&1 || range_status=$?
	if (( range_status != 0 )); then
		if grep -Eq "$CIFS_UNAVAILABLE_PATTERN" "$range_output"; then
			printf 'docker SMB smoke: supplemental kernel CIFS not tested: host cannot mount CIFS (%s); required smbclient and go-smb2 data/hash/seek gates remain mandatory\n' \
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
[[ "$normal_logs" == *'msg=stopped'* ]] || fail "daemon did not report a complete shutdown: $normal_logs"
printf 'docker SMB smoke: single Go service SIGTERM exited 0 and reported stopped\n'

# The Go client is always required; kernel CIFS is a supplementary client only.
{
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
	# Webseed dispatch runs every five seconds; shorter deadlines also cancel healthy reads.
	start_app "$app_concurrent" "$concurrent_torrents" true 10s
	expected_prefix="$(dd if="$webseed_dir/payload.bin" bs=64K count=4096 iflag=count_bytes status=none | sha256sum | awk '{ print $1 }')"
	go_probe "$app_concurrent" torrentfs payload.bin concurrent
	if (( cifs_available )); then
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
	printf 'docker SMB smoke: supplemental CIFS same-connection cold read passed with 16 missing videos pending\n'
	bounded 30 'remove concurrent client' docker rm -f "$client_concurrent" >/dev/null
	else
		printf 'docker SMB smoke: supplemental CIFS concurrency not tested; required go-smb2 concurrency passed\n'
	fi
	bounded "$DOCKER_OP_TIMEOUT" 'stop concurrent app' docker kill --signal TERM "$app_concurrent" >/dev/null
	concurrent_status="$(bounded "$DOCKER_OP_TIMEOUT" 'wait for concurrent app' docker wait "$app_concurrent")"
	[[ "$concurrent_status" == 0 ]] || fail "concurrent SMB shutdown returned $concurrent_status"
}

printf 'docker SMB smoke: starting isolated original-file and STRM shares\n'
strm_torrents="$work_dir/strm-torrents"
create_fixture "$strm_torrents" "$webseed_dir/Movie.mkv" Movie.mkv \
	"http://host.docker.internal:$webseed_port/Movie.mkv" "$work_dir/strm-infohash"
create_subtitle_fixture "$strm_torrents" "$work_dir/subtitle-infohash"
subtitle_hash="$(<"$work_dir/subtitle-infohash")"
app_strm="${APP_PREFIX}-strm"
start_app "$app_strm" "$strm_torrents" true 30s true
go_probe "$app_strm" torrentfs Movie.mkv readonly
go_probe "$app_strm" torrentfs-strm Movie.strm readonly
strm_original_listing="$(smb_client "$app_strm" -c 'ls' 2>&1)"
[[ "$strm_original_listing" == *Movie.mkv* ]] || fail 'original share lost the video in STRM-enabled mode'
strm_listing="$(smb_share_client "$app_strm" torrentfs-strm -c 'ls' 2>&1)"
[[ "$strm_listing" == *Movie.strm* ]] || fail 'STRM share did not list Movie.strm'
bounded "$CLIENT_TIMEOUT" 'read STRM over SMB and request HTTP HEAD without payload' docker run --rm --network "$NETWORK" \
	--env "APP=$app_strm" --env "FILE_SIZE=$FILE_SIZE" \
	--mount "type=bind,src=$credentials_file,dst=/run/secrets/$CLIENT_CREDENTIALS_NAME,readonly" \
	--mount "type=bind,src=$client_output_dir,dst=/output" \
	"$CLIENT_IMAGE" sh -eu -c '
		smbclient "//$APP/torrentfs-strm" -A /run/secrets/smb-credentials -m SMB3 -p 1445 \
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
	smb_share_client "$app_strm" "$share" -c 'ls .metadata' >/dev/null 2>&1 || metadata_status=$?
	[[ "$metadata_status" != 0 ]] || fail "STRM-enabled $share exposed session metadata"
done
strm_guest_status=0
bounded "$CLIENT_TIMEOUT" 'STRM guest client' docker run --rm --network "$NETWORK" "$CLIENT_IMAGE" \
	smbclient "//$app_strm/torrentfs-strm" -N -m SMB3 -p 1445 -t "$CLIENT_SMB_TIMEOUT" -c 'ls' >"$work_dir/strm-guest.log" 2>&1 || strm_guest_status=$?
[[ "$strm_guest_status" != 0 && "$strm_guest_status" != 124 ]] && grep -Eq 'NT_STATUS_(LOGON_FAILURE|ACCESS_DENIED)' "$work_dir/strm-guest.log" ||
	fail 'STRM guest access did not produce an authentication denial'
strm_wrong_status=0
bounded "$CLIENT_TIMEOUT" 'STRM client with a wrong password' docker run --rm --network "$NETWORK" \
	--mount "type=bind,src=$wrong_credentials,dst=/run/secrets/wrong,readonly" \
	"$CLIENT_IMAGE" smbclient "//$app_strm/torrentfs-strm" -A /run/secrets/wrong -m SMB3 -p 1445 \
	-t "$CLIENT_SMB_TIMEOUT" -c 'ls' >"$work_dir/strm-wrong-password.log" 2>&1 || strm_wrong_status=$?
[[ "$strm_wrong_status" != 0 && "$strm_wrong_status" != 124 ]] && grep -Eq 'NT_STATUS_(LOGON_FAILURE|ACCESS_DENIED)' "$work_dir/strm-wrong-password.log" ||
	fail 'STRM wrong password did not produce an authentication denial'
bounded "$DOCKER_OP_TIMEOUT" "signal $app_strm" docker kill --signal TERM "$app_strm" >/dev/null
strm_stop_status="$(bounded "$DOCKER_OP_TIMEOUT" "wait for $app_strm" docker wait "$app_strm")"
[[ "$strm_stop_status" == 0 ]] || fail "dual-share shutdown returned $strm_stop_status"
strm_logs="$(logs "$app_strm")"
[[ "$strm_logs" == *'msg=stopped'* ]] || fail 'dual-share Go service did not complete shutdown'
strm_url="$(cat "$client_output_dir/Movie.strm")"
strm_location="${strm_url#http://$app_strm:8080/stream/}"
strm_location="${strm_location#*/}"
strm_signature="${strm_location%%/*}"
[[ "$strm_logs" != *"$smb_password"* && "$strm_logs" != *"$strm_url"* && "$strm_logs" != *"$strm_signature"* ]] ||
	fail 'STRM-enabled application logs exposed credentials or a signed stream URL'
printf 'docker SMB smoke: dual read-only shares, no-payload HEAD, Range 206/hash, signature 403, and clean single-process shutdown passed\n'

printf 'docker SMB smoke: starting SMB-only share with HTTP auth disabled\n'
smb_only_torrents="$work_dir/smb-only-torrents"
mkdir -p "$smb_only_torrents"
chmod 0777 "$smb_only_torrents"
app_smb_only="${APP_PREFIX}-smb-only"
start_app "$app_smb_only" "$smb_only_torrents" false
probe 'verify HTTP is disabled in SMB-only mode' docker exec "$app_smb_only" /bin/bash -ec \
	'if (: >/dev/tcp/127.0.0.1/8080) 2>/dev/null; then exit 1; fi' || fail 'SMB-only mode kept an HTTP listener'
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
app_direct_term="${APP_PREFIX}-direct-term"
start_app "$app_direct_term" "$fault_torrents"
direct_pid="$(all_process_identities "$app_direct_term" torrentfs | awk '{ print $1 }')"
bounded 30 'signal Go child directly' docker exec "$app_direct_term" kill -TERM "$direct_pid"
direct_status="$(bounded "$DOCKER_OP_TIMEOUT" 'wait for direct Go shutdown' docker wait "$app_direct_term")"
[[ "$direct_status" == 0 ]] || fail "direct Go SIGTERM returned $direct_status instead of 0"
printf 'docker SMB smoke: direct Go SIGTERM is clean without a shell supervisor\n'

app_torrent_fault="${APP_PREFIX}-torrentfs-fault"
start_app "$app_torrent_fault" "$fault_torrents"
fault_pid="$(all_process_identities "$app_torrent_fault" torrentfs | awk '{ print $1 }')"
bounded 30 'kill the single Go service in fault scenario' docker exec "$app_torrent_fault" kill -KILL "$fault_pid"
torrent_fault_status="$(bounded "$DOCKER_OP_TIMEOUT" 'wait for Go process fault' docker wait "$app_torrent_fault")"
[[ "$torrent_fault_status" == 137 ]] || fail "Go SIGKILL returned $torrent_fault_status instead of 137"
printf 'docker SMB smoke: single Go failure propagated through tini as exit 137\n'

printf 'docker SMB smoke: all checks passed\n'
