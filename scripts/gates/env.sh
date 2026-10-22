# env.sh: sourced by every gate script. It moves to the checkout's root, loads the
# pinned tool versions, and sets the caps a local run takes.
#
# A LOCAL GATE SHARES THE MACHINE; CI'S RUNS AS IT ALWAYS DID. Measured on a
# 12-core, 32 GiB Mac on 2026-10-05: `go test -race ./...` at go's default -p
# (one package per CPU) peaked at 3.36 GiB resident over 96 processes, and beside
# a container VM holding 24 GiB it pushed swap from 33.4 to 39.4 GiB, at which
# point DNS and the VPN client stalled with everything else. At -p 4 the same
# step peaked at 0.99 GiB over 56 processes and finished sooner (2049s against
# 2299s). So outside CI the race build runs at most TEST_PARALLEL packages at
# once (4), and the heavy steps run under NICE (nice -n 10). nice rather than
# macOS's taskpolicy: the same package took 37s under nice -n 10 and 27s to 119s
# unprioritised as the machine's load moved, but 205s clamped to the utility QoS
# and 485s at background QoS, which throttle disk I/O as well as CPU. Set either
# empty to lift it. Under CI both are empty whatever the environment says: CI
# runs each gate on a runner of its own, with go test's own -p and no nice.

gates_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P) || {
	printf 'gates: could not find the checkout this script is in\n' >&2
	exit 2
}
cd "$gates_root" || exit 2

if ! [ -f .github/tool-versions.env ]; then
	printf 'gates: %s/.github/tool-versions.env is missing, so no tool version is known\n' "$gates_root" >&2
	exit 2
fi
set -a
# shellcheck source=../../.github/tool-versions.env
. .github/tool-versions.env
set +a

nice_cmd=()
test_p=()
if [ -z "${CI-}" ]; then
	read -r -a nice_cmd <<<"${NICE-nice -n 10}"
	parallel=${TEST_PARALLEL-4}
	if [ -n "$parallel" ]; then
		test_p=(-p "$parallel")
	fi
fi
