# billet converge fleet

Converge a billet fleet from GitHub Actions with one `uses:` line. The action reaches your hosts, proves each one answers, runs the collection's fleet playbook, and in converge mode runs it a second time and fails unless every host reports `changed=0`.

```yaml
- uses: junioryono/billet/actions/converge-fleet@v0.10.0
  with:
    mode: check                       # or converge
    inventory: fleet/inventory.yml
    known-hosts: fleet/known_hosts
    reach: cloudflare-warp            # or none
    ssh-private-key: ${{ secrets.BILLET_FLEET_SSH_KEY }}
    cloudflare-team-name: ${{ vars.CLOUDFLARE_TEAM_NAME }}
    cloudflare-service-token-id: ${{ secrets.CF_CONVERGE_SERVICE_TOKEN_ID }}
    cloudflare-service-token-secret: ${{ secrets.CF_CONVERGE_SERVICE_TOKEN_SECRET }}
    environment: |
      BILLET_CLOUDFLARED_TOKEN_CONTROL_1=${{ secrets.BILLET_CLOUDFLARED_TOKEN_CONTROL_1 }}
      BILLET_WARP_CONNECTOR_TOKEN_NODE_1=${{ secrets.BILLET_WARP_CONNECTOR_TOKEN_NODE_1 }}
```

`billet init hybrid --workflow` writes the two-job workflow around this, and [Converging from CI](https://billet.readthedocs.io/en/latest/deploying/converging-from-ci.html) is the page that explains it.

## The one pin

GitHub checks out this whole repository at the ref in `uses:` to run the action, so the Ansible collection under `ansible_collections/` in that same checkout is the collection the converge runs. There is no Galaxy fetch of billet's collection, no `requirements.yml`, no wrapper script: the ref pins the action and the roles together. With an exact tag, Dependabot's `github-actions` ecosystem opens a pull request for every billet release, and that pull request's `check` job is a `--check --diff` against your live fleet showing exactly what the new roles would change. A `@v0` reference moves with every accepted release and opens no pull request: the next converge runs the new roles, which is the trade-off [Action versioning](https://billet.readthedocs.io/en/latest/reference/action-versioning.html) states, and on this action it is a trade-off about the roles as well as the action.

What the action installs on the runner: `ansible-core` at the version pinned in `ansible_collections/junioryono/billet/tests/ansible-core-version` (into a venv, because Ubuntu 24.04's system Python is externally managed) and `ansible.posix` at the version pinned beside it (the collection's one dependency, fetched from Galaxy with retries). CI's host-lifecycle job tests the roles on the same two pins, and `actions/actions_test.go` fails if the two could disagree.

## Inputs

| Input | What it is |
|---|---|
| `mode` | `check` runs `--check --diff`; `converge` runs the playbook and then the idempotence proof. Required. |
| `inventory` | The inventory file, relative to the repository. Required. |
| `playbook` | Default `junioryono.billet.fleet`, the collection's playbook. |
| `known-hosts` | A committed file pinning every host's key. Required with `reach: cloudflare-warp`, because a hosted runner holds no pins; the action never runs `ssh-keyscan`. Lines are appended to the runner user's `known_hosts`, never replacing it. |
| `ssh-private-key` | The private half of the key `ssh_access` installs for the converge. Written 0600 under `RUNNER_TEMP` and exported as `ANSIBLE_PRIVATE_KEY_FILE`. |
| `reach` | `none` (the runner already reaches the hosts, Route A) or `cloudflare-warp` (Route D: enrol as a WARP client with a service token). |
| `cloudflare-team-name`, `cloudflare-service-token-id`, `cloudflare-service-token-secret` | Route D's team and the two halves of the token the `converge-cloudflare-warp` module created. |
| `environment` | `NAME=value` lines exported into `ansible-playbook`'s environment and never its argv: the per-host connector tokens `BILLET_CLOUDFLARED_TOKEN_<HOST>` and `BILLET_WARP_CONNECTOR_TOKEN_<HOST>`, and anything else a role reads from the environment. |
| `github-app-private-key` | For a control plane's first converge only: written 0600, exported as `BILLET_GITHUB_PRIVATE_KEY_PATH`, deleted when the job ends. |
| `extra-vars` | A JSON object passed as `-e`; non-secret flags only, because `-e` lands in argv. |
| `limit` | An Ansible host pattern. |
| `prove-idempotent` | Default `true`. |

Outputs: `recap` (the recap rows of the last pass) and `billet-ref` (the ref this action ran from).

## What it does, in order

1. Refuses an unknown mode, an unknown reach, and a `RUNNER_NAME` beginning `billet-`, before anything is installed or enrolled: a converge drains the node and restarts billet's services, which destroys the jobs on that host, including a deploy job running on a runner billet manages, and GitHub does not requeue a job whose runner vanished. The host role refuses the same shape again before its transaction.
2. Installs the pinned `ansible-core` and `ansible.posix`.
3. With `reach: cloudflare-warp`, installs the WARP client from a signing key verified against the fingerprint the `warp_connector` role pins (the action and the role cannot trust two different keys), writes `mdm.xml` from stdin with `install -m 0600` so the secret never lands in argv or a world-readable file, restarts the daemon, connects, and waits for *Connected* with a bounded loop whose failure names the enrolment policy.
4. Lists the hosts the playbook will touch with `ansible-playbook --list-hosts` (the same inventory, `--limit` and extra variables the run uses) and refuses an empty set in both modes, because a playbook that matches no host exits 0 having converged nothing. Renders every listed host's `ansible_host`, port and SSH arguments through the `debug` module, which templates without opening a connection; refuses an inventory that turns host-key checking off or points it at another file, because those variables replace anything the action could set; and proves each address answers on its port with `nc`, because a device that enrolled and is then denied by Gateway looks identical to a connected one until the first SSH times out.
5. Runs the playbook. In converge mode, runs it again streamed through `tee`, reads only the recap rows, compares hostnames as literal strings and counters as integers, and fails unless every listed host has exactly one row with `changed`, `unreachable`, `failed`, `rescued` and `ignored` all zero.
6. Whatever happened, removes the key files it wrote and, only when this run enrolled a WARP device, deletes that registration and its `mdm.xml`; with `reach: none` nothing WARP-related is touched.

## Reading a failed run

- *Refuse an unknown mode and a billet-managed runner*: the job is on a billet label. Move it to a hosted runner or one billet does not manage.
- *Join the Zero Trust network*: the service token could not enrol (expired, or the module's enrolment policy is not attached to the WARP enrolment application), or the signing key did not match the pin.
- *Converge*, before ansible-playbook: no host matched the playbook; an inventory SSH option disables host-key checking; or an address did not answer, which with Route D is one of three things (the device did not enrol; the Gateway rule does not admit the destination for `non_identity@`, which Gateway's network log names; the device profile's split tunnel excludes it).
- *Converge*, inside ansible-playbook: `Permission denied (publickey)` means the converge key is not on that host yet (the first converge installs it from a laptop with the break-glass key); `REMOTE HOST IDENTIFICATION HAS CHANGED` means a host was reinstalled and its pin must be refreshed out of band; anything inside a billet role is the role refusing the inventory, and its message is specific.
- *The second pass*: a role is not idempotent; the output names the host and the counter.

## Running the same thing from a laptop

The scripts take their inputs from the environment the action's steps set, so a laptop run is a clone of billet at the release you pin and the same variables:

```bash
git clone --branch v0.10.0 --depth 1 https://github.com/junioryono/billet
export GITHUB_ACTION_PATH=$PWD/billet/actions/converge-fleet RUNNER_TEMP=$(mktemp -d) GITHUB_OUTPUT=/dev/null GITHUB_PATH=/dev/null
"$GITHUB_ACTION_PATH/install-ansible.sh" && export PATH="$RUNNER_TEMP/billet-ansible/bin:$PATH"
BILLET_MODE=check BILLET_INVENTORY=fleet/inventory.yml BILLET_KNOWN_HOSTS=fleet/known_hosts BILLET_REACH=none \
  BILLET_ENVIRONMENT="BILLET_CLOUDFLARED_TOKEN_CONTROL_1=$(terraform output -raw tunnel_token)" \
  "$GITHUB_ACTION_PATH/converge.sh"
```

## What is not covered

- The render of each host's connection reads the inventory's variables. A play that sets `ansible_host`, `ansible_port` or the SSH arguments at play level is outside what the render and the probe can see; the collection's fleet playbook sets none, and a consumer's own play must not either.

## Measured

- The whole-repository checkout behind `ANSIBLE_COLLECTIONS_PATH=$GITHUB_ACTION_PATH/../..` and the venv on Ubuntu 24.04's externally managed Python are to be measured on the first real run of this action from a consumer repository; the date goes here.
