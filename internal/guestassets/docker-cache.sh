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

	type=$(blkid -o value -s TYPE "$device" 2>/dev/null || true)
	if [ -z "$type" ]; then
		if ! mkfs.ext4 -q -F "$device"; then
			log "the Docker image store could not be formatted; pulling cold"
			discard_attached "$slot"
			start_cold

			return $?
		fi
	elif [ "$type" != ext4 ]; then
		log "the Docker image store contains $type rather than ext4; pulling cold"
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
	if ! systemctl start docker.service; then
		log "Docker did not start on its image store; discarding the clone"
		umount /var/lib/docker >/dev/null 2>&1 || true
		discard_attached "$slot"
		start_cold

		return $?
	fi

	mkdir -p "$state_dir"
	printf '%s\n' "$slot" >"$state_dir/slot"
	printf '%s\n' "$device" >"$state_dir/device"
	if docker image ls --no-trunc --digests \
		--format '{{.Repository}} {{.Digest}} {{.ID}}' \
		>"$state_dir/images-before.unsorted" 2>/dev/null; then
		sort "$state_dir/images-before.unsorted" >"$state_dir/images-before"
		rm -f "$state_dir/images-before.unsorted"
	else
		rm -f "$state_dir/images-before" "$state_dir/images-before.unsorted"
		log "the initial Docker image inventory failed; this job's clone will not publish"
	fi

	return 0
}

complete() {
	status=${1:-}
	[ -r "$state_dir/slot" ] && [ -r "$state_dir/device" ] || return 0
	slot=$(sed -n '1p' "$state_dir/slot")
	device=$(sed -n '1p' "$state_dir/device")

	after_ok=0
	if docker image ls --no-trunc --digests \
		--format '{{.Repository}} {{.Digest}} {{.ID}}' \
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
		operation=commit
	fi

	if [ "$operation" = commit ]; then
		type=$(blkid -o value -s TYPE "$device" 2>/dev/null || true)
		uuid=$(blkid -o value -s UUID "$device" 2>/dev/null || true)
		clean=false
		if e2fsck -f -n "$device" >/dev/null 2>&1; then
			clean=true
		fi
		if body=$(jq -nc --arg type "$type" --arg uuid "$uuid" --argjson clean "$clean" \
			'{filesystem:{type:$type,uuid:$uuid,clean:$clean}}') &&
			response=$(cache_call "/v1/volumes/$slot/commit" "$body" 780) &&
			printf '%s' "$response" | jq -e '.published == true' >/dev/null 2>&1; then
			log "published the changed Docker image store"
		else
			log "the Docker image store could not be published; the job result is unchanged"
		fi
	else
		discard_attached "$slot"
	fi
	rm -f "$state_dir/slot" "$state_dir/device" "$state_dir/images-before" \
		"$state_dir/images-before.unsorted" "$state_dir/images-after" \
		"$state_dir/images-after.unsorted"
	rmdir "$state_dir" 2>/dev/null || true

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
complete) complete "${2:-}" ;;
service-status) service_status "${2:-}" ;;
*)
	log "usage: $0 prepare | complete RUNNER_STATUS | service-status RUNNER_STATUS"
	exit 2
	;;
esac
