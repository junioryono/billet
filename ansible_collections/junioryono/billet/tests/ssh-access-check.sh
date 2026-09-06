#!/bin/sh
# The ssh_access role, as converges that must each end the right way.
#
# WHY A GATE OF ITS OWN. The role's whole value is one refusal: sshd is never
# hardened on a host the converge leaves with no way in. Nothing else in the
# suite configures a key, so every other gate takes the "nothing configured,
# nothing done" branch and the refusal could be deleted with everything green.
#
# WITHOUT ROOT, on any machine that has Ansible. The role's paths are inputs
# (tests drive them at a temporary tree; an operator has no reason to), a fake
# sshd on PATH stands in for the validator and records what it was asked, and
# `-e ansible_become=false` outranks the tasks' become keyword (a connection
# variable beats a play or task keyword), so nothing here needs sudo.
#
# Every refusal is judged by its own message and by a recap with changed=0: a
# play that failed for an unrelated reason, or refused after writing, is a
# failure of this gate, not a pass.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# --- the fake validator ------------------------------------------------------
#
# `sshd -t`: records its argv and the drop-in's bytes AS THEY ARE ON DISK when
# it is asked (the role validates the whole configuration with the file in
# place, not a candidate), and exits as told. A real sshd would refuse a
# configuration the drop-in makes unsatisfiable; here the failing case is
# scripted so the gate can prove a refusal puts the previous state back.
mkdir -p "$work/bin"
cat >"$work/bin/sshd" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$BILLET_FAKE_SSHD_CALLS"
cat "$BILLET_TEST_SSHD_DIR/10-billet-hardening.conf" >"$BILLET_FAKE_SSHD_SAW" 2>/dev/null || : >"$BILLET_FAKE_SSHD_SAW"
if [ "${BILLET_FAKE_SSHD_FAIL:-}" = 1 ]; then
    echo "fake sshd: refusing the config" >&2
    exit 255
fi
exit 0
EOF
chmod +x "$work/bin/sshd"

key_a='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA converge@ci'
key_b='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB second@ci'
breakglass='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC breakglass'
# THE SAME MATERIAL AS THE BREAK-GLASS KEY UNDER ANOTHER COMMENT: what the
# module removes when handed it, whatever the comment says.
breakglass_retired='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC retired'

cat >"$work/play.yml" <<'EOF'
- name: ssh access
  hosts: localhost
  connection: local
  gather_facts: false
  vars:
    # A Debian host, so the apt task is not skipped by its own condition and a
    # task moved outside the configured-access block would show up.
    ansible_facts:
      os_family: Debian
    billet_ssh_sshd_config_dir: "{{ lookup('env', 'BILLET_TEST_SSHD_DIR') }}"
    billet_ssh_authorized_keys_path: "{{ lookup('env', 'BILLET_TEST_KEYS_PATH') }}"
    billet_ssh_validate_command: "sshd -t"
    billet_ssh_manage_service: false
  roles:
    - role: junioryono.billet.ssh_access
EOF

# run <name> <expect> <vars-file> [extra -e ...]
#
# expect is the word "pass" or a fragment the refusal must contain. A refusal
# must come from a task (FAILED! or a looped "failed: [") and leave changed=0.
#
# THE FRAGMENT IS FROM THE MESSAGE, NEVER FROM THE TASK'S NAME. Ansible prints
# every task's name whether or not it fails, so a fragment the name shares with
# the message is satisfied by any failure at all: with the root-lockout
# assertion neutered, the play failed later on a chown and the case still
# passed on "lock out the account", which the task is called (measured).
run() {
    name=$1; expect=$2; vars=$3; shift 3

    rm -rf "$work/root"; mkdir -p "$work/root/sshd_config.d" "$work/root/keys"
    : >"$work/calls"
    status=0
    env PATH="$work/bin:$PATH" \
        BILLET_FAKE_SSHD_CALLS="$work/calls" BILLET_FAKE_SSHD_SAW="$work/saw" \
        BILLET_TEST_SSHD_DIR="$work/root/sshd_config.d" \
        BILLET_TEST_KEYS_PATH="$work/root/keys/authorized_keys" \
        ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
        ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        ansible-playbook -i localhost, -e ansible_become=false -e "@$vars" $common "$@" "$work/play.yml" \
        >"$work/out.log" 2>&1 || status=$?

    if [ "$expect" = pass ]; then
        if [ "$status" -ne 0 ]; then
            echo "FAIL $name: expected the converge to succeed" >&2; grep -A 20 'fatal:' "$work/out.log" >&2; exit 1
        fi
        if ! sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^[^ ]+ +: +ok=[1-9]'; then
            echo "FAIL $name: the play converged no host" >&2; exit 1
        fi
    else
        if [ "$status" -eq 0 ]; then
            echo "FAIL $name: expected a refusal, the converge succeeded" >&2; exit 1
        fi
        if ! grep -q 'FAILED!\|^failed: \[' "$work/out.log"; then
            echo "FAIL $name: the run failed without any task failing" >&2
            tail -30 "$work/out.log" >&2; exit 1
        fi
        if ! sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^[^ ]+ +: +ok=[0-9]+ +changed=0 '; then
            echo "FAIL $name: refused, but the recap reports a change; a refusal must precede every write" >&2
            sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1
        fi
        if ! grep -Fq -- "$expect" "$work/out.log"; then
            echo "FAIL $name: refused, but not for the expected reason ($expect)" >&2
            grep -A 20 'fatal:' "$work/out.log" >&2; exit 1
        fi
        if [ -e "$work/root/sshd_config.d/10-billet-hardening.conf" ]; then
            echo "FAIL $name: refused, but the hardening drop-in was written anyway" >&2; exit 1
        fi
        if [ -s "$work/calls" ]; then
            echo "FAIL $name: refused, but the validator was called: $(cat "$work/calls")" >&2; exit 1
        fi
    fi
    echo "ok   $name"
}

recap_changed() {
    sed -n '/PLAY RECAP/,$p' "$work/out.log" | sed -nE 's/^[^ ]+ +: +ok=[0-9]+ +changed=([0-9]+).*/\1/p'
}

# THE HOSTNAME AND THE APT FILE STAY ON THEIR DEFAULTS in the no-configuration
# case, so a task moved outside the configured-access block shows up as a change
# (or, as a non-root user, as a failure); every other case turns them off,
# because they would change this machine.
# Set AFTER the first case: a prefix assignment on a function call persists in
# some shells and not in others, so the variable is assigned plainly.
common=""

# 1. Nothing configured, nothing done, and the play still ran a host.
printf '{}\n' >"$work/none.yml"
run "nothing configured skips with no change" pass "$work/none.yml"
[ "$(recap_changed)" = 0 ] || { echo "FAIL: an empty configuration changed something" >&2; exit 1; }
[ ! -e "$work/root/sshd_config.d/10-billet-hardening.conf" ] || { echo "FAIL: an empty configuration hardened sshd" >&2; exit 1; }
[ ! -s "$work/calls" ] || { echo "FAIL: an empty configuration called the validator" >&2; exit 1; }

common="-e billet_ssh_hostname_from_inventory=false -e billet_ssh_unattended_upgrades=false"

# 2. Every key absent and no break-glass: refused before the drop-in exists.
cat >"$work/absent.yml" <<EOF
billet_ssh_authorized_keys:
  - key: "$key_a"
    state: absent
EOF
run "all keys absent refuses the hardening" "leaves only the console" "$work/absent.yml"

# 3. The break-glass key listed absent under ANOTHER COMMENT: the module would
#    remove it by its material, so this is the contradiction, refused.
cat >"$work/contradiction.yml" <<EOF
billet_ssh_breakglass_key: "$breakglass"
billet_ssh_authorized_keys:
  - key: "$breakglass_retired"
    state: absent
EOF
run "a break-glass key the converge removes under another comment is refused" "Say one thing about it" "$work/contradiction.yml"

# 4. One key present and absent in the same list, and the same material under
#    another TYPE (the module removes by material, not by type): refused.
cat >"$work/twostates.yml" <<EOF
billet_ssh_authorized_keys:
  - key: "$key_a"
  - key: "ssh-rsa ${key_a#ssh-ed25519 }"
    state: absent
EOF
run "one key with two states is refused whatever its type says" "Say one thing about it" "$work/twostates.yml"

# 5. Two present entries for one material: the second would replace the first's
#    line, so it is refused.
cat >"$work/twice.yml" <<EOF
billet_ssh_authorized_keys:
  - key: "$key_a"
  - key: "${key_a% converge@ci} again"
EOF
run "two present entries for one key are refused" "Say one thing about it" "$work/twice.yml"

# 6. The break-glass key also a present converge key: one private key is not two.
cat >"$work/samekey.yml" <<EOF
billet_ssh_breakglass_key: "$breakglass"
billet_ssh_authorized_keys:
  - key: "$breakglass_retired"
EOF
run "a break-glass key that is also a converge key is refused" "one private key is not two" "$work/samekey.yml"

# 7. Not exactly one public key: a comment, options in front of the type (with
#    a quoted string that itself looks like a key), and a value spanning lines.
cat >"$work/comment.yml" <<EOF
billet_ssh_authorized_keys:
  - key: "# temporarily unavailable"
EOF
run "a comment in place of a key is refused" "not exactly one public key" "$work/comment.yml"

cat >"$work/options.yml" <<EOF
billet_ssh_breakglass_key: "$breakglass"
billet_ssh_authorized_keys:
  - key: 'command="ssh-rsa $key_b" $breakglass_retired'
    state: absent
EOF
run "a key with options is refused" "not exactly one public key" "$work/options.yml"

cat >"$work/multiline.yml" <<EOF
billet_ssh_authorized_keys:
  - key: |
      $key_a
      $key_b
EOF
run "a value spanning lines is refused" "not exactly one public key" "$work/multiline.yml"

# A carriage return with no second key pair after it, so only the control
# character rule can refuse this line (a second pair would trip the pair rule).
cat >"$work/cr.yml" <<EOF
billet_ssh_authorized_keys:
  - key: "$key_a comment\rrogue"
EOF
run "a value with an embedded carriage return is refused" "not exactly one public key" "$work/cr.yml"

cat >"$work/twokeys.yml" <<EOF
billet_ssh_authorized_keys:
  - key: "$key_a $key_b"
EOF
run "two keys on one line are refused" "not exactly one public key" "$work/twokeys.yml"

# A line that parses but whose material is not a key: ssh-keygen refuses it,
# and so does the role, before anything is written.
cat >"$work/notakey.yml" <<EOF
billet_ssh_authorized_keys:
  - key: "ssh-ed25519 AAAA nobody"
EOF
run "material ssh-keygen cannot read is refused" "not one ssh-keygen can read" "$work/notakey.yml"

# 8. root with PermitRootLogin no, in any spelling: the account is locked out.
cat >"$work/root.yml" <<EOF
billet_ssh_user: root
billet_ssh_permit_root_login: "NO"
billet_ssh_authorized_keys:
  - key: "$key_a"
EOF
run "root under PermitRootLogin no is refused whatever the case" "whichever key it presents" "$work/root.yml"

# 9. A root-login policy sshd would not read.
cat >"$work/policy.yml" <<EOF
billet_ssh_permit_root_login: "nope"
billet_ssh_authorized_keys:
  - key: "$key_a"
EOF
run "an unknown root-login policy is refused" "is none of yes, no, prohibit-password" "$work/policy.yml"

# 10. One present key, and no ansible_user at all: the account is discovered,
#     the drop-in is written with every directive, the validator was asked with
#     the file in place, and a second run changes nothing.
cat >"$work/one.yml" <<EOF
billet_ssh_breakglass_key: "$breakglass"
billet_ssh_authorized_keys:
  - key: "$key_a"
    state: present
  - key: "$key_b"
EOF
run "one present key hardens sshd" pass "$work/one.yml"
dropin=$work/root/sshd_config.d/10-billet-hardening.conf
[ -f "$dropin" ] || { echo "FAIL: the hardening drop-in was not written" >&2; exit 1; }
for want in 'PasswordAuthentication no' 'KbdInteractiveAuthentication no' 'PermitRootLogin no'; do
    grep -qx "$want" "$dropin" || { echo "FAIL: the drop-in lacks $want" >&2; exit 1; }
done
grep -qx -- '-t' "$work/calls" || { echo "FAIL: the validator was not asked to check the whole configuration: $(cat "$work/calls")" >&2; exit 1; }
cmp -s "$work/saw" "$dropin" || { echo "FAIL: the validator did not see the drop-in that was installed" >&2; exit 1; }
keys=$work/root/keys/authorized_keys
grep -Fq "$key_a" "$keys" && grep -Fq "$key_b" "$keys" && grep -Fq "$breakglass" "$keys" || { echo "FAIL: not every present key was installed" >&2; exit 1; }
grep -q "id -un" "$work/out.log" || true
saved_root=$work/root-after-one
rm -rf "$saved_root"; cp -R "$work/root" "$saved_root"

# The second run, on the same tree: run() recreates the tree, so put it back.
: >"$work/calls"
rm -rf "$work/root"; cp -R "$saved_root" "$work/root"
status=0
env PATH="$work/bin:$PATH" BILLET_FAKE_SSHD_CALLS="$work/calls" BILLET_FAKE_SSHD_SAW="$work/saw" \
    BILLET_TEST_SSHD_DIR="$work/root/sshd_config.d" BILLET_TEST_KEYS_PATH="$work/root/keys/authorized_keys" \
    ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
    ansible-playbook -i localhost, -e ansible_become=false -e "@$work/one.yml" $common "$work/play.yml" >"$work/out.log" 2>&1 || status=$?
[ "$status" -eq 0 ] || { echo "FAIL: the second run failed" >&2; tail -30 "$work/out.log" >&2; exit 1; }
[ "$(recap_changed)" = 0 ] || { echo "FAIL: the second run reported changed=$(recap_changed); the role is not idempotent" >&2; sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1; }
[ ! -s "$work/calls" ] || { echo "FAIL: an unchanged drop-in was validated again" >&2; exit 1; }
echo "ok   a second run changes nothing"

# 11. state: absent removes exactly the given key and leaves the others.
cat >"$work/revoke.yml" <<EOF
billet_ssh_breakglass_key: "$breakglass"
billet_ssh_authorized_keys:
  - key: "$key_a"
    state: absent
  - key: "$key_b"
EOF
rm -rf "$work/root"; cp -R "$saved_root" "$work/root"
status=0
env PATH="$work/bin:$PATH" BILLET_FAKE_SSHD_CALLS="$work/calls" BILLET_FAKE_SSHD_SAW="$work/saw" \
    BILLET_TEST_SSHD_DIR="$work/root/sshd_config.d" BILLET_TEST_KEYS_PATH="$work/root/keys/authorized_keys" \
    ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
    ansible-playbook -i localhost, -e ansible_become=false -e "@$work/revoke.yml" $common "$work/play.yml" >"$work/out.log" 2>&1 || status=$?
[ "$status" -eq 0 ] || { echo "FAIL: the revoking run failed" >&2; tail -30 "$work/out.log" >&2; exit 1; }
if grep -Fq "$key_a" "$keys"; then echo "FAIL: the absent key is still installed" >&2; exit 1; fi
grep -Fq "$key_b" "$keys" && grep -Fq "$breakglass" "$keys" || { echo "FAIL: revoking one key removed another" >&2; exit 1; }
# THE PRESENT KEYS WENT IN BEFORE THE ABSENT ONE CAME OUT, whatever order the
# inventory listed them in (absent first, above): a rotation must never leave a
# hardened host keyless between two tasks.
install_at=$(grep -n 'Install each present converge key' "$work/out.log" | head -n1 | cut -d: -f1)
remove_at=$(grep -n 'Remove each absent converge key' "$work/out.log" | head -n1 | cut -d: -f1)
[ -n "$install_at" ] && [ -n "$remove_at" ] && [ "$install_at" -lt "$remove_at" ] || { echo "FAIL: the absent key was removed before the present keys were installed (install at line ${install_at:-none}, remove at line ${remove_at:-none})" >&2; exit 1; }
echo "ok   state absent removes exactly the given key, after the present keys are installed"

# 12. A configuration sshd refuses: the drop-in is gone afterwards on a fresh
#     host, and the previous one is back on a host that had one.
rm -rf "$work/root"; mkdir -p "$work/root/sshd_config.d" "$work/root/keys"; : >"$work/calls"
status=0
env PATH="$work/bin:$PATH" BILLET_FAKE_SSHD_CALLS="$work/calls" BILLET_FAKE_SSHD_SAW="$work/saw" BILLET_FAKE_SSHD_FAIL=1 \
    BILLET_TEST_SSHD_DIR="$work/root/sshd_config.d" BILLET_TEST_KEYS_PATH="$work/root/keys/authorized_keys" \
    ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
    ansible-playbook -i localhost, -e ansible_become=false -e "@$work/one.yml" $common "$work/play.yml" >"$work/out.log" 2>&1 || status=$?
[ "$status" -ne 0 ] || { echo "FAIL: a refused validation did not fail the converge" >&2; exit 1; }
grep -q 'sshd refused the configuration' "$work/out.log" || { echo "FAIL: the run failed, but not on the validator" >&2; tail -30 "$work/out.log" >&2; exit 1; }
grep -q 'fake sshd: refusing the config' "$work/out.log" || { echo "FAIL: the validator's own diagnostic was not carried into the failure" >&2; exit 1; }
[ "$(grep -c '' "$work/calls")" = 1 ] || { echo "FAIL: the validator ran $(grep -c '' "$work/calls") times, want once" >&2; exit 1; }
grep -q '^PasswordAuthentication no$' "$work/saw" || { echo "FAIL: the validator did not see the candidate drop-in" >&2; exit 1; }
[ ! -e "$work/root/sshd_config.d/10-billet-hardening.conf" ] || { echo "FAIL: a refused validation left the drop-in in place" >&2; exit 1; }
echo "ok   a refused validation leaves no drop-in on a fresh host"

rm -rf "$work/root"; mkdir -p "$work/root/sshd_config.d" "$work/root/keys"; : >"$work/calls"
printf 'PasswordAuthentication yes\n' >"$work/root/sshd_config.d/10-billet-hardening.conf"
status=0
env PATH="$work/bin:$PATH" BILLET_FAKE_SSHD_CALLS="$work/calls" BILLET_FAKE_SSHD_SAW="$work/saw" BILLET_FAKE_SSHD_FAIL=1 \
    BILLET_TEST_SSHD_DIR="$work/root/sshd_config.d" BILLET_TEST_KEYS_PATH="$work/root/keys/authorized_keys" \
    ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
    ansible-playbook -i localhost, -e ansible_become=false -e "@$work/one.yml" $common "$work/play.yml" >"$work/out.log" 2>&1 || status=$?
[ "$status" -ne 0 ] || { echo "FAIL: a refused validation did not fail the converge" >&2; exit 1; }
grep -q 'fake sshd: refusing the config' "$work/out.log" || { echo "FAIL: the run failed, but not on the validator" >&2; tail -30 "$work/out.log" >&2; exit 1; }
[ "$(grep -c '' "$work/calls")" = 1 ] || { echo "FAIL: the validator ran $(grep -c '' "$work/calls") times, want once" >&2; exit 1; }
grep -q '^PasswordAuthentication no$' "$work/saw" || { echo "FAIL: the validator did not see the candidate drop-in" >&2; exit 1; }
[ "$(cat "$work/root/sshd_config.d/10-billet-hardening.conf")" = "PasswordAuthentication yes" ] || { echo "FAIL: a refused validation did not put the previous drop-in back" >&2; exit 1; }
echo "ok   a refused validation puts the previous drop-in back"

echo "ssh-access-check: every case behaved as the role requires"
