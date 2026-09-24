#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"
cd -- "$repo_root"

command -v docker >/dev/null
command -v curl >/dev/null

work_dir="$(mktemp -d)"
container_name="torrentfs-http-smoke-$$"
trap 'docker rm -f "$container_name" >/dev/null 2>&1 || true; rm -rf -- "$work_dir" || true' EXIT
mkdir -p "$work_dir/torrents"
chmod 0777 "$work_dir/torrents"
runtime_uid=1000
runtime_gid=1000

cat >"$work_dir/torrentfs.toml" <<'EOF'
[http]
listen_addr = "0.0.0.0:8080"
max_upload_bytes = 10485760

[http.auth]
enabled = true
username = "alice"
password_hash = "$2a$10$N9qo8uLOickgx2ZMRZoMye8fOsiTWZqYtkxvXkKm8BMzjT7t/vIdq"
token_ttl = "30m"
EOF

image="torrentfs-http-smoke:local"
docker build --tag "$image" .
docker run --detach --env "PUID=$runtime_uid" --env "PGID=$runtime_gid" --name "$container_name" --publish 127.0.0.1::8080 --volume "$work_dir/torrents:/torrents" --volume "$work_dir/torrentfs.toml:/config.toml:ro" "$image" -config /config.toml /torrents >/dev/null

port=''
for _ in {1..60}; do
  port="$(docker port "$container_name" 8080/tcp 2>/dev/null | awk -F: 'NF {print $NF; exit}')"
  if [[ -n "$port" ]]; then
    break
  fi
  sleep 0.2
done
[[ -n "$port" ]]
base_url="http://127.0.0.1:$port"

for _ in {1..60}; do
  if curl --silent --fail "$base_url/" >/dev/null; then
    break
  fi
  sleep 0.2
done

root_headers="$work_dir/root.headers"
curl --silent --show-error --dump-header "$root_headers" --output "$work_dir/root.html" "$base_url/"
grep -qi '^HTTP/.* 200 ' "$root_headers"
grep -qi '^Content-Type: text/html' "$root_headers"
grep -qi '^Cache-Control: no-cache' "$root_headers"

deep_headers="$work_dir/deep.headers"
curl --silent --show-error --dump-header "$deep_headers" --output "$work_dir/deep.html" "$base_url/torrents/deep-link"
grep -qi '^HTTP/.* 200 ' "$deep_headers"
cmp -- "$work_dir/root.html" "$work_dir/deep.html"

api_headers="$work_dir/api.headers"
api_status="$(curl --silent --show-error --dump-header "$api_headers" --output "$work_dir/api.json" --write-out '%{http_code}' "$base_url/api/v1/torrents")"
[[ "$api_status" == 401 ]]
grep -qi '^WWW-Authenticate: Bearer' "$api_headers"

login_headers="$work_dir/login.headers"
login_status="$(curl --silent --show-error --dump-header "$login_headers" --output "$work_dir/login.json" --write-out '%{http_code}' --header 'Content-Type: application/json' --data '{"username":"alice","password":"password"}' "$base_url/api/v1/auth/login")"
[[ "$login_status" == 200 ]]
grep -qi '^Cache-Control: no-store' "$login_headers"
token="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["token"])' "$work_dir/login.json")"
[[ -n "$token" ]]

auth_status="$(curl --silent --show-error --output "$work_dir/auth.json" --write-out '%{http_code}' --header "Authorization: Bearer $token" "$base_url/api/v1/torrents")"
[[ "$auth_status" == 200 ]]

logout_status="$(curl --silent --show-error --output "$work_dir/logout.body" --write-out '%{http_code}' --request POST --header "Authorization: Bearer $token" "$base_url/api/v1/auth/logout")"
[[ "$logout_status" == 204 ]]

refreshed_root_status="$(curl --silent --show-error --output "$work_dir/refreshed-root.html" --write-out '%{http_code}' "$base_url/")"
[[ "$refreshed_root_status" == 200 ]]
cmp -- "$work_dir/root.html" "$work_dir/refreshed-root.html"

refreshed_deep_status="$(curl --silent --show-error --output "$work_dir/refreshed-deep.html" --write-out '%{http_code}' "$base_url/torrents/refreshed")"
[[ "$refreshed_deep_status" == 200 ]]
cmp -- "$work_dir/root.html" "$work_dir/refreshed-deep.html"

unknown_status="$(curl --silent --show-error --dump-header "$work_dir/unknown.headers" --output "$work_dir/unknown.body" --write-out '%{http_code}' --header "Authorization: Bearer $token" "$base_url/api/v1/unknown")"
[[ "$unknown_status" == 401 ]]
! grep -q '<html' "$work_dir/unknown.body"

missing_asset_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' "$base_url/assets/missing.js")"
[[ "$missing_asset_status" == 404 ]]

printf 'HTTP smoke passed on %s\n' "$base_url"
