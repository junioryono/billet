#!/bin/sh

state_dir=${BILLET_DOCKER_CACHE_STATE_DIR:-/run/billet-docker-cache}

log() {
	printf 'billet docker cache: %s\n' "$*" >&2
}

cache_call() {
	path=$1
	body=${2:-}
	timeout=${3:-120}
	if [ -n "$body" ]; then
		curl -fsS --connect-timeout 3 --max-time "$timeout" -X POST \
			-H "Authorization: Bearer $BILLET_CACHE_TOKEN" \
			-H "Content-Type: application/json" --data "$body" \
			"$BILLET_CACHE_ENDPOINT$path"
	else
		curl -fsS --connect-timeout 3 --max-time "$timeout" -X POST \
			-H "Authorization: Bearer $BILLET_CACHE_TOKEN" \
			"$BILLET_CACHE_ENDPOINT$path"
	fi
}

discard_attached() {
	slot=$1
	cache_call "/v1/volumes/$slot/discard" "" 120 >/dev/null 2>&1 ||
		log "the unusable image-store clone could not be discarded; node cleanup will retry"
}

start_cold() {
	if ! systemctl start docker.service; then
		log "Docker did not start"

		return 1
	fi

	return 0
}

clear_state() {
	rm -f "$state_dir/slot" "$state_dir/device" "$state_dir/images-before" \
		"$state_dir/images-before.unsorted" "$state_dir/images-after" \
		"$state_dir/images-after.unsorted"
	rmdir "$state_dir" 2>/dev/null || true
}

prepare_filesystem() {
	device=$1
	type=""
	type=$(blkid -o value -s TYPE "$device" 2>/dev/null)
	status=$?
	if [ "$status" -eq 0 ]; then
		if [ "$type" = ext4 ]; then
			return 0
		fi
		log "the Docker image store contains ${type:-an unknown signature} rather than ext4"

		return 1
	fi
	if [ "$status" -ne 2 ]; then
		log "the Docker image-store signature could not be read; refusing to format the device"

		return 1
	fi
	if ! mkfs.ext4 -q -F "$device"; then
		log "the Docker image store could not be formatted"

		return 1
	fi

	return 0
}

record_inventory() {
	if docker image ls --no-trunc --digests \
		--format '{{.Repository}} {{.Tag}} {{.Digest}} {{.ID}}' \
		>"$state_dir/images-before.unsorted" 2>/dev/null; then
		sort "$state_dir/images-before.unsorted" >"$state_dir/images-before"
		rm -f "$state_dir/images-before.unsorted"
	else
		rm -f "$state_dir/images-before" "$state_dir/images-before.unsorted"
		log "the initial Docker image inventory failed; this job's clone will not publish"
	fi
}

activate_store() {
	slot=$1
	device=$2
	mkdir -p "$state_dir"
	printf '%s\n' "$slot" >"$state_dir/slot"
	printf '%s\n' "$device" >"$state_dir/device"
	if systemctl start docker.service; then
		record_inventory

		return 0
	fi

	log "Docker did not start on its image store"
	if umount /var/lib/docker; then
		discard_attached "$slot"
		clear_state

		start_cold
		return $?
	fi

	log "the image store is still mounted; retaining custody and retrying Docker without publication"
	start_cold
}

prepare() {
	umask 077
	if [ -z "${BILLET_CACHE_ENDPOINT:-}" ] || [ -z "${BILLET_CACHE_TOKEN:-}" ]; then
		start_cold

		return $?
	fi

	case "$(uname -m 2>/dev/null || true)" in
	x86_64) architecture=amd64 ;;
	aarch64 | arm64) architecture=arm64 ;;
	*)
		log "this guest architecture has no Docker image-store namespace; pulling cold"
		start_cold

		return $?
		;;
	esac

	request=$(printf '{"architecture":"%s"}' "$architecture")
	if ! response=$(cache_call /v1/docker-store "$request" 780 2>/dev/null); then
		log "the Docker image store is unavailable; pulling cold"
		start_cold

		return $?
	fi
	if ! slot=$(printf '%s' "$response" | jq -er '.slot | select(type == "number" and . == 0)' 2>/dev/null) ||
		! device=$(printf '%s' "$response" | jq -er '.device | select(type == "string" and length > 0)' 2>/dev/null); then
		log "the Docker image-store response was invalid; pulling cold"
		start_cold

		return $?
	fi

	ready=0
	i=0
	while [ "$i" -lt 100 ]; do
		if [ -b "$device" ]; then
			ready=1

			break
		fi
		i=$((i + 1))
		sleep 1
	done
	if [ "$ready" -ne 1 ]; then
		log "$device did not appear; pulling cold"
		discard_attached "$slot"
		start_cold

		return $?
	fi

	if ! systemctl stop docker.service docker.socket; then
		log "Docker could not be stopped before mounting its image store; pulling cold"
		discard_attached "$slot"
		start_cold

		return $?
	fi

	if ! prepare_filesystem "$device"; then
		discard_attached "$slot"
		start_cold

		return $?
	fi

	mkdir -p /var/lib/docker
	if ! mount -t ext4 -o noatime "$device" /var/lib/docker; then
		log "the Docker image store could not be mounted; pulling cold"
		discard_attached "$slot"
		start_cold

		return $?
	fi
	activate_store "$slot" "$device"
}

complete() {
	status=${1:-}
	[ -r "$state_dir/slot" ] && [ -r "$state_dir/device" ] || return 0
	slot=$(sed -n '1p' "$state_dir/slot")
	device=$(sed -n '1p' "$state_dir/device")

	after_ok=0
	if docker image ls --no-trunc --digests \
		--format '{{.Repository}} {{.Tag}} {{.Digest}} {{.ID}}' \
		>"$state_dir/images-after.unsorted" 2>/dev/null; then
		sort "$state_dir/images-after.unsorted" >"$state_dir/images-after"
		rm -f "$state_dir/images-after.unsorted"
		after_ok=1
	else
		rm -f "$state_dir/images-after" "$state_dir/images-after.unsorted"
	fi
	if ! systemctl stop docker.service docker.socket; then
		log "Docker did not stop cleanly; node cleanup will discard its cache writes"

		return 0
	fi
	sync -f /var/lib/docker >/dev/null 2>&1 || true
	if ! umount /var/lib/docker; then
		log "the Docker image store could not be unmounted; node cleanup will discard its writes"

		return 0
	fi

	operation=discard
	if [ "$status" = 100 ] && [ "$after_ok" -eq 1 ] && [ -r "$state_dir/images-before" ] &&
		! cmp -s "$state_dir/images-before" "$state_dir/images-after"; then
		operation=ready
	fi

	if [ "$operation" = ready ]; then
		type=$(blkid -o value -s TYPE "$device" 2>/dev/null || true)
		uuid=$(blkid -o value -s UUID "$device" 2>/dev/null || true)
		clean=false
		if e2fsck -f -n "$device" >/dev/null 2>&1; then
			clean=true
		fi
		prepared=0
		if body=$(jq -nc --arg type "$type" --arg uuid "$uuid" --argjson clean "$clean" \
			'{filesystem:{type:$type,uuid:$uuid,clean:$clean}}'); then
			i=0
			while [ "$i" -lt 100 ]; do
				if response=$(cache_call "/v1/docker-store/ready" "$body" 2 2>/dev/null) &&
					printf '%s' "$response" | jq -e '.ready == true' >/dev/null 2>&1; then
					prepared=1

					break
				fi
				i=$((i + 1))
				sleep 1
			done
		fi
		if [ "$prepared" -eq 1 ]; then
			log "prepared the changed Docker image store for host-side publication"
		else
			log "the Docker image store could not be prepared; the job result is unchanged"
			discard_attached "$slot"
		fi
	else
		discard_attached "$slot"
	fi
	clear_state

	return 0
}

service_status() {
	case "${1:-}" in
	100 | 101 | 102 | 103 | 104 | 105) printf '0\n' ;;
	*[!0-9]* | '') printf '1\n' ;;
	*) printf '%s\n' "$1" ;;
	esac
}

case "${1:-}" in
prepare) prepare ;;
prepare-filesystem) prepare_filesystem "${2:-}" ;;
activate-store) activate_store "${2:-}" "${3:-}" ;;
complete) complete "${2:-}" ;;
service-status) service_status "${2:-}" ;;
*)
	log "usage: $0 prepare | complete RUNNER_STATUS | service-status RUNNER_STATUS"
	exit 2
	;;
esac
