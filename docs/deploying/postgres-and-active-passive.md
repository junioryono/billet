# PostgreSQL and active-passive controllers

billet's control plane keeps a ledger: the capacity record, node registrations, custody, job history, rollouts. This is where it lives, and what each choice costs you.

The decision is smaller than it looks. **Both backends run exactly one controller at a time**, both hold the same invariants, and the ledger's shape is identical on either — the same 47 migrations, the same query set. What changes is where the rows are, and therefore what recovering the controller involves.

## The two profiles

| | SQLite | PostgreSQL |
|---|---|---|
| Ledger | a file in `identity_dir` | a database you operate |
| Extra to run | nothing | a PostgreSQL server |
| Recovering the controller | restore the state directory onto local storage | rebuild the machine; the ledger is already elsewhere |
| Ledger backup | `billet local backup` takes it with the identity, CA and App key in one archive | yours — `pg_dump`; `billet local backup` still takes the identity, CA and App key |
| A second controller is stopped by | the exclusive lock on the state directory | that, **and** a claim in the database |
| A replaced controller is stopped by | the same lock, which it never lost | the claim's epoch, which every write re-reads |
| Binary | — | +5.24 MB, carried by every node too |
| A standby that takes over | — | `controllers: active-passive` |

**Start with SQLite.** It is the default, it is what `server.state_dir` means, and it is what ADR-001 recommends for the small controller instance: the data is small and hot, and a dead controller delays CI rather than failing it because GitHub queues a job for 24 hours. Nothing about it is a stepping stone.

**Move to PostgreSQL when you want the controller to be disposable** — when "restore this directory onto local storage" is not a sentence you want in your recovery plan, or when the database is already something your organisation backs up and monitors and the state directory is not.

## SQLite

```yaml
server:
  state_dir: /var/lib/billet/server
```

One directory holding the ledger (`billet.db`), the deployment identity, the node-wire CA, the process lock and the maintenance fence.

**It must be on local storage.** SQLite's write-ahead log coordinates through shared memory that assumes a single host, so it cannot work on NFS or EFS — and billet reads the setting back at startup and refuses to serve rather than running without the durability it asked for. That check is what catches a state directory somebody moved onto a network mount.

## PostgreSQL

```yaml
server:
  identity_dir: /var/lib/billet/server
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_STATE_DSN
```

```
BILLET_STATE_DSN=postgres://billet:...@db.internal:5432/billet?sslmode=verify-full
```

**`identity_dir` is still a local directory, and it still matters.** The deployment identity, the node-wire CA and its rotation state, the process lock and the maintenance fence do not move into a database — a private key does not belong in a table, and local process coordination has nothing to do with SQL rows. Only the ledger moves. Back that directory up.

**The DSN is named, not written.** billet reads the connection string from the environment variable you name here, because it carries a password and a secret written into a config file ends up in a backup, a paste buffer, and eventually a support thread. It is the same rule the GitHub App private key follows.

### Generating it, and getting it onto a host

`billet init --state-backend postgres --state-dsn-env BILLET_STATE_DSN` writes the shape above rather than `state_dir`. The two spellings are mutually exclusive at load, so this is a choice the generator makes rather than a value it can add beside the default — and `--state-dsn-env` is required, because a postgres config with nothing to read the connection string from is a control plane that starts and finds an empty data source.

**The `junioryono.billet.host` role delivers the variable, and does not put it in `billet.yaml`.** Set `billet_server_environment` and the role writes `/etc/billet/server.env` (0640 root:billet) and names it as an `EnvironmentFile=` in the unit it renders:

```yaml
billet_server_environment:
  BILLET_STATE_DSN: "{{ vault_billet_state_dsn }}"
```

**Not a systemd drop-in**, and that is not a style preference: the transactional host upgrade *refuses* an effective drop-in on a billet unit, because it cannot replace and recover an overlay — so a DSN delivered that way breaks every later upgrade of that host. The file itself is deliberately outside the upgrade transaction (it is a credential, not something a new binary changes), so introducing it in the same run as a new binary is refused with a message saying to converge once first.

On AWS, `terraform/modules/billet/modules/state-rds-postgres` provisions the database and hands over the ARN of AWS's own managed credential plus the IAM statement that reads it — never the password, which would put it in the Terraform state file. `modules/control-plane-postgres` is the controller for that profile: the same instance and auto-recovery alarm as the SQLite one, and **no ledger volume**, because a volume pins the instance to an availability zone and undoes the replaceability this profile exists for. See `examples/postgres`.

### What billet needs from the database

- **A schema of its own.** billet's catalogue questions are scoped to `current_schema()`, and two billet deployments sharing one PostgreSQL need two schemas (or two databases). Point the DSN's `search_path` at it.
- **`synchronous_commit` not `off`.** With it off, PostgreSQL acknowledges a commit before the record is on disk, so a crash can lose scheduling decisions billet has already acted on. billet checks this at startup and refuses.
- **A role that can create tables** in that schema, because billet migrates it.
- **PostgreSQL 13 or later.** Nothing here needs anything newer; the test suite runs 18.

billet does **not** need any extension.

### What billet will not do for you

**It does not back up the ledger.** A consistent copy of a PostgreSQL database is `pg_dump` or your provider's snapshot, both of which are yours to run and to restore — and a half-measure that copied rows out through billet's own connection would produce an archive that *looks* like a backup and is not.

**It does back up everything else**, and you should let it. `billet local backup` writes an **identity-only archive** here: the deployment identity, the node-wire CA and its rotation state, and the GitHub App private key, with a manifest recording that the ledger is external and naming the engine and the `dsn_env` variable. It reports what it did:

```
backup   /var/backups/billet/2026-09-03T014500Z
         deployment 8f2a…
         the ledger is postgres and is NOT in this archive; your database's own
         backup is the other half
```

**This matters more than it sounds.** The App private key is issued exactly once, a node-wire CA no replacement controller has is a fleet whose certificates mean nothing — and the `control-plane-postgres` module has **no ledger volume by design** (a volume pins the instance to one availability zone, which is exactly what makes the SQLite controller un-replaceable) with a root volume that is `delete_on_termination`. On that host the archive is the only copy of the identity there is. Take one before you need it, and send it off-box: with `backup.s3` configured the upload happens automatically.

### Pairing the two halves on the way back

A ledger without its deployment identity is a fresh authority that cannot see the compute the old one launched; an identity without the CA cannot issue a node certificate. **Restore both from the same moment**, ledger first:

```bash
# 1. your database's own restore, into the endpoint this host's config names
pg_restore --dbname "$BILLET_STATE_DSN" billet-ledger.dump

# 2. then the identity half
billet local restore --from /var/backups/billet/2026-09-03T014500Z \
    --old-controller-fenced --external-ledger-attached
```

`billet local restore` refuses an identity-only archive twice over, and the two refusals are different in kind:

- **It will not install one onto a host whose config names a local ledger** — that is a check, and billet makes it. The control plane would otherwise create an empty `billet.db` of its own beside the restored identity and start against it: every node's certificate valid, every lease gone.
- **It will not install one at all without `--external-ledger-attached`** — that is a question, because billet cannot answer it. The database is on the other end of a connection string this command has not been given. An identity restored beside an *empty* ledger is a control plane that advertises capacity for a fleet it has no record of, and reaps as orphans the compute the old one launched.

**`billet local recover` is refused outright here**, and the message says so. That command exists to put a deployment back over itself, and the only thing it moves is the ledger — it seals the deployment, waits for it to go quiet, and renames the live `billet.db` aside. None of that exists for a database billet does not hold.

**The transactional host upgrade runs here without the ledger steps.** billet copies no PostgreSQL database, so on this backend `billet host-upgrade` fences nothing, snapshots nothing and migrates nothing: it preserves the binary, units and config, drains the node, stops the server, installs the candidate, proves it can open what it inherits (the schema is one it knows and not ahead of it, the deployment binding agrees, the release watermark admits it), and starts the services. The migration happens where [ADR-009](../reference/decisions/adr-009-controller-election.md) already put it, when the candidate takes the controller claim. The rollback boundary is therefore the candidate's start: before it, a failed candidate restores the binary, units and config and the ledger was never touched; after it, the new binary has claimed and migrated, and what lies past that is your database's own backup, as it is for every other write. The journal records the three skipped steps as reached so a resume runs the same shape.

**Every controller host runs `billet-upgrade.timer`, the standby included.** A standby is not a node in a rollout, so nothing dispatches to it; its own timer reads the decision, the open rollout or the last completed one, through a handle that deliberately ignores the release watermark (the leader has usually recorded the newer release by then, and that refusal is right everywhere except for the one read whose point is to learn what to become), and upgrades the host. What that handle cannot do is read a schema newer than its binary knows, so a release that adds a migration and a leader whose timer fired first leave the standby's timer refusing with the schema error until you run `billet host-upgrade --version <target>` on it by hand; a standby whose timer fired first is the follower-first shape and needs nothing. Whichever order the two timers fire in, a newer standby beside an older leader is the follower-first shape the election was designed for, and the release watermark is recorded only by the process that actually claims, so no older binary ever serves or claims the ledger after a newer one has recorded. The window that costs something is a leader upgraded before its standby: if that newer leader crashes in the minutes before the standby's own timer moves it, the older standby is refused as a downgrade until it is, and the deployment waits. The `junioryono.billet.host` role still refuses a binary change on such a host, because its transaction is the SQLite one; with automatic updates on, billet's timer is what upgrades a PostgreSQL controller, and the role's part is the config, the units and the timers. **A first install is a binary change**, so put `/usr/bin/billet` in place yourself and then converge; with the binary already there the role does everything else.


## One controller at a time, either way

**A second controller is a capacity ledger with two writers, and the failure is quiet**: both admit jobs, and the machine that has to run them finds out later.

On SQLite the exclusive lock on the state directory settles it, because the ledger is reachable from exactly one machine.

On PostgreSQL that lock still applies and is no longer sufficient — a controller on another host takes its own directory's lock happily. So a PostgreSQL deployment also takes a **claim in the database** before it polls GitHub or dispatches anything, and a second controller is refused at startup, naming the machine that holds it:

```
controller claim: state: another billet process is this deployment's controller
  (the ledger says db-controller-1 (pid 4412) holds it, at epoch 7)
```

The claim is held by the connection, not by a lease, so a controller that crashes frees the deployment immediately — there is no timeout deciding whether a live controller is dead, and no stale record to clear before a replacement can start.

**The claim catches the accident; the epoch beside it catches the case nobody could catch.** If a controller loses its database session while still running — a network partition, a failover, an `idle_session_timeout`, a pooling proxy — the claim is released and that process finds out about none of it, because nothing uses the connection again. A replacement can then legitimately claim, which advances the epoch.

Every write transaction re-reads that epoch, so the old controller's **next write is refused**. It stops there:

```
this process is no longer this deployment's controller, so it is stopping without
destroying compute, closing its message session or handing back capacity — the jobs
running here keep running and whichever controller replaced this one adopts them
```

That is deliberate, and it is exactly what a hard kill leaves behind: the guests keep running and the new controller re-adopts them, GitHub expires the message session the old one abandoned, and its leases come back once they stop being renewed. Nothing is destroyed and no capacity is handed back, because a process that has been replaced can prove nothing about either.

The process then exits non-zero, so systemd restarts it — which is what you want if the replacement was itself transient. If it was not, the restart meets the refusal above every `RestartSec`, naming the machine that now holds the deployment. `billet status` shows the same thing without stopping anything:

```
claim     db-controller-2 (pid 881) holds this deployment's controller, at epoch 8
```

**What this does and does not promise.** It cannot stop the old controller writing *before* the replacement claims — nothing can, and that is the ordinary limit of a fencing token. What it guarantees is that once a replacement has claimed, the old one writes nothing more, which is what makes the handover safe.

## Two controllers, active and passive

By default a second controller is a **mistake**, and billet says so loudly: it exits non-zero naming the machine that holds the claim, and `Restart=on-failure` repeats that every `RestartSec` until somebody fixes it. That is the right answer for two units misconfigured as active, and it stays the default.

Tell billet you meant it and the second one **waits** instead:

```yaml
server:
  identity_dir: /var/lib/billet/server
  controllers: active-passive
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_STATE_DSN
```

**Both hosts write the same thing.** Whichever takes the claim first is the controller; the other stands by. After a failover the standby *is* the controller, so a setting that named one host's role would be describing a job it no longer has.

A standby reports itself ready — it is doing its job — and says what it is waiting for:

```
$ systemctl status billet-server
   Active: active (running)
   Status: "standby: waiting for the controller claim (db-controller-1 (pid 4412) holds it at epoch 7)"
```

**A standby cannot write, and that is enforced by the store rather than by a rule.** Every write transaction on a standby's handle is refused until it claims. This matters more than it looks: the epoch fence stops a controller that has been *replaced*, and does nothing for one that has *never* claimed — so without a refusal of its own a standby would write to a live deployment's ledger completely unfenced.

**Promotion is a start.** When the incumbent's database session ends — it crashed, it was stopped, the server restarted, a partition timed it out — the lock is released, the standby takes it, and it then runs exactly the startup a fresh controller runs: it clears the fleet's liveness, reads the node-wire authority, opens its listeners, waits out the message session the old controller abandoned, and reaps the leases whose heartbeats stopped. Running jobs keep running and are re-adopted by their nodes, which is the same recovery a hard kill already gets.

**It is refused on a SQLite ledger.** There is nothing for a second control plane to take over: the ledger is a file on local storage, and billet refuses to put one anywhere a second machine could reach.

### What active/passive does not do for you

**It does not move the address.** Every node dials one `server_addr`. A failover is promotion *plus* that address arriving at the new host — a floating IP, a DNS change, a load balancer — and that half is your deployment's to arrange. Nodes reconnect and re-register on their own once it does.

**It does not decide that a controller is dead — PostgreSQL does.** Promotion happens when the incumbent's *session* ends. Under a crash that is immediate; under a network partition it is whenever your server's keepalive or `idle_session_timeout` policy says so. billet adds no watchdog on purpose: the window between a partition and any observation of it survives every detector, and a watchdog that stops a healthy control plane over a blip is its own outage. If you want a tighter bound, tune the database.

**The deployment identity is still one thing per host.** The `deployment-id` file has to be the same on both, and billet *proves* you did that rather than assuming it: the ledger records which deployment it belongs to, and a controller whose identity directory disagrees is refused at startup naming both.

## Sharing the identity material

Without an identity store, both hosts need the same node-wire CA and the same GitHub App private key, copied there by hand. That is workable and it has one sharp edge: a controller whose `ca` directory is empty **mints a new authority**, and every node in the fleet then fails to verify it and drops off at once — while the control plane looks perfectly healthy.

```yaml
server:
  identity_dir: /var/lib/billet/server
  controllers: active-passive
  identity:
    backend: aws-ssm
    aws_ssm:
      region: us-west-2
      prefix: /billet/prod
      kms_key_id: alias/billet     # optional; the account's default SSM key otherwise
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_STATE_DSN
```

Both the authority and the App key become SecureStrings under that prefix. `billet github-app create` publishes the key there — after writing it to disk, so a failed publish leaves a file and `billet github-app store-key --from <path>` finishes the job rather than costing you an App. A controller that starts with no authority **adopts** the deployment's; one that already holds it does nothing.

**It is replication, not a shared authority, and that is deliberate.** The files in `identity_dir` stay the source of truth on each host, with every rule they already have — the publication order that makes each instant of a rotation a state a reader answers correctly, the torn-read repair, the retire guard. Those rest on cross-key write ordering, `O_EXCL` and a local lock, none of which a remote key/value store provides; porting the state machine onto one would carry the code and discard the reasoning.

**So a rotation does not converge on its own.** `billet ca rotate` and `billet ca retire` publish what they produced, and the other controller picks it up when you ask:

```
billet ca sync              # take what the store publishes
billet ca sync --push       # publish what this host holds
billet ca sync --force      # this host is the one that is behind
```

A host holding a *different* authority is **refused** rather than overwritten, because billet cannot tell a host left behind by a rotation from one pointed at the wrong deployment — and the file it would replace is what every node verifies against. `--force` is you saying which is right, and even then the old one is moved aside rather than deleted: it is a private key.

**What the store needs from IAM**: `ssm:GetParameter` and `ssm:PutParameter` on `<prefix>/*`, and `kms:Decrypt` (plus `kms:GenerateDataKey` for writes) on whichever key encrypts them. billet issues no `ssm:DeleteParameter` for either value.

**One standby or five is the same thing.** A third host writing `active-passive` is simply another waiter on the same lock. Nothing orders the queue.

## Moving an existing deployment

There is no migration path from a SQLite ledger to a PostgreSQL one, and adding one is not planned: the ledger is a *live capacity record*, so moving it means moving compute obligations between two databases while jobs are running.

Do it the way you would replace a controller. Drain the deployment (`billet drain --wait`), stop it, point a new controller at PostgreSQL with the **same** `identity_dir`, and let the fleet re-register. Running jobs are not preserved across the change; the identity, the CA and the nodes are.
