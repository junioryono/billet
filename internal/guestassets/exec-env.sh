#!/bin/sh
# billet-exec-env reads the job's environment from descriptor 3 and execs its
# arguments with exactly that environment.
#
# THE ENVIRONMENT NEVER TRAVELS IN AN ARGUMENT LIST. It carries the runner's
# registration and the cache bearer, and an argument list is readable by every
# process in the guest through /proc/<pid>/cmdline. The launch writes the lines
# through a here-document, which the shell feeds through a pipe or an unlinked
# file, so no process between the agent and the runner carries a value in argv.
#
# Run as `env -i billet-exec-env COMMAND [ARG...] 3<<EOF`, one NAME=VALUE per line,
# then a line that is exactly `end`. Every other line is refused: a stream with
# no terminator is truncated, and a malformed line is a launch the writer got
# wrong. Neither is guessed at, and no refusal prints a value. A NUL byte cannot
# arrive: both writers build the stream from shell variables, and neither dash
# nor bash can hold one (measured, dash 0.5.12 and bash 5.2).
#
# TWO PHASES, AND THE ORDER IS THE SECURITY PROPERTY. The whole stream is read and
# validated before anything is exported, so nothing listed can change how this
# script runs while it still holds values: an exported PATH naming dash's
# %builtin turns `[` and `echo` into external processes whose argv is a value
# (measured, dash 0.5.12), and a listed name equal to one of this script's own
# variables would rewrite its state. The exports then use only keywords and
# special builtins, which no PATH redirects, and assign no variable.
set -u

if [ "$#" -eq 0 ]; then
	echo "billet-exec-env: no command to run" >&2
	exit 2
fi
if ! { true <&3; } 2>/dev/null; then
	echo "billet-exec-env: descriptor 3 is not open, so there is no environment to read" >&2
	exit 2
fi

billet_exec_env_nl='
'
billet_exec_env_count=0
billet_exec_env_all=""
billet_exec_env_done=""
while IFS= read -r billet_exec_env_kv <&3; do
	billet_exec_env_count=$((billet_exec_env_count + 1))
	# A VALUE IS NEVER A COMMAND'S ARGUMENT, so it is compared with `case`, a
	# keyword, and never `[`, which a PATH can make an external process.
	case "$billet_exec_env_kv" in
		end)
			billet_exec_env_done=1
			break
			;;
		# dash parses an assignment to OPTIND as a number and aborts on anything
		# else, printing the value (measured, dash 0.5.12); it is the shell's own
		# state and no job reads it.
		OPTIND=*)
			echo "billet-exec-env: line $billet_exec_env_count names OPTIND, which the shell keeps as its own state" >&2
			exit 2
			;;
		[A-Za-z_]*=*) ;;
		*)
			echo "billet-exec-env: line $billet_exec_env_count is not NAME=VALUE" >&2
			exit 2
			;;
	esac
	case "${billet_exec_env_kv%%=*}" in
		*[!A-Za-z0-9_]*)
			echo "billet-exec-env: line $billet_exec_env_count does not name a variable" >&2
			exit 2
			;;
	esac
	billet_exec_env_all="$billet_exec_env_all$billet_exec_env_kv$billet_exec_env_nl"
done
if [ -z "$billet_exec_env_done" ]; then
	echo "billet-exec-env: the environment ended without its terminator after $billet_exec_env_count line(s)" >&2
	exit 2
fi
# THE TERMINATOR IS THE LAST LINE: anything after it is a stream the writer got
# wrong, and would otherwise be dropped unread. Read with od, because dash's
# `read` answers 1 for a read error as for the end of the stream (measured, dash
# 0.5.12) and a failed read is not proof that nothing follows: od exits non-zero
# on an error, prints something for any byte (a bare newline included), and its
# argv holds no value. Not wc, which counts a regular file by its size on macOS
# rather than from the descriptor's offset.
if ! billet_exec_env_rest=$(od -An -c <&3); then
	echo "billet-exec-env: what follows the terminator could not be read" >&2
	exit 2
fi
case "$billet_exec_env_rest" in
	?*)
		echo "billet-exec-env: the environment goes on after its terminator" >&2
		exit 2
		;;
esac
exec 3<&-

# THE SHELL'S OWN BOOKKEEPING IS NOT THE JOB'S. dash exports PWD even under
# `env -i` (measured, dash 0.5.12), so it is removed before the listed set,
# which may name it. bash as sh also exports SHLVL at the exec, which no unset
# prevents (measured, bash 5.2 and 3.2); every guest's /bin/sh is dash.
unset PWD OLDPWD

# The lines go in front of the command, split on newlines alone and never
# globbed, with `end` between them: no line equals `end`, since every line holds
# an `=`. The scratch variables are never exported, so they reach no command,
# and a listed name equal to one of them is exported with its listed value.
IFS=$billet_exec_env_nl
set -f
set -- $billet_exec_env_all end "$@"
while :; do
	case "$1" in
		end)
			shift
			break
			;;
	esac
	export "$1"
	shift
done
exec "$@"
