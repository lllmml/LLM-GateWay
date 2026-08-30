#!/bin/sh

termination_marker=$1
termination_mode=${2:-normal}
started_marker=$3

case "$termination_mode" in
	normal)
		trap 'printf "terminated\n" >"$termination_marker"; exit 0' TERM INT
		;;
	delayed)
		trap 'sleep 1; printf "terminated\n" >"$termination_marker"; exit 0' TERM INT
		;;
	ignore)
		trap '' TERM INT
		;;
	*)
		echo "unknown descendant termination mode: $termination_mode" >&2
		exit 2
		;;
esac

printf 'started\n' >"$started_marker"

while :; do
	sleep 1
done
