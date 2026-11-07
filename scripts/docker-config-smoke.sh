#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly IMAGE="${TF_SMOKE_IMAGE:-torrentfs-config-smoke:${BASHPID}}"
readonly PREFIX="torrentfs-config-${BASHPID}"
readonly SECRET="config-smoke-${BASHPID}-test-only-password"
# The image reference the Compose template names. It is a placeholder the user
# replaces with a published tag, so the image built for this run stands in for
# it when the template is started.
readonly COMPOSE_IMAGE_TAG='ghcr.io/yakumioto/torrentfs-go:REPLACE_WITH_PUBLISHED_TAG'
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/torrentfs-config.XXXXXX")"
image_built=0
containers=()
compose_dir=""
compose_project=""
compose_tagged=0

fail() { printf 'docker config smoke: %s\n' "$*" >&2; exit 1; }
# compose runs the shipped template from a private project directory.
compose() {
	local timeout_seconds="$1"
	shift
	timeout "$timeout_seconds" docker compose --project-directory "$compose_dir" \
		--project-name "$compose_project" -f "$ROOT_DIR/docker-compose.yaml" "$@"
}
refuse_secret() {
	[[ "$1" != *"$SECRET"* ]] || fail "$2"
}
cleanup() {
	local status=$?
	trap - EXIT INT TERM
	if [[ -n "$compose_project" ]]; then
		compose 60 down --remove-orphans >/dev/null 2>&1 || true
	fi
	for container in "${containers[@]}"; do timeout 30 docker rm -f "$container" >/dev/null 2>&1 || true; done
	# Only the private mktemp tree is mounted into this root cleanup helper.
	timeout 30 docker run --rm --user 0:0 --entrypoint /bin/sh \
		--mount "type=bind,src=$work_dir,dst=/scratch" "$IMAGE" \
		-c 'rm -rf /scratch/* /scratch/..?* /scratch/.[!.]*' >/dev/null 2>&1 || true
	rm -rf -- "$work_dir"
	if (( compose_tagged )); then timeout 30 docker image rm "$COMPOSE_IMAGE_TAG" >/dev/null 2>&1 || true; fi
	if (( image_built )); then timeout 60 docker image rm "$IMAGE" >/dev/null 2>&1 || true; fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
for tool in docker curl timeout python3; do command -v "$tool" >/dev/null || fail "$tool is required"; done

if [[ -n "${TF_SMOKE_IMAGE:-}" ]]; then
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
			local output
			output="$(timeout 10 docker logs "$container" 2>&1)" || true
			printf '%s\n' "${output//"$SECRET"/[REDACTED]}" >&2
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
	refuse_secret "$output" 'startup/shutdown logs disclosed a password'
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
	refuse_secret "$output" "$name disclosed its password"
	# Filesystem permission failures occur inside Session.New; configuration
	# and identity failures must be rejected before the session is initialized.
	if [[ "$name" != 'inaccessible data' ]]; then
		[[ "$output" != *'msg="torrentfs starting"'* && "$output" != *'msg=torrentfs starting'* ]] || fail "$name reached session startup"
	fi
}

# The Compose template names the settings the program reads. A template that
# names a removed setting fails at the first start, so the names are checked
# before anything is started. The project directory supplies the .env the
# template reads its password from, exactly as a deployment would.
compose_dir="$work_dir/compose"
compose_project="${PREFIX}-compose"
compose_container="${compose_project}-torrentfs"
mkdir -p "$compose_dir/torrents"
chmod 0755 "$compose_dir"
# The template runs the container as 1000:1000, while the directory belongs to
# whoever runs the smoke: the data directory must be writable by the container
# user.
chmod 0777 "$compose_dir/torrents"
printf 'TORRENTFS_PASSWORD=%s\n' "$SECRET" >"$compose_dir/.env"
compose_config="$(compose 30 config 2>&1)" ||
	fail "the Compose template is not valid: ${compose_config//"$SECRET"/[REDACTED]}"
for expected in 'TORRENTFS_BACKEND' 'TORRENTFS_USERNAME' 'TORRENTFS_PASSWORD'; do
	[[ "$compose_config" == *"$expected"* ]] || fail "the Compose template does not set $expected"
done
for removed in 'TORRENTFS_FILESYSTEM_BACKEND' 'TORRENTFS_SMB_USERNAME' 'TORRENTFS_SMB_PASSWORD_FILE' 'TORRENTFS_MOUNT_READ_TIMEOUT'; do
	[[ "$compose_config" != *"$removed"* ]] || fail "the Compose template names the removed variable $removed"
done
printf 'docker config smoke: the Compose template names the current settings\n'

# A template that names the right settings still has to start. The service is
# started through `compose run`, which keeps the service definition (image,
# user, environment, volumes, capabilities) but publishes no host ports, so the
# check does not depend on the host's 445 and 8080 being free.
timeout 30 docker tag "$IMAGE" "$COMPOSE_IMAGE_TAG"
compose_tagged=1
containers+=("$compose_container")
compose 60 run --detach --no-deps --name "$compose_container" torrentfs >/dev/null 2>&1
wait_running "$compose_container"
compose_output="$(timeout 10 docker logs "$compose_container" 2>&1)"
refuse_secret "$compose_output" 'the Compose start disclosed the password'
[[ "$compose_output" == *'torrentfs starting'* ]] || fail 'the Compose template did not reach a start'
[[ "$compose_output" == *'backend=smb'* ]] || fail 'the Compose template did not start the SMB output'
# The template's user and capabilities reached the process, and the session got
# far enough to create its metadata directory in the mounted torrents volume.
assert_identity "$compose_container" 1000 1000
[[ -d "$compose_dir/torrents/.metadata" ]] || fail 'the Compose start did not write to the mounted torrents directory'
stop_clean "$compose_container"
compose 60 down --remove-orphans >/dev/null 2>&1
timeout 30 docker image rm "$COMPOSE_IMAGE_TAG" >/dev/null 2>&1 || true
compose_tagged=0
printf 'docker config smoke: the Compose template starts\n'

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
expect_failure 'unknown backend' 'backend' --env TORRENTFS_BACKEND=invalid
expect_failure 'no output' 'at least one output must be enabled' --env TORRENTFS_BACKEND=none --env TORRENTFS_HTTP_LISTEN_ADDR=
expect_failure 'missing SMB password' 'password' --env TORRENTFS_BACKEND=smb --env TORRENTFS_USERNAME=reader
expect_failure 'missing SMB user name' 'username' --env TORRENTFS_BACKEND=smb --env "TORRENTFS_PASSWORD=$SECRET"
expect_failure 'empty SMB password' 'password: value is required' --env TORRENTFS_BACKEND=smb --env TORRENTFS_USERNAME=reader --env TORRENTFS_PASSWORD=
expect_failure 'removed mountpoint flag' 'fuse.mountpoint' -- \
	-mountpoint /share -config /etc/torrentfs/torrentfs.toml /torrents
expect_failure 'removed SMB selector' 'TORRENTFS_BACKEND' --env TORRENTFS_SMB_ENABLED=true
expect_failure 'removed read deadline' 'TORRENTFS_FILESYSTEM_READ_TIMEOUT' --env TORRENTFS_MOUNT_READ_TIMEOUT=3s
expect_failure 'removed backend variable' 'TORRENTFS_BACKEND' --env TORRENTFS_FILESYSTEM_BACKEND=smb
expect_failure 'removed STRM switch' 'TORRENTFS_SMB_STRM' --env TORRENTFS_SMB_STRM_ENABLED=true
expect_failure 'removed SMB user name variable' 'TORRENTFS_USERNAME' --env TORRENTFS_SMB_USERNAME=reader
expect_failure 'removed SMB password file variable' 'TORRENTFS_PASSWORD' --env TORRENTFS_SMB_PASSWORD_FILE=/run/secrets/password
expect_failure 'STRM requires HTTP' 'strm.base_url' --env TORRENTFS_BACKEND=smb \
	--env TORRENTFS_USERNAME=reader --env "TORRENTFS_PASSWORD=$SECRET" \
	--env TORRENTFS_HTTP_LISTEN_ADDR= --env TORRENTFS_STRM_BASE_URL=http://player.example:8080
expect_failure 'unknown environment variable' 'not a known configuration item' --env TORRENTFS_HTTP_LISEN_ADDR=127.0.0.1:8080
expect_failure 'unknown environment variable is named' 'TORRENTFS_HTTP_LISEN_ADDR' --env TORRENTFS_HTTP_LISEN_ADDR=127.0.0.1:8080
printf 'docker config smoke: identity, selector, credential, removed name, unknown name, and STRM failures passed\n'

# The configuration commands work inside the image: they check the
# configuration of the image and print it without disclosing a password.
container="${PREFIX}-check"
containers+=("$container")
timeout 30 docker run --rm --name "$container" --cap-drop ALL --security-opt no-new-privileges:true \
	--entrypoint /usr/local/bin/torrentfs "$IMAGE" config check >/dev/null 2>&1 ||
	fail 'the configuration of the image does not pass the check'
timeout 30 docker run --rm --name "$container" --cap-drop ALL --security-opt no-new-privileges:true \
	--entrypoint /usr/local/bin/torrentfs "$IMAGE" config print >"$work_dir/printed.toml" 2>&1 ||
	fail 'the configuration of the image cannot be printed'
grep -q 'password = "<redacted>"' "$work_dir/printed.toml" ||
	fail 'the printed configuration does not redact the password'
! grep -q "$SECRET" "$work_dir/printed.toml" ||
	fail 'the printed configuration disclosed a password'
printf 'docker config smoke: the configuration commands work inside the image\n'

# A configuration with several mistakes reports all of them at once.
cat >"$work_dir/broken.toml" <<EOF
backend = "fuse"
[cache]
capacity = "0B"
[log]
level = "nonsense"
EOF
chmod 0444 "$work_dir/broken.toml"
output="$(timeout 30 docker run --rm --cap-drop ALL --security-opt no-new-privileges:true \
	--mount "type=bind,src=$work_dir/broken.toml,dst=/config.toml,readonly" \
	--entrypoint /usr/local/bin/torrentfs "$IMAGE" config check --config /config.toml 2>&1)" &&
	fail 'a broken configuration passed the check'
for expected in 'cache.capacity' 'log.level' 'fuse.mountpoint'; do
	[[ "$output" == *"$expected"* ]] || fail "the check did not report $expected: $output"
done
printf 'docker config smoke: the check reports every mistake at once\n'

mkdir -p "$work_dir/blocked"
chmod 000 "$work_dir/blocked"
expect_failure 'inaccessible data' 'permission denied' --user 99:100 --env PUID=99 --env PGID=100 \
	--mount "type=bind,src=$work_dir/blocked,dst=/torrents"
chmod 0700 "$work_dir/blocked"

# A TOML with the unified credentials enables SMB with no HTTP listener and no
# Unix-name lookup. The password file no longer exists, so the credentials
# come from the configuration file itself.
cat >"$work_dir/native.toml" <<EOF
backend = "smb"
username = "not-a-unix-account"
password = "$SECRET"
[filesystem]
read_timeout = "30s"
[http]
listen_addr = ""
[smb]
listen_addr = "0.0.0.0:1445"
[fuse]
mountpoint = "/not-needed-for-smb"
EOF
chmod 0444 "$work_dir/native.toml"
container="${PREFIX}-secret"
containers+=("$container")
timeout 30 docker run --detach --name "$container" --cap-drop ALL --security-opt no-new-privileges:true \
	--mount "type=bind,src=$work_dir/native.toml,dst=/config.toml,readonly" \
	"$IMAGE" -config /config.toml /torrents >/dev/null
wait_running "$container"
assert_identity "$container" 1000 1000
timeout 10 docker exec "$container" /bin/sh -eu -c '
	! grep -Eq ":[0]*1F90[[:space:]].*[[:space:]]0A[[:space:]]" /proc/net/tcp /proc/net/tcp6
	test ! -e /dev/fuse && test ! -d /run/samba
'
stop_clean "$container"
printf 'docker config smoke: TOML-selected SMB-only with a TOML password and no FUSE passed\n'

# The shared environment credentials enable HTTP password login.
container="${PREFIX}-http"
start "$container" --publish 127.0.0.1::8080 \
	--env TORRENTFS_BACKEND=none --env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 \
	--env TORRENTFS_HTTP_AUTH_ENABLED=true --env "TORRENTFS_PASSWORD=$SECRET"
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
