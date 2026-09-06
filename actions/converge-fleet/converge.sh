#!/usr/bin/env bash
# Converge the fleet: the hosts the playbook will touch are computed, reached and
# proved before ansible-playbook runs, and the run is `--check --diff` in check
# mode and the real thing followed by the idempotence proof otherwise.
#
# THE COLLECTION IS THE CHECKOUT THIS ACTION RUNS FROM. GitHub checks out the
# whole repository at the `uses:` ref, so ansible_collections/ two directories
# above this script is the collection at that same ref; ansible.posix is the
# one dependency install-ansible.sh fetched beside it.
#
# THE EXPECTED HOSTS COME FROM THE PLAYBOOK, NOT THE INVENTORY. An inventory may
# hold hosts no play targets, and --limit or a custom playbook may deliberately
# converge a subset; `ansible-playbook --list-hosts` with the same inventory,
# limit and extra variables answers exactly which hosts the run will touch, and
# an empty answer is refused in both modes, because a playbook that matched no
# host exits 0 having converged nothing.
#
# ansible_host IS RENDERED, NOT READ RAW. `ansible-inventory --list` prints
# templates unexpanded, and an inventory that spells a controller's address
# once and templates ansible_host from it is an ordinary shape; the debug
# module templates every host's connection variables without opening a
# connection.
#
# HOST KEYS ARE PINS. The known-hosts input is appended to the runner user's
# known_hosts line by line (never a truncation: a deploy runner's file is an
# operator's), host key checking is on, and an inventory that turns it off or
# points at another file through ansible_ssh_common_args, ansible_ssh_extra_args
# or ansible_ssh_args is refused, because those variables replace anything the
# action could set in the environment. ssh-keyscan is never run.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checkout=$(cd "$here/../.." && pwd)

mode=${BILLET_MODE:?}
inventory=${BILLET_INVENTORY:?}
playbook=${BILLET_PLAYBOOK:-junioryono.billet.fleet}
reach=${BILLET_REACH:-none}

[[ -f $inventory ]] || { echo "::error::inventory $inventory does not exist"; exit 1; }

export ANSIBLE_COLLECTIONS_PATH="$checkout:$RUNNER_TEMP/billet-collections"
export ANSIBLE_HOST_KEY_CHECKING=True
export ANSIBLE_STDOUT_CALLBACK=default
export ANSIBLE_RESULT_FORMAT=yaml
export ANSIBLE_FORCE_COLOR=0
export ANSIBLE_NOCOLOR=1

# --- the environment input: NAME=value lines into the environment ------------
#
# Into this process's environment, which ansible-playbook inherits, and never
# into its argv. A line that is not NAME=value is refused rather than exported
# as something else.
if [[ -n ${BILLET_ENVIRONMENT:-} ]]; then
  while IFS= read -r line; do
    [[ -z $line ]] && continue
    if [[ ! $line =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; then
      echo "::error::the environment input has a line that is not NAME=value (line starts with '${line%%=*}')"
      exit 1
    fi
    export "$line"
  done <<<"$BILLET_ENVIRONMENT"
fi

# --- the credentials, written 0600 from the environment ----------------------
#
# Created under umask 077 after removing any previous file, so the file exists
# only ever with mode 0600 and the value never appears in argv (a `printf` to a
# redirection is a builtin writing to a descriptor). BSD install refuses a pipe
# as its source, which is why this is not `install -m 0600 /dev/stdin`.
write_secret() {
  rm -f "$2"
  (umask 077 && printf '%s\n' "$1" >"$2")
}

if [[ -n ${BILLET_GITHUB_APP_PRIVATE_KEY:-} ]]; then
  write_secret "$BILLET_GITHUB_APP_PRIVATE_KEY" "$RUNNER_TEMP/billet-app-key.pem"
  export BILLET_GITHUB_PRIVATE_KEY_PATH="$RUNNER_TEMP/billet-app-key.pem"
fi
unset BILLET_GITHUB_APP_PRIVATE_KEY

if [[ -n ${BILLET_SSH_PRIVATE_KEY:-} ]]; then
  write_secret "$BILLET_SSH_PRIVATE_KEY" "$RUNNER_TEMP/billet-ssh-key"
  export ANSIBLE_PRIVATE_KEY_FILE="$RUNNER_TEMP/billet-ssh-key"
fi
unset BILLET_SSH_PRIVATE_KEY

# --- the host-key pins ----------------------------------------------------------
if [[ $reach == cloudflare-warp && -z ${BILLET_KNOWN_HOSTS:-} ]]; then
  echo "::error::reach cloudflare-warp needs known-hosts: a hosted runner holds no host-key pins, and the action never runs ssh-keyscan"
  exit 1
fi
if [[ -n ${BILLET_KNOWN_HOSTS:-} ]]; then
  [[ -f $BILLET_KNOWN_HOSTS ]] || { echo "::error::known-hosts $BILLET_KNOWN_HOSTS does not exist"; exit 1; }
  install -d -m 0700 "$HOME/.ssh"
  touch "$HOME/.ssh/known_hosts"
  chmod 0600 "$HOME/.ssh/known_hosts"
  added=0
  while IFS= read -r line; do
    [[ -z $line || $line == \#* ]] && continue
    if ! grep -qxF -- "$line" "$HOME/.ssh/known_hosts"; then
      printf '%s\n' "$line" >>"$HOME/.ssh/known_hosts"
      added=$((added + 1))
    fi
  done <"$BILLET_KNOWN_HOSTS"
  echo "host-key pins: $added line(s) added to $HOME/.ssh/known_hosts"
fi

# --- the common arguments ----------------------------------------------------------
args=(-i "$inventory")
if [[ -n ${BILLET_LIMIT:-} ]]; then
  args+=(--limit "$BILLET_LIMIT")
fi
if [[ -n ${BILLET_EXTRA_VARS:-} ]]; then
  if [[ ${BILLET_EXTRA_VARS:0:1} != "{" ]]; then
    echo "::error::extra-vars must be a JSON object (it is passed as -e and lands in argv, so it is for non-secret flags only)"
    exit 1
  fi
  args+=(-e "$BILLET_EXTRA_VARS")
fi

# --- the hosts this run will touch -----------------------------------------------------
listing=$(ansible-playbook "${args[@]}" --list-hosts "$playbook")
expected=$RUNNER_TEMP/billet-expected-hosts
# The listing indents each host by six spaces under "hosts (N):"; nothing else
# in it is indented that deep.
printf '%s\n' "$listing" | awk '/^      [^ ]/ { print $1 }' | sort -u >"$expected"
if [[ ! -s $expected ]]; then
  echo "::error::$playbook matches no host in $inventory${BILLET_LIMIT:+ under --limit $BILLET_LIMIT}. A playbook that matches no host exits 0 having converged nothing, so this is refused in both modes."
  printf '%s\n' "$listing"
  exit 1
fi
echo "hosts this run touches: $(paste -sd' ' "$expected")"

# --- every host's connection, rendered ----------------------------------------------------
#
# One debug call over the expected hosts, one line per host (-o), the values as
# one JSON document each; the module templates on the controller and opens no
# connection.
pattern=$(paste -sd: "$expected")
rendered=$(ansible "${args[@]}" -o -m debug \
  -a "msg={{ {'host': ansible_host | default(inventory_hostname), 'port': ansible_port | default(22), 'common': ansible_ssh_common_args | default(''), 'extra': ansible_ssh_extra_args | default(''), 'args': ansible_ssh_args | default('')} | to_json }}" \
  "$pattern")

reach_targets=$RUNNER_TEMP/billet-reach-targets
: >"$reach_targets"
while IFS= read -r line; do
  [[ -z $line ]] && continue
  host=${line%% *}
  json=${line#*=> }
  # The msg is a JSON string holding a JSON document: two decodes.
  fields=$(printf '%s' "$json" | python3 -c '
import json, sys
outer = json.loads(sys.stdin.read())
inner = json.loads(outer["msg"])
print(inner["host"], inner["port"], json.dumps(" ".join(str(inner[k]) for k in ("common", "extra", "args"))))
')
  addr=${fields%% *}
  rest=${fields#* }
  port=${rest%% *}
  sshargs=${rest#* }
  # THE PIN POLICY IS ENFORCED ON WHAT SSH WILL ACTUALLY SEE. An inventory may
  # add a ProxyCommand; it may not turn host-key checking off or point it at
  # another file, because that quietly discards the pins.
  if printf '%s' "$sshargs" | grep -Eq 'StrictHostKeyChecking=(no|off|accept-new)|StrictHostKeyChecking (no|off|accept-new)|UserKnownHostsFile|CheckHostIP=no|CheckHostIP no'; then
    echo "::error::$host sets SSH options that disable or redirect host-key checking ($sshargs); the pins the action installs would be discarded. Remove the option from ansible_ssh_common_args, ansible_ssh_extra_args or ansible_ssh_args."
    exit 1
  fi
  printf '%s %s %s\n' "$host" "$addr" "$port" >>"$reach_targets"
done <<<"$rendered"

# --- prove the path before trusting it ----------------------------------------------------
#
# A device that enrolled and is then denied by Gateway looks identical to a
# connected one until the first SSH times out, and that timeout surfaces as an
# Ansible "unreachable" that names nothing.
command -v nc >/dev/null || { echo "::error::nc is not on this runner; the reachability check cannot run"; exit 1; }
while read -r host addr port; do
  if ! nc -z -w 5 "$addr" "$port"; then
    echo "::error::No path to $host at $addr:$port. With reach cloudflare-warp, one of three things: the device did not enrol (the module's enrolment policy is not attached to the WARP enrolment application); the Gateway rule does not permit this destination for non_identity@ (Gateway's network log names the rule that matched); or the device profile's split tunnel does not include the destination. With reach none, the runner cannot reach the address."
    exit 1
  fi
  echo "reached $host at $addr:$port"
done <"$reach_targets"

# --- the run ---------------------------------------------------------------------------
if [[ $mode == check ]]; then
  ansible-playbook "${args[@]}" --check --diff "$playbook"
  exit 0
fi

log="$RUNNER_TEMP/billet-converge-1.log"
ansible-playbook "${args[@]}" "$playbook" 2>&1 | tee "$log"
if [[ ${BILLET_PROVE_IDEMPOTENT:-true} == true ]]; then
  "$here/prove-idempotent.sh" "$expected" -- "${args[@]}" "$playbook"
  log="$RUNNER_TEMP/billet-converge-2.log"
fi

{
  echo "recap<<BILLET_RECAP_EOF"
  sed -n '/PLAY RECAP/,$p' "$log" | grep -E '^[^ ]+ +: +ok=' || true
  echo "BILLET_RECAP_EOF"
} >>"$GITHUB_OUTPUT"
