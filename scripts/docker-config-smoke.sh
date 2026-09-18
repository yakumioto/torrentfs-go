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
default_container="torrentfs-config-default-$$"
env_container="torrentfs-config-env-$$"
file_container="torrentfs-config-file-$$"
password_hash='$2a$10$N9qo8uLOickgx2ZMRZoMye8fOsiTWZqYtkxvXkKm8BMzjT7t/vIdq'

cleanup() {
	local status=$?
	trap - EXIT INT TERM
	for container in "$default_container" "$env_container" "$file_container"; do
		docker rm -f "$container" >/dev/null 2>&1 || true
	done
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
			return 0
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

cat >"$work_dir/external.toml" <<EOF
[paths]
data_dir = "/data"
payload_dir = ""

[http]
listen_addr = "0.0.0.0:8080"

[http.auth]
enabled = true
username = "alice"
password_hash = "$password_hash"
token_ttl = "30m"
EOF
mkdir -p "$work_dir/data"

printf 'docker config smoke: building %s\n' "$image"
timeout 1800 docker build --tag "$image" . >/dev/null

timeout 30 docker run --rm --entrypoint /bin/sh "$image" -c \
	'test -r /etc/torrentfs/torrentfs.toml && test -d /data && test -d /torrents' \
	|| fail 'image is missing its readable default configuration or runtime directories'
printf 'docker config smoke: image default configuration is readable\n'

printf 'docker config smoke: starting with the image default CMD\n'
docker run --detach --name "$default_container" "$image" >/dev/null
wait_running "$default_container"
timeout 10 docker exec "$default_container" /bin/sh -c \
	'test -d /data && test -d /torrents && test -r /etc/torrentfs/torrentfs.toml' \
	|| fail 'default container cannot access its configured runtime paths'
stop_clean "$default_container"
printf 'docker config smoke: default CMD stayed running and stopped cleanly\n'

default_container=''
printf 'docker config smoke: starting with environment overrides only\n'
docker run --detach --name "$env_container" \
	--publish 127.0.0.1::8080 \
	--env 'TORRENTFS_HTTP_LISTEN_ADDR=0.0.0.0:8080' \
	--env 'TORRENTFS_HTTP_AUTH_ENABLED=true' \
	--env 'TORRENTFS_HTTP_AUTH_USERNAME=alice' \
	--env "TORRENTFS_HTTP_AUTH_PASSWORD_HASH=$password_hash" \
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
printf 'docker config smoke: environment-only HTTP startup passed\n'
env_container=''

printf 'docker config smoke: starting with an external TOML file\n'
docker run --detach --name "$file_container" \
	--user "$(id -u):$(id -g)" \
	--publish 127.0.0.1::8080 \
	--mount "type=bind,src=$work_dir/data,dst=/data" \
	--mount "type=bind,src=$work_dir/external.toml,dst=/config.toml,readonly" \
	"$image" -config /config.toml /data >/dev/null
wait_running "$file_container"
file_port="$(published_port "$file_container")"
file_url="http://127.0.0.1:$file_port"
wait_http "$file_url/"
file_api_status="$(curl --silent --show-error --connect-timeout 2 --max-time 5 \
	--output "$work_dir/file-api.json" --write-out '%{http_code}' "$file_url/api/v1/torrents")"
[[ "$file_api_status" == "401" ]] || fail "external-file API status was $file_api_status, want 401"
stop_clean "$file_container"
printf 'docker config smoke: external TOML startup passed\n'
file_container=''

printf 'docker config smoke: all checks passed\n'
