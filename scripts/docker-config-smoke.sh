#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"
cd -- "$repo_root"

for command_name in docker curl awk timeout; do
	command -v "$command_name" >/dev/null || {
		printf 'docker config smoke: %s is required\n' "$command_name" >&2
		exit 1
	}
done

work_dir="$(mktemp -d)"
image="torrentfs-config-smoke:$$"
defaults_container="torrentfs-config-defaults-$$"
explicit_container="torrentfs-config-explicit-$$"
env_container="torrentfs-config-env-$$"
file_container="torrentfs-config-file-$$"
positive_container="torrentfs-config-positive-$$"
password_hash='$2a$10$N9qo8uLOickgx2ZMRZoMye8fOsiTWZqYtkxvXkKm8BMzjT7t/vIdq'

cleanup() {
	local status=$?
	trap - EXIT INT TERM
	for container in \
		"$defaults_container" "$explicit_container" "$env_container" "$file_container" "$positive_container"; do
		docker rm -f "$container" >/dev/null 2>&1 || true
	done
	if [[ -d "$work_dir/torrents" ]]; then
		docker run --rm --mount "type=bind,src=$work_dir/torrents,dst=/scratch" \
			--entrypoint /bin/sh "$image" -c 'rm -rf /scratch/* /scratch/..?* /scratch/.[!.]*' >/dev/null 2>&1 || true
	fi
	docker image rm "$image" >/dev/null 2>&1 || true
	rm -rf -- "$work_dir"
	exit "$status"
}
trap cleanup EXIT INT TERM

fail() {
	printf 'docker config smoke: %s\n' "$*" >&2
	exit 1
}

wait_running() {
	local container="$1"
	local state=''
	for _ in {1..60}; do
		state="$(timeout 10 docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)"
		case "$state" in
		running)
			if timeout 10 docker exec "$container" test -r /etc/torrentfs/runtime-identity >/dev/null 2>&1; then
				return 0
			fi
			;;
		exited|dead)
			docker logs "$container" >&2 || true
			fail "$container exited before becoming ready (state=$state)"
			;;
		esac
		sleep 0.2
	done
	docker logs "$container" >&2 || true
	fail "$container did not become ready within 12 seconds"
}

published_port() {
	local container="$1"
	local port=''
	for _ in {1..60}; do
		port="$(timeout 10 docker port "$container" 8080/tcp 2>/dev/null | awk -F: 'NF {print $NF; exit}' || true)"
		if [[ -n "$port" ]]; then
			printf '%s\n' "$port"
			return 0
		fi
		sleep 0.2
	done
	fail "could not determine published port for $container"
}

wait_http() {
	local url="$1"
	for _ in {1..60}; do
		if curl --silent --show-error --fail --connect-timeout 2 --max-time 5 "$url" >/dev/null; then
			return 0
		fi
		sleep 0.2
	done
	fail "HTTP endpoint did not become ready: $url"
}

stop_clean() {
	local container="$1"
	local status
	timeout 30 docker kill --signal TERM "$container" >/dev/null || fail "could not signal $container"
	status="$(timeout 30 docker wait "$container")" || fail "$container did not stop within 30 seconds"
	[[ "$status" == "0" ]] || fail "$container stopped with exit code $status"
	docker rm "$container" >/dev/null || fail "could not remove $container"
}

runtime_identity() {
	local container="$1"
	timeout 10 docker exec "$container" /bin/sh -c 'cat /etc/torrentfs/runtime-identity'
}

assert_runtime_identity() {
	local container="$1" expected_uid="$2" expected_gid="$3" actual expected
	actual="$(runtime_identity "$container")"
	expected="$(printf 'uid=%s\ngid=%s\nuser=torrentfs\ngroup=torrentfs' "$expected_uid" "$expected_gid")"
	[[ "$actual" == "$expected" ]] ||
		fail "$container runtime identity was unexpected: $actual"
	[[ "$(timeout 10 docker exec "$container" id -u torrentfs)" == "$expected_uid" ]] ||
		fail "$container torrentfs user UID is not $expected_uid"
	[[ "$(timeout 10 docker exec "$container" id -g torrentfs)" == "$expected_gid" ]] ||
		fail "$container torrentfs group GID is not $expected_gid"
	[[ "$(timeout 10 docker exec "$container" stat -c '%u:%g %a' /etc/torrentfs/runtime-identity)" == '0:0 444' ]] ||
		fail "$container runtime identity file is not root-owned and mode 0444"
}

process_identity() {
	local container="$1" process_name="$2"
	timeout 10 docker exec "$container" /bin/sh -c '
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

assert_process_identity() {
	local container="$1" process_name="$2" expected_uid="$3" expected_gid="$4"
	local actual pid uid gid
	actual="$(process_identity "$container" "$process_name")" ||
		fail "$container has no $process_name process"
	IFS=' ' read -r pid uid gid <<<"$actual"
	[[ "$uid" == "$expected_uid:$expected_uid:$expected_uid:$expected_uid" ]] ||
		fail "$container $process_name UID credentials were $uid, expected $expected_uid in every field"
	[[ "$gid" == "$expected_gid:$expected_gid:$expected_gid:$expected_gid" ]] ||
		fail "$container $process_name GID credentials were $gid, expected $expected_gid in every field"
	printf 'docker config smoke: %s %s runs as %s:%s\n' "$process_name" "$pid" "$expected_uid" "$expected_gid"
}

expect_start_failure() {
	local name="$1" expected="$2" output status=0
	shift 2
	output="$(timeout 30 docker run --rm "$@" "$image" 2>&1)" || status=$?
	[[ "$status" != 0 && "$status" != 124 ]] || fail "$name unexpectedly started or timed out"
	[[ "$output" == *"$expected"* ]] || fail "$name lacked diagnostic $expected: $output"
	[[ "$output" != *'starting torrentfs'* ]] || fail "$name reached service startup: $output"
}

cat >"$work_dir/external.toml" <<EOF
[http]
listen_addr = "0.0.0.0:8080"

[http.auth]
enabled = true
username = "alice"
password_hash = "$password_hash"
token_ttl = "30m"
EOF
mkdir -p "$work_dir/torrents" "$work_dir/blocked-torrents"
chmod 0777 "$work_dir/torrents"

printf 'docker config smoke: building %s\n' "$image"
timeout 1800 docker build --tag "$image" . >/dev/null

timeout 30 docker run --rm --entrypoint /bin/sh "$image" -c \
	'test -r /etc/torrentfs/torrentfs.toml && test -d /torrents && test -d /share && \
	 test ! -e /etc/torrentfs/runtime-identity && test "$(stat -c %a /torrents)" = 777 && \
	 command -v useradd >/dev/null && command -v usermod >/dev/null && command -v groupadd >/dev/null && \
	 command -v groupmod >/dev/null && command -v setpriv >/dev/null && \
	 command -v smbd >/dev/null && command -v smbpasswd >/dev/null && command -v testparm >/dev/null && \
	 testparm -s /etc/samba/torrentfs-smb.conf | \
	 sed -n "/^\\[torrentfs\\]/,\$p" | \
	 grep -Eq "^[[:space:]]*path[[:space:]]=[[:space:]]*/share[[:space:]]*$"' \
	|| fail 'image is missing its readable configuration, writable /torrents, /share, or required runtime tools'
[[ "$(timeout 30 docker image inspect "$image" --format '{{json .Config.Entrypoint}}')" == \
	'["/usr/bin/tini","--","/usr/local/bin/torrentfs-entrypoint"]' ]] ||
	fail 'image entrypoint is not the tini-wrapped torrentfs entrypoint'
[[ "$(timeout 30 docker image inspect "$image" --format '{{json .Config.Cmd}}')" == \
	'["-config","/etc/torrentfs/torrentfs.toml","/torrents"]' ]] ||
	fail 'image CMD changed from the documented default'
timeout 30 docker image inspect "$image" --format '{{json .Config.ExposedPorts}}' |
	grep -F '"445/tcp"' >/dev/null || fail 'image does not expose 445/tcp'
printf 'docker config smoke: generic root-entrypoint image contract is readable\n'

printf 'docker config smoke: verifying unset PUID/PGID defaults to 1000:1000\n'
docker run --detach --name "$defaults_container" "$image" >/dev/null
wait_running "$defaults_container"
assert_runtime_identity "$defaults_container" 1000 1000
assert_process_identity "$defaults_container" torrentfs 1000 1000
stop_clean "$defaults_container"
defaults_container=''
printf 'docker config smoke: default runtime identity passed\n'

printf 'docker config smoke: starting HTTP-only service as 99:100\n'
docker run --detach --name "$explicit_container" \
	--publish 127.0.0.1::8080 \
	--env PUID=99 --env PGID=100 \
	"$image" >/dev/null
wait_running "$explicit_container"
assert_runtime_identity "$explicit_container" 99 100
assert_process_identity "$explicit_container" torrentfs 99 100
timeout 10 docker exec "$explicit_container" /bin/sh -c \
	'test ! -e /run/samba/smbd.pid && test ! -e /run/samba/private/passdb.tdb' ||
	fail 'HTTP-only startup unexpectedly initialized Samba'
stop_clean "$explicit_container"
explicit_container=''
printf 'docker config smoke: HTTP-only runtime identity and Samba isolation passed\n'

smb_preflight_secret='config-smoke-secret'
expect_smb_preflight_failure() {
	local name="$1" expected="$2" output status=0
	shift 2
	output="$(timeout 30 docker run --rm --env PUID=99 --env PGID=100 \
		--env TORRENTFS_SMB_ENABLED=true "$@" "$image" 2>&1)" || status=$?
	[[ "$status" != 0 && "$status" != 124 ]] || fail "$name unexpectedly started or timed out"
	[[ "$output" == *"$expected"* ]] || fail "$name lacked diagnostic $expected: $output"
	[[ "$output" != *"$smb_preflight_secret"* ]] || fail "$name leaked the password"
}

printf 'docker config smoke: checking that incomplete SMB credentials fail closed\n'
expect_smb_preflight_failure 'missing SMB username' 'TORRENTFS_USERNAME is required when SMB is enabled' \
	--env "TORRENTFS_PASSWORD=$smb_preflight_secret"
expect_smb_preflight_failure 'missing SMB password' 'TORRENTFS_PASSWORD is required when SMB is enabled' \
	--env TORRENTFS_USERNAME=torrentfs
expect_smb_preflight_failure 'invalid SMB username' 'TORRENTFS_USERNAME is not a valid Unix account name' \
	--env TORRENTFS_USERNAME='invalid/username' --env "TORRENTFS_PASSWORD=$smb_preflight_secret"
expect_smb_preflight_failure 'SMB username UID mismatch' 'TORRENTFS_USERNAME must resolve to the torrentfs runtime UID' \
	--env TORRENTFS_USERNAME=root --env "TORRENTFS_PASSWORD=$smb_preflight_secret"
printf 'docker config smoke: SMB credential failure was rejected before startup\n'

printf 'docker config smoke: checking invalid runtime identity values\n'
expect_start_failure 'empty PUID' 'PUID must be an unsigned decimal integer from 1 to 4294967294 (got empty value)' \
	--env PUID= --env PGID=100
expect_start_failure 'empty PGID' 'PGID must be an unsigned decimal integer from 1 to 4294967294 (got empty value)' \
	--env PUID=99 --env PGID=
expect_start_failure 'non-numeric PUID' 'PUID must be an unsigned decimal integer from 1 to 4294967294 (got invalid value)' \
	--env PUID=abc --env PGID=100
expect_start_failure 'negative PGID' 'PGID must be an unsigned decimal integer from 1 to 4294967294 (got invalid value)' \
	--env PUID=99 --env PGID=-1
expect_start_failure 'zero PUID' 'PUID must be between 1 and 4294967294 (got zero)' \
	--env PUID=0 --env PGID=100
expect_start_failure 'out-of-range PGID' 'PGID must be between 1 and 4294967294 (got out-of-range value)' \
	--env PUID=99 --env PGID=4294967295
expect_start_failure 'non-root entrypoint' 'container entrypoint must start as root; do not use docker run --user' \
	--user 99:100 --env PUID=99 --env PGID=100
printf 'docker config smoke: invalid identity and non-root entrypoint diagnostics passed\n'

printf 'docker config smoke: checking /torrents access validation\n'
docker run --detach --name "$positive_container" \
	--env PUID=99 --env PGID=100 \
	--mount "type=bind,src=$work_dir/torrents,dst=/torrents" \
	"$image" >/dev/null
wait_running "$positive_container"
stop_clean "$positive_container"
positive_container=''
printf 'docker config smoke: writable /torrents startup passed\n'

chmod 000 "$work_dir/blocked-torrents"
blocked_output=''
blocked_status=0
blocked_output="$(timeout 30 docker run --rm --env PUID=99 --env PGID=100 \
	--mount "type=bind,src=$work_dir/blocked-torrents,dst=/torrents" "$image" 2>&1)" || blocked_status=$?
chmod 0777 "$work_dir/blocked-torrents"
[[ "$blocked_status" != 0 && "$blocked_status" != 124 ]] || fail 'unwritable /torrents unexpectedly started or timed out'
[[ "$blocked_output" == *'runtime identity 99:100 cannot read and write /torrents'* ]] ||
	fail "unwritable /torrents lacked the runtime identity diagnostic: $blocked_output"
[[ "$blocked_output" != *'starting torrentfs'* ]] ||
	fail "unwritable /torrents reached service startup: $blocked_output"
printf 'docker config smoke: inaccessible /torrents fails before service startup\n'

printf 'docker config smoke: starting with environment overrides only\n'
docker run --detach --name "$env_container" \
	--publish 127.0.0.1::8080 \
	--env PUID=99 --env PGID=100 \
	--env 'TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080' \
	--env 'TORRENTFS_HTTP_AUTH_ENABLED=true' \
	--env TORRENTFS_USERNAME=alice \
	--env TORRENTFS_PASSWORD=password \
	"$image" >/dev/null
wait_running "$env_container"
env_port="$(published_port "$env_container")"
env_url="http://127.0.0.1:$env_port"
wait_http "$env_url/"
env_api_status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 \
	--output "$work_dir/env-api.json" --write-out '%{http_code}' "$env_url/api/v1/torrents")"
[[ "$env_api_status" == "401" ]] || fail "environment-only API status was $env_api_status, want 401"
env_login_status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 \
	--header 'Content-Type: application/json' \
	--data '{"username":"alice","password":"password"}' \
	--output "$work_dir/env-login.json" --write-out '%{http_code}' \
	"$env_url/api/v1/auth/login")"
[[ "$env_login_status" == "200" ]] || fail "environment-only login status was $env_login_status, want 200"
stop_clean "$env_container"
env_container=''
printf 'docker config smoke: environment-only HTTP startup passed\n'

printf 'docker config smoke: starting with an external TOML file\n'
docker run --detach --name "$file_container" \
	--env PUID=99 --env PGID=100 \
	--publish 127.0.0.1::8080 \
	--mount "type=bind,src=$work_dir/torrents,dst=/torrents" \
	--mount "type=bind,src=$work_dir/external.toml,dst=/config.toml,readonly" \
	"$image" -config /config.toml /torrents >/dev/null
wait_running "$file_container"
file_port="$(published_port "$file_container")"
file_url="http://127.0.0.1:$file_port"
wait_http "$file_url/"
file_api_status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 \
	--output "$work_dir/file-api.json" --write-out '%{http_code}' "$file_url/api/v1/torrents")"
[[ "$file_api_status" == "401" ]] || fail "external-file API status was $file_api_status, want 401"
stop_clean "$file_container"
file_container=''
printf 'docker config smoke: external TOML startup passed\n'

printf 'docker config smoke: all checks passed\n'
