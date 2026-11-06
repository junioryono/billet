#!/usr/bin/env bash
# Usage:
#   ssm-run.sh run  <instance-id> <timeout-seconds> <shell command>
#   ssm-run.sh send <instance-id> <timeout-seconds> <shell command>
#   ssm-run.sh wait <instance-id> <command-id> <timeout-seconds>
#
# Runs one shell command on an instance through SSM Run Command (AWS-RunShellScript).
# `send` prints the command's id and returns at once, so a long command can be awaited
# in a later step; `wait` waits for it, printing what it wrote; `run` is the two. `wait`
# and `run` exit with the command's own status, or 124 when it did not finish within
# <timeout-seconds> (SSM is told the same bound, so it does not run on), or 125 when SSM
# could not say how it ended. `aws ssm wait command-executed` gives up after about 100 s,
# which a matrix run outlasts many times over, so this polls instead.
#
# The command travels in the request and SSM keeps it in the command's history, so it
# carries no secret: a credential reaches the host through Parameter Store, read there.
set -uo pipefail

usage() {
	sed -n '2,5p' "$0" >&2
	exit 2
}

poll=${SSM_POLL:-10}
# grace is how long past its own bound SSM may take to say a command ended.
grace=${SSM_GRACE:-120}

send() {
	local instance=$1 timeout=$2 command=$3 params id
	params=$(jq -cn --arg c "$command" --arg t "$timeout" '{commands: [$c], executionTimeout: [$t]}') || return 125
	id=$(aws ssm send-command --instance-ids "$instance" --document-name AWS-RunShellScript \
		--timeout-seconds 60 --parameters "$params" --query Command.CommandId --output text)
	if [ -z "$id" ] || [ "$id" = None ]; then
		echo "ssm-run: SSM accepted no command" >&2
		return 125
	fi
	echo "$id"
}

await() {
	local instance=$1 id=$2 timeout=$3 status deadline code
	deadline=$((SECONDS + timeout + grace))
	while :; do
		status=$(aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" \
			--query Status --output text 2>/dev/null)
		case "$status" in
		Success | Failed | TimedOut | Cancelled)
			break
			;;
		esac
		if [ "$SECONDS" -ge "$deadline" ]; then
			echo "ssm-run: command $id is still ${status:-unknown} past its bound" >&2
			return 124
		fi
		sleep "$poll"
	done

	aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" \
		--query StandardOutputContent --output text
	aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" \
		--query StandardErrorContent --output text >&2

	case "$status" in
	TimedOut)
		echo "ssm-run: command $id did not finish within ${timeout}s" >&2
		return 124
		;;
	Cancelled)
		echo "ssm-run: command $id was cancelled" >&2
		return 125
		;;
	esac
	code=$(aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" \
		--query ResponseCode --output text)
	case "$code" in
	'' | *[!0-9-]*)
		echo "ssm-run: command $id ended $status with no exit status SSM would say" >&2
		return 125
		;;
	esac
	# SSM reports -1 for a command it never ran to an exit.
	if [ "$code" -lt 0 ]; then
		return 125
	fi
	return "$code"
}

case "${1:-}" in
run)
	[ "$#" -eq 4 ] || usage
	id=$(send "$2" "$3" "$4") || exit $?
	await "$2" "$id" "$3"
	exit $?
	;;
send)
	[ "$#" -eq 4 ] || usage
	send "$2" "$3" "$4"
	exit $?
	;;
wait)
	[ "$#" -eq 4 ] || usage
	await "$2" "$3" "$4"
	exit $?
	;;
*)
	usage
	;;
esac
