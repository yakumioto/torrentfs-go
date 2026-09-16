#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

: "${GITHUB_SHA:?GITHUB_SHA is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"
: "${GITHUB_RUN_URL:?GITHUB_RUN_URL is required}"
: "${NIGHTLY_DATE:?NIGHTLY_DATE is required}"
: "${NIGHTLY_TAG:?NIGHTLY_TAG is required}"

if [[ ! "$GITHUB_SHA" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'GITHUB_SHA must be a 40-character lowercase hexadecimal commit: %s\n' "$GITHUB_SHA" >&2
  exit 1
fi

if [[ ! "$GITHUB_RUN_ID" =~ ^[0-9]+$ ]]; then
  printf 'GITHUB_RUN_ID must be numeric: %s\n' "$GITHUB_RUN_ID" >&2
  exit 1
fi

if [[ ! "$NIGHTLY_DATE" =~ ^[0-9]{8}$ ]]; then
  printf 'NIGHTLY_DATE must use UTC YYYYMMDD format: %s\n' "$NIGHTLY_DATE" >&2
  exit 1
fi

parsed_date="$(date -u -d "${NIGHTLY_DATE:0:4}-${NIGHTLY_DATE:4:2}-${NIGHTLY_DATE:6:2}" +%Y%m%d 2>/dev/null)" || {
  printf 'NIGHTLY_DATE is not a valid UTC date: %s\n' "$NIGHTLY_DATE" >&2
  exit 1
}
if [[ "$parsed_date" != "$NIGHTLY_DATE" ]]; then
  printf 'NIGHTLY_DATE is not a valid UTC date: %s\n' "$NIGHTLY_DATE" >&2
  exit 1
fi

short_sha="${GITHUB_SHA:0:7}"
expected_tag="nightly-${NIGHTLY_DATE}-${short_sha}-${GITHUB_RUN_ID}"
if [[ "$NIGHTLY_TAG" != "$expected_tag" ]]; then
  printf 'NIGHTLY_TAG does not match the event commit and run: %s\n' "$NIGHTLY_TAG" >&2
  exit 1
fi

if [[ "$GITHUB_RUN_URL" =~ [[:space:]] ]]; then
  printf 'GITHUB_RUN_URL must not contain whitespace\n' >&2
  exit 1
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"
cd -- "$repo_root"

work_dir="$(mktemp -d)"
trap 'rm -rf -- "$work_dir"' EXIT

binary_path="$work_dir/torrentfs"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-mod=readonly \
  go build -trimpath -ldflags="-s -w" -o "$binary_path" ./cmd/torrentfs

go_version="$(go version)"
if [[ -z "$go_version" || "$go_version" == *$'\n'* || "$go_version" == *$'\r'* ]]; then
  printf 'go version returned an invalid value: %s\n' "$go_version" >&2
  exit 1
fi

cp -- "$repo_root/LICENSE" "$work_dir/LICENSE"
{
  printf 'COMMIT=%s\n' "$GITHUB_SHA"
  printf 'UTC_DATE=%s\n' "$NIGHTLY_DATE"
  printf 'NIGHTLY_TAG=%s\n' "$NIGHTLY_TAG"
  printf 'GOOS=linux\n'
  printf 'GOARCH=amd64\n'
  printf 'CGO_ENABLED=0\n'
  printf 'GO_VERSION=%s\n' "$go_version"
  printf 'GITHUB_RUN_ID=%s\n' "$GITHUB_RUN_ID"
  printf 'RUN_URL=%s\n' "$GITHUB_RUN_URL"
} > "$work_dir/BUILD_INFO"

artifact_dir="$repo_root/dist/$NIGHTLY_TAG"
archive_name="torrentfs-${NIGHTLY_TAG}-linux-amd64.tar.gz"
checksum_name="${archive_name}.sha256"
archive_path="$artifact_dir/$archive_name"
checksum_path="$artifact_dir/$checksum_name"
rm -rf -- "$artifact_dir"
mkdir -p -- "$artifact_dir"

tar --create --file=- \
  --directory="$work_dir" \
  --sort=name \
  --mtime='UTC 1970-01-01 00:00:00' \
  --owner=0 \
  --group=0 \
  --numeric-owner \
  torrentfs BUILD_INFO LICENSE | gzip -n > "$archive_path"

(
  cd -- "$artifact_dir"
  sha256sum -- "$archive_name" > "$checksum_name"
  sha256sum --check "$checksum_name"
)

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    printf 'archive=%s\n' "$archive_name"
    printf 'checksum=%s\n' "$checksum_name"
    printf 'artifact_dir=%s\n' "$artifact_dir"
  } >> "$GITHUB_OUTPUT"
fi

printf 'Created %s and %s\n' "$archive_path" "$checksum_path"
