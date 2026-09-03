# Deployment guide

Billet is pre-alpha. This guide separates the topology Billet is designed to support from the automation that is available today, so an operator can make a sound deployment decision without mistaking an open issue for a shipped feature.

## Start with the failure domains

A Billet installation has three independent parts:

| Part | What it does | What must survive |
|---|---|---|
| Control plane | `billet server` polls GitHub, schedules jobs, and owns the capacity ledger | The SQLite state, deployment identity, node-wire CA, and GitHub App credential |
| Compute fleet | One or more `billet node` processes launch jobs through Docker, Firecracker, EC2, or a future provider | Running jobs and enough replacement capacity for the availability target |
| Site storage | Firecracker nodes are designed to share Ceph images and caches; EC2 jobs use EBS and S3 | Cache data only if the deployment chooses to preserve it; the ledger remains authoritative |

A compute node is not a backup control plane. Billet currently permits exactly one authoritative controller. Likewise, a database backup does not provide compute when the local server is offline. Plan control-plane recovery and compute fallback separately.

## Choose a deployment shape

| Shape | Control plane and state | Compute | Best for | Current status |
|---|---|---|---|---|
| Local trial | Server and SQLite on one computer | Docker node on the same computer | Trying Billet with trusted workflows | `billet init` writes a runnable config whose trust policy is exercised end to end when given a non-default runner group and at least one exact workflow identity in its allowlist; the persistent lifecycle is still being built |
| Single Linux host | Server and SQLite on the server | Firecracker node on the same server, with Ceph on explicitly selected disks | An experimental owned-hardware installation | The Ansible host role is available; the machine, power, network, controller, and compute remain one failure domain |
| Hybrid local plus AWS | Small EC2 controller with SQLite on encrypted EBS | Local Firecracker first, EC2 On-Demand fallback | Organizations that want owned hardware without making local power or connectivity the CI availability boundary | The runtime pieces work and the in-repo Terraform module provisions the AWS side; end-to-end assembly is still manual, and the guided setup and composable module split are still being built |
| AWS only | Small EC2 controller with SQLite on encrypted EBS | EC2 instance per job | Linux CI without an owned server | The runtime pieces work and the in-repo Terraform module provisions the controller and fleet infrastructure; the complete guided AWS setup experience is still being built |
| Multiple controllers | Active/passive controllers with PostgreSQL and shared encrypted identity | Any fleet | Controller failover across hosts or availability zones | Mostly built. `server.controllers: active-passive` gives a standby that waits for the claim and promotes when the incumbent's database session ends, and a replaced controller is fenced out of the ledger rather than left writing beside its successor. The node-wire CA and the App key can be shared through AWS Parameter Store instead of copied. What is still the operator's is the address: every node dials one `server_addr`, so promotion needs a floating IP or a DNS change arriving at the new host |
| Managed macOS | AWS-hosted controller and state | CodeBuild reserved macOS fleet | Xcode jobs without owning Macs | Not built; planned, and subject to CodeBuild's external 36-hour build limit |
| Owned Apple Silicon | Local or AWS controller | Tart VMs on an owned Mac | macOS jobs that need owned hardware or may exceed a managed-service duration limit | Provider core built; lifecycle, host preflight and error contracts proven against real tart, while digest pinning of a pulled tag still needs its first real pull; NOT yet supported for production jobs — billet-built guest images remain. Network isolation for untrusted work (`softnet`) and the launchd host lifecycle have both landed since; what is unproven there is a fork pull request end to end, not the mechanism |

The simplest shape is not automatically the most available one. A single computer is the right default for evaluation and local use. Once CI availability matters, separating the controller from the local compute host removes the local site's power, ISP, and hardware from the scheduling path.

## A hybrid owned-hardware deployment

Assume an organization already has one Ubuntu server managed through Ansible. That machine can run both Billet roles from one configuration over loopback, store the SQLite ledger under `/var/lib/billet/server`, launch Firecracker guests, and host its Ceph pools on explicitly selected NVMe devices. A deployment playbook can install an exact Billet release, install the pinned Billet collection, and apply the host configuration over SSH. This is repeatable Day 2 configuration, but it deliberately starts after Ubuntu is installed and reachable.

That single-host shape is useful for proving the local path, but the server, controller, database, cache, power, and network all fail together. The target topology should be:

```text
                               outbound long-poll
                    GitHub <-------------------------+
                                                     |
                                      +--------------+---------------+
                                      | small EC2 control plane      |
                                      | billet server                |
                                      | SQLite on encrypted gp3 EBS  |
                                      +--------------+---------------+
                                                     ^
                                          nodes dial | mTLS over a private path
                                                     |
                          +--------------------------+-------------------------+
                          |                                                    |
               +----------+-----------+                             +----------+-----------+
               | Local Linux host     |                             | EC2 orchestrator node|
               | Firecracker + Ceph   |                             | EC2 instance per job  |
               | preferred capacity   |                             | On-Demand fallback    |
               +----------------------+                             +----------------------+
```

The controller should be a small Graviton EC2 instance with SQLite on a dedicated encrypted gp3 EBS volume and EC2 auto-recovery. Use auto-recovery rather than an Auto Scaling group: auto-recovery keeps the same instance and attached volume, while an ASG launches a new instance that does not automatically recover the ledger volume. SQLite must remain on local EBS, never EFS or another NFS filesystem. See [ADR-001](adr-001-control-plane-hosting.md).

The state volume must fail closed. Mount it by a verified volume identity, disable deletion on instance termination, retain it independently of the instance lifecycle, and make `billet-server.service` require that exact mount before startup. The packaged unit's `StateDirectory=` and Billet itself both create a missing `/var/lib/billet/server`; without a mount dependency and expected-volume preflight, a failed EBS mount could let the controller start on the root disk with a new ledger, deployment identity, and CA. Per ADR-004 the `terraform-aws-billet` module owns the dedicated ledger *volume* (and outputs its id for stable-identity mounting), while the fail-closed *mount* belongs to the configuration layer — set the `junioryono.billet.host` role's `billet_ledger_volume_id` to the module's `ledger_volume_id` output and the role does all of it: it resolves and mounts the volume by its own NVMe identity (never a filesystem UUID, which a snapshot-clone duplicates and could win at a later boot), formats it only when blank, uses a systemd mount unit with no `nofail`, adds `Requires=` on the exact mount unit plus `RequiresMountsFor=` to `billet-server.service`, proves the state directory is served by the expected volume, and refuses to shadow an existing root-disk ledger. A manual (non-role) deployment needs the equivalent systemd mount unit plus a source-identity check. EC2 auto-recovery covers supported underlying-host failures, not every instance failure or a missing filesystem mount.

How the node reaches the controller has four answers, and the right one depends on whether the node's address is stable. GitHub still makes no inbound connection; only nodes need to reach the controller's mTLS listener, and the node itself needs no open port of its own.

| Path | Use it when | Cost |
|---|---|---|
| Private network / VPC peering | the node is already inside the VPC | none |
| Restricted security-group ingress | the node has a **static** address | fails silently when that address changes |
| VPN or overlay (Tailscale, Headscale, WireGuard) or a reverse tunnel | you want the port unreachable from the internet | a component you run and keep running |
| A public node-wire port, protected by mTLS | the node's address is not stable — a machine in a spare room | the usual rate limiter any public TLS port wants, and you open the enrollment port yourself, briefly, when you add a machine |

**The last row is the one a machine with no static address needs, and it is supported now.** It works because the node wire has no unauthenticated route on it at all, and because the connections it will hold are charged only to callers that authenticated. `server.listen` is `RequireAndVerifyClientCert`: TLS 1.3, this deployment's CA as the only client-CA pool, the certificate's CN authoritative for which node is acting, and revocation checked on **every** authenticated request. A caller with nothing to present is refused during the handshake and never reaches billet's HTTP server, so it cannot occupy a connection slot an enrolled node needs.

That was not true before the enrollment routes moved to their own port, and the reason is worth knowing because it decides how you configure the second port. Two routes have to serve machines that have not enrolled yet — `GET /v1/ca` and `POST /v1/enroll` — so they cannot require a certificate. While they lived on the node wire, an anonymous caller that completed a handshake and then idled held a slot out of the same budget real nodes drew on, until the idle timeout; a few requests per second exhausted it, and once it was full new connections were never accepted at all.

So enrollment lives on a listener of its own:

```yaml
server:
  listen: 0.0.0.0:7717            # the node wire: a certificate, or no handshake
  bootstrap_listen: 0.0.0.0:7718  # /v1/ca and /v1/enroll, and nothing else
  node_tls_hosts: [billet.example]
```

**`bootstrap_listen` being unset is a refusal, not a default.** A control plane without one does not enroll over the network at all; you run `billet ca issue <node>` on the controller and copy the bundle to the host, which is the right shape for a machine you are provisioning anyway. Set it when you want a machine to be able to ask, and close the port again afterwards — nothing a running fleet does goes through it, so a node keeps working while it is shut. Saturating it delays an enrollment and cannot touch the node wire.

Enrolling then names that address. `billet ca token` on the controller prints the whole command, including the address, alongside the fingerprint from `billet ca show`:

```
billet node --enroll --ca-fingerprint <...> --join-token <...> --bootstrap-addr billet.example:7718
```

`node.bootstrap_addr` is the same thing in the node's config file. Unset, it falls back to `node.server_addr` — correct for a control plane with no separate enrollment address, and a handshake failure against one that has it.

Three things follow if you expose the port at all. `server.listen` cannot stay on loopback. `server.node_tls_hosts` must name every address and hostname a node will dial, **including the one it dials `bootstrap_listen` by** — both listeners present the same certificate, and getting that list wrong surfaces later as a TLS failure that does not name the cause. And the Terraform module outputs the controller's **private** address as `node_wire_address`, so a public posture means supplying your own name rather than using that output as-is.

**The connection budget is charged only to callers that proved who they are**, and that is the other half of the fix. It used to be taken in `Accept`, before the underlying accept and therefore before the handshake — which cannot tell an enrolled node from a stranger, because at that point nobody has presented anything. So opening sockets and sending no TLS bytes at all spent the fleet's capacity, and while it was full `Accept` blocked ahead of the kernel accept and a node's connection sat in the backlog until its own dial timeout. The listener now accepts unconditionally, bounds handshakes separately, and charges the fleet's budget only for a connection that verified against this deployment's authority.

**What that does and does not promise, stated precisely.** An admitted connection is untouchable: nothing that has not completed a handshake holds any of the budget a node occupies, so no volume of anonymous traffic can displace a node that is connected. A *handshake slot* is best effort — before a handshake the two are indistinguishable, so an attacker sustaining enough connections per second can still make a node's handshake be refused. It is refused immediately and the node redials, rather than waiting in a backlog nothing will ever read, which is the difference between a service under load and a fleet that is offline.

That residual is not closable in userspace and every TLS server has it. Front a public port with a rate limiter or connection-tracking firewall for the same reason you would any other internet-facing TLS listener — not because billet's authentication is weak, and no longer because a trickle of well-formed HTTP can take the fleet down.

What billet also does not do is rate-limit the enrollment port. Its own availability, while it is open, is your firewall's job; the fleet's is not.

The EC2 fallback can run as an orchestrator node beside the controller or on a separate small instance. It does not execute jobs on that host; it launches one isolated EC2 instance per job and therefore needs explicit vCPU and memory budgets, an IAM role, subnet and security-group policy, and region-scoped AMIs. Co-locating it with the controller is operationally smaller; separating it gives independent process and IAM boundaries. Neither choice changes controller availability because no new work can be scheduled while the controller itself is down.

Use a single tier with ordered providers so workflows keep one stable label:

```yaml
tiers:
  - label: billet-8vcpu-ubuntu-2404
    providers: [firecracker, ec2]
    trust: untrusted
    vcpu: 8
    memory: 32GiB
    disk: 160GiB
    launch:
      firecracker:
        image: ubuntu-2404-x64@verified
      ec2:
        image: ami-0123456789abcdef0
        command: [/usr/local/bin/billet-runner]
```

Provider order is policy: Billet fills Firecracker capacity first and uses EC2 only when the local node cannot accept the job. Each node still runs one provider. The control plane chooses the provider and host before it accepts the job.

The explicit `trust: untrusted` also makes the network requirement visible: every Firecracker node serving this tier needs a separate `node.firecracker.untrusted_bridge`, and every EC2 node needs `node.ec2.untrusted_security_group_ids` that cannot reach production or control-plane networks. Without those settings the providers correctly refuse the work. A trusted alternative requires a non-default `runner_group` and an exact `workflows` allowlist; changing only the word `trust` is not sufficient authorization.

## Which fallback compute should a hybrid deployment use?

| Option | Advantages | Costs and limitations | Recommendation |
|---|---|---|---|
| EC2 On-Demand | Available today, isolated instance per job, survives the local site's power or ISP outage, and has no provider-imposed job-duration ceiling | Roughly 48–59 seconds measured cold start, cold cache during failover, full On-Demand price, and the current Billet drain limitation described below | Default fallback for important Linux jobs |
| EC2 Spot | Lower compute price and the same workflow compatibility | AWS may reclaim the instance; GitHub does not automatically requeue a job that already started, so interruption fails the build | Use only for explicitly retryable, non-critical work; never the default availability path |
| Second local Firecracker server | Fast warm capacity, owned-hardware economics, and shares the site cache with every node that can reach the same Ceph cluster — proved on real hosts in [`site-acceptance.md`](site-acceptance.md) | A host in the same building still shares power, ISP, and often storage failure; another site needs its own storage and network design | Good capacity or hardware-failure protection now that same-site sharing is proved; a real outage fallback only when placed in a different failure domain |
| CodeBuild Linux or macOS | AWS manages hosts, logs, IAM/VPC integration, and macOS M2 capacity | Not implemented, macOS is reserved capacity, and CodeBuild imposes a 36-hour maximum | Attractive future managed option, especially for macOS; not a current deployment choice |
| Tart on an owned Mac | Owned Apple Silicon, local/private placement, and no external 36-hour service limit | Provider core implemented (launch/inventory/teardown, `billet check` preflight); still needs billet-built guest images; softnet isolation and the launchd host lifecycle have landed, and what is unproven is a fork pull request end to end | Future choice for long-running or owned-hardware macOS workloads |

For important Linux jobs today, EC2 On-Demand is the correct fallback. The cache boundaries it relies on are measured in [`site-acceptance.md`](site-acceptance.md): two nodes at one site reuse one generation, a second site starts cold and cannot reach the first site's store, and the same label falls back across sites. Add a second local server when the goal is more capacity or resilience to one physical server failing, and place it at another site when the goal is resilience to a local power or connectivity outage.

## Control-plane state and recovery

There is no separate database service in the recommended design. SQLite is embedded in `billet server` and lives in `server.state_dir`. The state directory also contains deployment identity and node-wire authority material, so restoring only `billet.db` is not a complete deployment restore.

A PostgreSQL ledger is also supported, for a deployment that wants the controller to be disposable rather than restored. It changes what recovering the controller involves and nothing else — one controller either way, the same ledger shape, the same invariants. See [state-backends.md](state-backends.md) for the choice and what each side costs, and [adr-008](adr-008-state-backends.md) for why it is built the way it is.

A recoverable backup unit must include:

- The SQLite ledger produced by a supported online backup or a safely stopped controller.
- The deployment identity and current/previous node-wire CA state.
- The GitHub App private key and the configuration that names its App and installation.
- The exact Billet configuration and a record of the binary version that wrote the schema.

Cache data is a separate decision. Firecracker guest images can be pulled and verified again, and build caches can repopulate; neither should be allowed to complicate restoration of the authoritative capacity ledger. Node-local custody state also must not be restored blindly from an old backup while compute may still exist.

Restoring an older controller snapshot while compute from after that snapshot still exists is destructive: the restored ledger has no lease for those jobs, so node recovery treats their instances as orphans and destroys them even though GitHub will not requeue work that already started. Fence every node from the restored controller first, inventory compute created after the backup, and either let it finish while isolated or terminate it while explicitly accepting the job failures; reconnect nodes only after that compute is proved gone. Billet does not yet provide a coordinated, lossless disaster-restore workflow.

`billet local backup --out <dir>` captures that unit, and it runs against a **live** control plane: the ledger copy is SQLite's `VACUUM INTO`, which is a consistent snapshot of a database somebody is writing to, so `billet.db-wal` need not travel beside it. It writes a 0700 directory of 0600 files — a manifest, the ledger snapshot, the deployment identity, the App key, the whole node-wire authority, and a reference copy of `billet.yaml` — and refuses a destination inside the state directory. It takes the same lock `billet ca rotate` and `billet ca retire` take, so it can never capture a key from one authority generation beside a certificate from another; if a rotation is running, `ca-previous.key` travels too, because that key is what signs the certificate the control plane presents until every node has renewed. An authority missing half a pair, or missing its `authority-created` marker, is a refusal rather than a shorter archive.

`billet local restore --from <dir>` puts it back, **as one unit or not at all**. Every piece is either absent (installed), byte-identical (already done), or different (preserved and refused) — there is no flag that overwrites one, because a deployment holding one installation's ledger beside another's authority starts, looks healthy, and cannot see the compute either of them launched. In particular a **different App key is never replaced**: GitHub issues it once and will not reissue it. `--dry-run` reports exactly what would be installed and every reason it would be refused, through the same code path the real restore takes.

The one thing it replaces is the schema-only `billet.db` a preflight `billet check` leaves on a host nobody has commissioned — and only after proving that directory has no deployment identity, no authority, and no row in any table a deployment writes to.

**What no lock on the target machine can establish is that the OLD controller is gone.** Restoring here while the controller the backup came from can still start anywhere produces two authoritative controllers sharing one identity, one certificate authority and one App credential, with ledgers that diverge from the moment both are up. `--old-controller-fenced` is the operator asserting they have stopped **and disabled** it everywhere; without it the restore refuses. Locally, billet does prove three things: it takes the state directory's lock (no control plane holds it), writes the maintenance fence (no handle can transact, including one an operator command already has open), and takes a writer barrier (any transaction that began before the fence has finished).

A restore that is interrupted leaves the state directory **fenced** and a `restore.journal` beside it, so nothing can start a control plane on a half-restored deployment. Re-run the same command to continue from where it stopped, or add `--abandon` to remove only the files that run recorded having created — and only those it can still prove are copies of what the backup holds.

**A restore run as root hands what it wrote to the service account**, and prints each path as it does. That is not tidiness: on a packaged host the restore *must* run as root, because the App key lands in root-owned `/etc/billet` — and systemd's `StateDirectory=` repairs ownership recursively only when the top directory's owner is wrong, which after a restore it is not. Before this, every restored file stayed root-owned inside a `billet`-owned directory and the control plane could not open one of them. See [the rehearsal](restore-rehearsal.md), which is what found it.

**The restore is now rehearsed on every pull request**, in two legs: an end-to-end test that restores a deployment onto a directory that has never seen it and proves the restored control plane serves the node that trusted the old one, and `make restore-rehearsal`, which does the same with the real Debian package and the real service account on a Linux host. What neither leg covers — a real GitHub App, the systemd units, `billet local up` — is written down in that document rather than left to be assumed.

### Getting the archive off the disk it protects

`--out <dir>` is the right primitive and it is not a strategy: one disk failure takes the deployment and its archive together. The directory remains the contract — its manifest carries a digest and a size for every entry precisely so that restic, rclone, a NAS or whatever you already run can carry it and verify what it carried — and leaving `backup:` out of the config is a supported answer rather than an omission.

What setting it adds is the half that matters on the day you use it. With `backup.s3` configured, `billet local backup` uploads what it just wrote, and `billet local restore --from-backup latest` fetches, verifies and restores on a machine that holds nothing but the billet binary. Upload-only would have you installing a second tool during an outage, and an `s3://` URL could not have replaced the config anyway: it carries no region and no endpoint, and SigV4 signs a region into every request.

Five properties are worth knowing before relying on it:

- **billet never deletes.** `internal/archivestore` has no delete operation at all, and the IAM grant the Terraform module attaches carries none — so the credential on the one host that also holds the App key and the node-wire CA cannot destroy the copies whose whole purpose is surviving the loss of that host. Retention belongs to the bucket: versioning plus a lifecycle rule on **noncurrent** versions, because a rule that expired current objects would remove backups on a timer.
- **Every write refuses to replace.** `If-None-Match: *`, the same no-clobber shape every credential publication in billet uses.
- **The manifest is uploaded last**, and its presence is what makes a prefix an archive. An upload interrupted anywhere leaves entries that nothing will offer you as a backup — `billet local restore --from-backup` lists only complete ones, and `deployarchive.Open` refuses a directory without a manifest.
- **The local archive is written and verified first, always.** A failed upload says so and says the directory is still complete; it never leaves you with neither.
- **The credential is the fleet's**: environment variables or IMDSv2, and nothing else. On the recommended AWS control plane that is an instance role, so nothing new is stored on the host. `endpoint` addresses an S3-compatible store — Ceph RGW, which the reference hardware already runs, or MinIO or R2 — path style, and https unless it is loopback.

`billet check` reports the newest archive in the bucket and how old it is. That line exists because the failure is silent by construction: a timer that stopped firing looks exactly like one that is working, and an unwatched backup is no more a backup than an untested one.

The packaged units include `billet-backup.service` and `billet-backup.timer` (daily, randomised, `Persistent=true`). Neither is enabled by the package, and neither is managed by `billet local up`/`down`/`status` — those own the two services that hold compute and a ledger, and their whole safety content is the order they act in. `systemctl enable --now billet-backup.timer` is the operator's decision. Nothing in either unit deletes a local archive: each holds a copy of the App key GitHub issues exactly once, so pruning `/var/lib/billet/backups` is deliberate work.

### Putting a deployment back over itself

`billet local restore` refuses a commissioned deployment, and that refusal is right: its ledger is a live capacity record, and replacing it loses the record of the compute it is tracking. What it leaves out is the ordinary disaster shape — the controller is sick, you have yesterday's archive, and the ledger on disk is not one you want to keep. Until `billet local recover` existed the documented path there was to move the state directory aside by hand, which works and gives you no help deciding whether it is safe.

`billet local recover --from <dir>` (or `--from-backup latest`) is that operation, and it is a separate command rather than a flag on restore because a commissioned deployment's ledger is a live capacity record. The order it acts in is the whole of its safety:

1. It refuses unless the archive is **this host's own deployment** — the identity on disk must be the one in the manifest, which is the same check a restore makes. It can never relabel a host.
2. It **seals** the deployment and waits for `alloc.Quiescence` to report it holding nothing. A seal only stops new work being *admitted*; what decides is the barrier, which is what `billet drain --wait` asks.
3. If work is still outstanding it **refuses and names it** — tier, node, phase, age and run id — unless you pass `--accept-failing-jobs`. Those jobs fail, and GitHub does not requeue a job that has already started. What the barrier cannot see is said out loud too: compute whose lease has already gone is not in that list.
4. It requires `--old-controller-fenced`, for the reason restore does.
5. It replaces the ledger and **renames the old one to `billet.db.superseded-<taken-at>`**. That file is never deleted: it is the only record of the work this operation failed.
6. It **seals the deployment again**, because the restored ledger carries the admission it had when the backup was taken — open. Without that last step the recovered control plane would take new work the moment it started, while its nodes still hold compute it has never heard of. The seal is an operator's, so `billet local up` does not clear it; `billet resume` is you saying that compute is proved gone.

An interrupted recovery leaves the directory fenced with a journal, exactly as a restore does, and **re-running the same command is the way forward from every point it can stop at**. The journal records how far the run got, so a retry after the files were all published skips straight to sealing and lifting the fence rather than planning a second supersede onto a name the first one already took. If `--from-backup` fetched the archive, the retry reuses the copy it kept rather than refusing it as a non-empty directory.

`billet local recover --abandon` is the other direction, and it **puts the superseded ledger back** — an abandon that only removed what the run created would take away the ledger it installed and leave your own under a name nothing looks at. It refuses rather than guessing whenever it cannot reassemble that ledger and its write-ahead log, and it refuses to lift the fence if it would leave the directory with no ledger at all; in both cases nothing is moved, the fence stays up and the journal stays put, and the message names the two files you have to choose between. It also refuses an operation that had already finished: at that point every file is in place and the deployment is sealed, so undoing it would remove the restored credentials and bring back the ledger you chose to replace. Re-run without `--abandon` to clear what such a run left behind.

Still true, and still the operator's problem: reconnecting nodes to a restored controller. That ledger has no lease for compute created after the backup, so node recovery treats those instances as orphans and destroys them, and GitHub does not requeue a job that already started. Fence the nodes, inventory that compute, and prove it gone first. Do not copy a live SQLite file by itself and do not put the state directory on EFS/NFS.

### Moving a control plane to another host without losing its identity

The state directory holds the deployment identity and the node-wire CA, so a control plane that starts on an empty one does not *fail* — it creates a **new deployment**. That is indistinguishable afterwards from a genuinely new install, and every node still trusting the old CA stops being able to authenticate.

That makes the move awkward in a way worth stating plainly: the state has to arrive from the old host *after* the new host's ledger volume is mounted and proved, but *before* anything starts a server on it. `billet_server_prepare_only: true` is that stopping point. With it the host role formats, mounts and proves the ledger volume, installs the units with their mount fence, and leaves **both `billet-server` and `billet-node`** installed, disabled, and refused by an assertion in their own units — so nothing can mint an identity, including across a reboot, until an operator has put one there.

The node is held for the server's reason rather than its own: on a host running both roles, whichever role starts first mints the deployment identity — for a node, when it has no certificate bundle of its own to read one from, and `billet node` claims the deployment before it reaches its upgrade probe. A prepare-only host that held only the server would have its identity minted by the node instead.

The order:

1. **Stage the existing GitHub App key first.** The role refuses a converge when `github.private_key_path` is the path it manages and no key is installed or supplied, so the prepare-only converge cannot run without it. Copy the key off the old controller and pass it as `billet_github_private_key_src`. It is the *same* key — a control plane that mints a new one is a different deployment, which is the thing this sequence exists to avoid.
2. Converge the new host with `billet_server_prepare_only: true`. Its ledger volume is now mounted at `server.state_dir`, and neither the server nor the node can run.
3. Drain the old control plane and wait for the barrier. Treat an interrupted `billet drain --wait` as *not drained* — it exits 2, and reading that as permission to continue destroys running jobs.
4. Stop the old controller and **disable** it, so a reboot cannot resurrect it. Two controllers on one identity is the failure this whole sequence exists to avoid.
5. Copy the whole stopped state directory, including the SQLite `-wal` and `-shm` sidecars. The App key is already in place from step 1; if you copy it again it must be byte-identical. Note the directory will not be empty: a preflight `billet check` creates `billet.db` and its schema. It does **not** create a deployment identity or CA, so what proves the host is still uncommissioned is the absence of *those*, not an empty directory. Check for the deployment identity specifically, and copy the old state over what the preflight left.
6. Point the nodes at the new endpoint. A remote `node.server_addr` requires node TLS and the certificate must be valid for the new name (`server.node_tls_hosts`); preserve the CA bundle.
7. Converge again with `billet_server_prepare_only: false`. That removes the hold and starts exactly one controller, on the copied identity.

To roll back, disable the new controller before re-enabling the old one, in that order.

**Do not reach for `systemctl mask` here.** Masking works by writing a `/dev/null` symlink at `/etc/systemd/system/<unit>`, and that is exactly where the role renders `billet-server.service` — so systemd refuses (`Failed to mask unit, file … already exists`). The unit assertion is what holds the host; disabling is what keeps it from starting at boot.

On a PostgreSQL ledger a second controller can now be a **standby**. Write `server.controllers: active-passive` on both hosts and whichever takes the claim first is the controller; the other reports itself ready, says in `systemctl status` which machine holds the deployment, and takes over when that machine's database session ends. It then runs the ordinary startup — clearing the fleet's liveness, reading the node-wire authority, opening its listeners — and the jobs already running are re-adopted by their nodes. A standby's ledger handle cannot write at all until it claims, so it does nothing authoritative while it waits. Without that key a second controller is still refused loudly at startup, which is the right answer for two units misconfigured as active. See [State backends](state-backends.md).

**The node-wire CA and the App key can be shared rather than copied.** `server.identity.backend: aws-ssm` puts both in AWS Parameter Store as SecureStrings under one prefix, so a controller that starts with an empty `ca` directory adopts the deployment's authority instead of minting a rival one and dropping the whole fleet. It is replication rather than a shared authority — the files stay the source of truth on each host — so `billet ca rotate` publishes what it produced and the other controller takes it with `billet ca sync`. A host holding a different authority is refused rather than overwritten. See [State backends](state-backends.md).

**The address is still yours.** Every node dials one `server_addr`, so promotion needs a floating IP, a DNS change or a load balancer arriving at the new host. And the `deployment-id` file has to be the same on both — billet proves that rather than assuming it, because the ledger records which deployment it belongs to and refuses a controller whose identity directory disagrees.

On a SQLite ledger there is no shared state to arrange any of this with, and `controllers: active-passive` is refused there. The recovery mechanism remains EC2 auto-recovery of the same instance and EBS volume.

## Terraform, Ansible, and Billet each own a different layer

| Tool | Owns | Does not own |
|---|---|---|
| Terraform | AWS instances, EBS, VPC/subnets, security groups, IAM, KMS, S3, DNS, monitoring, and eventually RDS/CodeBuild | Live jobs, leases, forced teardown decisions, or fingerprint approval |
| Ansible | Packages, users, files, systemd services, Firecracker, Ceph client/bootstrap, bridges, Billet configuration, validation, and host upgrades | Buying cloud resources or deciding whether a running job may be destroyed |
| Billet | GitHub polling, capacity escrow, placement, node identity, runner lifecycle, custody, and drains | Creating the server's operating system, VPC, disks, or general host configuration |

Terraform is appropriate for the EC2 controller and cloud fallback, even though Billet does not have a Terraform provider. A Terraform provider would imply CRUD ownership of Billet tiers and nodes, but Billet has no configuration API and a node is a self-registering runtime identity rather than a remote object. [ADR-004](adr-004-terraform-provider.md) therefore defers a provider in favour of ordinary infrastructure modules and the complete setup experience.

The intended AWS flow is for Terraform to create or adopt infrastructure and return narrow outputs such as private addresses, secret references, volume ids, and an Ansible inventory. Cloud-init or Ansible then installs and configures Billet. Terraform must not use resource deletion, `remote-exec`, or a timeout as implicit authorization to kill active work. Today, protect stateful resources from replacement, quiesce workflow admission outside Billet, independently verify that no jobs remain, and only then explicitly authorize a Terraform replacement or destroy. The planned durable drain should eventually become that separate lifecycle gate; it is not available now.

**All three layers name the same version.** A release carries the binary, the Ansible collection and the Terraform module, so `billet_version: vX.Y.Z`, `requirements.yml` and every `?ref=` in a module source should read the same tag — and the READMEs inside a release already do, because cutting one rewrites them from `main` to that tag. This matters for the same reason the host role refuses `billet_version: latest`: a moving target makes a converge non-deterministic and drives a real drain and restart on a day nobody chose. Note that every release up to and including `v0.3.26` predates the `terraform/` directory and carries none of it, so a module pin resolves only from the first release cut after the module was added to the release gate.

Terraform usually should not manage an existing physical server. Unless a hardware or virtualization provider owns that machine's lifecycle, there is no useful Terraform resource to create. Keep the server in Ansible inventory and let Ansible converge its operating-system state over SSH.

## How Ansible deployment works

Ansible is agentless. It runs on an operator workstation or CI job, reads an inventory of target machines and deployment variables, connects to each target over SSH, and applies idempotent roles. The target generally needs a reachable SSH service, Python, and an account that can use `sudo`; no long-running Ansible daemon is installed.

For an organization with an existing local server, the flow is:

1. Install Ubuntu and make the host reachable. This Day 0 step is intentionally outside the playbook because disk selection, firmware, networking, and remote access are machine-specific.
2. From the operator workstation or deployment repository, install the pinned Billet collection, download and verify the pinned Billet binary or build a supplied checkout, set `billet_binary_src`, and invoke the deployment's playbook. The [collection documentation](../ansible_collections/junioryono/billet/README.md) contains the generic invocation.
3. Ansible compares the declared roles and variables with the host, then installs or updates what differs. Re-running the playbook converges the same machine instead of replaying a one-time installation script.
4. Use `ansible-playbook --check --diff` where supported to inspect intended changes, then apply and verify the service and Billet status.

The current `junioryono.billet.host` role supports Linux Firecracker hosts and can independently enable the server and node services. Each organization supplies its own inventory and playbook. The role does not create the GitHub App, guess capacity, choose disks, perform Day 0 operating-system installation, or provide the AWS infrastructure modules described above. The cross-platform `development_host` role configures development TLS/proxy tooling; it is not a Tart compute-host installer.

## Reaching your hosts to converge them

**Billet needs no inbound connectivity to work.** Nodes always dial outbound to the control plane, and the server never dials out except to GitHub — so a node behind NAT, on a home or office connection, with no forwarded ports and no public address, is the ordinary case rather than a workaround. Nothing in this section is required to run jobs.

What it is about is *configuration management*: how the machine running `ansible-playbook` reaches the hosts it converges. That is a separate question from how Billet schedules work, and it has a different answer for different deployments.

| Route | Prerequisites | Third party | Best for |
|---|---|---|---|
| [A workstation on the same network](#route-a-a-workstation-on-the-same-network) | None | None | One or two hosts you can already reach. The default. |
| [AWS Systems Manager](#route-b-aws-systems-manager) | An AWS account, an S3 bucket, a hybrid activation | AWS | Fleets already administered from AWS |
| [Cloudflare Tunnel](#route-c-cloudflare-tunnel) | A Cloudflare account and a domain on it | Cloudflare | Hosts across several sites, with no ports opened |

A converge restarts Billet's services, which drains the node. **Whatever route you choose, do not drive a converge from a runner Billet itself manages** — the drain destroys the jobs running on that host, including the one running the playbook, and GitHub does not requeue a job whose runner vanished. The role refuses this by default; `billet_allow_converge_from_billet_runner` exists for an operator who has established that the runner is backed by some other host.

### Route A: a workstation on the same network

Ansible is agentless, so this needs nothing installed anywhere: a machine that can already reach the host over SSH runs the playbook. For a single server on your own network this is the whole answer, and the rest of this section is unnecessary.

```bash
ansible-galaxy collection install junioryono.billet
ansible-playbook -i inventory.ini site.yml --check --diff   # inspect
ansible-playbook -i inventory.ini site.yml                  # apply
```

Its limits are worth stating rather than discovering: there is no audit trail of who converged what, no review gate before a change reaches a host, and it needs your machine to be on and reachable. Those are process properties, not technical ones, and for many deployments they do not matter.

**If you want converges in CI without solving reachability at all**, put a small always-on machine on the same network and register it as an ordinary GitHub Actions runner — one Billet does not manage. It dials out to GitHub like any runner, picks up the converge job from inside the network, and reaches the hosts over the LAN. This needs no overlay, no tunnel, no cloud account, and no inbound ports. It must not be a Billet-managed runner, for the reason above.

### Route B: AWS Systems Manager

Ansible connects through SSM instead of SSH, and CI authenticates to AWS rather than holding an SSH key. No inbound ports are opened; the SSM agent dials out.

**Read this before choosing it.** The `amazon.aws.aws_ssm` connection plugin requires an S3 bucket, and its own documentation is explicit that this is not optional: files transit S3 *even for modules that send no files*, because Ansible ships the module's own `.py` through it. The same documentation warns that secrets in a task's arguments are written into those objects in plaintext, and recommends a bucket with versioning suspended.

**The Billet host role installs a GitHub App private key.** On this route, that key transits the bucket. Suspended versioning and a short expiry keep it from persisting; versioning left enabled preserves a copy indefinitely, readable by anyone with access to the bucket. Treat the bucket as a credential-bearing resource and scope its policy accordingly.

Because these defaults are easy to get wrong by hand, they are provisioned as a module rather than described as a checklist:

```hcl
module "converge" {
  source = "github.com/junioryono/billet//terraform/modules/converge-aws-ssm?ref=v0.5.0"

  name               = "billet-converge"
  github_repository  = "your-org/your-infra-repo"
}
```

It creates the hybrid activation and its IAM role, the transfer bucket with versioning suspended and public access blocked, an IAM policy limited to the operations the plugin performs and to sessions on this deployment's own nodes, and a GitHub OIDC role so CI needs no long-lived AWS keys.

The OIDC trust matches an **exact** subject — by default `repo:<your-repo>:ref:refs/heads/main` — so the converge workflow must run on that branch. A wildcard would admit any pull-request job in the repository, and this role reads the bucket the App key transits. An *environment* subject does not close that: GitHub emits it whenever a job references an environment, whatever triggered it, so a pull request declaring one still matches.

Your hosts are not EC2 instances, so each needs the SSM agent registered against that activation — a hybrid activation, which is off the path most SSM documentation describes. The module's README carries the registration command and the inventory variables the connection plugin needs.

### Route C: Cloudflare Tunnel

`cloudflared` dials out from each host and CI reaches them through Cloudflare Access with a service token. No inbound ports, no S3, and no plaintext-secret path — a materially simpler secret story than Route B.

The prerequisite is real: **you must own a domain on Cloudflare**, because the tunnel is addressed by hostname. Each host also runs another daemon, and Cloudflare sits in the path between CI and your infrastructure.

```hcl
module "converge" {
  source = "github.com/junioryono/billet//terraform/modules/converge-cloudflare?ref=v0.5.0"

  account_id = var.cloudflare_account_id
  zone_id    = var.cloudflare_zone_id
  hostname   = "billet-host-1.example.com"
}
```

It creates the tunnel, its ingress mapping the hostname to SSH, its DNS route, an Access application with the CI policy attached, and a service token. The host side is a `cloudflared` install and the tunnel credential, **installed out of band** — the Billet host role does not manage `cloudflared` today.

### What is deliberately not here

**A self-hosted mesh** (Headscale, WireGuard, or similar) is a reasonable choice for a fleet across several sites, and is not documented as a getting-started route: it means running and maintaining another control plane, and for one or two hosts a deploy runner on Route A achieves the same thing with nothing new to operate.

**Any of these as a requirement.** All three are conveniences for configuration management. Billet runs jobs without them.

## Installation and maintenance workflow

For the current experimental hybrid deployment:

1. Provision the small EC2 controller, the dedicated encrypted ledger EBS volume, private connectivity, IAM, monitoring, and the auto-recovery alarm — the `terraform-aws-billet` module now does this. Then the fail-closed state MOUNT described above is a configuration-layer step the `junioryono.billet.host` role performs when `billet_ledger_volume_id` is set to the module's `ledger_volume_id` output. Secrets stay out of Terraform state.
2. Install the exact same Billet release on the controller and every node, and build the region-specific EC2 runner AMI.
3. Create or adopt the GitHub App, write the complete tier catalogue with ordered local and cloud providers, issue or enroll a unique certificate for every node, and run `billet check` against each finished configuration. Tiers are read when the server starts; there is no dynamic tier-enable operation.
4. Start `billet server`, then start the local Firecracker and EC2 orchestrator nodes. Node registration is dynamic, so adding a node later does not require a control-plane restart; changing tiers or node policy does.
5. Exercise local execution, withdraw local capacity, exercise EC2 fallback with the unchanged workflow label, and verify that all job instances disappear afterward.
6. Rehearse controller recovery and state restoration before calling the deployment recoverable.

Publishing a Billet release does not modify a running installation or interrupt jobs. Installing a new binary and restarting services does. The current protocol and drain implementation are not yet the seamless update model: mixed server/node versions cannot negotiate, and the current six-hour default/24-hour maximum drain can destroy jobs still running when it expires. There is no race-free CLI maintenance gate today: `billet leases` omits healthy running jobs. Until the rollout contract lands, treat all Billet processes as one pinned version, quiesce workflow admission outside Billet, and independently confirm no jobs are running before updating. Do not use a repeated SIGTERM as an update shortcut because it authorizes destructive teardown in the current implementation.

The intended lifecycle is a signed stable channel that updates compatible controllers and nodes without edits to workflow files or Terraform variables, drains nodes one at a time for incompatible changes, waits indefinitely for active jobs, and rolls back a failed binary. The complete compatibility, rollout, and non-destructive drain contract is still being completed. Exact version pins will remain available for operators who prefer explicit promotion.

## Setup roadmap

The remaining deployment work is grouped by user-facing capability:

- Complete user-facing local and AWS setup, with composable Terraform infrastructure modules and hosts configured and verified by Ansible.
- Physical same-site cache sharing and cross-site isolation acceptance.
- sqlc/file-backed migrations, SQLite and PostgreSQL state, and fenced active/passive controllers.
- Seamless automatic upgrades, including rolling compatibility, indefinite non-destructive drains, restart-safe controller intake, and signed release channels.
- Managed AWS macOS compute. Owned Apple Silicon compute is delivered; what remains there is billet-built guest images and an Apple Silicon cache.
