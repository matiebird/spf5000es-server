#!/bin/sh
set -eu

service_name=spf5000es-server
service_user=spf5000es
service_group=spf5000es
config_dir=/etc/spf5000es-server
binary_path=/usr/local/bin/spf5000es-server
recovery_program=/usr/local/bin/spf5000es-recovery
unit_dir=/etc/systemd/system
polkit_rules_dir=/etc/polkit-1/rules.d
start_service=false
remote_host=
prebuilt_binary=

usage() {
	printf 'Usage: %s [--start|--no-start] [user@hostname]\n' "$0"
	printf '       sudo %s [--start|--no-start]              # install locally\n' "$0"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--start) start_service=true ;;
		--no-start) start_service=false ;;
		--prebuilt)
			[ "$#" -ge 2 ] || { usage >&2; exit 2; }
			prebuilt_binary=$2
			shift
			;;
		-h|--help) usage; exit 0 ;;
		-*) usage >&2; exit 2 ;;
		*)
			[ -z "$remote_host" ] || { usage >&2; exit 2; }
			remote_host=$1
			;;
	esac
	shift
done

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_dir=$script_dir
if [ -f "$script_dir/../go.mod" ]; then
	source_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
fi

require_commands() {
	for command in "$@"; do
		if ! command -v "$command" >/dev/null 2>&1; then
			printf 'error: required command not found: %s\n' "$command" >&2
			exit 1
		fi
	done
}

build_binary() {
	output=$1
	goarch=$2
	goarm=${3-}
	if [ -n "$goarm" ]; then
		(cd "$source_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOARM="$goarm" \
			go build -trimpath -ldflags='-s -w' -o "$output" .)
	else
		(cd "$source_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
			go build -trimpath -ldflags='-s -w' -o "$output" .)
	fi
}

install_remote() {
	require_commands go ssh scp mktemp

	case "$remote_host" in
		*[!A-Za-z0-9._@:-]*)
			printf 'error: invalid SSH hostname: %s\n' "$remote_host" >&2
			exit 2
			;;
	esac

	target=$(ssh "$remote_host" 'printf "%s %s\n" "$(uname -s)" "$(uname -m)"')
	case "$target" in
		"Linux x86_64"|"Linux amd64") target_goarch=amd64; target_goarm= ;;
		"Linux aarch64"|"Linux arm64") target_goarch=arm64; target_goarm= ;;
		"Linux armv7l"|"Linux armv7") target_goarch=arm; target_goarm=7 ;;
		"Linux armv6l"|"Linux armv6") target_goarch=arm; target_goarm=6 ;;
		"Linux i386"|"Linux i486"|"Linux i586"|"Linux i686") target_goarch=386; target_goarm= ;;
		*)
			printf 'error: unsupported remote operating system/architecture: %s\n' "$target" >&2
			exit 1
			;;
	esac

	build_dir=$(mktemp -d)
	trap 'rm -rf "$build_dir"' EXIT HUP INT TERM
	build_binary "$build_dir/$service_name" "$target_goarch" "$target_goarm"
	printf 'Built linux/%s for %s.\n' "$target_goarch" "$remote_host"

	remote_dir=$(ssh "$remote_host" 'mktemp -d /tmp/spf5000es-install.XXXXXX')
	case "$remote_dir" in
		/tmp/spf5000es-install.*) ;;
		*) printf 'error: unsafe remote temporary path: %s\n' "$remote_dir" >&2; exit 1 ;;
	esac

	scp "$build_dir/$service_name" \
		"$script_dir/install-systemd.sh" \
		"$source_dir/config.ini.example" \
		"$script_dir/spf5000es-server.service" \
		"$script_dir/spf5000es-recovery.service" \
		"$script_dir/spf5000es-recovery" \
		"$script_dir/50-spf5000es-recovery.rules" \
		"$remote_host:$remote_dir/"

	remote_start_flag=--no-start
	[ "$start_service" = false ] || remote_start_flag=--start
	install_status=0
	ssh -t "$remote_host" \
		"sudo '$remote_dir/install-systemd.sh' --prebuilt '$remote_dir/$service_name' $remote_start_flag" \
		|| install_status=$?
	ssh "$remote_host" "rm -rf '$remote_dir'" || true
	exit "$install_status"
}

if [ -n "$remote_host" ]; then
	[ -z "$prebuilt_binary" ] || { usage >&2; exit 2; }
	install_remote
fi

if [ "$(id -u)" -ne 0 ]; then
	printf 'error: local installation must run as root (use sudo), or specify an SSH hostname\n' >&2
	exit 1
fi

require_commands install systemctl pkaction useradd usermod groupadd getent mktemp

config_example=$source_dir/config.ini.example
if [ ! -e "$config_example" ] || \
	[ ! -e "$script_dir/spf5000es-server.service" ] || \
	[ ! -e "$script_dir/spf5000es-recovery.service" ] || \
	[ ! -e "$script_dir/spf5000es-recovery" ] || \
	[ ! -e "$script_dir/50-spf5000es-recovery.rules" ]; then
	printf 'error: installer assets are missing from %s\n' "$script_dir" >&2
	exit 1
fi

if [ -e "$recovery_program" ]; then
	if [ ! -x "$recovery_program" ]; then
		printf 'error: recovery program is not executable: %s\n' "$recovery_program" >&2
		exit 1
	fi
elif [ ! -x /usr/bin/usbreset ]; then
	printf 'error: /usr/bin/usbreset is required by the default recovery program\n' >&2
	exit 1
fi

if [ -z "$prebuilt_binary" ]; then
	require_commands go
	build_dir=$(mktemp -d)
	trap 'rm -rf "$build_dir"' EXIT HUP INT TERM
	prebuilt_binary="$build_dir/$service_name"
	build_binary "$prebuilt_binary" "$(go env GOARCH)"
elif [ ! -f "$prebuilt_binary" ]; then
	printf 'error: prebuilt binary not found: %s\n' "$prebuilt_binary" >&2
	exit 1
fi

if ! getent group dialout >/dev/null 2>&1; then
	printf 'error: the dialout group does not exist; create the serial-device group first\n' >&2
	exit 1
fi

if ! getent group "$service_group" >/dev/null 2>&1; then
	groupadd --system "$service_group"
fi

if ! id "$service_user" >/dev/null 2>&1; then
	useradd --system \
		--gid "$service_group" \
		--groups dialout \
		--no-create-home \
		--home-dir /nonexistent \
		--shell /usr/sbin/nologin \
		--comment 'SPF 5000 ES service' \
		"$service_user"
else
	usermod --append --groups dialout "$service_user"
fi

install -d -o root -g "$service_group" -m 0750 "$config_dir"
install -o root -g root -m 0755 "$prebuilt_binary" "$binary_path"

if [ ! -e "$recovery_program" ]; then
	install -o root -g root -m 0755 \
		"$script_dir/spf5000es-recovery" "$recovery_program"
	printf 'Installed default recovery program at %s.\n' "$recovery_program"
else
	printf 'Preserved existing recovery program at %s.\n' "$recovery_program"
fi

if [ ! -e "$config_dir/config.ini" ]; then
	install -o root -g "$service_group" -m 0640 \
		"$config_example" "$config_dir/config.ini"
	printf 'Created %s/config.ini; review it before production use.\n' "$config_dir"
else
	chown root:"$service_group" "$config_dir/config.ini"
	chmod 0640 "$config_dir/config.ini"
	printf 'Preserved existing %s/config.ini.\n' "$config_dir"
fi

install -o root -g root -m 0644 \
	"$script_dir/spf5000es-server.service" \
	"$script_dir/spf5000es-recovery.service" \
	"$unit_dir/"
install -d -o root -g root -m 0755 "$polkit_rules_dir"
install -o root -g root -m 0644 \
	"$script_dir/50-spf5000es-recovery.rules" \
	"$polkit_rules_dir/"

systemctl daemon-reload
if [ "$start_service" = true ]; then
	systemctl enable "$service_name.service"
	systemctl restart "$service_name.service"
	printf 'Installed and started %s.service.\n' "$service_name"
else
	printf 'Installed %s.service without starting it.\n' "$service_name"
fi
