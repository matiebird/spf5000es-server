#!/bin/sh
set -eu

service_name=spf5000es-server
config_dir=/etc/spf5000es-server
binary_path=/usr/local/bin/spf5000es-server
recovery_program=/usr/local/bin/spf5000es-recovery
unit_dir=/etc/systemd/system
polkit_rule=/etc/polkit-1/rules.d/50-spf5000es-recovery.rules
remote_host=

usage() {
	printf 'Usage: %s [user@hostname]\n' "$0"
	printf '       sudo %s                 # uninstall locally\n' "$0"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		-h|--help) usage; exit 0 ;;
		-*) usage >&2; exit 2 ;;
		*)
			[ -z "$remote_host" ] || { usage >&2; exit 2; }
			remote_host=$1
			;;
	esac
	shift
done

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

require_commands() {
	for command in "$@"; do
		if ! command -v "$command" >/dev/null 2>&1; then
			printf 'error: required command not found: %s\n' "$command" >&2
			exit 1
		fi
	done
}

uninstall_remote() {
	require_commands ssh scp

	case "$remote_host" in
		*[!A-Za-z0-9._@:-]*)
			printf 'error: invalid SSH hostname: %s\n' "$remote_host" >&2
			exit 2
			;;
	esac

	remote_dir=$(ssh "$remote_host" 'mktemp -d /tmp/spf5000es-uninstall.XXXXXX')
	case "$remote_dir" in
		/tmp/spf5000es-uninstall.*) ;;
		*) printf 'error: unsafe remote temporary path: %s\n' "$remote_dir" >&2; exit 1 ;;
	esac

	scp "$script_dir/uninstall-systemd.sh" "$remote_host:$remote_dir/"
	uninstall_status=0
	ssh -t "$remote_host" \
		"sudo '$remote_dir/uninstall-systemd.sh'" \
		|| uninstall_status=$?
	# remote_dir is created remotely and restricted to /tmp/spf5000es-uninstall.* above.
	# shellcheck disable=SC2029
	ssh "$remote_host" "rm -rf '$remote_dir'" || true
	exit "$uninstall_status"
}

if [ -n "$remote_host" ]; then
	uninstall_remote
fi

if [ "$(id -u)" -ne 0 ]; then
	printf 'error: local uninstallation must run as root (use sudo), or specify an SSH hostname\n' >&2
	exit 1
fi

require_commands systemctl

# These operations are intentionally tolerant of a partial or repeated uninstall.
systemctl disable --now "$service_name.service" >/dev/null 2>&1 || true
systemctl stop "$service_name-recovery.service" >/dev/null 2>&1 || true

rm -f \
	"$unit_dir/$service_name.service" \
	"$unit_dir/$service_name-recovery.service" \
	"$polkit_rule" \
	"$binary_path"

systemctl daemon-reload
systemctl reset-failed "$service_name.service" "$service_name-recovery.service" \
	>/dev/null 2>&1 || true

printf 'Uninstalled %s systemd services and binary.\n' "$service_name"
printf 'Configuration files were kept intact and were not removed: %s\n' "$config_dir"
printf 'The potentially customized recovery program was also kept: %s\n' "$recovery_program"
