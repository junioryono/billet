#!/bin/bash
# A pool host's boot prelude (ADR-030): server.pool.boot_prelude for a Firecracker pool
# on AWS metal, as #553's run used it. Read README.md before rendering it.
#
# It installs billet's binary (never the package, whose seeded billet.yaml the join
# would leave alone), converges the collection's host role in node prepare-only mode
# (Firecracker, its network, a single-OSD Ceph on a disk of the host's own, the guests'
# egress limits), installs the guest kernel the node's config names, and leaves the
# node's config and start to `billet join`, which the pool runs after it. A unit it
# starts imports the guest image once the node has registered.
#
# Rendered per deployment, as exports above this file's body:
#   BILLET_URL, BILLET_SHA256          billet's linux binary and its sha256 (or, for a
#                                      private bucket, ARTIFACT_BUCKET, ARTIFACT_REGION
#   COLLECTION_URL, COLLECTION_SHA256  and BILLET_KEY / COLLECTION_KEY, read with the
#                                      instance profile's own credentials)
#   PRELUDE_JOIN_SERVER                server.pool.join_server
#   PRELUDE_IMAGE                      the tier's image name (default ubuntu-2404-x64)
#   PRELUDE_IMAGE_SOURCE               optional: a guest image release to pull from
#   PRELUDE_OSD_DEVICE                 optional: the OSD's disk, else the first free one
#   PRELUDE_BUDGET_SECONDS             optional: how long after it starts the prelude
#                                      still begins a kernel pull (default 5400)
#
# It runs under the pool's `bash -euo pipefail`, and its output reaches the console the
# source reads, so it prints nothing a network answered and no line of the join's form:
# what a command that talks to a network says goes to a log under $work, and a failure
# is reported by the line it stopped on. It may be run again on the same host: every
# choice it makes is recorded and reused.
set -Eeuo pipefail
export DEBIAN_FRONTEND=noninteractive HOME=/root
work=/var/lib/billet-pool-prelude
mkdir -p "$work"
cd "$work"
trap 'echo "prelude: stopped at line $LINENO; its logs are in /var/lib/billet-pool-prelude" >&2' ERR

# retry N DELAY COMMAND...: COMMAND until it succeeds, at most N times.
retry() {
	local n=$1 delay=$2 i
	shift 2
	for ((i = 1; ; i++)); do
		"$@" && return 0
		[ "$i" -lt "$n" ] || return 1
		sleep "$delay"
	done
}

apt-get -o Acquire::Retries=10 -o DPkg::Lock::Timeout=600 update -qq >"$work/apt.log" 2>&1
apt-get -o Acquire::Retries=10 -o DPkg::Lock::Timeout=600 install -y -qq python3-venv curl >>"$work/apt.log" 2>&1

# The instance metadata service, retried: one refused or slow answer would otherwise end
# the boot before the join. What curl says goes to a log, as every fetch's does.
imds_token() {
	curl -fsS --retry 10 --retry-all-errors --retry-delay 2 --connect-timeout 2 --max-time 10 \
		-X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 300' http://169.254.169.254/latest/api/token
} 2>>"$work/fetch.log"
imds() {
	curl -fsS --retry 10 --retry-all-errors --retry-delay 2 --connect-timeout 2 --max-time 10 \
		-H "X-aws-ec2-metadata-token: $1" "http://169.254.169.254/latest/meta-data/$2"
} 2>>"$work/fetch.log"

# A DOWNLOAD THAT STALLS IS ABANDONED AND ASKED AGAIN: under 1 KB/s for a minute or
# fifteen minutes in all ends an attempt, and no attempt begins after twenty, so a stalled
# connection cannot hold the boot until the join's two-hour token has lapsed.
download=(curl -fsSL --connect-timeout 10 --speed-limit 1024 --speed-time 60 --max-time 900
	--retry 20 --retry-all-errors --retry-delay 5 --retry-max-time 1200)

# PLAIN HTTPS, OR S3 WITH THE INSTANCE'S ROLE. A link presigned with a person's session
# stops working when the session ends, and the control plane reads boot_prelude once,
# when it starts, so every later order would fail on it (2026-11-09). The role's
# credentials are fetched per call and reach curl on its standard input, never its
# arguments or the console.
fetch() {
	if [ -z "${ARTIFACT_BUCKET:-}" ]; then
		"${download[@]}" -o "$2" "$1"
		return
	fi
	local token role creds ak sk st
	token=$(imds_token)
	role=$(imds "$token" iam/security-credentials/)
	creds=$(imds "$token" "iam/security-credentials/$role")
	ak=$(printf '%s' "$creds" | python3 -c 'import json,sys; print(json.load(sys.stdin)["AccessKeyId"])')
	sk=$(printf '%s' "$creds" | python3 -c 'import json,sys; print(json.load(sys.stdin)["SecretAccessKey"])')
	st=$(printf '%s' "$creds" | python3 -c 'import json,sys; print(json.load(sys.stdin)["Token"])')
	"${download[@]}" --aws-sigv4 "aws:amz:${ARTIFACT_REGION}:s3" -K - \
		-o "$2" "https://${ARTIFACT_BUCKET}.s3.${ARTIFACT_REGION}.amazonaws.com/$1" <<EOF
user = "$ak:$sk"
header = "x-amz-security-token: $st"
EOF
} 2>>"$work/fetch.log"

# A release's archive (its sha256 is in the release's checksums.txt) or a bare binary.
fetch "${BILLET_KEY:-${BILLET_URL:-}}" billet.download
echo "$BILLET_SHA256  billet.download" | sha256sum -c - >/dev/null 2>&1
if tar -tzf billet.download billet >/dev/null 2>&1; then
	tar -xzf billet.download billet
else
	mv billet.download billet
fi
install -m 0755 -o root -g root billet /usr/bin/billet

fetch "${COLLECTION_KEY:-${COLLECTION_URL:-}}" collection.tar.gz
echo "$COLLECTION_SHA256  collection.tar.gz" | sha256sum -c - >/dev/null 2>&1
rm -rf src
mkdir src
tar -xzf collection.tar.gz -C src
# ansible_collections at the tarball's top, or one directory down (a GitHub source
# archive of a tag or commit).
collections_root=$(dirname "$(find src -maxdepth 2 -type d -name ansible_collections -print -quit)")
[ -d "$collections_root/ansible_collections/junioryono/billet" ] || {
	echo "prelude: the collection tarball holds no ansible_collections/junioryono/billet" >&2
	exit 1
}
tests=$collections_root/ansible_collections/junioryono/billet/tests

# The install is retried whole: pip retries a request, not every answer an index gives.
pip_install() {
	venv/bin/pip install -q --retries 10 "ansible-core==$(cat "$tests/ansible-core-version")" >>"$work/pip.log" 2>&1
}
[ -x venv/bin/ansible-playbook ] || {
	rm -rf venv
	python3 -m venv venv
	retry 5 15 pip_install
}
galaxy() {
	venv/bin/ansible-galaxy collection install -p "$work/collections" \
		"ansible.posix:$(cat "$tests/ansible-posix-version")" >>"$work/galaxy.log" 2>&1
}
retry 5 15 galaxy

# THE DISKS, CHOSEN ONCE AND RECORDED. The OSD takes PRELUDE_OSD_DEVICE, else the first
# whole disk with no partitions, nothing mounted, no signature and not the root's; a
# second such disk holds what the image's small root cannot. A run again reads the
# record: the OSD's disk is no longer free, and the second is mounted.
#
# BLANK IS WIPEFS'S EMPTY LIST, read successfully, as the role decides a ledger volume:
# blkid's "nothing found" is also its answer when it could not read the device, and a
# partition table carries no filesystem type. A disk wipefs could not read is not free,
# and a probe that fails fails the scan, so a scan cut short is never recorded.
signatures() {
	wipefs --no-act --noheadings --output TYPE "$1" 2>>"$work/disks.log"
}
free_disks() {
	local root_disk names d type parts mounts sigs
	root_disk=$(lsblk -no PKNAME "$(findmnt -no SOURCE /)") || return 1
	root_disk=${root_disk%%$'\n'*}
	names=$(lsblk -dnpo NAME,TYPE) || return 1
	while read -r d type; do
		[ "$type" = disk ] || continue
		[ "${d##*/}" != "$root_disk" ] || continue
		parts=$(lsblk -no NAME "$d") || return 1
		[ "$(wc -l <<<"$parts")" -eq 1 ] || continue
		mounts=$(lsblk -no MOUNTPOINT "$d") || return 1
		[ -z "${mounts//[[:space:]]/}" ] || continue
		sigs=$(signatures "$d") || continue
		[ -z "${sigs//[[:space:]]/}" ] || continue
		echo "$d"
	done <<<"$names"
	return 0
}
# stable_name DISK: a /dev/disk/by-id name of the disk, waiting for udev to make one. A
# kernel name is only the order the disks were found in at this boot, so a disk with no
# such name is refused rather than recorded by one.
stable_name() {
	local l try
	for try in 1 2 3 4 5 6 7 8 9 10; do
		udevadm settle --timeout=10 >/dev/null 2>&1 || true
		for l in /dev/disk/by-id/*; do
			if [ -e "$l" ] && [ "$(readlink -f "$l")" = "$1" ]; then
				echo "$l"
				return 0
			fi
		done
		[ "$try" -eq 10 ] || sleep 3
	done
	echo "prelude: $1 has no /dev/disk/by-id name to record it by" >&2
	return 1
}
if [ ! -s "$work/disks.env" ]; then
	free=$(free_disks)
	disks=()
	[ -z "$free" ] || mapfile -t disks <<<"$free"
	osd=${PRELUDE_OSD_DEVICE:-${disks[0]:-}}
	[ -n "$osd" ] || {
		echo "prelude: no free disk for the OSD" >&2
		exit 1
	}
	# Compared by kernel name, so a /dev/disk/by-id alias is not taken for another disk.
	osd=$(readlink -f "$osd")
	scratch=""
	for d in "${disks[@]}"; do
		[ "$d" = "$osd" ] || {
			scratch=$d
			break
		}
	done
	# RECORDED BY IDENTITY, and only once every name is in hand.
	osd_id=$(stable_name "$osd")
	scratch_id=""
	[ -z "$scratch" ] || scratch_id=$(stable_name "$scratch")
	printf 'osd=%q\nscratch=%q\n' "$osd_id" "$scratch_id" >"$work/disks.env"
fi
# shellcheck source=/dev/null
. "$work/disks.env"
osd=$(readlink -f "$osd")
[ -b "$osd" ] || {
	echo "prelude: the OSD's recorded disk is not here" >&2
	exit 1
}
if [ -n "$scratch" ]; then
	scratch=$(readlink -f "$scratch")
	[ -b "$scratch" ] || {
		echo "prelude: the scratch disk's recorded disk is not here" >&2
		exit 1
	}
fi

# THE SECOND DISK holds Docker's data (cephadm's containers), Ceph's (the monitor warns
# below 30% free, which an 8 GB root reaches) and the guest image's staging, which needs
# room for its parts, the assembled archive and the decompressed image at once: 97 GB at
# its peak for the x86_64 image of 2026-11-08, which filled a 40 GB disk (2026-11-09).
# It is formatted only when wipefs read it and found nothing, used when it holds exactly
# one ext4 (this prelude's, from a run cut short before the mount), and refused
# otherwise. It is mounted by its filesystem's UUID from /etc/fstab, so a reboot brings
# Docker's and Ceph's data back before either starts. nofail, because a disk gone after a
# reboot must not stop the boot: the host is then not a working node, and the pool's
# drain finds it so.
staging=""
if [ -n "$scratch" ]; then
	if ! mountpoint -q /var/lib/pool-scratch; then
		sigs=$(signatures "$scratch") || {
			echo "prelude: could not tell what $scratch holds" >&2
			exit 1
		}
		sigs=${sigs//[[:space:]]/}
		case $sigs in
		"") mkfs.ext4 -q -F "$scratch" ;;
		ext4) ;;
		*)
			echo "prelude: $scratch holds something other than one ext4 filesystem" >&2
			exit 1
			;;
		esac
	fi
	uuid=$(blkid -s UUID -o value "$scratch")
	[ -n "$uuid" ] || {
		echo "prelude: $scratch has no filesystem UUID" >&2
		exit 1
	}
	mkdir -p /var/lib/pool-scratch
	grep -q '[[:space:]]/var/lib/pool-scratch[[:space:]]' /etc/fstab ||
		echo "UUID=$uuid /var/lib/pool-scratch ext4 defaults,nofail 0 2" >>/etc/fstab
	mountpoint -q /var/lib/pool-scratch || mount /var/lib/pool-scratch
	for dir in docker ceph; do
		mkdir -p "/var/lib/pool-scratch/$dir" "/var/lib/$dir"
		grep -q "[[:space:]]/var/lib/${dir}[[:space:]]" /etc/fstab ||
			echo "/var/lib/pool-scratch/$dir /var/lib/$dir none bind,nofail,x-systemd.requires-mounts-for=/var/lib/pool-scratch 0 0" >>/etc/fstab
		mountpoint -q "/var/lib/$dir" || mount "/var/lib/$dir"
	done
	# nofail lets the boot go on without the mounts, so Docker is made to wait for both
	# itself, and Ceph's daemons, which are its containers, with it: Docker on the root's
	# empty directories would start Ceph without its data.
	mkdir -p /etc/systemd/system/docker.service.d
	printf '[Unit]\nRequiresMountsFor=/var/lib/docker /var/lib/ceph\n' \
		>/etc/systemd/system/docker.service.d/billet-pool-scratch.conf
	systemctl daemon-reload
	mkdir -p /var/lib/pool-scratch/images
	staging=/var/lib/pool-scratch/images
fi

ip=$(imds "$(imds_token)" local-ipv4)

# THE ROLE'S INPUTS ARE THE JOIN'S OWN CONFIG: `billet init --join`, with the flags
# server.pool.init_args hands the join, renders it to a scratch path (never
# /etc/billet/billet.yaml, which is the join's), so the bridges, the pools and the
# kernel path the role prepares are the ones the join's config names.
rm -rf "$work/preview"
mkdir -p "$work/preview"
billet init --join "$PRELUDE_JOIN_SERVER" --provider firecracker --profile local-service \
	--node-name pool-prepare --kernel-image /var/lib/billet/kernels/vmlinux \
	--ceph-user billet --ceph-keyring /etc/ceph/ceph.client.billet.keyring \
	--config "$work/preview/billet.yaml" >"$work/preview/init.log" 2>&1
venv/bin/python3 -I -c 'import sys, yaml; print(yaml.safe_dump({"billet_config": yaml.safe_load(open(sys.argv[1]))}))' \
	"$work/preview/billet.yaml" >"$work/billet-vars.yml"

cat >"$work/prepare-vars.yml" <<EOF
billet_enable_server: false
billet_enable_node: true
billet_node_prepare_only: true
billet_converge_guard_holder: pool-prelude
ansible_user: root
billet_uplink_shaping: false
billet_fetch_retries: 20
billet_ceph_bootstrap: true
billet_ceph_mon_ip: "${ip}"
billet_ceph_devices: ["${osd}"]
billet_ceph_pool_size: 1
billet_ceph_pool_min_size: 1
# The guests' bridges off 172.31.0.0/16: an AWS default VPC's resolver is 172.31.0.2,
# inside the role's default first bridge.
billet_networks:
  - {name: billet0, address: 10.240.0.1, prefix: 24, netmask: 255.255.255.0, dhcp_start: 10.240.0.10, dhcp_end: 10.240.0.250}
  - {name: billet1, address: 10.240.1.1, prefix: 24, netmask: 255.255.255.0, dhcp_start: 10.240.1.10, dhcp_end: 10.240.1.250}
# What a hosting provider polices (ADR-030), recommended in docs/operating/bare-metal-pool.md.
billet_guest_refuse_smtp: true
billet_guest_new_connections_per_second: 1000
billet_guest_packets_per_second: 400000
billet_guest_egress_bytes_per_second: 125000000
EOF
# THE HOST BY ITS OWN NAME: the role addresses Ceph's orchestrator host as the
# inventory host, and cephadm registers this machine by its hostname.
cat >"$work/prepare.yml" <<'EOF'
- name: Prepare this host for billet join
  hosts: all
  connection: local
  become: true
  roles:
    - role: junioryono.billet.host
EOF
# THE GUARD IS THE DRIVER'S TO RELEASE, and this is the driver. The role takes the
# converge guard under pool-prelude and never releases it, and a guard never expires, so
# a host left holding it would refuse every upgrade for its life. It is released however
# the converge ended, as the published action releases it; after a converge that
# succeeded, a release that fails stops the boot.
#
# THE CONVERGE IS RETRIED WHOLE: its package installs and Ceph's container pulls are not
# all retried inside the role, and a converge run again on a host it has prepared, its
# own guard still held, finishes it (four runs on one host, 2026-11-09).
converge() {
	ANSIBLE_COLLECTIONS_PATH="$work/$collections_root:$work/collections" ANSIBLE_FORCE_COLOR=0 venv/bin/ansible-playbook \
		-i "$(hostname)," -e "@$work/billet-vars.yml" -e "@$work/prepare-vars.yml" "$work/prepare.yml" \
		>>"$work/converge.log" 2>&1
}
: >"$work/converge.log"
converged=0
retry 3 60 converge || converged=$?
if [ "$converged" -ne 0 ]; then
	billet converge-guard release --holder pool-prelude >"$work/guard-release.log" 2>&1 || true
	echo "prelude: the host role's converge failed three times; see $work/converge.log" >&2
	exit 1
fi
billet converge-guard release --holder pool-prelude >"$work/guard-release.log" 2>&1

# A SINGLE OSD HOLDS NO REPLICA, BY DESIGN (#553's plan change of 2026-11-08): its pools
# hold golden images, pulled again from the signed source, and per-job clones. The
# managers are restarted to read it: one of the two cephadm starts kept the size it
# started with, and reported TOO_FEW_OSDS against it until restarted (2026-11-09).
{
	retry 5 10 ceph config set global mon_warn_on_pool_no_redundancy false
	retry 5 10 ceph config set global osd_pool_default_size 1
	retry 5 10 ceph orch restart mgr
} >>"$work/ceph.log" 2>&1

# THE KERNEL NOW, THE IMAGE AFTER THE JOIN. A pull installs the image's paired kernel
# first and then refuses to import until the node has registered and recorded what its
# control plane accepts (#584). So this pull is for the kernel, which the node's config
# names and needs to start, retried because GitHub's edge answered release downloads
# from an AWS instance with 504 on two requests in three (2026-11-09).
pull=(billet images pull --config /etc/billet/billet.yaml --verify)
[ -z "$staging" ] || pull+=(--staging-dir "$staging")
[ -z "${PRELUDE_IMAGE_SOURCE:-}" ] || pull+=(--source "$PRELUDE_IMAGE_SOURCE")
pull+=("${PRELUDE_IMAGE:-ubuntu-2404-x64}")
# The directory is asked for before it is listed: a pull that failed before installing
# anything leaves none, and a find of a missing directory fails the pipeline, which under
# pipefail ended the boot on the first failed pull instead of retrying it (2026-11-09).
#
# A BUDGET, NOT A COUNT: a pull's download has no deadline of its own, so each attempt is
# given what is left of PRELUDE_BUDGET_SECONDS from the prelude's start, and none begins
# with under a minute left. The join's token lives two hours from the order, and the host
# needs time after this to enroll.
budget=${PRELUDE_BUDGET_SECONDS:-5400}
kernel=""
while [ $((budget - SECONDS)) -gt 60 ]; do
	timeout --kill-after=60 $((budget - SECONDS)) billet images pull --config "$work/preview/billet.yaml" \
		"${pull[@]:5}" >"$work/kernel-pull.log" 2>&1 || true
	if [ -d /var/lib/billet/kernels ]; then
		kernel=$(find /var/lib/billet/kernels -maxdepth 1 -type f -name 'vmlinux-*' -printf '%T@ %p\n' |
			sort -n | tail -1 | cut -d' ' -f2-)
	fi
	[ -z "$kernel" ] || break
	sleep 30
done
[ -n "$kernel" ] || {
	echo "prelude: no guest kernel was installed within the budget; see $work/kernel-pull.log" >&2
	exit 1
}
install -m 0644 "$kernel" /var/lib/billet/kernels/vmlinux

# THE IMAGE, ONCE THE NODE HAS REGISTERED: a transient unit waits for its acceptance
# record and pulls, retrying, then stops. A run again leaves one that is running alone,
# and starts another in place of one that failed or finished.
printf '%q ' "${pull[@]}" >"$work/image-pull.cmd"
case $(systemctl show -p ActiveState --value billet-pool-image-pull.service) in
active | activating | reloading) ;;
*)
	systemctl reset-failed billet-pool-image-pull.service >/dev/null 2>&1 || true
	systemd-run --unit billet-pool-image-pull --no-block --property=Type=oneshot /bin/bash -c \
		"until [ -s /var/lib/billet/node/image-acceptance.json ]; do sleep 10; done
		for _ in \$(seq 1 10); do $(cat "$work/image-pull.cmd") >>$work/image-pull.log 2>&1 && exit 0; sleep 60; done
		exit 1" >/dev/null
	;;
esac
echo "prelude: host prepared"
