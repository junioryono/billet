# Troubleshooting

The failure mode of this system is a job that queues rather than an error. Start with `billet check` and `billet status`, both of which work against a live deployment.

## A job sits in the queue

| Symptom | Cause | Fix |
|---|---|---|
| `billet status` shows the tier at `0 available` | billet advertises nothing for it; another tier's reservation holds the capacity, or no live node can serve it | reduce a tier, raise the ceiling, or check the node is registered and live |
| the label matches nothing | `runs-on` must equal a tier's label exactly; the label is the scale set's name | fix the label or the tier |
| the runner group grants no repository | a group with selected visibility and an empty repository list routes nothing and reports nothing; `billet check` refuses it, and a REST `PATCH` that sets visibility without re-sending repository ids clears the list | grant the repository in the group |
| the workflow is not on the group's allowlist | the allowlist includes the ref, so `…/ci.yml@refs/heads/main` does not match a run on another branch | add the exact workflow identity |
| old queued runs pile up | a backlog of runs that were never assignable keeps new dispatches queued | cancel them and dispatch fresh |
| the tier lists codebuild and names a `site` | a sited tier is confined to hosts at that site and a CodeBuild node declares none, so the tier advertises 0 with everything healthy | config load now refuses this; remove the `site` |
| a job aimed at a stopped host waits minutes | a node that died is forgotten only by silence, about four and a half minutes; a node that stopped cleanly withdraws at once | wait, or stop nodes cleanly |
| GitHub cancels and requeues every five minutes on CodeBuild | the fleet is `ACTIVE` with no Mac behind it (`INSUFFICIENT_CAPACITY`), or a build is queued behind the fleet's capacity | `billet check --provider codebuild`; warm a new fleet with one build; keep `macos_vm_limit` at the fleet's capacity |

## The control plane will not start, or restarts forever

| Symptom | Cause | Fix |
|---|---|---|
| a bare `404 Not Found` on `/access_tokens`, restarting on failure | the App is uninstalled from the organization, or `installation_id` is left over from a reinstall; every token billet mints is scoped to an installation | `billet check` says so; reinstall the App and correct `installation_id` |
| `another billet process is this deployment's controller` | a second control plane on the same deployment; on PostgreSQL, another host holds the claim | stop one, or set `server.controllers: active-passive` on both if a standby was intended |
| `this process is no longer this deployment's controller` | the controller's database session ended and a successor claimed; the old one refuses its next write and exits without destroying anything | expected under a partition or failover; the successor re-adopts the jobs |
| `migrations are append-only and must never be edited` | a migration's statement bytes differ from what this deployment recorded; a checkout with `core.autocrlf` rewriting line endings, or an edited migration | never edit a migration; restore the file byte-for-byte |
| the schema is newer than this binary | an operator command from a newer billet migrated a stopped ledger | upgrade the server binary too |
| the state directory is on NFS or EFS | SQLite's write-ahead log is unsafe there; billet reads the setting back and refuses | put `server.state_dir` on local storage |
| the identity directory disagrees with the ledger | the ledger records which deployment it belongs to | use the matching `identity_dir`, or restore the whole unit |
| the control plane waits for its own message session after a crash | GitHub will not hand a session to a successor; it expires the abandoned one | nothing; queued jobs are not lost while it waits |

## A node will not register

| Symptom | Cause | Fix |
|---|---|---|
| `json: unknown field "min_version"` | the node is newer than the control plane; the wire's strict decoder rejects it before any version check | upgrade the server first |
| the node's protocol range does not overlap the plane's | one side is too old | upgrade the older side; `billet status` shows each host's version |
| a TLS failure that names no cause | `server.node_tls_hosts` does not name the address the node dials, including the one it dials the bootstrap port by | list every name and address in `node_tls_hosts` |
| the node claims a site the plane never declared | a typo would otherwise become a place of its own with an always-empty cache | declare the site or fix the name |
| an enrollment sits pending forever | nobody ran `billet nodes approve`, or the fingerprint did not match | compare the fingerprint the node printed with `billet nodes pending` |
| enrollment fails against a plane with no `bootstrap_listen` | its absence is a refusal | set `server.bootstrap_listen`, or issue a certificate with `billet ca issue` |
| a node that shares a name with another | two hosts under one name are one host to the plane | give each host its own name |
| `node.tls` against a loopback server | a loopback wire has no certificates | remove `node.tls` |

## Launches fail

| Symptom | Cause | Fix |
|---|---|---|
| `exec: "tart": executable file not found in $PATH` on a Mac | a launch agent does not inherit your shell's `PATH` | `billet local up` writes the agents with the right `PATH`; do not install them by hand |
| `SecKeyCreateRandomKey_ios failed` or `Interaction is not allowed with the Security Server` | Virtualization.framework needs an unlocked login keychain and a headless session leaves it locked | automatic login, and one first login through Screen Sharing |
| `billet check` fails on `untrusted_isolation` | softnet carries no setuid-root grant, or the grant was reset by `brew upgrade` | run the exact `chown` and `chmod` `billet check` prints, in that order |
| jobs on an untrusted tart tier cannot clone | softnet blocks the guest's DHCP resolver; billet configures a public resolver and proves resolution, and this is the symptom when that mechanism is stopped | check `node.tart.untrusted_dns`; the launch should have refused rather than started |
| a launch is refused because the image is not present | a Mac never pulls inside a launch | `billet images pull` first |
| a firecracker guest is missing from the inventory after a Firecracker upgrade | the jailer names its chroot after the resolved binary; billet enumerates every directory a jailer built in, so this should not happen, and if it does it frees capacity for a running guest | do not retarget the symlink under a running node without a drain |
| `@verified` refuses to boot | nothing has passed verification, or the verified generation's snapshot is gone | `billet images verify`, then `billet images list` |
| `Must not run interactively with sudo` reported as a successful CodeBuild build | the runner refuses to run as root and exits zero | billet's buildspec sets `RUNNER_ALLOW_RUNASROOT=1`; a custom buildspec must too |
| `ValidationException` from Parameter Store at staging | a standard parameter caps at 4096 characters and a JIT config exceeds it | billet asks for Intelligent-Tiering; a policy that forbids it breaks every launch |
| `command-missing` in the node log | the tier's `command` does not exist in the guest | fix the tier's command; billet does not retry it, because a retry burns the node's slot for ten minutes |

## Capacity that does not come back

| Symptom | Cause | Fix |
|---|---|---|
| `billet leases held` shows a lease in `quarantine` | its holder stopped heartbeating while something was running behind it; the capacity stays charged until the host reports | it resolves on the host's next sweep; `billet leases release --force` records your assertion instead |
| a lease in `teardown` for minutes on EC2 | `TerminateInstances` returns when the request is accepted, not when the machine stops | wait; a wedged one can be forced without stopping the node |
| a lease in `custody` with a live holder | compute billet adopted after a restart, or an ambiguous launch | `billet leases release --force` goes through the holder |
| `billet drain --wait` never proves clear | a host is off, decommissioned by force, or on a protocol too old to be asked | `billet nodes decommission`, or `--without-compute-proof`, which prints a different conclusion |
| a CodeBuild registration path shows as unswept | a node registered before wire version 18, or changed its path | upgrade the node; remove the old path's parameters by hand |

## Something looks wrong in the cache

| Symptom | Cause | Fix |
|---|---|---|
| every job runs cold at a site | the store's publication failed; on AWS the first real publication was refused by AWS for parameters a fake accepted | `billet check`; the node log names the store error |
| `billet check` refuses the Ceph cluster | it would clone the old way (`require-min-compat-client` below mimic, or `rbd_default_clone_format` set to 1 on either pool) | `ceph osd set-require-min-compat-client mimic`; remove the pool-level override `billet check` names |
| BuildKit `type=gha` fails with `x509: certificate signed by unknown authority` | a `docker-container` builder carries its own trust store the interception CA cannot reach | opt in with `url_v2=${{ env.BILLET_ACTIONS_CACHE_URL }}` and `network=host` |
| cache steps fall back to GitHub | the kill switch blocks the scope, the node listener is down, or the request shape is one billet does not serve | `billet cache enable`; a fallback never fails the job |

## Where to look

- `billet check --config <path>`: every precondition billet can test, including the GitHub credential.
- `billet status`: admission, controller, capacity, tiers, nodes, protocol versions, cost peak, unproven hosts.
- `billet leases held|quarantined|failures`: what is holding capacity and why.
- `billet local status`: what the service manager actually has loaded on this host.
- `billet host-upgrade --status` and `/var/lib/billet/upgrades/`: what an upgrade transaction is holding.
- `billet rollout status`: where each host has got to, and what it proved.
