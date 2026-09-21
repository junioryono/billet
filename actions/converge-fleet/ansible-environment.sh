# Sourced by converge.sh and release-guard.sh: the Ansible settings every step
# that reaches the fleet runs under, defined once so the two cannot drift.
# shellcheck shell=bash

export ANSIBLE_HOST_KEY_CHECKING=True
export ANSIBLE_STDOUT_CALLBACK=default
export ANSIBLE_RESULT_FORMAT=yaml
export ANSIBLE_FORCE_COLOR=0
export ANSIBLE_NOCOLOR=1

# A CONNECTION THE NETWORK DROPPED ENDS THE TASK AS UNREACHABLE: ssh gives up
# once more than four keepalives go unanswered, about 75 seconds. Without one,
# ssh never learns that a flow lost between the runner and the host is gone, and
# the job waits on it until it is cancelled; a dry run through WARP sat for 36
# minutes after the host logged the close (2026-09-21, not reproduced). This
# replaces Ansible's default ssh_args rather than adding to them, so the
# connection sharing that default carried is restated; it bounds a dead
# transport, not a task that is slow over a live one.
export ANSIBLE_SSH_ARGS="-C -o ControlMaster=auto -o ControlPersist=60s -o ServerAliveInterval=15 -o ServerAliveCountMax=4"
