#!/usr/bin/env bash
# handshake-probe.sh OUT_DIR SECONDS URL...
#
# Times a fresh connection to each URL once a second for SECONDS seconds, and one
# ping to PING_HOST (default 1.1.1.1; empty skips it), from inside a job, for
# #818: whether a guest's TLS handshakes stall while CI downloads, and whether the
# line was queueing when they did. Rows go to OUT_DIR/handshakes.csv and
# OUT_DIR/ping.csv as they are taken, so a job cut short keeps what it measured.
#
# A FAILED REQUEST IS A ROW, never the end of the run: its curl exit status is the
# point. Each URL is fetched with a new connection (one curl per request), so
# every row has its own DNS, connect and TLS times. curl's -w line is printed on
# failure too, with the phases it never reached at zero. remote_ip is where the
# name resolved, so an origin the guest's resolver remaps to the node shows.

set -u

if [ "$#" -lt 3 ]; then
	echo "usage: handshake-probe.sh OUT_DIR SECONDS URL..." >&2
	exit 2
fi

out=$1
seconds=$2
shift 2

case $seconds in
'' | *[!0-9]*)
	echo "handshake-probe.sh: SECONDS must be a whole number, not '$seconds'" >&2
	exit 2
	;;
esac

ping_host=${PING_HOST-1.1.1.1}

mkdir -p "$out" || exit 1
handshakes=$out/handshakes.csv
pings=$out/ping.csv

echo "utc,url,exit,http_code,remote_ip,dns_s,connect_s,tls_s,first_byte_s,total_s" >"$handshakes"
echo "utc,host,exit,rtt_ms" >"$pings"

format='%{http_code},%{remote_ip},%{time_namelookup},%{time_connect},%{time_appconnect},%{time_starttransfer},%{time_total}'
end=$(($(date +%s) + seconds))

while [ "$(date +%s)" -lt "$end" ]; do
	tick=$(date +%s)
	utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)

	for url in "$@"; do
		timing=$(curl -sS -o /dev/null --connect-timeout 10 --max-time 15 -w "$format" "$url" 2>/dev/null)
		status=$?
		echo "$utc,$url,$status,${timing:-000,,,,,,}" >>"$handshakes"
	done

	if [ -n "$ping_host" ]; then
		reply=$(ping -n -c 1 -W 2 "$ping_host" 2>/dev/null)
		status=$?
		rtt=$(printf '%s\n' "$reply" | sed -n 's/.*time=\([0-9.]*\).*/\1/p' | head -n 1)
		echo "$utc,$ping_host,$status,$rtt" >>"$pings"
	fi

	# ONCE A SECOND FROM THE TICK'S START, and at once after a tick that ran long.
	if [ "$(date +%s)" -eq "$tick" ]; then
		sleep 1
	fi
done
