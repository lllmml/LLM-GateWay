#!/bin/sh

set -u

if [ "$#" -ne 3 ]; then
	echo "usage: $0 GO_COMMAND NPM_COMMAND WEB_DIRECTORY" >&2
	exit 2
fi

if ! command -v setsid >/dev/null 2>&1; then
	echo "make dev requires setsid to supervise complete child process groups" >&2
	exit 1
fi

go_command=$1
npm_command=$2
web_directory=$3
status_directory=/tmp/gateway-dev-supervisor.$$

if ! mkdir "$status_directory"; then
	echo "could not create dev supervisor state directory" >&2
	exit 1
fi

backend_status_file=$status_directory/backend.status
web_status_file=$status_directory/web.status
backend_pid=
web_pid=

remove_status_directory() {
	rm -f "$backend_status_file" "$web_status_file"
	rmdir "$status_directory" 2>/dev/null || :
}

run_managed() {
	managed_status_file=$1
	shift
	managed_pid=

	terminate_managed_group() {
		if [ -n "$managed_pid" ] && kill -0 -"$managed_pid" 2>/dev/null; then
			kill -TERM -"$managed_pid" 2>/dev/null || :
		fi
		if [ -n "$managed_pid" ]; then
			wait "$managed_pid" 2>/dev/null || :
		fi
		if [ -n "$managed_pid" ] && kill -0 -"$managed_pid" 2>/dev/null; then
			kill -KILL -"$managed_pid" 2>/dev/null || :
		fi
	}

	stop_managed() {
		managed_exit_status=$1
		trap - HUP INT TERM
		terminate_managed_group
		printf '%s\n' "$managed_exit_status" >"$managed_status_file"
		exit "$managed_exit_status"
	}

	trap 'stop_managed 129' HUP
	trap 'stop_managed 130' INT
	trap 'stop_managed 143' TERM

	setsid "$@" &
	managed_pid=$!
	wait "$managed_pid"
	managed_exit_status=$?
	trap - HUP INT TERM
	if kill -0 -"$managed_pid" 2>/dev/null; then
		kill -TERM -"$managed_pid" 2>/dev/null || :
		sleep 1
		kill -KILL -"$managed_pid" 2>/dev/null || :
	fi
	printf '%s\n' "$managed_exit_status" >"$managed_status_file"
	return "$managed_exit_status"
}

stop_wrapper() {
	child_pid=$1
	if [ -n "$child_pid" ] && kill -0 "$child_pid" 2>/dev/null; then
		kill -TERM "$child_pid" 2>/dev/null || :
	fi
}

wait_wrapper() {
	child_pid=$1
	if [ -n "$child_pid" ]; then
		wait "$child_pid" 2>/dev/null || :
	fi
}

stop_all() {
	stop_wrapper "$backend_pid"
	stop_wrapper "$web_pid"
	wait_wrapper "$backend_pid"
	wait_wrapper "$web_pid"
}

handle_interrupt() {
	trap - HUP INT TERM
	stop_all
	remove_status_directory
	exit 130
}

handle_terminate() {
	trap - HUP INT TERM
	stop_all
	remove_status_directory
	exit 143
}

handle_hangup() {
	trap - HUP INT TERM
	stop_all
	remove_status_directory
	exit 129
}

trap handle_hangup HUP
trap handle_interrupt INT
trap handle_terminate TERM

run_managed "$backend_status_file" "$go_command" run ./cmd/gateway &
backend_pid=$!
run_managed "$web_status_file" "$npm_command" --prefix "$web_directory" run dev &
web_pid=$!

while [ ! -f "$backend_status_file" ] && [ ! -f "$web_status_file" ]; do
	sleep 1
done

backend_finished=false
web_finished=false
backend_status=0
web_status=0

if [ -f "$backend_status_file" ]; then
	backend_finished=true
	IFS= read -r backend_status <"$backend_status_file" || backend_status=1
	wait_wrapper "$backend_pid"
fi
if [ -f "$web_status_file" ]; then
	web_finished=true
	IFS= read -r web_status <"$web_status_file" || web_status=1
	wait_wrapper "$web_pid"
fi

if [ "$backend_finished" = false ]; then
	stop_wrapper "$backend_pid"
	wait_wrapper "$backend_pid"
fi
if [ "$web_finished" = false ]; then
	stop_wrapper "$web_pid"
	wait_wrapper "$web_pid"
fi

remove_status_directory
trap - HUP INT TERM

if [ "$backend_finished" = true ] && [ "$backend_status" -ne 0 ]; then
	exit "$backend_status"
fi
if [ "$web_finished" = true ] && [ "$web_status" -ne 0 ]; then
	exit "$web_status"
fi
if [ "$backend_finished" = true ]; then
	exit "$backend_status"
fi
exit "$web_status"
