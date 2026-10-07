#!/bin/sh
# with-check-lock.sh LOCKFILE COMMAND [ARG...]
#
# Runs COMMAND holding an exclusive lock on LOCKFILE, so two gates on one
# machine, from two sessions, worktrees or repositories, take turns instead of
# starving the machine together. A second run waits for the first and says so
# once; the exit status is COMMAND's own.
#
# An empty LOCKFILE means the lock every repository's gate shares:
# $XDG_CACHE_HOME/dev-gate.lock when XDG_CACHE_HOME is absolute, otherwise
# $HOME/.cache/dev-gate.lock. It is worked out here, in the shell, because
# make's word functions split a value holding spaces.
#
# macOS ships lockf(1) and no flock(1); Linux ships flock(1) and no lockf(1). The
# lock is a courtesy to the rest of the machine and never a gate, so a lock that
# cannot be taken (no tool, a file that cannot be created) is reported and the
# command runs without it.
#
# A run inside a command that already holds the same lock (a gate that runs
# another gate) runs under it: the command is handed DEV_GATE_LOCK_HELD naming
# the lock, and a run that finds its own lock there would otherwise wait for an
# ancestor that cannot finish until it does.
set -u

if [ "$#" -lt 2 ]; then
	echo "usage: with-check-lock.sh LOCKFILE COMMAND [ARG...]" >&2
	exit 2
fi

lock=$1
shift

if [ -z "$lock" ]; then
	case "${XDG_CACHE_HOME-}" in
	/*) lock=$XDG_CACHE_HOME/dev-gate.lock ;;
	*)
		if [ -z "${HOME-}" ]; then
			echo "with-check-lock: neither an absolute XDG_CACHE_HOME nor HOME is set, so this run is not serialised with others" >&2
			exec "$@"
		fi
		lock=$HOME/.cache/dev-gate.lock
		;;
	esac
fi

if [ "${DEV_GATE_LOCK_HELD-}" = "$lock" ]; then
	echo "with-check-lock: $lock is held by the run this one is inside; running under it" >&2
	exec "$@"
fi

# The lock's directory is made here, quoted, so a path holding spaces is one
# path, and a directory that cannot be made still ends in the command running,
# unlocked and saying so.
dir=$(dirname -- "$lock")
if ! mkdir -p -- "$dir" 2>/dev/null; then
	echo "with-check-lock: could not create $dir, so this run is not serialised with others" >&2
	exec "$@"
fi

# The probe runs `true` under a lock it cannot wait for, so its status is the
# lock tool's own answer and never the command's: 0 free, 75 held, anything
# else a lock that could not be taken at all.
if command -v lockf >/dev/null 2>&1; then
	lockf -k -t 0 "$lock" true 2>/dev/null
	probe=$?
	held="lockf -k"
elif command -v flock >/dev/null 2>&1; then
	flock -n -E 75 "$lock" true 2>/dev/null
	probe=$?
	# -o closes the locked descriptor before the command runs, so a process the
	# command leaves behind cannot go on holding the lock after it exits.
	held="flock -o"
else
	echo "with-check-lock: neither lockf nor flock is installed, so this run is not serialised with others" >&2
	exec "$@"
fi

case "$probe" in
0) ;;
75) echo "with-check-lock: another run holds $lock; waiting for it to finish" >&2 ;;
*)
	echo "with-check-lock: could not take $lock (status $probe), so this run is not serialised with others" >&2
	exec "$@"
	;;
esac

# The lock is taken a second time, now waiting, and that take can fail too (the
# file's directory replaced since the probe, say). The command's first act under
# the lock removes a marker, so a failure that leaves the marker never ran the
# command, and the command runs unlocked; any other status is the command's own.
# Without a marker to tell the two apart, the take is the last act, as before.
if ! started=$(mktemp "${TMPDIR:-/tmp}/with-check-lock.XXXXXX" 2>/dev/null); then
	# An assignment before exec is not exported everywhere, so it is exported.
	DEV_GATE_LOCK_HELD=$lock
	export DEV_GATE_LOCK_HELD
	# $held is one of the two literal spellings above; it is split on purpose.
	# shellcheck disable=SC2086
	exec $held "$lock" "$@"
fi

# The inner script's $1 and $@ are its own, so they stay in single quotes.
# shellcheck disable=SC2086,SC2016
DEV_GATE_LOCK_HELD=$lock $held "$lock" sh -c 'rm -f -- "$1"; shift; exec "$@"' with-check-lock "$started" "$@"
status=$?

if [ "$status" -ne 0 ] && [ -e "$started" ]; then
	rm -f -- "$started"
	echo "with-check-lock: could not take $lock (status $status), so this run is not serialised with others" >&2
	exec "$@"
fi

rm -f -- "$started"
exit "$status"
