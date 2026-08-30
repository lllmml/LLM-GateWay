#!/bin/sh

set -eu

repository_directory=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
supervisor=$repository_directory/scripts/dev-supervisor.sh
backend_child=$repository_directory/scripts/testdata/dev-backend-child.sh
web_child=$repository_directory/scripts/testdata/dev-web-child.sh
test_directory=$(mktemp -d /tmp/gateway-dev-supervisor-test.XXXXXX)

cleanup() {
	if [ -n "${supervisor_pid:-}" ] && kill -0 "$supervisor_pid" 2>/dev/null; then
		kill -TERM "$supervisor_pid" 2>/dev/null || :
		wait "$supervisor_pid" 2>/dev/null || :
	fi
	rm -f "$test_directory"/*
	rmdir "$test_directory" 2>/dev/null || :
}
trap cleanup EXIT INT TERM

export DEV_TEST_BACKEND_STARTED=$test_directory/backend.started
export DEV_TEST_BACKEND_TERMINATED=$test_directory/backend.terminated
export DEV_TEST_BACKEND_DESCENDANT_TERMINATED=$test_directory/backend-descendant.terminated
export DEV_TEST_WEB_STARTED=$test_directory/web.started
export DEV_TEST_WEB_TERMINATED=$test_directory/web.terminated
export DEV_TEST_WEB_DESCENDANT_TERMINATED=$test_directory/web-descendant.terminated

reset_state() {
	rm -f "$DEV_TEST_BACKEND_STARTED" "$DEV_TEST_BACKEND_TERMINATED" "$DEV_TEST_BACKEND_DESCENDANT_TERMINATED" \
		"$DEV_TEST_WEB_STARTED" "$DEV_TEST_WEB_TERMINATED" "$DEV_TEST_WEB_DESCENDANT_TERMINATED"
	DEV_TEST_BACKEND_MODE=running
	DEV_TEST_WEB_MODE=running
	DEV_TEST_BACKEND_EXIT_STATUS=21
	DEV_TEST_WEB_EXIT_STATUS=22
	export DEV_TEST_BACKEND_MODE DEV_TEST_WEB_MODE DEV_TEST_BACKEND_EXIT_STATUS DEV_TEST_WEB_EXIT_STATUS
}

assert_file() {
	if [ ! -f "$1" ]; then
		echo "expected lifecycle marker $1" >&2
		exit 1
	fi
}

reset_state
DEV_TEST_WEB_MODE=fail
export DEV_TEST_WEB_MODE
set +e
/bin/sh "$supervisor" "$backend_child" "$web_child" . >/dev/null 2>&1
status=$?
set -e
if [ "$status" -ne "$DEV_TEST_WEB_EXIT_STATUS" ]; then
	echo "frontend failure status $status; want $DEV_TEST_WEB_EXIT_STATUS" >&2
	exit 1
fi
assert_file "$DEV_TEST_BACKEND_TERMINATED"
assert_file "$DEV_TEST_BACKEND_DESCENDANT_TERMINATED"

reset_state
DEV_TEST_BACKEND_MODE=fail
export DEV_TEST_BACKEND_MODE
set +e
/bin/sh "$supervisor" "$backend_child" "$web_child" . >/dev/null 2>&1
status=$?
set -e
if [ "$status" -ne "$DEV_TEST_BACKEND_EXIT_STATUS" ]; then
	echo "backend failure status $status; want $DEV_TEST_BACKEND_EXIT_STATUS" >&2
	exit 1
fi
assert_file "$DEV_TEST_WEB_TERMINATED"
assert_file "$DEV_TEST_WEB_DESCENDANT_TERMINATED"

reset_state
/bin/sh "$supervisor" "$backend_child" "$web_child" . >/dev/null 2>&1 &
supervisor_pid=$!
attempt=0
while { [ ! -f "$DEV_TEST_BACKEND_STARTED" ] || [ ! -f "$DEV_TEST_WEB_STARTED" ]; } && [ "$attempt" -lt 5 ]; do
	attempt=$((attempt + 1))
	sleep 1
done
assert_file "$DEV_TEST_BACKEND_STARTED"
assert_file "$DEV_TEST_WEB_STARTED"
kill -TERM "$supervisor_pid"
set +e
wait "$supervisor_pid"
status=$?
set -e
supervisor_pid=
if [ "$status" -ne 143 ]; then
	echo "SIGTERM supervisor status $status; want 143" >&2
	exit 1
fi
assert_file "$DEV_TEST_BACKEND_TERMINATED"
assert_file "$DEV_TEST_BACKEND_DESCENDANT_TERMINATED"
assert_file "$DEV_TEST_WEB_TERMINATED"
assert_file "$DEV_TEST_WEB_DESCENDANT_TERMINATED"

echo "dev supervisor lifecycle tests passed"
