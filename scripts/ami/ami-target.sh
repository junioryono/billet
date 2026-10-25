#!/bin/bash
# ami-target.sh: run-runner-images.sh's target driver on the EC2 builder itself.
#
# The AMI is the builder's own root disk (ADR-022), so the target is this machine:
#   exec ARGV...        run ARGV as root here, in a clean environment as the guest's
#                       systemd-run gives a step, so nothing of the build unit's
#                       own environment reaches GitHub's scripts
#   copy-in SRC DEST    `cp -R` semantics: a DEST that is a directory receives SRC
#   copy-out SRC DIR    copy SRC into DIR
#   reboot              record that the build is rebooting and where it resumes,
#                       enable the build unit for the next boot, and reboot; it
#                       never returns, because the reboot ends this process
#
# Environment for reboot: BILLET_RI_NEXT_STEP (from the runner) and
# BILLET_AMI_STATE, the build's record (build-ami.sh's).
set -euo pipefail

fail() {
	echo "ami-target: $*" >&2
	exit 1
}

case "${1:-}" in
exec)
	shift
	[ "$#" -gt 0 ] || fail "exec needs a command"
	exec env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
		HOME=/root DEBIAN_FRONTEND=noninteractive "$@"
	;;
copy-in)
	[ "$#" -eq 3 ] || fail "copy-in needs a source and a destination"
	src=$2 dest=$3
	if [ -d "$dest" ]; then
		dest=${dest%/}/$(basename "$src")
	fi
	cp -R -- "$src" "$dest"
	;;
copy-out)
	[ "$#" -eq 3 ] || fail "copy-out needs a source and a directory"
	cp -R -- "$2" "${3%/}/$(basename "$2")"
	;;
reboot)
	next=${BILLET_RI_NEXT_STEP:-}
	state=${BILLET_AMI_STATE:-}
	case "$next" in
	'' | *[!0-9]* | 0*) fail "reboot needs BILLET_RI_NEXT_STEP, the step the build resumes at" ;;
	esac
	[ -n "$state" ] || fail "reboot needs BILLET_AMI_STATE, the build's record"
	# THE RECORD FIRST, THEN THE UNIT: a crash between the two leaves a record no
	# boot acts on (the unit is not enabled), and the build ends at billet's deadline
	# rather than resuming from a record the reboot never made.
	"$(dirname "$0")/build-ami.sh" record-reboot "$next"
	systemctl enable billet-ami-build.service >/dev/null
	sync
	systemctl reboot
	# THE REBOOT ENDS THIS PROCESS. Returning would let the runner run the next
	# step in a boot that is going down.
	sleep infinity
	;;
*)
	fail "unknown command ${1:-}; want exec, copy-in, copy-out or reboot"
	;;
esac
