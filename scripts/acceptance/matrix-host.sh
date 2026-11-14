#!/usr/bin/env bash
# Usage:
#   matrix-host.sh prepare <run-dir> <base-config> <account> <kms-key-arn> [<guest-role-arn>]
#   matrix-host.sh prelude <run-dir> <base-config> <listen> <bucket> <region> <run> <lifetime-minutes>
#   matrix-host.sh prepare-pool <run-dir> <base-config> <account> <listen> <bootstrap> <pool-host-role-arn>
#   matrix-host.sh run <run-dir> <jobs> [<pool-released>]
#   matrix-host.sh stop <run-dir>
#   matrix-host.sh judge <run-dir> <archs> <runs-file> <owner/repo> [<backends>]
#   matrix-host.sh stopped <run-dir>
#   matrix-host.sh down <run-dir>
#   matrix-host.sh block-s3 <run-dir> <region> <expiry-seconds>
#   matrix-host.sh unblock-s3 <run-dir> <region>
#   matrix-host.sh breaker <run-dir> <seconds>
#
# The acceptance matrix's half that runs on the acceptance host, beside the node its
# instances dial (#554). <run-dir> holds this run's billet binary at <run-dir>/billet, and
# everything the run makes goes under it.
#
# prepare stands an isolated deployment up from <base-config> (`billet acceptance up`),
# writes <run-dir>/tiers (one "<backend> <arch> <label>" line per tier the matrix
# dispatches to), <run-dir>/deployment (its id), <run-dir>/iam.json (the node's policy
# scoped to that id, which the caller puts on this host's role before `run`) and
# <run-dir>/teardown.json (the delete of that deployment's cache state objects, which
# decommission needs and the node's policy never grants, and which the caller puts on the
# role only for `down`; empty when the config has no ebs-s3 cache).
# <guest-role-arn> is the role inside the base config's node.ec2.instance_profile, for the
# policy's iam:PassRole; omit it when the guests carry no instance profile.
#
# prelude renders a pool's boot prelude (#829) at <run-dir>/prelude.sh, from the
# collection's template at <run-dir>/prelude-template.sh: the exports name this run's
# billet and collection in <bucket> under runs/<run>/ with their sha256, the node wire
# <listen> the join dials, <base-config>'s server.pool.init_args (as `billet acceptance
# matrix pool-init-args` prints them, without the kernel and Ceph flags the prelude sets
# itself), and the host's halt <lifetime-minutes> after it boots; then the template's body
# without its comments, which is what fits EC2's 16384 bytes of user data.
#
# prepare-pool stands a pool's deployment up from <base-config>, a control plane with a
# server.pool and no node, serving its hosts at <listen> and <bootstrap> with the prelude
# above, and writes <run-dir>/tiers, deployment and teardown.json as prepare does,
# <run-dir>/iam.json (the control plane's pool policy, `billet init iam --pool`, passing
# <pool-host-role-arn>) and <run-dir>/pool-host.json (the pool hosts' policy, `billet
# acceptance matrix pool-host-iam`), each scoped to the run's deployment.
#
# run runs the deployment until <jobs> jobs have finished (`billet acceptance run
# --no-teardown`, which writes the evidence and stops the services, and leaves the destroy
# to `down`, the one teardown, which runs with the teardown's grant), under
# matrix-supervise.py beside this script: it holds <run-dir>/run.lock for as long as the run lives, writes the run's exit
# to <run-dir>/run.exit, and, when <run-dir>/stop-requested appears, sends the run ONE
# SIGTERM, as an operator's Ctrl-C would, so the run writes its evidence and stops. A stop requested before it starts means it never starts.
#
# stop requests that, and returns; it signals nothing itself.
#
# judge judges that evidence (`billet acceptance matrix judge`) for the runs named in
# <runs-file> of <owner/repo> into <run-dir>/out, beside a copy of the evidence, and exits
# with the judge's status. It judges nothing for a run with no recorded exit, which may
# still be writing.
#
# stopped requests a stop and waits as down does, and takes nothing down: it exits 0 only
# once no run lives in <run-dir> and `billet acceptance stopped` has taken both of the
# run's services' own locks, which is when the caller may put the teardown's grant on
# the role, so no live node ever holds it.
#
# down requests a stop, waits up to MATRIX_DOWN_WAIT seconds (default 1800) for the run's
# lock to be free (a reboot frees it; nothing else does while the run lives), then destroys whatever the run left (`billet acceptance down`, idempotent)
# and removes the workspace; a run still live after that is said, and nothing is taken
# down beside it. It is the step every path ends with. Before the workspace goes, the
# services' logs are moved to <run-dir>/logs on this host, never to <run-dir>/out, which
# the workflow uploads: they name the account's resources, and a cell that failed for a
# reason only the node saw cannot be explained once they are gone (run 42406366812). A
# pool's run keeps its hosts' consoles with them (`billet acceptance matrix
# pool-consoles`), where the prelude forwards the node's warnings and errors, since the
# host the node ran on is ended (#861).
#
# block-s3, as root, drops every packet this host sends to <region>'s S3, the S3 entries
# of AWS's published address ranges, so the node's site store hangs rather than refuses,
# as #531's did; the EC2 API its launches and volumes use stays in reach. The rules are a
# table of billet's own, removed by a timer <expiry-seconds> on whatever else happens,
# armed before the table exists; and the block is installed only once a request to S3 is
# proved to go unanswered through it, else it is lifted and the command fails.
#
# unblock-s3, as root, removes that table and its timer, and exits 0 only once S3 answers
# this host again. It is idempotent, and a run that never blocked has nothing to remove.
#
# breaker reads the run's `billet status` until no store breaker is open, or <seconds>
# pass, keeping each read in <run-dir>/logs and printing the store's breaker lines: 0 once
# none is open, 1 if one still is, 2 if the status could not be read.
set -uo pipefail

usage() {
	sed -n '2,13p' "$0" >&2
	exit 2
}

# The block's table and its expiry's unit. One run holds the host at a time (the
# workflow's concurrency group), so a fixed name is the run's, and one a killed run left is
# replaced.
s3_table=billet_matrix_s3
s3_expiry=billet-matrix-s3-expiry

# s3_rules prints the block's nftables table from AWS's published ranges ($1) for one
# region ($2), and fails, printing nothing, when they name no IPv4 range for it or one that
# is not an address range.
s3_rules='import ipaddress, json, sys
doc = json.load(open(sys.argv[1]))
region, table = sys.argv[2], sys.argv[3]
def ranges(key, field):
    return sorted({str(ipaddress.ip_network(p[field])) for p in doc.get(key, [])
                   if p.get("service") == "S3" and p.get("region") == region})
v4, v6 = ranges("prefixes", "ip_prefix"), ranges("ipv6_prefixes", "ipv6_prefix")
if not v4:
    sys.exit("no S3 range for " + region)
out = ["table inet %s {" % table,
       "\tset v4 { type ipv4_addr; flags interval; auto-merge; elements = { %s } }" % ", ".join(v4)]
if v6:
    out.append("\tset v6 { type ipv6_addr; flags interval; auto-merge; elements = { %s } }" % ", ".join(v6))
out += ["\tchain out {", "\t\ttype filter hook output priority 0; policy accept;", "\t\tip daddr @v4 drop"]
if v6:
    out.append("\t\tip6 daddr @v6 drop")
out += ["\t}", "}"]
print("\n".join(out))'

# reach asks the region's S3 endpoint for anything at all within 8 s and answers with
# curl's status: 0 is an answer, whatever it said; 28 is none in time; anything else is a
# question that could not be put, which is neither.
reach() {
	curl -s -o /dev/null --max-time 8 "https://s3.$1.amazonaws.com/"
}

# lift removes the block's table and its expiry, and answers 0 only once no table of
# that name is installed.
lift() {
	systemctl stop "$s3_expiry.timer" >/dev/null 2>&1
	nft delete table inet "$s3_table" >/dev/null 2>&1
	nft list tables >"$run_dir/nft-tables" 2>&1 || {
		echo "matrix: nftables' tables could not be listed, so whether the block is lifted cannot be told" >&2
		return 2
	}
	grep -qx "table inet $s3_table" "$run_dir/nft-tables"
	case $? in
	1) return 0 ;;
	0)
		echo "matrix: the block on S3 is still installed" >&2
		return 1
		;;
	*)
		echo "matrix: nftables' tables could not be read" >&2
		return 2
		;;
	esac
}

# lstat_kind PATH prints what lstat says PATH is: dir, file, link or other, or absent
# when lstat answered ENOENT, and exits 0 only having printed one of those. Only that
# ENOENT is a name that is not there: the shell's -e and -L answer false for an I/O error
# as well, and a helper that failed for any reason prints nothing a caller accepts.
lstat_kind() {
	python3 -I -c 'import errno, os, stat, sys
try:
    m = os.lstat(sys.argv[1]).st_mode
except FileNotFoundError:
    print("absent")
    sys.exit(0)
print("link" if stat.S_ISLNK(m) else "dir" if stat.S_ISDIR(m) else "file" if stat.S_ISREG(m) else "other")' "$1"
}

# recorded answers 0 when the workspace holds its record, 1 when it holds none and
# nothing ever ran in it (no workspace, or no ledger: neither a control plane nor a
# pool's `up` made one), and anything else, said, when it could not tell or the record
# is missing beside a ledger, whose deployment nothing here can then name. A missing
# record is never on its own a run with nothing to take down, and a name is absent only
# when lstat says so of it inside a directory that is one: a link is not looked through.
recorded() {
	local kind
	kind=$(lstat_kind "$ws/acceptance.json")
	case $?:$kind in
	0:file) return 0 ;;
	0:absent) ;;
	*)
		echo "matrix: $ws/acceptance.json could not be read, so whether its run made anything cannot be told" >&2
		return 2
		;;
	esac
	for dir in "$ws" "$ws/server"; do
		kind=$(lstat_kind "$dir")
		case $?:$kind in
		0:absent) return 1 ;;
		0:dir) ;;
		*)
			echo "matrix: $dir could not be read, so whether its run made anything cannot be told" >&2
			return 2
			;;
		esac
	done
	kind=$(lstat_kind "$ws/server/billet.db")
	case $?:$kind in
	0:absent) return 1 ;;
	0:*)
		echo "matrix: $ws holds a ledger and no acceptance.json, so what its run made cannot be named;" \
			"a person decides" >&2
		;;
	*) echo "matrix: $ws/server/billet.db could not be read, so whether its run made anything cannot be told" >&2 ;;
	esac

	return 2
}

phase=${1:-}
run_dir=${2:-}
[ -n "$phase" ] && [ -n "$run_dir" ] || usage
billet="$run_dir/billet"
ws="$run_dir/ws"
supervise="$(dirname "$0")/matrix-supervise.py"

case "$phase" in
prepare)
	[ "$#" -eq 5 ] || [ "$#" -eq 6 ] || usage
	base=$3 account=$4 kms_key_arn=$5 guest_role_arn=${6:-}
	if ! "$billet" acceptance up --config "$base" --workspace "$ws" --account "$account"; then
		echo "matrix: the deployment could not be stood up" >&2
		exit 1
	fi
	if ! "$billet" acceptance matrix tiers --workspace "$ws" >"$run_dir/tiers"; then
		echo "matrix: the workspace's tiers could not be read" >&2
		exit 1
	fi
	if [ ! -s "$run_dir/tiers" ]; then
		echo "matrix: the base config gives the matrix no tier to dispatch to" >&2
		exit 1
	fi
	if ! jq -er '.deployment_id | select(. != "")' "$ws/acceptance.json" >"$run_dir/deployment"; then
		echo "matrix: the workspace records no deployment id" >&2
		exit 1
	fi
	iam=("$billet" init iam --config "$ws/billet.yaml" --kms-key-arn "$kms_key_arn")
	if [ -n "$guest_role_arn" ]; then
		iam+=(--role-arn "$guest_role_arn")
	fi
	if ! "${iam[@]}" >"$run_dir/iam.json"; then
		echo "matrix: the node's policy could not be rendered" >&2
		exit 1
	fi
	if ! "$billet" acceptance matrix teardown-policy --workspace "$ws" >"$run_dir/teardown.json"; then
		echo "matrix: the teardown's policy could not be rendered" >&2
		exit 1
	fi
	echo "matrix: deployment $(cat "$run_dir/deployment") is up; its tiers:"
	cat "$run_dir/tiers"
	;;
prelude)
	[ "$#" -eq 8 ] || usage
	base=$3 listen=$4 bucket=$5 region=$6 run=$7 lifetime=$8
	if ! args=$("$billet" acceptance matrix pool-init-args --config "$base"); then
		echo "matrix: the base config's pool init_args could not be read" >&2
		exit 1
	fi
	billet_sha=$(sha256sum "$billet" | cut -d' ' -f1) || exit 1
	# THE POOL ORDERS BOTH ARCHITECTURES, and an arm64 host runs the arm64 build the run
	# sent beside this host's own.
	arm_sha=$(sha256sum "$run_dir/billet-arm64" | cut -d' ' -f1) || exit 1
	collection_sha=$(sha256sum "$run_dir/collection.tar.gz" | cut -d' ' -f1) || exit 1
	{
		printf 'export ARTIFACT_BUCKET=%q ARTIFACT_REGION=%q\n' "$bucket" "$region"
		printf 'export BILLET_KEY=%q BILLET_SHA256=%q\n' "runs/$run/billet" "$billet_sha"
		printf 'export BILLET_KEY_ARM64=%q BILLET_SHA256_ARM64=%q\n' "runs/$run/billet-arm64" "$arm_sha"
		printf 'export COLLECTION_KEY=%q COLLECTION_SHA256=%q\n' "runs/$run/collection.tar.gz" "$collection_sha"
		printf 'export PRELUDE_JOIN_SERVER=%q\n' "$listen"
		printf 'export PRELUDE_INIT_ARGS=%q\n' "$(echo $args)"
		printf 'export PRELUDE_MAX_LIFETIME_MINUTES=%q\n' "$lifetime"
		# WITHOUT ITS COMMENTS OR ITS LEADING TABS, neither of which means anything inside
		# the template's here-documents and quoted lines (its test checks the result parses).
		tail -n +2 "$run_dir/prelude-template.sh" | sed -e '/^[[:space:]]*#/d' -e $'s/^\t*//'
	} >"$run_dir/prelude.sh" || exit 1
	size=$(wc -c <"$run_dir/prelude.sh")
	# THE CONTROL PLANE ADDS ITS WRAPPER AND THE JOIN'S LINE to EC2's 16384: about 310
	# bytes of its own, then the token, the order and every init_arg quoted again, about
	# 1,120 bytes in all with this lane's init_args, and it refuses the order past 16384
	# itself (app.PoolBoot).
	if [ "$size" -gt 15200 ]; then
		echo "matrix: the rendered prelude is $size bytes, past what fits beside the join in EC2's user data" >&2
		exit 1
	fi
	echo "matrix: the prelude is $size bytes"
	;;
prepare-pool)
	[ "$#" -eq 7 ] || usage
	base=$3 account=$4 listen=$5 bootstrap=$6 role=$7
	if ! "$billet" acceptance up --config "$base" --workspace "$ws" --account "$account" --label-prefix accept-pool \
		--fleet-listen "$listen" --fleet-bootstrap "$bootstrap" --boot-prelude "$run_dir/prelude.sh"; then
		echo "matrix: the pool's deployment could not be stood up" >&2
		exit 1
	fi
	if ! "$billet" acceptance matrix tiers --workspace "$ws" >"$run_dir/tiers"; then
		echo "matrix: the workspace's tiers could not be read" >&2
		exit 1
	fi
	if [ ! -s "$run_dir/tiers" ]; then
		echo "matrix: the base config gives the matrix no pool tier to dispatch to" >&2
		exit 1
	fi
	if ! jq -er '.deployment_id | select(. != "")' "$ws/acceptance.json" >"$run_dir/deployment"; then
		echo "matrix: the workspace records no deployment id" >&2
		exit 1
	fi
	if ! "$billet" init iam --pool --config "$ws/billet.yaml" --deployment "$(cat "$run_dir/deployment")" \
		--role-arn "$role" >"$run_dir/iam.json"; then
		echo "matrix: the pool's policy could not be rendered" >&2
		exit 1
	fi
	if ! "$billet" acceptance matrix pool-host-iam --workspace "$ws" >"$run_dir/pool-host.json"; then
		echo "matrix: the pool hosts' policy could not be rendered" >&2
		exit 1
	fi
	if ! "$billet" acceptance matrix teardown-policy --workspace "$ws" >"$run_dir/teardown.json"; then
		echo "matrix: the teardown's policy could not be rendered" >&2
		exit 1
	fi
	echo "matrix: the pool's deployment $(cat "$run_dir/deployment") is up; its tiers:"
	cat "$run_dir/tiers"
	;;
run)
	[ "$#" -eq 3 ] || [ "$#" -eq 4 ] || usage
	# A POOL'S RUN WAITS LESS FOR ITS JOBS, so the wait for the pool's own release after them
	# still ends inside the 250 minutes SSM gives the command.
	wait=210m released=()
	if [ "$#" -eq 4 ]; then
		wait=125m released=(--pool-released "$4")
	fi
	exec python3 -I "$supervise" run "$run_dir" "$billet" acceptance run --workspace "$ws" --jobs "$3" --wait "$wait" \
		"${released[@]}" --no-teardown
	;;
stop)
	[ "$#" -eq 2 ] || usage
	if [ ! -d "$run_dir" ]; then
		echo "matrix: no run directory at $run_dir; nothing to stop"
		exit 0
	fi
	: >"$run_dir/stop-requested" || exit 1
	echo "matrix: a stop is requested; a live run is signalled once by its own supervisor"
	;;
judge)
	[ "$#" -eq 5 ] || [ "$#" -eq 6 ] || usage
	archs=$3 runs=$4 repo=$5 backends=${6:-ec2}
	mkdir -p "$run_dir/out"
	# THE LOG FIRST, whatever is judged: it is what explains a run with no exit.
	if [ -f "$run_dir/run.log" ]; then
		cp "$run_dir/run.log" "$run_dir/out/"
	fi
	if [ ! -s "$run_dir/run.exit" ]; then
		echo "matrix: the run has no recorded exit, so it may still be writing; judging nothing" >&2
		exit 1
	fi
	if [ -f "$ws/evidence.json" ]; then
		cp "$ws/evidence.json" "$run_dir/out/"
	fi
	"$billet" acceptance matrix judge --workspace "$ws" --archs "$archs" --runs "$runs" --repository "$repo" \
		--backends "$backends" --out "$run_dir/out/matrix.json" >"$run_dir/out/matrix.txt"
	judged=$?
	cat "$run_dir/out/matrix.txt"
	exit "$judged"
	;;
stopped)
	[ "$#" -eq 2 ] || usage
	if [ -d "$run_dir" ]; then
		: >"$run_dir/stop-requested" || exit 1
		if ! python3 -I "$supervise" await "$run_dir" "${MATRIX_DOWN_WAIT:-1800}"; then
			echo "matrix: the run is still live; its workspace is $ws" >&2
			exit 1
		fi
	fi
	# AND ITS SERVICES, by their own locks: the run returns once its stop's grace is
	# over, and a node still draining past it holds none of the run's lock.
	recorded
	case $? in
	0)
		if ! "$billet" acceptance stopped --workspace "$ws"; then
			echo "matrix: a service of the run may still live; its workspace is $ws" >&2
			exit 1
		fi
		;;
	1) ;;
	*) exit 1 ;;
	esac
	echo "matrix: no run lives in $run_dir"
	;;
down)
	[ "$#" -eq 2 ] || usage
	if [ -d "$run_dir" ]; then
		: >"$run_dir/stop-requested" || exit 1
		if ! python3 -I "$supervise" await "$run_dir" "${MATRIX_DOWN_WAIT:-1800}"; then
			echo "matrix: the run is still live; nothing is taken down beside it, and its workspace is $ws" >&2
			exit 1
		fi
	fi
	recorded
	case $? in
	0) ;;
	1)
		echo "matrix: no workspace at $ws; nothing to take down"
		exit 0
		;;
	*) exit 1 ;;
	esac
	# A POOL'S HOSTS' CONSOLES FIRST, into the workspace, where the node's warnings outlive
	# a host the teardown ends (#861). WHATEVER IT KEEPS, the teardown runs and answers: a
	# log not kept is said, and costs only the log.
	# BOUNDED ON ITS OWN, so a slow console read never spends the teardown's time.
	if ! timeout --kill-after=30 600 "$billet" acceptance matrix pool-consoles --workspace "$ws"; then
		echo "matrix: not every pool host's console was kept" >&2
	fi
	python3 -I "$supervise" keep-logs "$run_dir"
	"$billet" acceptance down --workspace "$ws"
	;;
block-s3)
	[ "$#" -eq 4 ] || usage
	region=$3 expiry=$4
	case "$expiry" in
	'' | *[!0-9]*)
		echo "matrix: the block's expiry is \"$expiry\", not a number of seconds" >&2
		exit 2
		;;
	esac
	if ! curl -fsS --max-time 60 -o "$run_dir/ip-ranges.json" \
		"${MATRIX_IP_RANGES_URL:-https://ip-ranges.amazonaws.com/ip-ranges.json}"; then
		echo "matrix: AWS's published address ranges could not be fetched; nothing is blocked" >&2
		exit 1
	fi
	if ! python3 -I -c "$s3_rules" "$run_dir/ip-ranges.json" "$region" "$s3_table" >"$run_dir/s3-block.nft"; then
		echo "matrix: AWS's published ranges name no S3 range for $region that could be read; nothing is blocked" >&2
		exit 1
	fi
	if ! nft_path=$(command -v nft); then
		echo "matrix: nft is not on this host; nothing is blocked" >&2
		exit 1
	fi
	# ONE A KILLED RUN LEFT IS REPLACED, its timer first, so it cannot lift this run's block.
	lift || exit 1
	# THE EXPIRY BEFORE THE RULE: no moment holds a block nothing will remove.
	if ! systemd-run --quiet --collect --unit "$s3_expiry" --on-active="${expiry}s" \
		"$nft_path" delete table inet "$s3_table"; then
		echo "matrix: the block's expiry could not be armed; nothing is blocked" >&2
		exit 1
	fi
	if ! nft -f "$run_dir/s3-block.nft"; then
		echo "matrix: the block could not be installed" >&2
		lift
		exit 1
	fi
	reach "$region"
	reached=$?
	if [ "$reached" -ne 28 ]; then
		echo "matrix: a request to S3 in $region did not go unanswered through the block (curl exit $reached);" \
			"it is lifted" >&2
		lift
		exit 1
	fi
	echo "matrix: S3 in $region is out of this host's reach, for at most ${expiry}s"
	;;
unblock-s3)
	[ "$#" -eq 3 ] || usage
	region=$3
	lift || exit 1
	reach "$region"
	reached=$?
	if [ "$reached" -ne 0 ]; then
		echo "matrix: the block is lifted, and S3 in $region did not answer this host (curl exit $reached)" >&2
		exit 1
	fi
	echo "matrix: S3 in $region answers this host again"
	;;
breaker)
	[ "$#" -eq 3 ] || usage
	case "$3" in
	'' | *[!0-9]*)
		echo "matrix: \"$3\" is not a number of seconds" >&2
		exit 2
		;;
	esac
	end=$((SECONDS + $3))
	mkdir -p "$run_dir/logs" || exit 2
	while :; do
		at=$(date -u +%Y%m%dT%H%M%SZ)
		status="$run_dir/logs/status-$at.txt"
		if ! "$billet" status --config "$ws/billet.yaml" >"$status" 2>&1; then
			echo "matrix: the run's status could not be read at $at" >&2
			exit 2
		fi
		grep 'breaker' "$status" >"$status.breaker"
		case $? in
		0) ;;
		1) : >"$status.breaker" ;;
		*)
			echo "matrix: the run's status at $at could not be read back" >&2
			exit 2
			;;
		esac
		grep -q 'breaker OPEN' "$status.breaker"
		open=$?
		case $open in
		1)
			echo "matrix: $at: no store breaker is open"
			cat "$status.breaker"
			exit 0
			;;
		0) ;;
		*)
			echo "matrix: the run's status at $at could not be read back" >&2
			exit 2
			;;
		esac
		if [ "$SECONDS" -ge "$end" ]; then
			cat "$status.breaker"
			echo "matrix: a store breaker is still open at $at" >&2
			exit 1
		fi
		sleep "${MATRIX_POLL:-10}"
	done
	;;
*)
	usage
	;;
esac
