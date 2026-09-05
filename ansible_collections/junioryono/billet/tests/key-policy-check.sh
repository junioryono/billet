#!/bin/sh
# The role's App-key policy, as converges that must each end the right way.
#
# WHY THIS IS A FILE AND NOT A NOTE IN A COMMIT MESSAGE. The policy has two
# halves and the other gates only exercise one: emitted-block-check and
# example-check both carry the owned path, so `billet_github_key_managed` is
# true for both and the foreign-path branch never runs. Delete that branch
# entirely and every other suite stays green — which is the shape of a fix with
# no test, however carefully it was measured by hand once.
#
# NOT COVERED, and named here rather than left to be discovered: where the
# install actually lands. Every case runs in check mode, so nothing is written
# and there is no file to stat, and a src-based copy returns an empty diff in
# check mode — measured — so the destination is not observable from the result
# either. Retargeting `dest` in account.yml would keep every gate green and fail
# in production, inside the upgrade transaction, when `billet check` cannot find
# the key where the config names it. Closing it needs a real converge on a
# disposable host, which is what tests/host-upgrade-live.sh is for.
#
# The policy:
#   owned path   — a source or an already-installed file is required
#   foreign path — a source is REFUSED, and the file must already be there
# and the point of both is that a config billet would reject when it LOADS is
# rejected here, before normal convergence changes anything.
#
# THE SAME POLICY FOR A FURTHER TARGET (billet_config.targets), whose owned path
# is /etc/billet/app-private-key-<name>.pem and whose source is
# billet_github_target_key_srcs.<name>. The target cases keep the github block
# on its owned path with a key supplied, so the only thing that can refuse is
# the target's own rule.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
repo_root=$collections_root

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# The emission measures THIS machine, so it only runs on linux; elsewhere the
# gate is skipped rather than faked. Ahead of the build: skipping must not need
# a Go toolchain.
if [ "$(uname -s)" != "Linux" ]; then
    echo "key-policy-check: skipped on $(uname -s) — the emission is linux-only"
    exit 0
fi

billet=${BILLET_EMITTED_BLOCK_BINARY:-}
if [ -z "$billet" ]; then
    echo "Building billet to generate the block..."
    (cd "$repo_root" && go build -o "$work/billet" ./cmd/billet)
    billet=$work/billet
fi

noop=""
for candidate in /usr/bin/true /bin/true; do
    if [ -f "$candidate" ]; then noop=$candidate; break; fi
done
if [ -z "$noop" ]; then
    echo "key-policy-check: no no-op binary found for the role" >&2
    exit 1
fi

owned=/etc/billet/app-private-key.pem
foreign=$work/operator/key.pem

# One real block, generated once, with an identity that is not zero so the role
# reaches the key policy rather than stopping short of it.
cat >"$work/app.yaml" <<EOF
github:
  org: key-policy-check
  app_id: 4722347
  installation_id: 156647704
  private_key_path: $owned
EOF
"$billet" init --emit ansible \
    --config "$work/app.yaml" \
    --org key-policy-check \
    --runner-group key-policy-check \
    --workflow 'acme/repo/.github/workflows/ci.yml@refs/heads/main' \
    >"$work/base.yml" 2>"$work/emit.err" || {
    echo "key-policy-check: the emission failed" >&2; cat "$work/emit.err" >&2; exit 1
}

key=$work/supplied.pem
: >"$key"
chmod 600 "$key"

cat >"$work/site.yml" <<'EOF'
- name: key policy
  hosts: all
  become: true
  gather_facts: true
  pre_tasks:
    # THE VALUE ANSIBLE ACTUALLY LOADED, not the text of the file. A duplicate
    # key, a quoted value, a comment or a change of style can all satisfy a
    # textual check while billet_config carries something else — and then the
    # case exercises a branch it was not written for and still reports ok.
    - name: Prove the fixture put the requested key path into the effective config
      ansible.builtin.assert:
        that:
          - billet_config.github.private_key_path == expected_key_path
        fail_msg: >-
          The fixture did not take: billet_config.github.private_key_path is
          {{ billet_config.github.private_key_path | default('undefined') }},
          not {{ expected_key_path }}. This case would exercise the wrong half
          of the key policy.
  roles:
    - role: junioryono.billet.host
      billet_enable_server: false
      billet_enable_node: false
  post_tasks:
    # A RECAP ROW IS NOT PROOF THE ROLE RAN. `ok=1` can be Gathering Facts and
    # nothing else, so every case expecting success would pass with the role
    # removed from this play. These facts exist only because the preflight
    # under test computed them.
    - name: Prove the role reached the key preflight
      ansible.builtin.assert:
        that:
          - billet_github_key_required is defined
          - billet_github_key_managed is defined
          - billet_github_key_path is defined
          - billet_github_target_keys is defined
        fail_msg: >-
          The role did not reach its GitHub App key preflight, so a converge
          that succeeded here proves nothing about the policy.
EOF

# expect: the word "pass", or a fragment the refusal must contain.
run_case() {
    name=$1; path=$2; supply=$3; expect=$4

    # An oracle with a sloppy contract blesses the wrong things: an empty expect
    # matches any failure, and a supply that is neither yes nor no silently
    # means no.
    case $supply in
        yes|no) ;;
        *) echo "key-policy-check: $name: supply must be yes or no, got '$supply'" >&2; exit 1 ;;
    esac
    if [ -z "$expect" ]; then
        echo "key-policy-check: $name: an empty expectation matches any failure" >&2
        exit 1
    fi

    rm -rf "$work/gv"; mkdir -p "$work/gv"
    # The KEY's line, matched at its start and rewritten whole, keeping the
    # indentation. A substitution over the file would treat the owned path's
    # dots as wildcards and could take more than one line.
    awk -v p="$path" '
        /^[[:space:]]*private_key_path:/ && !done {
            match($0, /^[[:space:]]*/)
            printf "%sprivate_key_path: %s\n", substr($0, 1, RLENGTH), p
            done = 1
            seen++
            next
        }
        /^[[:space:]]*private_key_path:/ { seen++; next }
        { print }
        END { if (seen != 1) { printf "seen=%d\n", seen+0 > "/dev/stderr"; exit 3 } }
    ' "$work/base.yml" >"$work/gv/all.yml" || {
        echo "key-policy-check: $name: the block does not carry exactly one" \
             "private_key_path line, so the fixture cannot be built from it" >&2
        exit 1
    }

    # Fixed-string counts, both directions: a substitution that silently matched
    # nothing would otherwise leave every case exercising the owned path and
    # reporting five passes.
    if [ "$(grep -Fc -- "private_key_path: $path" "$work/gv/all.yml")" != 1 ]; then
        echo "key-policy-check: $name: the fixture does not name $path exactly once" >&2
        exit 1
    fi
    if [ "$path" != "$owned" ] && grep -Fq -- "private_key_path: $owned" "$work/gv/all.yml"; then
        echo "key-policy-check: $name: the fixture still names $owned, so this case" \
             "would exercise the managed path it was written to avoid" >&2
        exit 1
    fi

    set -- --check -i localhost, --connection=local \
        -e "@$work/gv/all.yml" -e "billet_binary_src=$noop" \
        -e "expected_key_path=$path"
    if [ "$supply" = yes ]; then
        set -- "$@" -e "billet_github_private_key_src=$key"
    fi

    status=0
    ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        ansible-playbook "$@" "$work/site.yml" >"$work/out.log" 2>&1 || status=$?

    if [ "$expect" = pass ]; then
        if [ "$status" -ne 0 ]; then
            echo "FAIL $name: expected the converge to succeed"; grep -A 20 'fatal:' "$work/out.log" >&2; exit 1
        fi
        # A play that matched no host exits 0 having converged nothing.
        if ! sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^[^ ]+ +: +ok=[1-9]'; then
            echo "FAIL $name: the play converged no host" >&2; exit 1
        fi
    else
        if [ "$status" -eq 0 ]; then
            echo "FAIL $name: expected a refusal, the converge succeeded" >&2; exit 1
        fi
        # A task FAILED, rather than ansible-playbook failing to start, an
        # unparsable playbook, or a missing interpreter — any of which exit
        # non-zero and would otherwise count as the refusal under test.
        # A looped assert prints "failed: [host] (item=...)" per item and no
        # "FAILED!" summary, so both spellings of a task failing are accepted.
        if ! grep -q 'FAILED!\|^failed: \[' "$work/out.log"; then
            echo "FAIL $name: the run failed without any task failing" >&2
            grep -n -A 25 'FAILED!\|fatal:\|ERROR!' "$work/out.log" | head -80 >&2
            tail -30 "$work/out.log" >&2; exit 1
        fi
        if ! sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^[^ ]+ +: +ok=[0-9]+ +changed=0 '; then
            echo "FAIL $name: refused, but the recap reports a change — an invalid" \
                 "config must be rejected before convergence mutates the host" >&2
            sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1
        fi
        if ! grep -Fq -- "$expect" "$work/out.log"; then
            echo "FAIL $name: refused, but not for the expected reason ($expect)" >&2
            grep -A 20 'fatal:' "$work/out.log" >&2; exit 1
        fi
    fi
    echo "ok   $name"
}

# Phrases unique to one refusal each. "installs a key only to" is in both, so
# matching on it would let the missing-file refusal satisfy the source case.
absent_says="no readable file is there"
source_says="would leave a secret on the host that billet never reads"
owned_says="no key is there and none was supplied"

# run_play runs the fixture at $work/gv/all.yml with the extra -e arguments
# given, and judges the result the one way both case shapes share.
run_play() {
    name=$1; expect=$2; shift 2

    set -- --check -i localhost, --connection=local \
        -e "@$work/gv/all.yml" -e "billet_binary_src=$noop" "$@"

    status=0
    ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        ansible-playbook "$@" "$work/site.yml" >"$work/out.log" 2>&1 || status=$?

    if [ "$expect" = pass ]; then
        if [ "$status" -ne 0 ]; then
            echo "FAIL $name: expected the converge to succeed"; grep -A 20 'fatal:' "$work/out.log" >&2; exit 1
        fi
        if ! sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^[^ ]+ +: +ok=[1-9]'; then
            echo "FAIL $name: the play converged no host" >&2; exit 1
        fi
    else
        if [ "$status" -eq 0 ]; then
            echo "FAIL $name: expected a refusal, the converge succeeded" >&2; exit 1
        fi
        # A looped assert prints "failed: [host] (item=...)" per item and no
        # "FAILED!" summary, so both spellings of a task failing are accepted.
        if ! grep -q 'FAILED!\|^failed: \[' "$work/out.log"; then
            echo "FAIL $name: the run failed without any task failing" >&2
            grep -n -A 25 'FAILED!\|fatal:\|ERROR!' "$work/out.log" | head -80 >&2
            tail -30 "$work/out.log" >&2; exit 1
        fi
        if ! sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^[^ ]+ +: +ok=[0-9]+ +changed=0 '; then
            echo "FAIL $name: refused, but the recap reports a change" >&2
            sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1
        fi
        if ! grep -Fq -- "$expect" "$work/out.log"; then
            echo "FAIL $name: refused, but not for the expected reason ($expect)" >&2
            grep -A 20 'fatal:' "$work/out.log" >&2; exit 1
        fi
    fi
    echo "ok   $name"
}

# run_target_case is run_case for a FURTHER target named personal: the github
# block stays on its owned path with its key supplied, and a targets entry is
# appended to the emitted block naming the given key path for the target.
run_target_case() {
    name=$1; path=$2; supply=$3; expect=$4

    case $supply in
        yes|no) ;;
        *) echo "key-policy-check: $name: supply must be yes or no, got '$supply'" >&2; exit 1 ;;
    esac
    if [ -z "$expect" ]; then
        echo "key-policy-check: $name: an empty expectation matches any failure" >&2
        exit 1
    fi

    rm -rf "$work/gv"; mkdir -p "$work/gv"
    cp "$work/base.yml" "$work/gv/all.yml"

    # The emission ends with billet_config, so a two-space-indented key appended
    # to it is a key of the config. Proved below by the count rather than assumed.
    cat >>"$work/gv/all.yml" <<EOF
  targets:
    - name: personal
      repository: someone/widgets
      app_id: 4722348
      installation_id: 156647705
      private_key_path: $path
EOF
    if [ "$(grep -Fc -- "private_key_path: $path" "$work/gv/all.yml")" != 1 ]; then
        echo "key-policy-check: $name: the fixture does not name $path exactly once" >&2
        exit 1
    fi
    if [ "$(grep -Fc -- "private_key_path: $owned" "$work/gv/all.yml")" != 1 ]; then
        echo "key-policy-check: $name: the fixture lost the github block's owned path" >&2
        exit 1
    fi

    set -- -e "expected_key_path=$owned" -e "billet_github_private_key_src=$key"
    if [ "$supply" = yes ]; then
        set -- "$@" -e "{\"billet_github_target_key_srcs\": {\"personal\": \"$key\"}}"
    fi

    run_play "$name" "$expect" "$@"
}

target_owned=/etc/billet/app-private-key-personal.pem
target_foreign=$work/operator/personal-key.pem
target_absent_says="no readable file is there for target personal"
target_source_says="would leave a secret on the host that billet never reads"
target_owned_says="none was supplied for target personal"

rm -rf "$work/operator"; mkdir -p "$work/operator"
run_case "owned path, key supplied"          "$owned"   yes pass

rm -rf "$work/operator"; mkdir -p "$work/operator"
run_case "foreign path, absent"              "$foreign" no  "$absent_says"

rm -rf "$work/operator"; mkdir -p "$work/operator"; : >"$foreign"
run_case "foreign path, regular file"        "$foreign" no  pass

rm -rf "$work/operator"; mkdir -p "$work/operator"; : >"$foreign"
run_case "foreign path, file plus a source"  "$foreign" yes "$source_says"

# A key managed as a symlink is ordinary; billet opens the path and follows it.
rm -rf "$work/operator"; mkdir -p "$work/operator"
: >"$work/operator/real.pem"; ln -s "$work/operator/real.pem" "$foreign"
run_case "foreign path, symlink to a file"   "$foreign" no  pass

# EXISTS IS NOT ENOUGH, and without these two the isreg check and follow: true
# can both be removed with every case above still green. A directory satisfies
# exists; a dangling link satisfies exists only when the link itself is stat'd.
rm -rf "$work/operator"; mkdir -p "$work/operator/key.pem"
run_case "foreign path, a directory"         "$foreign" no  "$absent_says"

rm -rf "$work/operator"; mkdir -p "$work/operator"
ln -s "$work/operator/gone.pem" "$foreign"
run_case "foreign path, dangling symlink"    "$foreign" no  "$absent_says"

# THE OWNED PATH WITH NOTHING BEHIND IT. Every case above supplies a source for
# the owned path or uses a foreign one, so the clause requiring a source or an
# installed file at the owned path was not covered by any of them.
if [ -e "$owned" ]; then
    echo "skip owned path, absent — $owned exists on this machine, and this gate" \
         "will not remove an operator's key to test that it is missing"
else
    run_case "owned path, absent, no source"  "$owned"   no  "$owned_says"
fi

echo "key-policy-check: every case behaved as the policy requires"

# THE FURTHER TARGET'S OWN CASES. Each runs with the github block satisfied, so
# a refusal is the target rule's and nothing else's.
rm -rf "$work/operator"; mkdir -p "$work/operator"
run_target_case "target owned path, key supplied"        "$target_owned"   yes pass

rm -rf "$work/operator"; mkdir -p "$work/operator"
run_target_case "target owned path, nothing supplied"    "$target_owned"   no  "$target_owned_says"

rm -rf "$work/operator"; mkdir -p "$work/operator"
run_target_case "target foreign path, absent"            "$target_foreign" no  "$target_absent_says"

rm -rf "$work/operator"; mkdir -p "$work/operator"; : >"$target_foreign"
run_target_case "target foreign path, regular file"      "$target_foreign" no  pass

rm -rf "$work/operator"; mkdir -p "$work/operator"; : >"$target_foreign"
run_target_case "target foreign path, source supplied"   "$target_foreign" yes "$target_source_says"
