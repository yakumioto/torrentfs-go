#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"
for command_name in docker curl awk timeout; do
	command -v "$command_name" >/dev/null || {
		printf 'docker config smoke: %s is required\n' "$command_name" >&2
		exit 1
	}
done

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/mio93-config-smoke.XXXXXX")"
image="torrentfs-mio93-config-smoke:${BASHPID}"
container_prefix="torrentfs-mio93-config-${BASHPID}"
containers=()
password_hash='$2a$10$N9qo8uLOickgx2ZMRZoMye8fOsiTWZqYtkxvXkKm8BMzjT7t/vIdq'
config_secret='config-smoke-secret'

cleanup() {
	local status=$? container
	trap - EXIT INT TERM
	for container in "${containers[@]}"; do
		docker logs "$container" >"$work_dir/$container.log" 2>&1 || true
		docker rm -f "$container" >/dev/null 2>&1 || true
	done
	if [[ "${KEEP_SMOKE_ARTIFACTS:-0}" == 1 ]]; then
		printf 'docker config smoke: artifacts retained at %s\n' "$work_dir" >&2
	else
		# Metadata may be owned by the configured container UID, not this user.
		docker run --rm --mount "type=bind,src=$work_dir,dst=/scratch" \
			--entrypoint /bin/sh "$image" -c 'rm -rf /scratch/* /scratch/..?* /scratch/.[!.]*' >/dev/null 2>&1 || true
		rm -rf -- "$work_dir"
	fi
	docker image rm "$image" >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

fail() {
	printf 'docker config smoke: %s\n' "$*" >&2
	exit 1
}

wait_running() {
	local container="$1" state=''
	for _ in {1..60}; do
		state="$(timeout 10 docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)"
		case "$state" in
		running)
			if timeout 10 docker exec "$container" test -r /etc/torrentfs/runtime-identity >/dev/null 2>&1 &&
				process_identity "$container" torrentfs >/dev/null 2>&1; then
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
	timeout 10 docker port "$1" 8080/tcp | awk -F: 'NF {print $NF; exit}'
}

wait_http() {
	local url="$1"
	for _ in {1..60}; do
		if curl --silent --fail --connect-timeout 2 --max-time 5 "$url" >/dev/null; then
			return 0
		fi
		sleep 0.2
	done
	fail "HTTP endpoint did not become ready: $url"
}

stop_clean() {
	local container="$1" status
	timeout 30 docker kill --signal TERM "$container" >/dev/null || fail "could not signal $container"
	status="$(timeout 30 docker wait "$container")" || fail "$container did not stop within 30 seconds"
	[[ "$status" == 0 ]] || fail "$container stopped with exit code $status"
}

assert_runtime_identity() {
	local container="$1" expected_uid="$2" expected_gid="$3" actual expected
	actual="$(timeout 10 docker exec "$container" /bin/sh -c 'cat /etc/torrentfs/runtime-identity')"
	expected="$(printf 'uid=%s\ngid=%s\nuser=torrentfs\ngroup=torrentfs' "$expected_uid" "$expected_gid")"
	[[ "$actual" == "$expected" ]] || fail "$container runtime identity was unexpected: $actual"
	[[ "$(timeout 10 docker exec "$container" id -u torrentfs)" == "$expected_uid" ]] || fail 'incorrect runtime UID'
	[[ "$(timeout 10 docker exec "$container" id -g torrentfs)" == "$expected_gid" ]] || fail 'incorrect runtime GID'
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
	local container="$1" expected_uid="$2" expected_gid="$3" actual pid uid gid
	actual="$(process_identity "$container" torrentfs)" || fail "$container has no torrentfs process"
	IFS=' ' read -r pid uid gid <<<"$actual"
	[[ "$uid" == "$expected_uid:$expected_uid:$expected_uid:$expected_uid" ]] || fail "incorrect daemon UIDs: $uid"
	[[ "$gid" == "$expected_gid:$expected_gid:$expected_gid:$expected_gid" ]] || fail "incorrect daemon GIDs: $gid"
	[[ "$(timeout 10 docker exec "$container" /bin/sh -c 'cat /proc/1/task/1/children')" == "$pid " ]] ||
		fail 'tini must directly own the single torrentfs process, not a supervisor'
	if process_identity "$container" smbd >/dev/null 2>&1; then fail 'unexpected smbd process'; fi
	timeout 10 docker exec "$container" /bin/sh -c '! grep -q " - fuse" /proc/self/mountinfo; test ! -e /dev/fuse' ||
		fail 'non-FUSE startup unexpectedly has FUSE resources'
	printf 'docker config smoke: single torrentfs PID %s runs as %s:%s\n' "$pid" "$expected_uid" "$expected_gid"
}

expect_start_failure() {
	local name="$1" expected="$2" output status=0
	shift 2
	output="$(timeout 30 docker run --rm "$@" "$image" 2>&1)" || status=$?
	[[ "$status" != 0 && "$status" != 124 ]] || fail "$name unexpectedly started or timed out"
	[[ "$output" == *"$expected"* ]] || fail "$name lacked diagnostic $expected: $output"
	[[ "$output" != *'starting torrentfs'* ]] || fail "$name reached service startup: $output"
}

expect_config_failure() {
	local name="$1" expected="$2" output status=0
	shift 2
	output="$(timeout 30 docker run --rm "$@" "$image" 2>&1)" || status=$?
	[[ "$status" == 2 ]] || fail "$name exited with status $status, want configuration error: $output"
	[[ "$output" == *"$expected"* ]] || fail "$name lacked diagnostic $expected: $output"
	[[ "$output" != *'torrentfs starting'* ]] || fail "$name reached daemon startup: $output"
	[[ "$output" != *"$config_secret"* && "$output" != *'strm-config-secret'* ]] || fail "$name exposed credentials"
}

expect_cli_config_failure() {
	local name="$1" expected="$2" output status=0
	shift 2
	output="$(timeout 30 docker run --rm --entrypoint /usr/local/bin/torrentfs \
		"$@" "$image" -config /etc/torrentfs/torrentfs.toml -strm-mountpoint /share-strm /torrents 2>&1)" || status=$?
	[[ "$status" == 2 && "$output" == *"$expected"* ]] || fail "$name: status=$status: $output"
	[[ "$output" != *'torrentfs starting'* && "$output" != *'strm-config-secret'* ]] || fail "$name reached startup or leaked URL credentials"
}

start_container() {
	local container="$1"
	shift
	containers+=("$container")
	docker run --detach --name "$container" "$@" >/dev/null
	wait_running "$container"
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
cat >"$work_dir/smb.toml" <<'EOF'
[filesystem]
mode = "smb"
[smb]
listen_addr = "0.0.0.0:1555"
share_name = "library"
strm_enabled = true
EOF
mkdir -p "$work_dir/torrents" "$work_dir/blocked-torrents"
chmod 0777 "$work_dir/torrents"
printf 'docker config smoke: building %s\n' "$image"
timeout --foreground 1800 docker build --tag "$image" "$repo_root" >"$work_dir/build.log" 2>&1 || {
	cat "$work_dir/build.log" >&2
	fail 'image build failed'
}

timeout 30 docker run --rm --entrypoint /bin/sh "$image" -eu -c '
	test -r /etc/torrentfs/torrentfs.toml
	for path in /torrents /share /share-strm; do test -d "$path"; done
	test ! -e /etc/torrentfs/runtime-identity
	test "$(stat -c %a /torrents)" = 777
	for tool in useradd usermod groupadd groupmod setpriv; do command -v "$tool" >/dev/null; done
	for tool in smbd smbpasswd testparm; do
		if command -v "$tool" >/dev/null 2>&1; then exit 1; fi
	done
	test ! -e /etc/samba/torrentfs-smb.conf
	test ! -e /etc/samba/torrentfs-smb-strm.conf
	test ! -d /run/samba
' || fail 'image runtime/configuration contract failed'
[[ "$(timeout 30 docker image inspect "$image" --format '{{json .Config.Entrypoint}}')" == \
	'["/usr/bin/tini","--","/usr/local/bin/torrentfs-entrypoint"]' ]] || fail 'unexpected image entrypoint'
[[ "$(timeout 30 docker image inspect "$image" --format '{{json .Config.Cmd}}')" == \
	'["-config","/etc/torrentfs/torrentfs.toml","/torrents"]' ]] || fail 'unexpected default CMD'
ports="$(timeout 30 docker image inspect "$image" --format '{{json .Config.ExposedPorts}}')"
for port in '8080/tcp' '1445/tcp' '6881/tcp' '6881/udp'; do
	[[ "$ports" == *"\"$port\""* ]] || fail "image does not expose $port"
done
[[ "$ports" != *'"445/tcp"'* ]] || fail 'image still exposes the old SMB port'

printf 'docker config smoke: checking runtime identities and HTTP-only defaults\n'
defaults_container="$container_prefix-defaults"
start_container "$defaults_container" "$image"
assert_runtime_identity "$defaults_container" 1000 1000
assert_process_identity "$defaults_container" 1000 1000
stop_clean "$defaults_container"
explicit_container="$container_prefix-explicit"
start_container "$explicit_container" --env PUID=99 --env PGID=100 --env TORRENTFS_STRM_ENABLED=0 "$image"
assert_runtime_identity "$explicit_container" 99 100
assert_process_identity "$explicit_container" 99 100
stop_clean "$explicit_container"

printf 'docker config smoke: checking SMB credentials and configuration failures\n'
expect_config_failure 'missing SMB username' 'smb.credentials' --env TORRENTFS_FILESYSTEM_MODE=smb --env "TORRENTFS_PASSWORD=$config_secret"
expect_config_failure 'missing SMB password' 'smb.credentials' --env TORRENTFS_FILESYSTEM_MODE=smb --env TORRENTFS_USERNAME=media
expect_config_failure 'invalid SMB username' 'smb.username' --env TORRENTFS_FILESYSTEM_MODE=smb \
	--env TORRENTFS_USERNAME='invalid/username' --env "TORRENTFS_PASSWORD=$config_secret"
for value in '' yes 2 ' true'; do
	expect_config_failure 'invalid STRM boolean alias' 'TORRENTFS_STRM_ENABLED' --env "TORRENTFS_STRM_ENABLED=$value"
	expect_config_failure 'invalid canonical STRM boolean' 'TORRENTFS_SMB_STRM_ENABLED' --env "TORRENTFS_SMB_STRM_ENABLED=$value"
	expect_config_failure 'invalid SMB mode alias' 'TORRENTFS_SMB_ENABLED' --env "TORRENTFS_SMB_ENABLED=$value"
done
for value in true 1; do
	expect_config_failure 'SMB STRM without base URL' 'smb.strm_enabled' --env TORRENTFS_FILESYSTEM_MODE=smb \
		--env "TORRENTFS_SMB_STRM_ENABLED=$value" --env TORRENTFS_USERNAME=media --env "TORRENTFS_PASSWORD=$config_secret"
done
expect_config_failure 'SMB STRM with disabled HTTP' 'strm.base_url' --env TORRENTFS_FILESYSTEM_MODE=smb \
	--env TORRENTFS_SMB_STRM_ENABLED=true --env TORRENTFS_USERNAME=media --env "TORRENTFS_PASSWORD=$config_secret" \
	--env TORRENTFS_STRM_BASE_URL=http://player.example:8080 --env TORRENTFS_HTTP_LISTEN_ADDR=
expect_cli_config_failure 'STRM without base URL' 'strm.base_url'
expect_cli_config_failure 'STRM with disabled HTTP' 'strm.base_url' \
	--env TORRENTFS_STRM_BASE_URL=http://player.example:8080 --env TORRENTFS_HTTP_LISTEN_ADDR=
expect_cli_config_failure 'STRM with URL userinfo' 'strm.base_url' \
	--env TORRENTFS_STRM_BASE_URL=http://user:strm-config-secret@player.example:8080
expect_cli_config_failure 'STRM with URL query' 'strm.base_url' --env 'TORRENTFS_STRM_BASE_URL=http://player.example:8080?token=secret'
for strm_enabled in false true; do
	for mount_flag in mountpoint strm-mountpoint; do
		for flag_form in "-$mount_flag" "--$mount_flag" "-$mount_flag=" "--$mount_flag="; do
			args=("$flag_form" /override)
			[[ "$flag_form" != *= ]] || args=("${flag_form}/override")
			output=''; status=0
			output="$(timeout 30 docker run --rm --env PUID=99 --env PGID=100 \
				--env TORRENTFS_FILESYSTEM_MODE=smb --env "TORRENTFS_SMB_STRM_ENABLED=$strm_enabled" \
				--env TORRENTFS_STRM_BASE_URL=http://player.example:8080 \
				--env TORRENTFS_USERNAME=media --env "TORRENTFS_PASSWORD=$config_secret" \
				"$image" "${args[@]}" -config /etc/torrentfs/torrentfs.toml /torrents 2>&1)" || status=$?
			[[ "$status" == 2 && "$output" == *'SMB mode does not accept -mountpoint or -strm-mountpoint'* ]] ||
				fail "$flag_form was not rejected by SMB mode: $output"
			[[ "$output" != *'torrentfs starting'* && "$output" != *"$config_secret"* ]] || fail 'mount flag reached startup or leaked credentials'
		done
	done
done

printf 'docker config smoke: checking env > TOML and canonical > deprecated aliases\n'
mode_container="$container_prefix-mode-fuse"
start_container "$mode_container" --env PUID=99 --env PGID=100 \
	--env TORRENTFS_FILESYSTEM_MODE=fuse --env TORRENTFS_SMB_ENABLED=true \
	--env TORRENTFS_SMB_STRM_ENABLED=false --env TORRENTFS_STRM_ENABLED=true \
	--mount "type=bind,src=$work_dir/smb.toml,dst=/config.toml,readonly" "$image" -config /config.toml /torrents
assert_process_identity "$mode_container" 99 100
mode_logs="$(docker logs "$mode_container" 2>&1)"
[[ "$mode_logs" == *'filesystem_mode=fuse'* && "$mode_logs" == *'deprecated environment variable'* ]] || fail 'canonical mode did not override TOML/alias or warn'
stop_clean "$mode_container"
mode_container="$container_prefix-alias-fuse"
start_container "$mode_container" --env TORRENTFS_SMB_ENABLED=false --env TORRENTFS_STRM_ENABLED=false \
	--mount "type=bind,src=$work_dir/smb.toml,dst=/config.toml,readonly" "$image" -config /config.toml /torrents
assert_process_identity "$mode_container" 1000 1000
[[ "$(docker logs "$mode_container" 2>&1)" == *'filesystem_mode=fuse'* ]] || fail 'deprecated mode alias did not override TOML'
stop_clean "$mode_container"
mode_container="$container_prefix-mode-smb"
start_container "$mode_container" --env PUID=99 --env PGID=100 \
	--env TORRENTFS_FILESYSTEM_MODE=smb --env TORRENTFS_SMB_ENABLED=false \
	--env TORRENTFS_SMB_LISTEN_ADDR=0.0.0.0:1445 --env TORRENTFS_SMB_SHARE_NAME=media \
	--env TORRENTFS_SMB_STRM_ENABLED=false --env TORRENTFS_STRM_ENABLED=true \
	--env TORRENTFS_USERNAME=media --env "TORRENTFS_PASSWORD=$config_secret" \
	--mount "type=bind,src=$work_dir/smb.toml,dst=/config.toml,readonly" "$image" -config /config.toml /torrents
assert_process_identity "$mode_container" 99 100
# A non-Unix username is allowed; the effective listener comes from canonical env.
timeout 10 docker exec "$mode_container" /bin/bash -ec '! getent passwd media; for _ in {1..30}; do if (: >/dev/tcp/127.0.0.1/1445) 2>/dev/null; then exit 0; fi; sleep .1; done; exit 1' || fail 'canonical SMB listener or independent username failed'
timeout 10 docker exec "$mode_container" /bin/bash -ec 'if (: >/dev/tcp/127.0.0.1/1555) 2>/dev/null; then exit 1; fi' || fail 'TOML listener was not overridden'
stop_clean "$mode_container"

printf 'docker config smoke: checking invalid runtime identity and data permissions\n'
expect_start_failure 'empty PUID' 'PUID must be an unsigned decimal integer from 1 to 4294967294 (got empty value)' --env PUID= --env PGID=100
expect_start_failure 'empty PGID' 'PGID must be an unsigned decimal integer from 1 to 4294967294 (got empty value)' --env PUID=99 --env PGID=
expect_start_failure 'non-numeric PUID' 'PUID must be an unsigned decimal integer from 1 to 4294967294 (got invalid value)' --env PUID=abc --env PGID=100
expect_start_failure 'negative PGID' 'PGID must be an unsigned decimal integer from 1 to 4294967294 (got invalid value)' --env PUID=99 --env PGID=-1
expect_start_failure 'zero PUID' 'PUID must be between 1 and 4294967294 (got zero)' --env PUID=0 --env PGID=100
expect_start_failure 'out-of-range PGID' 'PGID must be between 1 and 4294967294 (got out-of-range value)' --env PUID=99 --env PGID=4294967295
expect_start_failure 'non-root entrypoint' 'container entrypoint must start as root; do not use docker run --user' --user 99:100 --env PUID=99 --env PGID=100
positive_container="$container_prefix-positive"
start_container "$positive_container" --env PUID=99 --env PGID=100 --mount "type=bind,src=$work_dir/torrents,dst=/torrents" "$image"
stop_clean "$positive_container"
chmod 000 "$work_dir/blocked-torrents"
expect_start_failure 'unwritable /torrents' 'runtime identity 99:100 cannot read and write /torrents' \
	--env PUID=99 --env PGID=100 --mount "type=bind,src=$work_dir/blocked-torrents,dst=/torrents"
chmod 0777 "$work_dir/blocked-torrents"

printf 'docker config smoke: checking environment-only HTTP authentication\n'
env_container="$container_prefix-env"
start_container "$env_container" --publish 127.0.0.1::8080 --env PUID=99 --env PGID=100 \
	--env TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080 --env TORRENTFS_HTTP_AUTH_ENABLED=true \
	--env TORRENTFS_USERNAME=alice --env TORRENTFS_PASSWORD=password "$image"
env_url="http://127.0.0.1:$(published_port "$env_container")"
wait_http "$env_url/"
status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 --output "$work_dir/env-api.json" --write-out '%{http_code}' "$env_url/api/v1/torrents")"
[[ "$status" == 401 ]] || fail "environment API status was $status, want 401"
status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 --header 'Content-Type: application/json' \
	--data '{"password":"password"}' --output "$work_dir/env-login.json" --write-out '%{http_code}' "$env_url/api/v1/auth/login")"
[[ "$status" == 200 ]] || fail "environment login status was $status, want 200"
stop_clean "$env_container"

printf 'docker config smoke: checking external read-only TOML and HTTP credential override\n'
for variant in file override; do
	file_container="$container_prefix-$variant"
	env_args=()
	if [[ "$variant" == override ]]; then
		env_args=(--env TORRENTFS_USERNAME=bob --env TORRENTFS_PASSWORD=override-password)
	fi
	start_container "$file_container" --env PUID=99 --env PGID=100 --env TORRENTFS_STRM_ENABLED=false \
		--publish 127.0.0.1::8080 --mount "type=bind,src=$work_dir/torrents,dst=/torrents" \
		--mount "type=bind,src=$work_dir/external.toml,dst=/config.toml,readonly" "${env_args[@]}" "$image" -config /config.toml /torrents
	file_url="http://127.0.0.1:$(published_port "$file_container")"
	wait_http "$file_url/"
	status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 --output "$work_dir/$variant-api.json" --write-out '%{http_code}' "$file_url/api/v1/torrents")"
	[[ "$status" == 401 ]] || fail "external-file API status was $status, want 401"
	if [[ "$variant" == override ]]; then
		status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 --header 'Content-Type: application/json' \
			--data '{"password":"override-password"}' --output "$work_dir/override-login.json" --write-out '%{http_code}' "$file_url/api/v1/auth/login")"
		[[ "$status" == 200 ]] || fail 'runtime credentials did not override TOML bcrypt source'
	fi
	stop_clean "$file_container"
done
printf 'docker config smoke: all checks passed\n'
