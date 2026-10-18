#!/bin/bash
# Runs INSIDE a container whose PID 1 is real systemd. Driven by
# scripts/test-systemd-lifecycle.sh, which is where the setup is explained.
#
# EVERY ASSERTION IS ABOUT UNIT STATE, not about an exit code. `billet local
# down`'s whole job is deciding whether to stop a service, and a test that only
# checked the status it returned would pass against a version that stopped
# everything and complained afterwards.
set -u

BILLET=/usr/bin/billet
CFG=/etc/billet/billet.yaml
# A lease id no ledger will ever issue, so the container carrying it is exactly
# the class the compute barrier exists for: the ledger is quiet and the host has compute.
STRAY_LEASE=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaff
STRAY="billet-${STRAY_LEASE}"
fail=0

say() { printf '\n=== %s ===\n' "$1"; }
ok() { printf 'ok    %s\n' "$1"; }
bad() {
    printf 'FAIL  %s\n' "$1"
    fail=$((fail + 1))
}

active() { systemctl is-active "$1" 2>/dev/null; }

say "the service manager"
systemctl --version | head -1

say "installing the package"
# APT FAILS FAST OR NOT AT ALL. On 2026-09-05 Ubuntu's archive was mid-sync and a
# fresh container's apt-get update sat silent for eighteen minutes inside a
# thirty-minute job, then was killed with nothing to read; the run that did finish
# reported "File has unexpected size ... Mirror sync in progress?" after ten. A
# gate that can only fail by being killed is not a gate: every fetch is bounded,
# the transient half is retried, and the whole call is capped so the failure
# arrives with its reason while the job still has time to print it (-v names the
# signal and the command it went to, -k kills a dpkg that shrugs off TERM). And an index
# apt-get update could not fetch is only a WARNING to it, exit 0, measured against
# a black-holed proxy; so every update is followed by a check that at least one
# InRelease landed in /var/lib/apt/lists, which is the failure it is, here rather
# than two commands later as "openssl has no installation candidate". Not
# Error-Mode=any: with TWO sources on amd64 (archive.ubuntu.com, then a kernel.org
# mirror; apt reads them as two repositories and takes a package from whichever
# lists it; arm64's ports archive has no second mirror there and keeps one, HTTPS)
# a strict update fails while EITHER is down, measured on a fleet guest 2026-09-11,
# which is the outage this shape exists to survive.
APT="timeout -v -k 10 300 apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::CAInfo=/usr/local/share/billet-host-ca.crt"
if ! ${APT} install -y -qq /tmp/billet.deb >/dev/null 2>&1; then
    ${APT} install -y /tmp/billet.deb 2>&1 | tail -20
    echo "the package would not install" >&2
    exit 1
fi

# The postinstall makes the account and the directories; the units arrive with
# the package, which is the only way `billet local up` will accept them.
install -m 0600 -o billet -g billet /tmp/app-key.pem /etc/billet/app-private-key.pem
install -m 0640 -o root -g billet /tmp/billet.yaml "$CFG"
systemctl daemon-reload

dpkg -l billet | tail -1
ls -l /usr/lib/systemd/system/billet-server.service \
    /usr/lib/systemd/system/billet-node.service 2>/dev/null

# THE HOST LOCK DIRECTORY IS THE PACKAGE'S TMPFILES ENTRY'S, made by postinstall
# where systemd runs, and no unit's: a node unit naming it in ReadWritePaths
# refuses to start without it (#175).
lockdir=$(stat -c '%U:%G %a' /run/billet/locks 2>&1)
[ "$lockdir" = "root:root 750" ] && ok "postinstall made /run/billet/locks root:root 0750" ||
    bad "/run/billet/locks after the install: $lockdir"

say "billet local up"
"$BILLET" local up --config "$CFG" 2>&1 | tail -8
[ "${PIPESTATUS[0]}" -eq 0 ] && ok "local up exited 0" || bad "local up did not exit 0"

for u in billet-server.service billet-node.service; do
    printf '%-26s %s  enabled=%s\n' "$u" "$(active "$u")" \
        "$(systemctl is-enabled "$u" 2>&1)"
    [ "$(active "$u")" = active ] && ok "$u is active" || bad "$u is not active"
done

say "billet local status"
"$BILLET" local status --config "$CFG" 2>&1 | tail -8

say "a stray whose lease the ledger never issued"
identity=$(cat /var/lib/billet/server/deployment-id)
echo "deployment identity: $identity"

docker run -d --name "$STRAY" --label "sh.billet.owner=$identity" \
    alpine:3 sleep 3600 2>&1 | tail -2

state=$(docker inspect -f '{{.State.Status}}' "$STRAY" 2>/dev/null)
echo "stray container state: ${state:-<none>}"
# RUNNING, not merely present: `provider.List` reports exited containers too, so
# a stray that had already finished would satisfy everything below while proving
# nothing about compute that is still executing.
[ "$state" = running ] && ok "the stray is RUNNING" ||
    bad "the stray is not running, so nothing below proves anything"

say "local down must refuse, and stop nothing"
server_before=$(active billet-server.service)
node_before=$(active billet-node.service)
echo "before: server=$server_before node=$node_before"

down_out=$("$BILLET" local down --timeout 40s \
    --reason "systemd lifecycle: stray compute" --config "$CFG" 2>&1)
down_status=$?
echo "$down_out" | tail -12

[ "$down_status" -ne 0 ] && ok "local down refused (exit $down_status)" ||
    bad "local down reported success while a host was running compute"

# THE REASON MATTERS. An identity refusal, or a host that simply never answered,
# would also be non-zero and would also stop nothing — and would say nothing
# about the barrier. This is what separates the test from its own good luck.
if echo "$down_out" | grep -q "SAYS IT IS RUNNING WORK"; then
    ok "it refused because the host said it is running work"
else
    bad "it refused for another reason; the barrier is not what stopped it"
fi

if [ "$server_before" != active ] || [ "$node_before" != active ]; then
    bad "a unit was not active beforehand, so 'still active' proves nothing"
else
    [ "$(active billet-server.service)" = active ] &&
        ok "the server unit was NOT stopped" || bad "the server unit was stopped"
    [ "$(active billet-node.service)" = active ] &&
        ok "the node unit was NOT stopped" || bad "the node unit was stopped"
fi

say "remove the stray, then local down must stop both units"
docker rm -f "$STRAY" >/dev/null 2>&1
docker inspect "$STRAY" >/dev/null 2>&1 &&
    bad "the stray was not removed" || echo "stray removed"

# Generous, because the absence grace is five minutes of real time and this is
# the one place billet waits it out rather than steering a clock.
down_out=$("$BILLET" local down --timeout 12m \
    --reason "systemd lifecycle: clean stop" --config "$CFG" 2>&1)
down_status=$?
echo "$down_out" | tail -14

[ "$down_status" -eq 0 ] && ok "local down exited 0 once the fleet was proved idle" ||
    bad "local down did not succeed on a proved-idle fleet (exit $down_status)"

for u in billet-node.service billet-server.service; do
    st=$(active "$u")
    printf '%-26s %s\n' "$u" "$st"
    [ "$st" = active ] && bad "$u is still active after a successful down" ||
        ok "$u was stopped"
done

# THE NODE'S STOP LEAVES THE LOCK DIRECTORY AND WHAT IS IN IT (#175). The node
# took its deployment lock there at its start; a unit that declared the
# directory a RuntimeDirectory removed it at this stop, under any lock another
# process held in it.
[ -f "/run/billet/locks/deployment-$identity.lock" ] &&
    ok "the node's stop left /run/billet/locks and its deployment lock in place" ||
    bad "/run/billet/locks/deployment-$identity.lock is gone after the node's stop"

# THE NODE STOPS BEFORE THE SERVER. A control plane that outlives its node has
# nothing left to talk to, and stopping the server first strands the node's
# renewals — so the order is part of what the command promises.
node_line=$(echo "$down_out" | grep -n "billet-node.service" | head -1 | cut -d: -f1)
srv_line=$(echo "$down_out" | grep -n "billet-server.service" | head -1 | cut -d: -f1)
if [ -n "$node_line" ] && [ -n "$srv_line" ] && [ "$node_line" -lt "$srv_line" ]; then
    ok "the node was stopped before the server"
else
    bad "the stop order is not node-then-server (node@$node_line server@$srv_line)"
fi

# A DOWN'S NODE STOP IS PROVED BY THE NODE'S OWN ENDING (#127): the record the
# node keeps says the process the down stopped drained.
"$BILLET" node ending show --config "$CFG" --json | grep -q '"ending": "drained"' &&
    ok "the down's node stop is recorded drained" ||
    bad "the node's ending record does not say the down's stop drained"

say "a node killed at its stop is not started over until acknowledged"
"$BILLET" local up --config "$CFG" 2>&1 | tail -4
[ "${PIPESTATUS[0]}" -eq 0 ] && ok "local up started the node over a drained ending" ||
    bad "local up did not start the node over a drained ending"

invocation=$(systemctl show -p InvocationID --value billet-node.service)
echo "node invocation: ${invocation:-<none>}"

# KILLED, AND KEPT DOWN: SIGKILL ends the process without its ending, and the
# stop that follows cancels the restart Restart=on-failure would queue.
systemctl kill --kill-whom=main --signal=SIGKILL billet-node.service
systemctl stop billet-node.service
st=$(active billet-node.service)
[ "$st" != active ] && ok "the killed node is down ($st)" || bad "the killed node is still active"

up_out=$("$BILLET" local up --config "$CFG" 2>&1)
up_status=$?
echo "$up_out" | tail -6
[ "$up_status" -ne 0 ] && [ "$(active billet-node.service)" != active ] &&
    ok "local up refused to start the node over the killed process (exit $up_status)" ||
    bad "local up started the node over a process killed without its ending"
echo "$up_out" | grep -qF "acknowledge --invocation ${invocation}" &&
    ok "the refusal names the acknowledgement" ||
    bad "the refusal does not name acknowledge --invocation ${invocation}"

"$BILLET" node ending acknowledge --config "$CFG" --invocation "$invocation" --json >/dev/null &&
    ok "the killed stop was acknowledged" || bad "the acknowledgement was refused"

"$BILLET" local up --config "$CFG" 2>&1 | tail -4
[ "${PIPESTATUS[0]}" -eq 0 ] && [ "$(active billet-node.service)" = active ] &&
    ok "local up started the node over the acknowledged stop" ||
    bad "local up did not start the node over the acknowledged stop"

down_out=$("$BILLET" local down --timeout 12m 2>&1)
down_status=$?
echo "$down_out" | tail -6
[ "$down_status" -eq 0 ] && ok "local down drained and stopped the node again" ||
    bad "local down did not succeed after the acknowledged start (exit $down_status)"

say "result"
if [ "$fail" -eq 0 ]; then
    echo "ALL ASSERTIONS PASSED"
else
    echo "$fail ASSERTION(S) FAILED"
fi

exit "$fail"
