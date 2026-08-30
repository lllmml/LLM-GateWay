#!/bin/sh

/bin/sh "$(dirname "$0")/dev-descendant-child.sh" "$DEV_TEST_WEB_DESCENDANT_TERMINATED" &
descendant_pid=$!

trap 'printf "terminated\n" >"$DEV_TEST_WEB_TERMINATED"; wait "$descendant_pid" 2>/dev/null || :; exit 0' TERM INT
printf 'started\n' >"$DEV_TEST_WEB_STARTED"

if [ "${DEV_TEST_WEB_MODE:-running}" = "fail" ]; then
	kill -TERM "$descendant_pid" 2>/dev/null || :
	wait "$descendant_pid" 2>/dev/null || :
	exit "${DEV_TEST_WEB_EXIT_STATUS:-22}"
fi

while :; do
	sleep 1
done
