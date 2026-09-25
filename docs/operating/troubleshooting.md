# Troubleshooting

The failure mode of this system is a job that queues rather than an error. Start with `billet check` and `billet status`, both of which work against a live deployment.

## A job sits in the queue

| Symptom | Cause | Fix |
|---|---|---|
| `billet status` shows the tier at `0 available` | billet advertises nothing for it; another tier's reservation holds the capacity, or no live node can serve it | reduce a tier, raise the ceiling, or check the node is registered and live |
| the label matches nothing | `runs-on` must equal a tier's scale-set name exactly: its `runs_on`, or its label when it has none (`billet check` lists both) | fix the label or the tier |
| the runner group grants no repository | a group with selected visibility and an empty repository list routes nothing and reports nothing; `billet check` refuses it, and a REST `PATCH` that sets visibility without re-sending repository ids clears the list | grant the repository in the group |
| the workflow is not on the group's allowlist | the allowlist includes the ref, so `…/ci.yml@refs/heads/main` does not match a run on another branch | add the exact workflow identity |
| old queued runs pile up | a backlog of runs that were never assignable keeps new dispatches queued | cancel them and dispatch fresh |
| the tier lists codebuild and names a `site` | a sited tier is confined to hosts at that site and a CodeBuild node declares none, so the tier advertises 0 with everything healthy | config load now refuses this; remove the `site` |
| a job aimed at a stopped host waits minutes | a node that died is forgotten only by silence, about four and a half minutes; a node that stopped cleanly withdraws at once | wait, or stop nodes cleanly |
| jobs on several tiers wait with room free, and the plane logs `assigned a request with no escrow to back it; declining it` | under `fair`, another tier that shares their host has waited longer and holds the line until its shape fits; if `billet status` also prints NO ADMISSION PROGRESS PUBLISHED beneath that tier, its listener has published no progress for over three minutes: either it is in a long launch, which still holds the line, or it has stalled (on 2026-09-23 a dead connection to GitHub, 18 minutes), in which case other tiers buy past it | wait for the longest waiter's shape to fit; if its listener is stalled, look for `request failed` and `connection timed out` in the server log for its target |
| GitHub cancels and requeues every five minutes on CodeBuild | the fleet is `ACTIVE` with no Mac behind it (`INSUFFICIENT_CAPACITY`), or a build is queued behind the fleet's capacity | `billet check --provider codebuild`; warm a new fleet with one build; keep `macos_vm_limit` at the fleet's capacity |

## The control plane will not start, or restarts forever

| Symptom | Cause | Fix |
|---|---|---|
| a bare `404 Not Found` on `/access_tokens`, restarting on failure | the App is uninstalled from the organization or repository, or `installation_id` is left over from a reinstall; every token billet mints is scoped to an installation | `billet check` says so, per target when there are several; reinstall the App and correct that target's `installation_id` |
| `tiers[N].target is required` at load | the config declares several targets and a tier names none | set `target` on every tier; with one target it defaults |
| `trust: trusted` (or `runner_group`, `workflows`, `intercept`) refused under a repository target | a repository has no runner groups, so no trusted pool can exist there | make the tier untrusted on a backend that admits it, or serve an organization target instead |
| a node's trusted-runner-group check is refused naming wire version 21 | the node predates version 21 and the plane serves several targets, so an untargeted request cannot say which owner's group it means | upgrade the node; a one-target plane answers an older node as before |
| on a deployment with several targets, a runner of a tier the config no longer declares cannot be removed or recovered (`names GitHub target ""`) | the plane resolves a runner's credential through the tier catalogue, and a removed tier names no target; the lease does not record one | declare the tier again until its runners are gone, or release the quarantined lease with `billet leases release --force` once the runner is proved gone on GitHub |
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

## A retirement or retained-node converge refuses

The refusal names the boundary it could not prove and the input or operation to fix. Follow that diagnosis before retrying the [retirement procedure](upgrades.md#when-a-controller-retires); removing the journal, closure status or guard marker discards recovery evidence and does not authorize ordinary work.

| Symptom | Cause | Fix |
|---|---|---|
| the node endpoint names the retiring controller | retirement cannot preserve a node that still dials the controller being removed | converge the endpoint move to the survivor first with retirement disabled, then establish matching receipt, registration and installed configuration; the failover assertion covers a DNS name, a shared address and an address held by neither controller, and records only that you assert it survives this controller's removal |
| survivor outside this play, no longer active, or missing a settled guard | earlier-play facts, a failed/unreachable host or an unsettled preparation cannot authorize the request | include both controllers in the same play and host limit, resolve the survivor's failure, and prepare both under the same holder with settled pointer-free guards |
| `billet_config still contains server on a retired host` | removing the flag beside frozen controller inventory could request recommissioning | keep `billet_server_retire: true` while that inventory contains `server`, or supply explicitly serverless configuration before removing the flag |
| `planned path traverses protected resource` or `recursive operation contains a protected resource` | a proposed path crosses controller state, archive, retirement/upgrade metadata or controller unit sources, or a recursive operation includes them | move the node destination outside the named protected path or narrow the operation; a symlink or a later `..` does not erase traversal, even through missing parents |
| an effect or unit-replacement refusal names a relationship, property or source | a node operation could affect protected controller resources, or the proposed node unit changes execution semantics the current manager graph cannot prove | remove the named unsupported relationship or redirection, or restore the supported installed definition; a stopped unit or familiar target name alone grants no permission |
| `EnvironmentFile observation or validation refused` | the installed node environment is unreadable, changed, or outside the supported path/content grammar | check the [retained environment grammar](upgrades.md#retained-node-environment-files), file readability and optionality; the role preserves the installed operand and does not repair its contents |
| `settled-entry-node-*` | activity is transitional, a process or job remains, observations are unknown, or loaded and installed unit definitions disagree | resolve the named process, job or unit condition and retry; quiet inactive/failed entry is allowed only with its complete proof and is not a drain certificate |
| settled closing refuses after node work | the current active-node, registration, path or controller-inertness proof failed | fix the reported condition and converge again; historical done/settled records and an earlier receipt do not certify this pass succeeded |
| `A retained-node host cannot run this role's binary transaction` | the converge selected a binary change on a retained host | use the [supported retained-host upgrade paths](upgrades.md#upgrading-a-retained-node), then converge with an unchanged binary |

The operation walker judges every path component, including symlink targets and hypothetical missing parents, before dependent mutation. An unreadable component, changed observation or exhausted symlink bound is could-not-tell, never an absent resource. Retained work cannot recreate the original controller identity, rewrite its units or timers, repair its metadata or recursively change their ancestors. A refusal preserves that boundary and names what the next converge needs corrected.

## Where to look

- `billet check --config <path>`: every precondition billet can test, including the GitHub credential.
- `billet status`: admission, controller, capacity, tiers, nodes, protocol versions, cost peak, unproven hosts.
- `billet leases held|quarantined|failures`: what is holding capacity and why.
- `billet local status`: what the service manager actually has loaded on this host.
- `billet host-upgrade --status` and `/var/lib/billet/upgrades/`: what an upgrade transaction is holding.
- `billet rollout status`: where each host has got to, and what it proved.
