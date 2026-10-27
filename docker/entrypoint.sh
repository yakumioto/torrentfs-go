#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'
# Keep metadata readable across host/container UIDs; private secrets explicitly
# choose their own restrictive mode in torrentfs.
umask 022

readonly IDENTITY_FILE=/etc/torrentfs/runtime-identity
runtime_uid=''
runtime_gid=''
runtime_user=torrentfs
runtime_group=torrentfs

log() {
	printf 'torrentfs-entrypoint: %s\n' "$*" >&2
}

fail() {
	log "ERROR: $*"
	exit 1
}

validate_runtime_id() {
	local variable_name="$1" value="$2" normalized
	if [[ -z "$value" ]]; then
		fail "$variable_name must be an unsigned decimal integer from 1 to 4294967294 (got empty value)"
	fi
	if [[ ! "$value" =~ ^[0-9]+$ ]]; then
		fail "$variable_name must be an unsigned decimal integer from 1 to 4294967294 (got invalid value)"
	fi

	normalized="$value"
	while [[ "${#normalized}" -gt 1 && "${normalized:0:1}" == 0 ]]; do
		normalized="${normalized:1}"
	done
	if [[ "$normalized" == 0 ]]; then
		fail "$variable_name must be between 1 and 4294967294 (got zero)"
	fi
	if [[ "${#normalized}" -gt 10 || ( "${#normalized}" -eq 10 && "$normalized" > 4294967294 ) ]]; then
		fail "$variable_name must be between 1 and 4294967294 (got out-of-range value)"
	fi
	printf '%s\n' "$normalized"
}

initialize_runtime_identity() {
	local identity_tmp
	runtime_uid="$(validate_runtime_id PUID "${PUID-1000}")"
	runtime_gid="$(validate_runtime_id PGID "${PGID-1000}")"

	if getent group "$runtime_group" >/dev/null 2>&1; then
		groupmod --gid "$runtime_gid" --non-unique "$runtime_group" ||
			fail "could not set group $runtime_group to GID $runtime_gid"
	else
		groupadd --gid "$runtime_gid" --non-unique "$runtime_group" ||
			fail "could not create group $runtime_group with GID $runtime_gid"
	fi
	if getent passwd "$runtime_user" >/dev/null 2>&1; then
		usermod --uid "$runtime_uid" --gid "$runtime_group" --non-unique "$runtime_user" ||
			fail "could not set user $runtime_user to UID $runtime_uid:$runtime_gid"
	else
		useradd --uid "$runtime_uid" --gid "$runtime_group" --non-unique \
			--no-create-home --shell /usr/sbin/nologin "$runtime_user" ||
			fail "could not create user $runtime_user with UID $runtime_uid:$runtime_gid"
	fi

	[[ "$(id -u "$runtime_user")" == "$runtime_uid" ]] ||
		fail "user $runtime_user does not resolve to UID $runtime_uid"
	[[ "$(id -g "$runtime_user")" == "$runtime_gid" ]] ||
		fail "group $runtime_group does not resolve to GID $runtime_gid"

	identity_tmp="$(mktemp "${IDENTITY_FILE}.tmp.XXXXXX")" ||
		fail 'could not create temporary runtime identity file'
	printf 'uid=%s\ngid=%s\nuser=%s\ngroup=%s\n' \
		"$runtime_uid" "$runtime_gid" "$runtime_user" "$runtime_group" >"$identity_tmp"
	chown root:root "$identity_tmp"
	chmod 0444 "$identity_tmp"
	mv -f -- "$identity_tmp" "$IDENTITY_FILE"
}

validate_torrents_dir() {
	[[ -d /torrents ]] || fail 'missing /torrents directory'
	if ! setpriv --reuid="$runtime_uid" --regid="$runtime_gid" --clear-groups -- \
		/bin/sh -c 'test -r "$1" && test -w "$1" && test -x "$1"' sh /torrents; then
		fail "runtime identity $runtime_uid:$runtime_gid cannot read and write /torrents"
	fi
}

[[ "$EUID" == 0 ]] ||
	fail 'container entrypoint must start as root; do not use docker run --user; configure PUID and PGID instead'
initialize_runtime_identity
# Convenience directories for explicit FUSE flags only. SMB does not mount them
# or need /dev/fuse, SYS_ADMIN, NET_BIND_SERVICE, or a relaxed AppArmor profile.
for mountpoint in /share /share-strm; do
	chown "$runtime_uid:$runtime_gid" "$mountpoint"
	chmod 0755 "$mountpoint"
done
validate_torrents_dir

# The daemon owns mode/config/credential validation, listeners and shutdown.
# exec keeps a single Go service under tini and passes FUSE flags unchanged.
log "starting torrentfs as $runtime_uid:$runtime_gid"
exec setpriv --reuid="$runtime_uid" --regid="$runtime_gid" --clear-groups -- \
	/usr/local/bin/torrentfs "$@"
