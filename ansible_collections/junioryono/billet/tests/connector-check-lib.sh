# Shared by cloudflared-connector-check.sh and warp-connector-check.sh: the
# fakes both roles are driven against, and the judge both apply to a run.
#
# EVERY FAKE RECORDS ITS ARGV to $BILLET_FAKE_CALLS, one line per invocation
# prefixed by its own name, and judges nothing: the scripts read the record and
# decide. The gpg fake reads the file it is handed and fails when the file is
# absent, so a role that "verified" a key check mode never fetched would fail
# here rather than pass on a fingerprint printed from thin air; which
# fingerprint it prints depends on the file's content, so a wrong key is a
# wrong key and not an environment variable the role could have read.
#
# The systemctl fake answers `show` with a loaded, inactive, disabled unit and
# exits 0 for everything else, which is what the systemd_service module needs
# to decide to start, enable or restart; the calls it makes are the record.

connector_fakes() {
    bin=$1
    mkdir -p "$bin"

    cat >"$bin/cloudflared" <<'EOF'
#!/bin/sh
printf 'cloudflared %s\n' "$*" >>"$BILLET_FAKE_CALLS"
exit 0
EOF

    cat >"$bin/warp-cli" <<'EOF'
#!/bin/sh
printf 'warp-cli %s\n' "$*" >>"$BILLET_FAKE_CALLS"
case " $* " in
    *" status "*)
        if [ "${BILLET_FAKE_WARP_STATUS_RC:-0}" != 0 ]; then
            # A daemon that fails still prints a registration line, so a role
            # that read stdout without the status would enrol on an error.
            echo "Status update: Registration Missing"
            echo "Error communicating with daemon" >&2
            exit "$BILLET_FAKE_WARP_STATUS_RC"
        fi
        # After an enrolment in this run, the daemon reports Connected. The
        # keyword is one word because the checks pass settings as words; a
        # `silent` daemon exits 0 with no status line at all.
        if [ -f "$BILLET_FAKE_STATE/warp-enrolled" ] || [ "${BILLET_FAKE_WARP_STATUS:-missing}" = connected ]; then
            echo "Status update: Connected"
        elif [ "${BILLET_FAKE_WARP_STATUS:-missing}" = silent ]; then
            :
        else
            echo "Status update: Registration Missing"
        fi
        ;;
    *" connector new "*)
        : >"$BILLET_FAKE_STATE/warp-enrolled"
        ;;
esac
exit 0
EOF

    cat >"$bin/gpg" <<'EOF'
#!/bin/sh
printf 'gpg %s\n' "$*" >>"$BILLET_FAKE_CALLS"
file=""
for arg in "$@"; do file=$arg; done
if [ ! -f "$file" ]; then
    echo "fake gpg: no such file: $file" >&2
    exit 2
fi
# --dearmor writes the output it was asked for, so a gate that asserts the
# keyring is absent after a refusal is asserting something a role that
# dearmored before verifying would violate.
out=""; prev=""
for arg in "$@"; do
    [ "$prev" = -o ] && out=$arg
    prev=$arg
done
case " $* " in
    *" --dearmor "*) [ -n "$out" ] && printf 'DEARMORED %s' "$(cat "$file")" >"$out"; exit 0 ;;
esac
# The fingerprint follows the FILE, never the environment: a good key is the
# fixture's bytes and anything else is a different key.
if [ "$(cat "$file")" = "GOOD CLOUDFLARE KEY" ]; then
    printf 'pub:-:4096:1:8A682D308D4E5E73:1700000000::::::scESC::::::23::0:\nfpr:::::::::%s:\n' "$BILLET_FAKE_GOOD_FPR"
else
    printf 'pub:-:4096:1:0000000000000000:1700000000::::::scESC::::::23::0:\nfpr:::::::::DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF:\n'
fi
exit 0
EOF

    # THE JOURNAL ANSWERS FOR THE INVOCATION ASKED ABOUT: the registration line
    # of the invocation systemd currently runs, an OLD registration line for a
    # window read that named no invocation (which a role that fell back to a
    # window would accept), and nothing for any other invocation.
    cat >"$bin/journalctl" <<'EOF'
#!/bin/sh
printf 'journalctl %s\n' "$*" >>"$BILLET_FAKE_CALLS"
current=""
[ -f "$BILLET_FAKE_STATE/unit-active" ] && current=$(cat "$BILLET_FAKE_STATE/unit-active")
asked=""
for arg in "$@"; do
    case "$arg" in _SYSTEMD_INVOCATION_ID=*) asked=${arg#_SYSTEMD_INVOCATION_ID=} ;; esac
done
if [ -z "$asked" ]; then
    echo "Sep 01 00:00:00 host cloudflared[1]: INF Registered tunnel connection connIndex=0"
elif [ -n "$current" ] && [ "$asked" = "$current" ]; then
    echo "Sep 06 00:00:00 host cloudflared[1]: INF Registered tunnel connection connIndex=0"
fi
exit 0
EOF

    cat >"$bin/systemctl" <<'EOF'
#!/bin/sh
printf 'systemctl %s\n' "$*" >>"$BILLET_FAKE_CALLS"
# STATEFUL, across the run: a started or restarted unit is active afterwards
# and an enabled one is enabled, or a second converge would start it again
# and no second run could ever be changed=0.
case " $* " in
    *" start "*|*" restart "*) printf 'inv%s\n' "$(date +%s%N 2>/dev/null || date +%s)$$" >"$BILLET_FAKE_STATE/unit-active" ;;
    *" enable "*) : >"$BILLET_FAKE_STATE/unit-enabled" ;;
    *" stop "*) rm -f "$BILLET_FAKE_STATE/unit-active" ;;
    *" disable "*) rm -f "$BILLET_FAKE_STATE/unit-enabled" ;;
esac
active=inactive; sub=dead; enabled=disabled
[ -f "$BILLET_FAKE_STATE/unit-active" ] && { active=active; sub=running; }
[ -f "$BILLET_FAKE_STATE/unit-enabled" ] && enabled=enabled
# A unit in a transitional state, for the guard's reading of it.
[ -n "${BILLET_FAKE_ACTIVE_STATE:-}" ] && { active=$BILLET_FAKE_ACTIVE_STATE; sub=start-post; }
# `show -p NAME --value` answers one property, the way the role asks for the
# ActiveState and the InvocationID; a bare `show` is what the systemd_service
# module asks and gets the whole set. The invocation id changes on every start
# or restart, so a wait keyed on it can be told from a wait on an old window.
prop=""; want_value=false; prev=""
for a in "$@"; do
    case "$prev" in -p|--property) prop=$a ;; esac
    [ "$a" = --value ] && want_value=true
    prev=$a
done
case " $* " in
    *" show "*)
        if [ "$want_value" = true ]; then
            case "$prop" in
                ActiveState) echo "$active" ;;
                InvocationID) [ -f "$BILLET_FAKE_STATE/unit-active" ] && cat "$BILLET_FAKE_STATE/unit-active" ;;
                *) echo "" ;;
            esac
        else
            # ExecStart from the unit on disk, as systemd reports it: the module
            # returns the whole status, and an adopted unit's ExecStart is where
            # an inline token would be.
            exec_start=""
            unit_file="${BILLET_TEST_ROOT:-/nonexistent}/etc/systemd/system/cloudflared.service"
            [ -f "$unit_file" ] && exec_start=$(sed -n 's/^ExecStart=//p' "$unit_file" | head -n1)
            printf 'Id=cloudflared.service\nLoadState=loaded\nActiveState=%s\nSubState=%s\nUnitFileState=%s\nFragmentPath=/etc/systemd/system/cloudflared.service\nExecStart={ path=%s ; argv[]=%s }\n' "$active" "$sub" "$enabled" "${exec_start%% *}" "$exec_start"
        fi
        ;;
    *" is-enabled "*)
        echo "$enabled"
        [ "$enabled" = enabled ] || exit 1
        ;;
    *" is-active "*)
        echo "$active"
        [ "$active" = active ] || exit 3
        ;;
    *" --version "*)
        echo "systemd 255 (255.4-1ubuntu8)"
        ;;
esac
exit 0
EOF

    chmod +x "$bin"/*
}

# judge <name> <status> <log> <expect> [staged]
#
# expect is the word "pass" or a fragment the refusal must contain; the
# fragment is from a message, never a task name (Ansible prints every task's
# name whether or not it fails). A refusal must come from a task and leave the
# recap at changed=0, except a refusal the caller marks "staged": the signing
# key is fetched into a private staging directory BEFORE it is verified, and
# that fetch is a change; what the refusal must precede there is the trusted
# path, which the caller proves by the keyring's absence.
#
# BILLET_TEST_FAILS names the hosts the refusal must come FROM, each with its
# own failed=1 and changed=0 row: a run_once assertion that failed on one host
# ends the play for the others without their ever having asserted anything,
# and a judge that only asked "did the run fail" could not tell the two apart.
judge() {
    name=$1; status=$2; log=$3; expect=$4; staged=${5:-}

    if [ "$expect" = pass ]; then
        if [ "$status" -ne 0 ]; then
            echo "FAIL $name: expected the converge to succeed" >&2; grep -A 20 'fatal:' "$log" >&2; exit 1
        fi
        if ! sed -n '/PLAY RECAP/,$p' "$log" | grep -qE '^[^ ]+ +: +ok=[1-9]'; then
            echo "FAIL $name: the play converged no host" >&2; exit 1
        fi
    else
        if [ "$status" -eq 0 ]; then
            echo "FAIL $name: expected a refusal, the converge succeeded" >&2; exit 1
        fi
        if ! grep -q 'FAILED!\|^failed: \[' "$log"; then
            echo "FAIL $name: the run failed without any task failing" >&2; tail -30 "$log" >&2; exit 1
        fi
        # EVERY host's row, not any one: a refusal on one host beside a write on
        # another is a refusal that did not precede every write.
        rows=$(sed -n '/PLAY RECAP/,$p' "$log" | grep -cE '^[^ ]+ +: +ok=[0-9]+ +changed=[0-9]+ ')
        if [ "$rows" -lt 1 ]; then
            echo "FAIL $name: the recap names no host" >&2; tail -30 "$log" >&2; exit 1
        fi
        if [ "$staged" != staged ] && sed -n '/PLAY RECAP/,$p' "$log" | grep -qE '^[^ ]+ +: +ok=[0-9]+ +changed=[1-9]'; then
            echo "FAIL $name: refused, but the recap reports a change; a refusal must precede every write" >&2
            sed -n '/PLAY RECAP/,$p' "$log" >&2; exit 1
        fi
        if ! grep -Fq -- "$expect" "$log"; then
            echo "FAIL $name: refused, but not for the expected reason ($expect)" >&2; grep -A 20 'fatal:' "$log" >&2; exit 1
        fi
        for h in ${BILLET_TEST_FAILS:-}; do
            if ! sed -n '/PLAY RECAP/,$p' "$log" | grep -qE "^$h +: +ok=[0-9]+ +changed=0 .*failed=1"; then
                echo "FAIL $name: $h must be a host that refused, unchanged, and its row says otherwise" >&2
                sed -n '/PLAY RECAP/,$p' "$log" >&2; exit 1
            fi
        done
    fi
    echo "ok   $name"
}

recap_changed_of() {
    sed -n '/PLAY RECAP/,$p' "$1" | sed -nE 's/^[^ ]+ +: +ok=[0-9]+ +changed=([0-9]+).*/\1/p' | head -n1
}
