# CLI

`billet <command> [flags]`. Every command that reads a deployment's configuration takes `--config <path>` (the exceptions are `version`, `release record`, and the `acceptance` subcommands other than `up`, which work from their workspace); the default is the per-user config path (`billet check -h` prints it), never the working directory, and the `local` commands default to the packaged service config (`/etc/billet/billet.yaml` on Linux, `/usr/local/etc/billet/billet.yaml` on macOS). Most failures exit 1. Where a command uses 2 or 3 as an answer rather than an error, so a monitor can tell a task from an outage, the entry below says so.

## Roles

### `billet server`

Run the control plane: long-poll GitHub for assigned jobs, own the capacity ledger, tell nodes what to launch. Run `billet node` beside it to run jobs on the same machine.

| Flag | Meaning |
|---|---|
| `--dry-run` | connect to GitHub and advertise zero capacity: proves the whole path without accepting a job |
| `--upgrade-probe` | open candidate state without polling, dispatching or accepting work; the host-upgrade transaction's quiescent probe |

### `billet node`

Run a compute host that dials a control plane. One per machine, including the machine the server is on.

| Flag | Meaning |
|---|---|
| `--enroll` | ask the control plane to admit this machine, then wait for an operator to approve it |
| `--ca-fingerprint <fp>` | the control plane's CA fingerprint, from `billet ca show` (required with `--enroll`) |
| `--join-token <token>` | a short-lived token from `billet ca token` (required with `--enroll`) |
| `--bootstrap-addr <addr>` | the control plane's enrollment address; overrides `node.bootstrap_addr`, defaults to `node.server_addr` |
| `--upgrade-probe` | initialise the candidate provider without dialing or accepting work |

## Setup

### `billet init`

Generate a `billet.yaml` that runs. It measures the host, writes a ceiling below what it found, and generates tiers that fit together under it. A fresh generation lands at `<path>.new` unless it can prove the existing file is its own output; `--force` overwrites.

| Flag | Meaning |
|---|---|
| `--profile local\|local-service` | user-session paths for two terminals, or the paths the shipped services read (default `local`) |
| `--provider docker\|firecracker\|tart\|ec2\|codebuild` | this host's backend (default `docker`) |
| `--org`, `--runner-group`, `--workflow` (repeatable) | the organization and the trusted pool; group and workflow are required for docker |
| `--listen` | the loopback address the server binds and the node dials (default `127.0.0.1:7717`) |
| `--image` | the tier image (default: a runner container for docker, a golden generation for firecracker) |
| `--emit file\|ansible` | write the file, or print the `billet_config` block for an inventory and write nothing |
| `--state-backend sqlite\|postgres`, `--state-dsn-env` | where the ledger lives; the DSN's environment variable is required for postgres |
| `--node-name` | this host's name; required for a macOS tier on tart or codebuild |
| `--guest-os macos\|linux` (repeatable), `--macos-image`, `--linux-image` | tart guest kinds and their images (defaults `ghcr.io/cirruslabs/macos-tahoe-xcode:latest` and `ghcr.io/cirruslabs/ubuntu-runner-arm64:latest`) |
| `--kernel-image`, `--ceph-user`, `--ceph-keyring`, `--cache-listen`, `--cache-guest-endpoint` | firecracker: the fallback kernel, the RADOS identity (never `admin`), its keyring, the guest-facing cache endpoint |
| `--region`, `--subnet`, `--security-group` (repeatable), `--untrusted-security-group` (repeatable), `--instance-type` (repeatable), `--price type=usd` (repeatable), `--max-vcpu`, `--max-memory` | ec2: the region signed into every request, network, shapes (prices are fetched; `--price` overrides), and the required cloud budget |
| `--codebuild-project`, `--codebuild-environment`, `--codebuild-fleet-arn`, `--codebuild-fleet-capacity`, `--compute-type NAME=vcpu,memory,price` (repeatable), `--jit-parameter-path`, `--jit-kms-key-id`, `--codebuild-log-group`, `--privileged`, `--build-timeout-minutes`, `--queued-timeout-minutes`, `--accept-external-build-ceiling` | codebuild: the dedicated project, environment type, reserved fleet and its capacity, ordered purchasable shapes, the parameter path the IAM policy is scoped to, and the acknowledgement of CodeBuild's 36-hour and 8-hour ceilings, which is required and changes nothing about how billet behaves |

### `billet init iam`

Print the IAM policy this config's node role needs, derived from the config: cache adds EBS and S3, spot adds the queue, an instance profile adds `PassRole`.

| Flag | Meaning |
|---|---|
| `--builder` | also grant what `billet ami build` needs |
| `--build-role` | print the CodeBuild build service role's policy instead of the node's |
| `--controller-sweep` | print the control plane's grant for sweeping staged registrations under this node's parameter path |
| `--account <id>` | the 12-digit account (codebuild) |
| `--deployment <id>` | scope to this deployment (defaults to the state directory's id) |
| `--account-wide` | scope by tag presence instead of deployment id; only safe for a single deployment per account |
| `--role-arn`, `--kms-key-arn` | the profile's role for `PassRole` scoping; the full key ARN when the configured key is a bare id or alias |

### `billet github-app create`

Create the GitHub App in a browser, install it on the organization, write the five `github:` scalars into an existing config in place, and save the private key beside it. Refuses everything it can before touching GitHub, because the key is issued once.

| Flag | Meaning |
|---|---|
| `--org` | the organization (required) |
| `--config` | the `billet.yaml` to write the `github:` block into |
| `--key-path` | where to write the key (default: beside the config) |
| `--name` | a suggested App name |
| `--no-browser` | print URLs instead of opening a browser |
| `--port` | a fixed loopback callback port, for `ssh -L` |

### `billet github-app store-key --from <path>`

Publish an existing App private key into a store-backed deployment (`server.identity.backend: aws-ssm`).

### `billet check`

Validate the config and the state directory, prove the App key signs a JWT GitHub accepts and the App is installed with exactly the requested permissions, check every trusted tier's runner group, and report registered nodes, images, backups and cloud cost peaks. Works while the control plane is running.

| Flag | Meaning |
|---|---|
| `--authorize` | also dry-run a launch against AWS to prove the ec2 role may `RunInstances` |
| `--maintenance-probe` | the host-upgrade transaction's quiescent probe: cross the maintenance fence, skip every network call |

## Running the services

### `billet local status|up|down|uninstall`

Manage the systemd units (Linux) or launch agents (macOS). `status` reports what the service manager actually has loaded. `up` runs `billet check`, starts the server, then the node, proves each held its process, then enables them; it writes no units on systemd and writes the agents on macOS. `up` exits 2 when the services are up but admission could not be reopened (an operator's seal, or a seal it could not read), so the host is running and takes no work until `billet resume`. `down` seals admission, waits for running work, stops the node, stops the server and disables both. `uninstall` is `down` plus forgetting the services, leaving the data.

| Flag | Applies to | Meaning |
|---|---|---|
| `--dry-run` | up, down, uninstall | report and change nothing |
| `--reason` | down, uninstall | recorded on the seal |
| `--timeout` | down, uninstall | give up waiting after this long (default: wait for as long as the jobs take) |
| `--force` | down, uninstall | stop services running a different billet build |
| `--without-compute-proof` | down | stop once the ledger is quiet, without asking each host what it is running |

### `billet local backup|restore|recover`

Capture a deployment as one unit, put it back as one unit or not at all, or put it back over itself. [Backup, restore and recover](../operating/backup-restore-recover.md).

| Flag | Applies to | Meaning |
|---|---|---|
| `--out <dir>` | backup | the destination, created and empty |
| `--no-upload` | backup | write the archive and do not copy it to `backup.s3` |
| `--from <dir>` | restore, recover | a backup directory |
| `--from-backup <name\|latest>` | restore, recover | fetch an archive from `backup.s3` |
| `--into <dir>` | restore, recover | where a fetched archive is kept |
| `--deployment <id>` | restore, recover | which deployment's archives to look at |
| `--old-controller-fenced` | restore, recover | assert the controller this backup came from is stopped and disabled everywhere (required) |
| `--external-ledger-attached` | restore | assert a PostgreSQL ledger has been restored and this host's config points at it |
| `--accept-failing-jobs` | recover | proceed even though work is outstanding; every job named fails |
| `--reason`, `--timeout` | recover | recorded on the seal; give up waiting for quiescence |
| `--dry-run` | restore, recover | report what would happen and every refusal, and change nothing |
| `--abandon` | restore, recover | undo an interrupted run, removing only what it created; recover also puts the superseded ledger back |

## Capacity and admission

| Command | Meaning |
|---|---|
| `billet status` | admission, controller and epoch, capacity, tiers, nodes with protocol versions, cost peaks, unproven hosts |
| `billet leases held` | every lease whose compute is not confirmed gone |
| `billet leases quarantined` | capacity held for compute nobody has accounted for |
| `billet leases failures [--since 24h] [--limit 50]` | jobs GitHub did not report as succeeded on leases billet's infrastructure disrupted; billet re-runs nothing |
| `billet leases release <lease> --force` | hand capacity back on your assertion that its compute is gone (`--force` required) |
| `billet drain [--reason] [--wait] [--timeout] [--without-compute-proof]` | seal admission and, with `--wait`, wait for the ledger and then every host to prove nothing is running. Exit 2: still draining or interrupted; the seal remains |
| `billet resume` | open admission again |
| `billet force-destroy [--tier] [--node] --reason --yes` | destroy compute still running a job, failing those builds. Refuses unless admission is sealed by `billet drain`; without `--yes` it only reports. Exit 2: refused, nothing destroyed |

## Nodes and the authority

| Command | Meaning |
|---|---|
| `billet nodes pending [--all]` | machines waiting to be let in, with their fingerprints |
| `billet nodes approve <node> --fingerprint <fp>` | admit one machine; the fingerprint is required |
| `billet nodes deny <node> --fingerprint <fp>` | refuse one |
| `billet nodes revoke <node> [--reason]` | take back every credential a machine holds |
| `billet nodes decommission <node> [--force]` | stop expecting a host to answer for its compute; `--force` records the exclusion as unproven |
| `billet ca issue <node> [--out <dir>] [--reissue]` | write a node's certificate bundle (default `./<node>-billet-tls`) |
| `billet ca token [--ttl 1h] [--uses 1] [--note]` | mint a join token; shown once, stored as a hash |
| `billet ca show` | the authority's fingerprint and expiry, with a warning once it is shortening what it issues |
| `billet ca rotate` | start a rotation: the new authority issues, both are trusted |
| `billet ca retire [--force]` | finish it: drop the old authority once every node has renewed |
| `billet ca revoke <node> [--cert <path>] [--reason]` | withdraw one certificate by serial |
| `billet ca revocations` | list what has been withdrawn |
| `billet ca sync [--push] [--force]` | adopt the authority the identity store publishes, publish this host's, or replace this host's (moved aside, not deleted) |

## Images

| Command | Meaning |
|---|---|
| `billet images refresh [--keep 3] [--dry-run]` | take up a newer published image: on a firecracker node, pull, boot-verify and promote only when the channel's image was built after the newest generation imported, then reap to `--keep` verified generations per contract; on a tart node, pull every configured image that is absent; nothing under `release.automatic: false`. What `billet-images.timer` runs daily |
| `billet images pull [<ref>] [--from <dir>] [--source <url>] [--verify] [--result-file <path>] [--allow-stale] [--kernel-dir <dir>] [--staging-dir <dir>] [--keep-staging] [--signing-identity <pattern>] [--signing-issuer <issuer>] [--skip-signature-verification]` | fetch a published guest image, verify its signature and every asset, install the paired kernel durably, import it as a generation; on a tart node, pull the tiers' images into tart's store |
| `billet images verify <image>@<gen> [--wait 3m] [--record] [--allow-unpaired]` | boot one microVM and make the guest prove it works; `--record` (default) marks it so `@verified` resolves to it |
| `billet images compatible [--wait 3m] [--result-file]` | prove every configured image speaks this binary's guest contract. Exit 2: images need a compatible generation |
| `billet images due [--max-age 144h]` | is the newest generation old enough to rebuild. Exit 2: nothing to do |
| `billet images list` | what exists, what is verified, what tiers boot |
| `billet images reap [--keep 3] [--dry-run] [--kernel-dir]` | remove generations nothing needs, and orphaned kernels |
| `billet images promote\|unpromote <image>@<gen>` | the manual half of promotion |
| `billet runner check [--quiet]` | how close the pinned `actions/runner` is to being refused. Exit 0 nothing to do, 2 rebuild due, 3 GitHub already refusing |
| `billet ami build [--arch x64\|arm64] [--base-image] [--instance-type] [--builder-disk] [--runner-version] [--name] [--ca-cert] --payload-bucket [--region] [--subnet] [--security-group] [--public-ip] [--timeout 1h] [--verify] [--verify-instance-type]` | build the AMI the ec2 backend launches, boot it, and stamp it only after it proved itself |
| `billet ami verify <ami-id> [--instance-type] [--region] [--subnet] [--security-group] [--public-ip] [--timeout 20m]` | boot an existing AMI and stamp it if it proves itself |

## Cache

| Command | Meaning |
|---|---|
| `billet cache enable\|disable --org <org>` or `--repository <owner/repo>` | the central kill switch for transparent Actions caching |
| `billet cache conformance install --repository <owner/repo> --runner-label <label> [--billet-ref] [--billet-repository] [--expected-runner-version] [--expected-guest-contract] [--workflow-ref] [--output] [--force]` | write the consumer-owned conformance workflow, pinning the resolved billet commit, runner version, guest contract and one label |

## Upgrades

| Command | Meaning |
|---|---|
| `billet rollout start [--channel stable] [--version] [--cohort 1] [--failure-budget 1] [--allow-downgrade] [--skip-signature-verification]` | resolve a channel once into an immutable target and record the decision; a target older than the running release is refused without `--allow-downgrade`. With `release.automatic` on (the default) the control plane does this itself when the channel advances |
| `billet rollout status` | where each host has got to and what it proved |
| `billet rollout abort --reason` | abandon the decision; converged hosts stay converged |
| `billet rollout retry\|exempt\|decommission <node> [--reason] [--force]` | put a host back to pending, mark it not part of this rollout, or record it gone |
| `billet host-upgrade [--channel] [--version] [--manifest-sha256] [--rollout] [--generation] [--reinstall] [--resume] [--status] [--ack-fd] [--from-rollout] [--allow-downgrade] [--skip-signature-verification]` | replace billet on this machine transactionally with rollback, under systemd or launchd; `--status` reports what the machine holds; `--resume` continues or unwinds; `--reinstall` repairs a host a rollout blocked; `--from-rollout` acts on the rollout the ledger records and is what `billet-upgrade.timer` and the `sh.billet.upgrade` agent run; `--allow-downgrade` lowers the ledger's release watermark to admit an older release |
| `billet release record --manifest --archive --binary` | record which signed manifest produced the installed binary, after proving the manifest names it |

## Removal

| Command | Meaning |
|---|---|
| `billet teardown --tier <label>\|--all [--runner-group] [--force] [--yes]` | delete the scale sets billet created on GitHub |
| `billet decommission [--yes] [--terminate-instances]` | delete the EC2 instances and EBS+S3 cache billet made outside Terraform; without `--yes` it reports |

## Acceptance

`billet acceptance up|run|evidence|down|sweep` stands an isolated deployment up beside this one against a real account, runs a real job, and destroys exactly what it made. `up --config <derive-from> --workspace <dir> [--account] [--label-prefix accept] [--region]`; `run --workspace [--jobs 1] [--wait 30m] [--no-teardown]`; `evidence --workspace [--out]`; `down --workspace [--wait 20m] [--keep-workspace]`; `sweep --workspace` asks whether anything billable is left without destroying anything. A weekly workflow runs it; it is never run on a pull request.

## `billet version`

Print the version and the Go version.
