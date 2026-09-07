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
# connection. Every expected host must render exactly once, or the run stops:
# a partial render is a partial probe. What the render reads is the inventory's
# connection variables, and a play that overrides ansible_host, ansible_port or
# the SSH arguments at play level is outside what this can see; the collection's
# fleet playbook sets none, and a consumer's own play must not either.
#
# HOST KEYS ARE PINS. The known-hosts input is appended to the runner user's
# known_hosts line by line (never a truncation: a deploy runner's file is an
# operator's), host key checking is on, and an inventory that turns it off or
# points at another file, through ansible_ssh_common_args, ansible_ssh_extra_args,
# ansible_ssh_args or ansible_host_key_checking, is refused, because those
# variables replace anything the action could set in the environment.
# ssh-keyscan is never run.
#
# THE ENVIRONMENT INPUT NEVER TOUCHES THIS SHELL AND NEVER AN ARGV. Its lines
# are written to a 0600 file and with-environment.py puts them into the child's
# environment as a dictionary before execvpe, so a line naming `mode` or
# `inventory` cannot rewrite a decision this script already made, no shell
# assignment evaluates a value (bash treats RANDOM's and SECONDS' as
# arithmetic), and no process ever carries a value as an argument. Names that
# would change how Ansible or the runner behaves (ANSIBLE_*, GITHUB_*,
# RUNNER_*, PATH, HOME and their kin) are refused by name. A malformed line is
# reported by its line number, never by its contents, because its contents may
# be a credential.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checkout=$(cd "$here/../.." && pwd)

# THE DECISIONS, FIXED BEFORE ANY INPUT IS READ. readonly, so nothing later in
# this file can reassign them, whatever an input says; under a prefix the
# environment input refuses by name, so no line of the input can even be spelled
# as one of them.
readonly billet_action_mode=${BILLET_MODE:?}
readonly billet_action_inventory=${BILLET_INVENTORY:?}
readonly billet_action_playbook=${BILLET_PLAYBOOK:-junioryono.billet.fleet}
readonly billet_action_reach=${BILLET_REACH:-none}
readonly billet_action_limit=${BILLET_LIMIT:-}
readonly billet_action_extra_vars=${BILLET_EXTRA_VARS:-}
readonly billet_action_prove=${BILLET_PROVE_IDEMPOTENT:-true}
readonly billet_action_known_hosts_input=${BILLET_KNOWN_HOSTS:-}
readonly billet_action_runner_temp=${RUNNER_TEMP:?}

[[ -f $billet_action_inventory ]] || { echo "::error::inventory $billet_action_inventory does not exist"; exit 1; }

export ANSIBLE_COLLECTIONS_PATH="$checkout:$billet_action_runner_temp/billet-collections"
export ANSIBLE_HOST_KEY_CHECKING=True
export ANSIBLE_STDOUT_CALLBACK=default
export ANSIBLE_RESULT_FORMAT=yaml
export ANSIBLE_FORCE_COLOR=0
export ANSIBLE_NOCOLOR=1

# --- the environment input: NAME=value lines for the child, and only the child ---
child_env_file="$billet_action_runner_temp/billet-child-env"
rm -f "$child_env_file"
(umask 077 && : >"$child_env_file")
if [[ -n ${BILLET_ENVIRONMENT:-} ]]; then
  n=0
  while IFS= read -r line || [[ -n $line ]]; do
    n=$((n + 1))
    line=${line%$'\r'}
    [[ -z $line ]] && continue
    # A CONTROL CHARACTER INSIDE A LINE IS REFUSED, a carriage return first of
    # all: the launcher reads the file without newline translation and refuses
    # the same bytes, so a value cannot become a second line anywhere.
    if [[ $line == *[[:cntrl:]]* ]]; then
      echo "::error::the environment input's line $n carries a control character; a value is one line of printable text"
      exit 1
    fi
    if [[ ! $line =~ ^([A-Za-z_][A-Za-z0-9_]*)= ]]; then
      echo "::error::the environment input's line $n is not NAME=value. Every line is one variable for ansible-playbook's environment; a value with newlines (a key) cannot be carried this way."
      exit 1
    fi
    name=${BASH_REMATCH[1]}
    lowered=$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')
    case "$name" in
      ANSIBLE_*|GITHUB_*|RUNNER_*|LD_*|PYTHON*|SSH_*|DISPLAY|PATH|HOME|SHELL|TMPDIR|IFS|ENV|BASH_ENV|CDPATH)
        echo "::error::the environment input's line $n sets $name, which changes how Ansible or this runner behaves rather than what a role reads; it is refused. Connection and host-key policy are the action's, and extra-vars is the input for a non-secret flag."
        exit 1
        ;;
    esac
    case "$lowered" in
      billet_action*)
        echo "::error::the environment input's line $n sets $name, which changes how Ansible or this runner behaves rather than what a role reads; it is refused. Connection and host-key policy are the action's, and extra-vars is the input for a non-secret flag."
        exit 1
        ;;
    esac
    printf '%s\n' "$line" >>"$child_env_file"
  done <<<"$BILLET_ENVIRONMENT"
fi
# The raw block goes: every line it carried is in child_env now, and a child
# does not need the whole input under one name (a test reading `env` would
# also mistake its continuation lines for variables of their own).
unset BILLET_ENVIRONMENT

# run_ansible runs an Ansible command with the environment input applied to
# that process alone, through with-environment.py. Every Ansible call goes
# through it, the listing and the render included, so what the render judges is
# what the play will see: an inventory that reads an option through
# lookup('env') renders with the same value.
run_ansible() {
  python3 "$here/with-environment.py" "$child_env_file" -- "$@"
}

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
  write_secret "$BILLET_GITHUB_APP_PRIVATE_KEY" "$billet_action_runner_temp/billet-app-key.pem"
  export BILLET_GITHUB_PRIVATE_KEY_PATH="$billet_action_runner_temp/billet-app-key.pem"
fi
unset BILLET_GITHUB_APP_PRIVATE_KEY

if [[ -n ${BILLET_SSH_PRIVATE_KEY:-} ]]; then
  write_secret "$BILLET_SSH_PRIVATE_KEY" "$billet_action_runner_temp/billet-ssh-key"
  export ANSIBLE_PRIVATE_KEY_FILE="$billet_action_runner_temp/billet-ssh-key"
fi
unset BILLET_SSH_PRIVATE_KEY

# --- the host-key pins ----------------------------------------------------------
if [[ $billet_action_reach == cloudflare-warp && -z $billet_action_known_hosts_input ]]; then
  echo "::error::reach cloudflare-warp needs known-hosts: a hosted runner holds no host-key pins, and the action never runs ssh-keyscan"
  exit 1
fi
if [[ -n $billet_action_known_hosts_input ]]; then
  [[ -f $billet_action_known_hosts_input ]] || { echo "::error::known-hosts $billet_action_known_hosts_input does not exist"; exit 1; }
  install -d -m 0700 "$HOME/.ssh"
  touch "$HOME/.ssh/known_hosts"
  chmod 0600 "$HOME/.ssh/known_hosts"
  # An existing file that does not end in a newline would otherwise have the
  # first pin glued onto its last line.
  if [[ -s $HOME/.ssh/known_hosts && $(tail -c 1 "$HOME/.ssh/known_hosts" | od -An -c | tr -d ' ') != '\n' ]]; then
    printf '\n' >>"$HOME/.ssh/known_hosts"
  fi
  added=0
  # `|| [[ -n $line ]]` so a last line without a newline is read too; a
  # one-line pins file without one installed no pin before.
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%$'\r'}
    [[ -z $line || $line == \#* ]] && continue
    if ! grep -qxF -- "$line" "$HOME/.ssh/known_hosts"; then
      printf '%s\n' "$line" >>"$HOME/.ssh/known_hosts"
      added=$((added + 1))
    fi
  done <"$billet_action_known_hosts_input"
  echo "host-key pins: $added line(s) added to $HOME/.ssh/known_hosts"
fi

# --- the common arguments ----------------------------------------------------------
args=(-i "$billet_action_inventory")
if [[ -n $billet_action_limit ]]; then
  args+=(--limit "$billet_action_limit")
fi
if [[ -n $billet_action_extra_vars ]]; then
  if [[ ${billet_action_extra_vars:0:1} != "{" ]]; then
    echo "::error::extra-vars must be a JSON object (it is passed as -e and lands in argv, so it is for non-secret flags only)"
    exit 1
  fi
  args+=(-e "$billet_action_extra_vars")
fi

# --- the hosts this run will touch -----------------------------------------------------
listing=$(run_ansible ansible-playbook "${args[@]}" --list-hosts "$billet_action_playbook")
expected=$billet_action_runner_temp/billet-expected-hosts
# The listing indents each host by six spaces under "hosts (N):"; nothing else
# in it is indented that deep.
printf '%s\n' "$listing" | awk '/^      [^ ]/ { sub(/^      /, ""); print }' | sort -u >"$expected"
if [[ ! -s $expected ]]; then
  echo "::error::$billet_action_playbook matches no host in $billet_action_inventory${billet_action_limit:+ under --limit $billet_action_limit}. A playbook that matches no host exits 0 having converged nothing, so this is refused in both modes."
  printf '%s\n' "$listing"
  exit 1
fi
# A NAME IS ONE WORD WITHOUT A PATTERN SEPARATOR: the render below names the
# hosts as one host pattern, where ':' and ',' mean "or", and the recap rows
# and this listing split on whitespace. An IPv6 literal or a name with a space
# is an inventory name to change; ansible_host carries the address.
while IFS= read -r host; do
  if [[ ! $host =~ ^[A-Za-z0-9._-]+$ ]]; then
    echo "::error::the inventory names a host as '$host', which this action cannot carry through a host pattern or a recap row; name it with letters, digits, dots and hyphens and put the address in ansible_host"
    exit 1
  fi
done <"$expected"
echo "hosts this run touches: $(paste -sd' ' "$expected")"

# --- every host's connection, rendered ----------------------------------------------------
#
# One debug call over the expected hosts, one line per host (-o), the values as
# one JSON document each; the module templates on the controller and opens no
# connection. `checking` is the SSH plugin's effective setting: its own alias
# outranks the generic variable, as the plugin reads them.
pattern=$(paste -sd: "$expected")
rendered=$(run_ansible ansible "${args[@]}" -o -m debug \
  -a "msg={{ {'host': ansible_host | default(inventory_hostname), 'port': ansible_port | default(22), 'common': ansible_ssh_common_args | default(''), 'extra': ansible_ssh_extra_args | default(''), 'args': ansible_ssh_args | default(''), 'checking': ansible_ssh_host_key_checking | default(ansible_host_key_checking | default(true))} | to_json }}" \
  "$pattern")

reach_targets=$billet_action_runner_temp/billet-reach-targets
: >"$reach_targets"
while IFS= read -r line; do
  [[ -z $line ]] && continue
  host=${line%% *}
  json=${line#*=> }
  # The msg is a JSON string holding a JSON document: two decodes, in the
  # reader beside this script, which judges the SSH arguments and never prints
  # them, because a ProxyCommand may carry something an operator would not want
  # in a log. What ssh will see and what is refused is written on that file.
  verdict=$(printf '%s' "$json" | python3 "$here/judge-ssh-options.py")
  read -r addr port bad <<<"$verdict"
  # THE PIN POLICY IS ENFORCED ON WHAT SSH WILL ACTUALLY SEE. An inventory may
  # add a ProxyCommand; it may not turn host-key checking off or point it at
  # another file, because that quietly discards the pins.
  if [[ $bad != - ]]; then
    echo "::error::$host sets $bad in its SSH options, which would disable or redirect host-key checking and discard the pins the action installs. Remove it from ansible_ssh_common_args, ansible_ssh_extra_args, ansible_ssh_args, ansible_host_key_checking or ansible_ssh_host_key_checking."
    exit 1
  fi
  printf '%s %s %s\n' "$host" "$addr" "$port" >>"$reach_targets"
done <<<"$rendered"

# EVERY EXPECTED HOST RENDERED, EXACTLY ONCE. A render that answered for some
# hosts and not others (a dynamic inventory that moved, a host the pattern did
# not reach) would otherwise be probed for the ones that arrived and trusted
# for the rest.
while IFS= read -r host; do
  count=$(awk -v h="$host" '$1 == h' "$reach_targets" | wc -l | tr -d ' ')
  if [[ $count != 1 ]]; then
    echo "::error::$host was listed by the playbook but rendered $count times; every host the run will touch has to render exactly once before it is probed"
    exit 1
  fi
done <"$expected"
if [[ $(wc -l <"$reach_targets" | tr -d ' ') != $(wc -l <"$expected" | tr -d ' ') ]]; then
  echo "::error::the render answered for a host the playbook did not list; the expected set and the rendered set must be the same hosts"
  exit 1
fi

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
#
# Both modes stream through tee and both publish the recap, a failed pass
# included, so a consumer reads what the run said before it read that it
# failed; the pass's own status is what this script exits with.
log="$billet_action_runner_temp/billet-converge-1.log"
status=0
if [[ $billet_action_mode == check ]]; then
  run_ansible ansible-playbook "${args[@]}" --check --diff "$billet_action_playbook" 2>&1 | tee "$log" || status=$?
else
  run_ansible ansible-playbook "${args[@]}" "$billet_action_playbook" 2>&1 | tee "$log" || status=$?
  if [[ $status == 0 && $billet_action_prove == true ]]; then
    BILLET_CHILD_ENV_FILE="$child_env_file" \
      "$here/prove-idempotent.sh" "$expected" -- "${args[@]}" "$billet_action_playbook" || status=$?
    log="$billet_action_runner_temp/billet-converge-2.log"
  fi
fi

{
  echo "recap<<BILLET_RECAP_EOF"
  sed -n '/PLAY RECAP/,$p' "$log" 2>/dev/null | grep -E '^[^ ]+ +: +ok=' || true
  echo "BILLET_RECAP_EOF"
} >>"$GITHUB_OUTPUT"
exit "$status"
