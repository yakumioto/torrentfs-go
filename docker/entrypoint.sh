#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'
# Keep the image default so files torrentfs creates (`.metadata`, lock files)
# stay readable to the host user that owns the bind-mounted torrents directory,
# which may have a different UID than the container runtime identity.
umask 022

readonly MOUNTPOINT=/mnt/torrentfs
readonly IDENTITY_FILE=/etc/torrentfs/runtime-identity
readonly SMB_TEMPLATE=/etc/samba/torrentfs-smb.conf
readonly SMB_CONFIG=/run/samba/smb.conf
readonly TORRENTFS_PID_FILE=/run/torrentfs.pid
readonly START_TIMEOUT_SECONDS=60
readonly SAMBA_STOP_TIMEOUT_SECONDS=10
readonly TORRENTFS_STOP_TIMEOUT_SECONDS=45

stop_requested=0
torrentfs_pid=''
smbd_pid=''
runtime_uid=''
runtime_gid=''
runtime_user=''
runtime_group=''
smb_username=''

log() {
	printf 'torrentfs-entrypoint: %s\n' "$*" >&2
}

fail() {
	log "ERROR: $*"
	exit 1
}

cleanup() {
	rm -f -- "$TORRENTFS_PID_FILE" /run/samba/smbd.pid
}
trap cleanup EXIT

smb_enabled() {
	case "${TORRENTFS_SMB_ENABLED:-false}" in
	false|0)
		return 1
		;;
	true|1)
		return 0
		;;
	*)
		fail 'TORRENTFS_SMB_ENABLED must be exactly true, false, 1, or 0'
		;;
	esac
}

valid_account_name() {
	[[ "$1" =~ ^[A-Za-z_][A-Za-z0-9_.-]*\$?$ ]]
}

read_identity() {
	[[ -r "$IDENTITY_FILE" ]] || fail "missing runtime identity file $IDENTITY_FILE"
	runtime_uid="$(awk -F= '$1 == "uid" { print $2; exit }' "$IDENTITY_FILE")"
	runtime_gid="$(awk -F= '$1 == "gid" { print $2; exit }' "$IDENTITY_FILE")"
	runtime_user="$(awk -F= '$1 == "user" { print $2; exit }' "$IDENTITY_FILE")"
	runtime_group="$(awk -F= '$1 == "group" { print $2; exit }' "$IDENTITY_FILE")"
	[[ "$runtime_uid" =~ ^[1-9][0-9]*$ ]] || fail 'runtime UID must be a non-root numeric UID'
	[[ "$runtime_gid" =~ ^[1-9][0-9]*$ ]] || fail 'runtime GID must be a non-root numeric GID'
	valid_account_name "$runtime_user" || fail 'runtime username is invalid'
	valid_account_name "$runtime_group" || fail 'runtime group name is invalid'
	[[ "$(id -u "$runtime_user" 2>/dev/null || true)" == "$runtime_uid" ]] ||
		fail 'runtime username and UID do not match'
	[[ "$(getent group "$runtime_gid" | awk -F: 'NR == 1 { print $1 }')" == "$runtime_group" ]] ||
		fail 'runtime group name and GID do not match'
	(( runtime_uid != 0 )) || fail 'SMB mode cannot use UID 0 for torrentfs'
}

validate_mountpoint() {
	[[ -c /dev/fuse ]] || fail 'SMB mode requires /dev/fuse'
	[[ -d "$MOUNTPOINT" ]] || fail "missing mountpoint directory $MOUNTPOINT"
	if findmnt -T "$MOUNTPOINT" -n -o TARGET 2>/dev/null | awk -v target="$MOUNTPOINT" '$1 == target { found = 1 } END { exit found ? 0 : 1 }'; then
		fail "$MOUNTPOINT is already mounted"
	fi
	if find "$MOUNTPOINT" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null | grep -q .; then
		fail "$MOUNTPOINT must be empty before FUSE startup"
	fi
	if ! setpriv --reuid="$runtime_uid" --regid="$runtime_gid" --clear-groups -- \
		/bin/sh -c 'test -r "$1" && test -x "$1"' sh "$MOUNTPOINT"; then
		fail "runtime identity $runtime_uid:$runtime_gid cannot access $MOUNTPOINT"
	fi
}

validate_torrents_dir() {
	[[ -d /torrents ]] || fail 'missing /torrents directory'
	if ! setpriv --reuid="$runtime_uid" --regid="$runtime_gid" --clear-groups -- \
		/bin/sh -c 'test -r "$1" && test -w "$1" && test -x "$1"' sh /torrents; then
		fail "runtime identity $runtime_uid:$runtime_gid cannot read and write /torrents"
	fi
}

validate_user_args() {
	local arg
	for arg in "$@"; do
		case "$arg" in
		-mountpoint|--mountpoint|-mountpoint=*|--mountpoint=*)
			fail 'SMB mode owns the fixed /mnt/torrentfs mountpoint; do not pass -mountpoint'
			;;
		esac
	done
}

validate_secret() {
	local mode size lines password_file
	password_file="${TORRENTFS_SMB_PASSWORD_FILE:-}"
	[[ -n "$password_file" ]] || fail 'TORRENTFS_SMB_PASSWORD_FILE is required when SMB is enabled'
	[[ -f "$password_file" && ! -L "$password_file" ]] ||
		fail 'SMB password file must be a regular non-symlink file'
	[[ -r "$password_file" ]] || fail 'SMB password file is not readable'
	mode="$(stat -c '%a' "$password_file")"
	(( (8#$mode & 077) == 0 )) || fail 'SMB password file must not be readable by group or other users'
	size="$(stat -c '%s' "$password_file")"
	(( size > 0 && size <= 1024 )) || fail 'SMB password file must be between 1 and 1024 bytes'
	if od -An -tx1 "$password_file" | grep -q '00'; then
		fail 'SMB password file must not contain NUL bytes'
	fi
	lines="$(wc -l < "$password_file")"
	(( lines <= 1 )) || fail 'SMB password file must contain one line only'
	password="$(<"$password_file")"
	case "$password" in
	*$'\n'*|*$'\r'*)
		fail 'SMB password file must contain one line only'
		;;
	esac
	[[ -n "$password" ]] || fail 'SMB password must not be empty'
}

validate_samba_capability() {
	if ! setpriv --reuid="$runtime_uid" --regid="$runtime_gid" --clear-groups \
		--inh-caps=+net_bind_service --ambient-caps=+net_bind_service -- true; then
		fail 'SMB mode requires CAP_NET_BIND_SERVICE so the non-root Samba process can bind TCP 445'
	fi
}

prepare_samba() {
	local template config password password_file smbpasswd_error
	mkdir -p /run/samba /run/samba/private /run/samba/lock /run/samba/state /run/samba/cache \
		/var/lib/samba /var/cache/samba /var/log/samba
	chown -R "$runtime_uid:$runtime_gid" /run/samba /var/lib/samba /var/cache/samba /var/log/samba
	chmod 0755 /run/samba
	chmod 0700 /run/samba/private

	smb_username="${TORRENTFS_SMB_USERNAME:-$runtime_user}"
	valid_account_name "$smb_username" || fail 'TORRENTFS_SMB_USERNAME is not a valid Unix account name'
	[[ "$(id -u "$smb_username" 2>/dev/null || true)" == "$runtime_uid" ]] ||
		fail 'TORRENTFS_SMB_USERNAME must resolve to the torrentfs runtime UID'

	validate_secret
	password_file="${TORRENTFS_SMB_PASSWORD_FILE:-}"
	password="$(<"$password_file")"
	template="$(<"$SMB_TEMPLATE")"
	config="${template//__TORRENTFS_SMB_USER__/$smb_username}"
	config="${config//__TORRENTFS_SMB_GROUP__/$runtime_group}"
	# The restrictive umask applies to this write only. A process-wide umask
	# would be inherited by torrentfs, whose `.metadata` would then be created
	# unreadable to a host user with a different UID.
	(
		umask 077
		printf '%s\n' "$config" >"$SMB_CONFIG"
	)
	chmod 0644 "$SMB_CONFIG"
	if ! testparm -s "$SMB_CONFIG" >/dev/null; then
		unset password
		fail 'Samba configuration validation failed'
	fi

	if ! smbpasswd_error="$(printf '%s\n%s\n' "$password" "$password" |
		smbpasswd -L -c "$SMB_CONFIG" -s -a "$smb_username" 2>&1)"; then
		unset password
		log "smbpasswd: $smbpasswd_error"
		fail 'Samba password database initialization failed'
	fi
	# The root-owned passdb setup above also creates the account policy and
	# group mapping databases, so the whole runtime directory is handed to the
	# Samba user after it finishes; the non-root smbd owns every file it opens.
	unset password
	chown -R "$runtime_uid:$runtime_gid" /run/samba
	export HOME=/run/samba
}

mount_ready() {
	findmnt -T "$MOUNTPOINT" -n -o TARGET,FSTYPE 2>/dev/null |
		awk -v target="$MOUNTPOINT" '$1 == target && $2 ~ /^fuse\./ { found = 1 } END { exit found ? 0 : 1 }'
}

child_running() {
	local pid="$1" state
	[[ -d "/proc/$pid" ]] || return 1
	state="$(awk '{ print $3 }' "/proc/$pid/stat" 2>/dev/null || true)"
	[[ "$state" != Z && "$state" != X ]]
}

wait_tick() {
	local delay="$1" timer_pid
	sleep "$delay" &
	timer_pid=$!
	wait "$timer_pid" 2>/dev/null || true
}

wait_for_child() {
	local pid="$1" timeout_seconds="$2" deadline rc
	deadline=$((SECONDS + timeout_seconds))
	while child_running "$pid"; do
		if (( SECONDS >= deadline )); then
			return 124
		fi
		wait_tick 0.1
	done
	wait "$pid" 2>/dev/null || true
	return 0
}

stop_child() {
	local pid="$1" name="$2" timeout_seconds="$3" rc
	if ! child_running "$pid"; then
		wait "$pid" 2>/dev/null || true
		return 0
	fi
	kill -TERM "$pid" 2>/dev/null || true
	if wait_for_child "$pid" "$timeout_seconds"; then
		return 0
	else
		rc=$?
	fi
	if (( rc == 124 )); then
		log "$name did not stop within ${timeout_seconds}s; sending SIGKILL"
		kill -KILL "$pid" 2>/dev/null || true
		if wait_for_child "$pid" 5; then
			return 1
		fi
		return 1
	fi
	return "$rc"
}

stop_smbd() {
	local rc
	[[ -n "$smbd_pid" ]] || return 0
	if stop_child "$smbd_pid" smbd "$SAMBA_STOP_TIMEOUT_SECONDS"; then
		log 'smbd stopped'
		smbd_pid=''
		return 0
	else
		rc=$?
	fi
	log "smbd stop failed (status=$rc)"
	smbd_pid=''
	return 1
}

stop_torrentfs() {
	local rc
	[[ -n "$torrentfs_pid" ]] || return 0
	log 'torrentfs stopping'
	if stop_child "$torrentfs_pid" torrentfs "$TORRENTFS_STOP_TIMEOUT_SECONDS"; then
		log 'torrentfs unmounted'
		torrentfs_pid=''
		return 0
	else
		rc=$?
	fi
	log "torrentfs stop failed (status=$rc)"
	torrentfs_pid=''
	return 1
}

normal_shutdown() {
	local failed=0
	trap - INT TERM
	if ! stop_smbd; then
		failed=1
	fi
	if ! stop_torrentfs; then
		failed=1
	fi
	return "$failed"
}

unexpected_smbd_exit() {
	local rc
	if wait "$smbd_pid"; then
		rc=0
	else
		rc=$?
	fi
	smbd_pid=''
	log "smbd exited unexpectedly (status=$rc)"
	stop_torrentfs || true
	exit 1
}

unexpected_torrentfs_exit() {
	local rc
	if wait "$torrentfs_pid"; then
		rc=0
	else
		rc=$?
	fi
	torrentfs_pid=''
	log "torrentfs exited unexpectedly before supervisor shutdown (status=$rc)"
	stop_smbd || true
	exit 1
}

if ! smb_enabled; then
	exec /usr/local/bin/torrentfs "$@"
fi

[[ "$EUID" == 0 ]] || fail 'SMB mode requires the container entrypoint to start as root'
read_identity
validate_user_args "$@"
validate_torrents_dir
validate_mountpoint
validate_samba_capability
prepare_samba

on_shutdown_signal() {
	stop_requested=1
	log 'shutdown signal received'
}
trap on_shutdown_signal INT TERM
log "starting torrentfs as $runtime_uid:$runtime_gid"
setsid --wait -- setpriv --reuid="$runtime_uid" --regid="$runtime_gid" --clear-groups -- \
	/usr/local/bin/torrentfs -mountpoint "$MOUNTPOINT" "$@" &
torrentfs_pid=$!
printf '%s\n' "$torrentfs_pid" >"$TORRENTFS_PID_FILE"

ready_deadline=$((SECONDS + START_TIMEOUT_SECONDS))
while :; do
	if (( stop_requested )); then
		if normal_shutdown; then
			exit 0
		fi
		exit 1
	fi
	if ! child_running "$torrentfs_pid"; then
		unexpected_torrentfs_exit
	fi
	if mount_ready; then
		log "fuse ready: $MOUNTPOINT"
		break
	fi
	if (( SECONDS >= ready_deadline )); then
		log "FUSE mount did not become ready within ${START_TIMEOUT_SECONDS}s"
		stop_torrentfs || true
		exit 1
	fi
	wait_tick 0.2
done

log 'starting smbd on TCP 445'
setsid --wait -- setpriv --reuid="$runtime_uid" --regid="$runtime_gid" --clear-groups \
	--inh-caps=+net_bind_service --ambient-caps=+net_bind_service -- \
	/usr/sbin/smbd --foreground --no-process-group --debug-stdout --configfile="$SMB_CONFIG" &
smbd_pid=$!

while :; do
	if (( stop_requested )); then
		if normal_shutdown; then
			exit 0
		fi
		exit 1
	fi
	if ! child_running "$torrentfs_pid"; then
		unexpected_torrentfs_exit
	fi
	if ! child_running "$smbd_pid"; then
		unexpected_smbd_exit
	fi
	if ! mount_ready; then
		log 'FUSE mount disappeared while services were running'
		stop_smbd || true
		stop_torrentfs || true
		exit 1
	fi
	wait_tick 1
done
