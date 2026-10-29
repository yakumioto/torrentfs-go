#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'
umask 022

fail() {
	printf 'torrentfs-entrypoint: ERROR: %s\n' "$*" >&2
	exit 1
}

runtime_id() {
	local name="$1" value="$2"
	[[ "$value" =~ ^[0-9]+$ ]] || fail "$name must be an integer from 1 to 4294967294"
	while [[ "${#value}" -gt 1 && "${value:0:1}" == 0 ]]; do value="${value:1}"; done
	[[ "$value" != 0 && "${#value}" -le 10 ]] || fail "$name must be between 1 and 4294967294"
	if [[ "${#value}" -eq 10 && "$value" > 4294967294 ]]; then
		fail "$name must be between 1 and 4294967294"
	fi
	printf '%s\n' "$value"
}

uid="$(id -u)"
gid="$(id -g)"
# Non-root Docker --user works without a matching /etc/passwd account. Native
# SMB's login identity is independent of this Unix process identity.
default_uid="$uid"
default_gid="$gid"
[[ "$default_uid" != 0 ]] || default_uid=1000
[[ "$default_gid" != 0 ]] || default_gid=1000
puid="$(runtime_id PUID "${PUID-$default_uid}")"
pgid="$(runtime_id PGID "${PGID-$default_gid}")"
if [[ "$uid" == 0 ]]; then
	# Explicit --user 0 retains the legacy bootstrap path. It needs SETUID and
	# SETGID, plus KILL for root tini to signal its different-UID child. The
	# recommended non-root path needs no capabilities at all.
	exec setpriv --reuid="$puid" --regid="$pgid" --clear-groups \
		--inh-caps=-all --ambient-caps=-all -- \
		/usr/local/bin/torrentfs "$@"
fi
[[ "$uid" == "$puid" && "$gid" == "$pgid" ]] ||
	fail 'PUID/PGID must match docker --user; use --user UID:GID (or explicit root bootstrap)'

printf 'torrentfs-entrypoint: starting torrentfs as %s:%s\n' "$uid" "$gid" >&2
exec /usr/local/bin/torrentfs "$@"
