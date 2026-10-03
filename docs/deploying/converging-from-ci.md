# Converging from CI

A billet fleet is machines you converge with the collection billet ships: the control plane, the Linux compute hosts and the Macs. This page is how a repository that owns a fleet keeps that converge running from GitHub Actions, and from an operator's machine when it has to, while holding configuration only. The implementation (reaching the hosts, the playbook, the roles, the idempotence proof, the guard that stops a converge and a rollout touching one host at once) lives in billet and is pinned by one ref.

## What your repository holds

| File | What it is |
|---|---|
| `fleet/inventory.yml` | The hosts and their groups (`control_plane`, `linux`, `macos`, empty ones as `hosts: {}`), how to reach each (`ansible_host`, `ansible_user`), and each host's `billet_config`: the tiers, targets and node block `billet init` generates. `billet init hybrid` writes a first one. |
| `fleet/known_hosts` | One pinned host key per host. The converge never runs `ssh-keyscan`. |
| `.github/workflows/fleet.yml` | Two jobs that call `junioryono/billet/actions/converge-fleet`: a dry run on pull requests and the converge on the default branch. Below. |
| Repository secrets | The converge's SSH key, the reach credentials for your route, and any connector tokens. |

Nothing else: no playbook, no roles, no `requirements.yml`, no `ansible.cfg`, no wrapper script. The collection's own playbook, `junioryono.billet.fleet`, converges the control plane first, then the Linux nodes, then the Macs, which is the order a release must reach them in. A play of your own that CI must never run (a developer tool on a workstation, say) belongs in a separate file you run by hand, not in the fleet.

## The workflow

```yaml
name: Fleet
on:
  pull_request:
    paths: [fleet/**, .github/workflows/fleet.yml]
  push:
    branches: [main]
    paths: [fleet/**, .github/workflows/fleet.yml]
  workflow_dispatch:

permissions:
  contents: read

jobs:
  check:
    if: github.event_name == 'pull_request'
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v6
      - uses: junioryono/billet/actions/converge-fleet@v0.12.32
        with:
          mode: check
          inventory: fleet/inventory.yml
          known-hosts: fleet/known_hosts
          reach: cloudflare-warp
          ssh-private-key: ${{ secrets.BILLET_FLEET_SSH_KEY }}
          cloudflare-team-name: ${{ vars.CLOUDFLARE_TEAM_NAME }}
          cloudflare-service-token-id: ${{ secrets.CF_CONVERGE_SERVICE_TOKEN_ID }}
          cloudflare-service-token-secret: ${{ secrets.CF_CONVERGE_SERVICE_TOKEN_SECRET }}

  converge:
    if: github.event_name != 'pull_request'
    runs-on: ubuntu-24.04
    environment: fleet          # a required reviewer approves each converge
    concurrency:
      group: fleet
      cancel-in-progress: false # never cancel a converge halfway
    timeout-minutes: 90
    steps:
      - uses: actions/checkout@v6
      - uses: junioryono/billet/actions/converge-fleet@v0.12.32
        with:
          mode: converge
          inventory: fleet/inventory.yml
          known-hosts: fleet/known_hosts
          reach: cloudflare-warp
          ssh-private-key: ${{ secrets.BILLET_FLEET_SSH_KEY }}
          cloudflare-team-name: ${{ vars.CLOUDFLARE_TEAM_NAME }}
          cloudflare-service-token-id: ${{ secrets.CF_CONVERGE_SERVICE_TOKEN_ID }}
          cloudflare-service-token-secret: ${{ secrets.CF_CONVERGE_SERVICE_TOKEN_SECRET }}
          environment: |
            BILLET_CLOUDFLARED_TOKEN_CONTROL_1=${{ secrets.BILLET_CLOUDFLARED_TOKEN_CONTROL_1 }}
            BILLET_WARP_CONNECTOR_TOKEN_NODE_1=${{ secrets.BILLET_WARP_CONNECTOR_TOKEN_NODE_1 }}
```

Replace `@main` with the release your fleet runs: these docs name `main` and each release's copy names its own tag, so the block you copy from a release's docs is already pinned. `reach` is how a hosted runner gets to your hosts. `cloudflare-warp` is Route D in [Reaching your hosts](reaching-hosts.md), and its Terraform module creates the service token and the one Gateway rule the runner needs. With `reach: none` the runner must already reach the hosts, which means a runner of your own on their network (Route A). The [action's README](https://github.com/junioryono/billet/tree/main/actions/converge-fleet) lists every input, what each step does, and how to read a failed run.

**Never run either job on a runner billet manages.** A converge drains a node and restarts billet's services, which destroys the jobs on that host, the converge's own included, and GitHub does not requeue a job whose runner vanished. The action and the host role both refuse a `RUNNER_NAME` beginning `billet-`.

## Secrets

| Secret | Where it comes from |
|---|---|
| `BILLET_FLEET_SSH_KEY` | A key pair you generate for the converge. Its public half goes in the inventory under `billet_ssh_authorized_keys`, and the `ssh_access` role installs it. |
| `CF_CONVERGE_SERVICE_TOKEN_ID`, `CF_CONVERGE_SERVICE_TOKEN_SECRET` | The `converge-cloudflare-warp` module's outputs (Route D). |
| `BILLET_CLOUDFLARED_TOKEN_<HOST>`, `BILLET_WARP_CONNECTOR_TOKEN_<HOST>` | One per host that runs a connector, from the `converge-cloudflare` and `converge-cloudflare-warp` modules. `<HOST>` is the inventory hostname upper-cased, with every other character an underscore. They go through the `environment` input, never `extra-vars`, because `-e` lands on a command line. |
| `github-app-private-key` input | Only for a control plane's first converge, when the App key is not on the host yet. |

## The trust boundary

Both jobs read the same secrets and run the action as pinned, so the pull-request dry run is a view for the reviewer, not a security boundary: anyone who can push a branch in this repository can read what the dry run reads. Keep the repository's write access to the people you would let converge the fleet, and the `environment` with a required reviewer on the converge job is the approval. Forks receive no secrets. If a dry run on every pull request is too wide, scope the secrets to the `fleet` environment as well; each dry run then waits for the same approval.

## Upgrading billet

A release carries the binary, the collection and the action together, and the running fleet upgrades its own binary from the stable channel (`release.automatic`). What your repository pins is the collection, through the one `uses:` ref:

- An exact tag (`@vX.Y.Z`): Dependabot's `github-actions` ecosystem opens a pull request for each billet release, and that pull request's dry run shows exactly what the new roles would change on your live fleet. Merge it once every host reports that release (`billet rollout status`), because the role refuses to move a host's binary backwards.
- `@v0`: the next converge runs whatever the latest accepted release's roles are, with no pull request in between. That trades the dry run for zero upkeep; [Action versioning](../reference/action-versioning.md) says more.

## From an operator's machine

The same converge runs from a laptop with `billet fleet converge`:

```bash
billet fleet converge -inventory fleet/inventory.yml -known-hosts fleet/known_hosts -check
billet fleet converge -inventory fleet/inventory.yml -known-hosts fleet/known_hosts \
  -environment-file ~/fleet.env
```

It fetches the checkout of the installed billet's release into the user cache directory once, and runs the action's own steps from it, with the collection from the same checkout. So a laptop and CI converge with exactly one implementation, and your repository needs no second pin. It connects over your machine's own network and your SSH agent, or `-ssh-key`, and always releases the converge guard it took. A development build of billet must name `-ref vX.Y.Z`.

## The first converge

The converge key cannot install itself: `ssh_access` puts it on each host, and until then only the key you installed the machine with can log in. So converge once from a laptop that can reach the hosts, with that key loaded in your agent and the converge key's public half in the inventory:

```bash
billet fleet converge -inventory fleet/inventory.yml -known-hosts fleet/known_hosts
```

From then on CI converges with `BILLET_FLEET_SSH_KEY`. A control plane's first converge also needs its App key: pass `-github-app-key <file>`, or `github-app-private-key` in CI.
