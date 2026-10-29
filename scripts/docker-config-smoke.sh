#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly IMAGE="${TORRENTFS_SMOKE_IMAGE:-torrentfs-config-smoke:${BASHPID}}"
readonly PREFIX="torrentfs-config-${BASHPID}"
readonly SECRET="config-smoke-${BASHPID}-test-only-password"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/torrentfs-config.XXXXXX")"
image_built=0
containers=()

fail() { printf 'docker config smoke: %s\n' "$*" >&2; exit 1; }
cleanup() {
	local status=$?
	trap - EXIT INT TERM
	for container in "${containers[@]}"; do timeout 30 docker rm -f "$container" >/dev/null 2>&1 || true; done
	# Only the private mktemp tree is mounted into this root cleanup helper.
	timeout 30 docker run --rm --user 0:0 --entrypoint /bin/sh \
		--mount "type=bind,src=$work_dir,dst=/scratch" "$IMAGE" \
		-c 'rm -rf /scratch/* /scratch/..?* /scratch/.[!.]*' >/dev/null 2>&1 || true
	rm -rf -- "$work_dir"
	if (( image_built )); then timeout 60 docker image rm "$IMAGE" >/dev/null 2>&1 || true; fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
for tool in docker curl timeout python3; do command -v "$tool" >/dev/null || fail "$tool is required"; done

if [[ -n "${TORRENTFS_SMOKE_IMAGE:-}" ]]; then
	timeout 30 docker image inspect "$IMAGE" >/dev/null
else
	printf 'docker config smoke: building native image\n'
	timeout 1800 docker build --tag "$IMAGE" "$ROOT_DIR"
	image_built=1
fi

timeout 30 docker run --rm --cap-drop ALL --security-opt no-new-privileges:true \
	--entrypoint /bin/sh "$IMAGE" -eu -c '
		test "$(id -u)" = 1000 && test "$(id -g)" = 1000
		test -r /etc/torrentfs/torrentfs.toml
		test -x /usr/local/bin/torrentfs-entrypoint
		test -d /torrents && test -w /torrents
		test ! -e /dev/fuse && test ! -d /run/samba
		! command -v smbd >/dev/null 2>&1
		! command -v smbpasswd >/dev/null 2>&1
		! command -v testparm >/dev/null 2>&1
	' || fail 'native image contract failed'
[[ "$(docker image inspect "$IMAGE" --format '{{.Config.User}}')" == '1000:1000' ]] || fail 'image does not default to non-root'
ports="$(docker image inspect "$IMAGE" --format '{{json .Config.ExposedPorts}}')"
[[ "$ports" == *'"1445/tcp"'* && "$ports" != *'"445/tcp"'* ]] || fail 'image must advertise internal SMB 1445, not privileged 445'
printf 'docker config smoke: non-root image and absence of Samba passed\n'

process_identity() {
	timeout 10 docker exec "$1" /bin/sh -eu -c '
		for status in /proc/[0-9]*/status; do
			[ -r "$status" ] || continue
			grep -q "^Name:[[:space:]]*torrentfs$" "$status" || continue
			grep -E "^(Uid|Gid|CapEff|CapBnd|NoNewPrivs):" "$status"
			exit 0
		done
		exit 1'
}
wait_running() {
	local container="$1" state
	for _ in {1..100}; do
		state="$(timeout 10 docker inspect "$container" --format '{{.State.Status}}')"
		if [[ "$state" == running ]] && process_identity "$container" >/dev/null 2>&1 &&
			timeout 10 docker exec "$container" /bin/sh -c 'grep -Eq ":(1F90|05A5)[[:space:]].*[[:space:]]0A[[:space:]]" /proc/net/tcp /proc/net/tcp6'; then return 0; fi
		if [[ "$state" == exited || "$state" == dead ]]; then
			docker logs "$container" >&2 || true
			fail "$container exited before readiness"
		fi
		sleep 0.1
	done
	fail "$container did not become ready"
}
assert_identity() {
	local output uid="$2" gid="$3"
	output="$(process_identity "$1")"
	python3 -I -c 'import sys
text, uid, gid=sys.argv[1:]
fields=dict(line.split(":",1) for line in text.splitlines())
assert fields["Uid"].split()==[uid]*4
assert fields["Gid"].split()==[gid]*4
assert fields["CapEff"].strip()=="0000000000000000"
assert fields["NoNewPrivs"].strip()=="1"
' "$output" "$uid" "$gid" || fail 'process did not retain its non-root/no-privileges identity'
}
start() {
	local container="$1"
	shift
	containers+=("$container")
	timeout 30 docker run --detach --name "$container" --cap-drop ALL \
		--security-opt no-new-privileges:true "$@" "$IMAGE" >/dev/null
	wait_running "$container"
}
stop_clean() {
	timeout 30 docker kill --signal TERM "$1" >/dev/null
	local status
	status="$(timeout 30 docker wait "$1")" || fail 'shutdown exceeded 30 seconds'
	local output
	output="$(timeout 10 docker logs "$1" 2>&1)"
	[[ "$status" == 0 ]] || fail "shutdown returned $status: ${output//"$SECRET"/[REDACTED]}"
	[[ "$output" != *"$SECRET"* ]] || fail 'startup/shutdown logs disclosed a password'
}
expect_failure() {
	local name="$1" expected="$2" output status=0
	shift 2
	local docker_args=() go_args=() delimiter=0
	for arg in "$@"; do
		if [[ "$delimiter" == 0 && "$arg" == -- ]]; then delimiter=1; continue; fi
		if (( delimiter )); then go_args+=("$arg"); else docker_args+=("$arg"); fi
	done
	output="$(timeout 30 docker run --rm --cap-drop ALL --security-opt no-new-privileges:true \
		"${docker_args[@]}" "$IMAGE" "${go_args[@]}" 2>&1)" || status=$?
	[[ "$status" != 0 && "$status" != 124 ]] || fail "$name started or timed out"
	[[ "$output" == *"$expected"* ]] || fail "$name lacked $expected: $output"
	[[ "$output" != *"$SECRET"* ]] || fail "$name disclosed its password"
	# Filesystem permission failures occur inside Session.New; configuration
	# and identity failures must be rejected before the session is initialized.
	if [[ "$name" != 'inaccessible data' ]]; then
		[[ "$output" != *'msg="torrentfs starting"'* && "$output" != *'msg=torrentfs starting'* ]] || fail "$name reached session startup"
	fi
}

start "${PREFIX}-default"
assert_identity "${PREFIX}-default" 1000 1000
stop_clean "${PREFIX}-default"
start "${PREFIX}-uid" --user 99:100 --env PUID=99 --env PGID=100
assert_identity "${PREFIX}-uid" 99 100
stop_clean "${PREFIX}-uid"
# Legacy root bootstrap is not the zero-capability path: SETUID/SETGID change
# the child identity, and root tini needs KILL to forward signals to that UID.
start "${PREFIX}-bootstrap" --user 0:0 --cap-add SETUID --cap-add SETGID --cap-add KILL --env PUID=99 --env PGID=100
assert_identity "${PREFIX}-bootstrap" 99 100
stop_clean "${PREFIX}-bootstrap"
printf 'docker config smoke: default, numeric --user and explicit root-bootstrap identity passed\n'

expect_failure 'empty PUID' 'PUID must be' --env PUID=
expect_failure 'invalid PGID' 'PGID must be' --env PGID=-1
expect_failure 'zero PUID' 'PUID must be between' --env PUID=0
expect_failure 'overflow PGID' 'PGID must be between' --env PGID=4294967295
expect_failure 'mismatched PUID' 'PUID/PGID must match docker --user' --env PUID=99
expect_failure 'unknown backend' 'filesystem.backend' --env TORRENTFS_FILESYSTEM_BACKEND=invalid
expect_failure 'no output' 'Usage:' --env TORRENTFS_FILESYSTEM_BACKEND=none --env TORRENTFS_HTTP_LISTEN_ADDR=
expect_failure 'missing SMB credential pair' 'must be set together' --env TORRENTFS_FILESYSTEM_BACKEND=smb --env "TORRENTFS_PASSWORD=$SECRET"
expect_failure 'missing SMB password' 'must be set together' --env TORRENTFS_FILESYSTEM_BACKEND=smb --env TORRENTFS_USERNAME=reader
expect_failure 'empty SMB password' 'password is required' --env TORRENTFS_FILESYSTEM_BACKEND=smb --env TORRENTFS_USERNAME=reader --env TORRENTFS_PASSWORD=
expect_failure 'SMB/FUSE CLI conflict' 'conflict' --env TORRENTFS_FILESYSTEM_BACKEND=smb \
	--env TORRENTFS_USERNAME=reader --env "TORRENTFS_PASSWORD=$SECRET" -- \
	-mountpoint /share -config /etc/torrentfs/torrentfs.toml /torrents
expect_failure 'conflicting old selector' 'conflicts' --env TORRENTFS_FILESYSTEM_BACKEND=none --env TORRENTFS_SMB_ENABLED=true
expect_failure 'conflicting read deadlines' 'conflicts' --env TORRENTFS_MOUNT_READ_TIMEOUT=3s --env TORRENTFS_FILESYSTEM_READ_TIMEOUT=4s
expect_failure 'STRM requires HTTP' 'strm.base_url' --env TORRENTFS_FILESYSTEM_BACKEND=smb \
	--env TORRENTFS_USERNAME=reader --env "TORRENTFS_PASSWORD=$SECRET" \
	--env TORRENTFS_HTTP_LISTEN_ADDR= --env TORRENTFS_STRM_BASE_URL=http://player.example:8080
printf 'docker config smoke: identity, selector, credential, alias, and STRM failures passed\n'

mkdir -p "$work_dir/blocked"
chmod 000 "$work_dir/blocked"
expect_failure 'inaccessible data' 'permission denied' --user 99:100 --env PUID=99 --env PGID=100 \
	--mount "type=bind,src=$work_dir/blocked,dst=/torrents"
chmod 0700 "$work_dir/blocked"

# A private secret file enables SMB with no HTTP listener and no Unix-name lookup.
printf '%s\n' "$SECRET" >"$work_dir/password"
# The mktemp directory is private; the fake fixture must be readable by the
# image's numeric UID even when the smoke runner uses a different host UID.
chmod 0444 "$work_dir/password"
cat >"$work_dir/native.toml" <<'EOF'
[filesystem]
backend = "smb"
read_timeout = "30s"
[http]
listen_addr = ""
[smb]
username = "not-a-unix-account"
password_file = "/run/secrets/smb-password"
listen_addr = "0.0.0.0:1445"
[fuse]
mountpoint = "/not-needed-for-smb"
EOF
chmod 0444 "$work_dir/native.toml"
container="${PREFIX}-secret"
containers+=("$container")
timeout 30 docker run --detach --name "$container" --cap-drop ALL --security-opt no-new-privileges:true \
	--mount "type=bind,src=$work_dir/password,dst=/run/secrets/smb-password,readonly" \
	--mount "type=bind,src=$work_dir/native.toml,dst=/config.toml,readonly" \
	"$IMAGE" -config /config.toml /torrents >/dev/null
wait_running "$container"
assert_identity "$container" 1000 1000
timeout 10 docker exec "$container" /bin/sh -eu -c '
	! grep -Eq ":[0]*1F90[[:space:]].*[[:space:]]0A[[:space:]]" /proc/net/tcp /proc/net/tcp6
	test ! -e /dev/fuse && test ! -d /run/samba
'
stop_clean "$container"
printf 'docker config smoke: TOML-selected SMB-only with a secret file and no FUSE passed\n'

# Existing shared environment credentials still enable HTTP password login.
container="${PREFIX}-http"
start "$container" --publish 127.0.0.1::8080 \
	--env TORRENTFS_FILESYSTEM_BACKEND=none --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
	--env TORRENTFS_HTTP_AUTH_ENABLED=true --env TORRENTFS_USERNAME=reader --env "TORRENTFS_PASSWORD=$SECRET"
port="$(docker port "$container" 8080/tcp | awk -F: 'NR == 1 { print $NF }')"
url="http://127.0.0.1:$port"
for _ in {1..100}; do
	if curl --silent --fail --max-time 2 "$url/" >/dev/null; then break; fi
	sleep 0.1
done
status="$(curl --silent --show-error --max-time 5 -o /dev/null -w '%{http_code}' "$url/api/v1/torrents")"
[[ "$status" == 401 ]] || fail 'HTTP authentication was not enabled'
status="$(curl --silent --show-error --max-time 5 --header 'Content-Type: application/json' \
	--data "{\"password\":\"$SECRET\"}" -o /dev/null -w '%{http_code}' "$url/api/v1/auth/login")"
[[ "$status" == 200 ]] || fail 'HTTP shared credential login failed'
stop_clean "$container"
printf 'docker config smoke: all checks passed\n'
