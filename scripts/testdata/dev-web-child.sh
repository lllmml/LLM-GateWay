#!/bin/sh

web_mode=${DEV_TEST_WEB_MODE:-running}
descendant_mode=normal
if [ "$web_mode" = "leader_exits_descendant_delayed" ]; then
	descendant_mode=delayed
elif [ "$web_mode" = "ignore_term" ]; then
	descendant_mode=ignore
fi

/bin/sh "$(dirname "$0")/dev-descendant-child.sh" "$DEV_TEST_WEB_DESCENDANT_TERMINATED" "$descendant_mode" "$DEV_TEST_WEB_DESCENDANT_STARTED" &
descendant_pid=$!
while [ ! -f "$DEV_TEST_WEB_DESCENDANT_STARTED" ]; do
	sleep 0.1
done

if [ "$web_mode" = "ignore_term" ]; then
	trap '' TERM INT
else
	trap 'printf "terminated\n" >"$DEV_TEST_WEB_TERMINATED"; wait "$descendant_pid" 2>/dev/null || :; exit 0' TERM INT
fi
printf 'started\n' >"$DEV_TEST_WEB_STARTED"

if [ "$web_mode" = "fail" ]; then
	kill -TERM "$descendant_pid" 2>/dev/null || :
	wait "$descendant_pid" 2>/dev/null || :
	exit "${DEV_TEST_WEB_EXIT_STATUS:-22}"
fi
if [ "$web_mode" = "leader_exits_descendant_delayed" ]; then
	exit "${DEV_TEST_WEB_EXIT_STATUS:-22}"
fi

while :; do
	sleep 1
done
