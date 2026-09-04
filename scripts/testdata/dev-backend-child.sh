#!/bin/sh

backend_mode=${DEV_TEST_BACKEND_MODE:-running}
descendant_mode=normal
if [ "$backend_mode" = "leader_exits_descendant_delayed" ]; then
	descendant_mode=delayed
elif [ "$backend_mode" = "ignore_term" ]; then
	descendant_mode=ignore
fi

/bin/sh "$(dirname "$0")/dev-descendant-child.sh" "$DEV_TEST_BACKEND_DESCENDANT_TERMINATED" "$descendant_mode" "$DEV_TEST_BACKEND_DESCENDANT_STARTED" &
descendant_pid=$!
while [ ! -f "$DEV_TEST_BACKEND_DESCENDANT_STARTED" ]; do
	sleep 0.1
done

if [ "$backend_mode" = "ignore_term" ]; then
	trap '' TERM INT
else
	trap 'printf "terminated\n" >"$DEV_TEST_BACKEND_TERMINATED"; wait "$descendant_pid" 2>/dev/null || :; exit 0' TERM INT
fi
printf 'started\n' >"$DEV_TEST_BACKEND_STARTED"

if [ "$backend_mode" = "fail" ]; then
	kill -TERM "$descendant_pid" 2>/dev/null || :
	wait "$descendant_pid" 2>/dev/null || :
	exit "${DEV_TEST_BACKEND_EXIT_STATUS:-21}"
fi
if [ "$backend_mode" = "leader_exits_descendant_delayed" ]; then
	exit "${DEV_TEST_BACKEND_EXIT_STATUS:-21}"
fi

while :; do
	sleep 1
done
