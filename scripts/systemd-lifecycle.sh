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
# a black-holed proxy; Error-Mode=any makes that the failure it is, here rather
# than two commands later as "openssl has no installation candidate".
APT="timeout -v -k 10 300 apt-get -o APT::Update::Error-Mode=any -o Acquire::Retries=3 -o Acquire::http::Timeout=30"
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

say "result"
if [ "$fail" -eq 0 ]; then
    echo "ALL ASSERTIONS PASSED"
else
    echo "$fail ASSERTION(S) FAILED"
fi

exit "$fail"
