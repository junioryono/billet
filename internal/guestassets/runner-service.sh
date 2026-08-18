#!/bin/sh

runner_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)

while :; do
	"$runner_root/bin/Runner.Listener" run "$@"
	status=$?
	case "$status" in
	100 | 101 | 102 | 103 | 104 | 105)
		exit "$status"
		;;
	0 | 1 | 5)
		exit 0
		;;
	2)
		sleep 5
		;;
	3 | 4)
		i=0
		while [ "$i" -le 30 ]; do
			if [ -f "$runner_root/update.finished" ]; then
				rm -f "$runner_root/update.finished"

				break
			fi
			i=$((i + 1))
			sleep 1
		done
		;;
	7)
		if [ "${ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE:-}" = 1 ]; then
			exit 7
		fi
		exit 0
		;;
	*)
		exit "$status"
		;;
	esac
done
