#!/bin/sh

termination_marker=$1
trap 'printf "terminated\n" >"$termination_marker"; exit 0' TERM INT

while :; do
	sleep 1
done
