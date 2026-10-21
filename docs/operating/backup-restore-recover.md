# Backup, restore and recover

A deployment is four things and they are useless apart: the ledger, the deployment identity, the GitHub App private key (one per target, on a deployment serving several), and the node-wire certificate authority. A ledger without its identity is a fresh authority that cannot see the compute the old one launched; an identity without the CA cannot issue a node certificate; a CA without the App key cannot get a token. So `billet local backup` captures all four as one unit, and `billet local restore` puts back all four or none.

## Backup

```bash
billet local backup --out /var/backups/billet/$(date -u +%Y%m%dT%H%M%SZ)
```

It runs against a **live** control plane: the ledger copy is SQLite's `VACUUM INTO` (or, on PostgreSQL, `pg_dump`; see below), a consistent snapshot of a database somebody is writing to. It writes a 0700 directory of 0600 files (a manifest with a digest and size for every entry, the ledger, the identity, the App key, the whole node-wire authority, and a reference copy of `billet.yaml`), refuses a destination inside the state directory, and takes the same lock `billet ca rotate` and `billet ca retire` take, so it can never capture a key from one authority generation beside a certificate from another. If a rotation is running, the previous key travels too. An authority missing half a pair is a refusal, not a shorter archive.

The packaged `billet-backup.service` and `billet-backup.timer` run this daily, randomised, catching up after downtime. Neither is enabled by the package; `billet local up` enables and arms the timer on a control-plane host after the services are up and proved, the Ansible host role renders and enables the same two units on every host whose control plane may run (a host converged from a tarball has no package and no `local up` to arm it; `billet_backup_timer: false` installs them disabled), and `billet check` reports the newest local archive's age and whether the timer is enabled, warning past two missed daily runs/`down`; on a packaged host that ran no `local up`, `systemctl enable --now billet-backup.timer` is your decision. The service unit carries `RequiresMountsFor=` on the state directory, so on a host whose ledger lives on its own volume a failed mount stops the backup the way it stops the server, and the role refuses a drop-in on the unit for the same reason it refuses one on either service. Nothing in either unit deletes a local archive, because each holds a copy of the App key GitHub issues exactly once.

**On a PostgreSQL ledger the ledger travels as `pg_dump`'s own copy** (archive schema 4, `ledger/billet.pgdump`), never as rows billet copied through its own connection. billet exports a snapshot of the ledger, reads the migrations and the deployment binding in it, and has `pg_dump --snapshot` dump exactly that snapshot of the ledger's schema, so the manifest describes the dump exactly and the dump holds no write lock while it runs. The connection reaches `pg_dump` through its environment, never its arguments, because it carries a password. `pg_dump` is the PostgreSQL client's, not billet's, and it must be at least the server's major version (`pg_dump` refuses a newer server): install `postgresql-client-<major>` from apt.postgresql.org where the distribution's is older. Without it the backup still captures the identity, the authority and the App key (the identity-only archive older billets wrote, uploaded as usual) and then fails, naming the package, so a timer run on such a host is reported as failed rather than mistaken for a backup. Install the client on every PostgreSQL controller. Archives an older billet wrote for such a deployment are identity-only (schema 2 or 3) and are still restored the way they always were.

## Off the disk it protects

`--out <dir>` is the right primitive and not a strategy: one disk failure takes the deployment and its archive together. The directory is the contract, so restic, rclone or a NAS can carry it, and leaving `backup:` out of the config is a supported answer. With `backup.s3` set, `billet local backup` uploads what it just wrote, and `billet local restore --from-backup latest` fetches, verifies and restores on a machine that holds nothing but the binary:

```yaml
backup:
  s3:
    bucket: my-billet-backups
    region: us-west-2            # required even with an endpoint
    prefix: billet-backups       # archives land at <prefix>/<deployment-id>/<created-at>/
    endpoint: https://rgw.internal   # optional: Ceph RGW, MinIO, R2, path-style
```

Five properties to rely on. **billet never deletes**: there is no delete operation in its code and none in the IAM grant the Terraform module attaches, so the credential on the one host that also holds the App key cannot destroy the copies that exist to survive losing that host; retention belongs to the bucket, through versioning and a lifecycle rule on noncurrent versions only. On the host, the packaged directory `/var/lib/billet/backups` keeps the newest seven archives: after an upload finishes, an older archive there is removed only when the bucket holds that archive's own manifest, byte for byte, under the name it was uploaded as, so one that never finished uploading stays, and so does one holding a file its manifest does not declare or one changed since. An archive written with `--out` anywhere else is never touched. **Every write refuses to replace.** **The manifest is uploaded last**, so an interrupted upload is not something a listing will offer. **The local archive is written and verified first**, always. **The credential is the fleet's**: environment variables or the instance role, nothing else. `billet check` reports the newest archive and its age, because a timer that stopped firing looks exactly like one that works.

## Restore

```bash
billet local restore --from /var/backups/billet/20260903T014500Z --old-controller-fenced
billet local restore --from-backup latest --old-controller-fenced
billet local restore --from … --dry-run
```

It puts the deployment back **as one unit or not at all**. Every piece is absent (installed), byte-identical (already done), or different (preserved and refused). There is no flag that overwrites one: a different App key is never replaced, because GitHub issues it once. The one thing it replaces is the schema-only `billet.db` a preflight `billet check` leaves on a host nobody has commissioned, and only after proving that directory has no identity, no authority and no row in any table a deployment writes to. `--dry-run` reports exactly what would be installed and every reason it would be refused, through the same code path.

**What no lock on the target can establish is that the old controller is gone.** Locally, billet proves three things before publishing anything: no control plane holds the state directory, no handle can transact (a maintenance fence that reaches even a command already open), and every transaction that began before the fence has finished. None of them reaches another machine. Restoring while the controller the backup came from can still start anywhere produces two authoritative controllers sharing one identity, one CA and one App credential, with ledgers that diverge from the moment both are up, so `--old-controller-fenced` is you asserting you have stopped and disabled it everywhere, and without it the restore refuses.

An interrupted restore leaves the directory **fenced** with a journal beside it, so nothing can start a control plane on a half-restored deployment. Re-run the same command to continue, or `--abandon` to remove only what that run created and can still prove it wrote.

**On a PostgreSQL ledger, what the database already holds decides**, read through the connection the config names:

| The schema the connection resolves to | What restore does |
|---|---|
| holds no tables | `pg_restore`s the archive's dump into it, in one transaction |
| holds only what a preflight wrote: billet's migrations recorded, every table empty | the same, replacing those empty tables |
| holds **this deployment's** ledger (its binding names this deployment) | **keeps it**: the host died and the database did not, so those rows are newer than the archive's; the identity half is restored beside them. Refused instead when the schema has no `schema_migrations` (it is not a billet ledger) or its migrations are behind the archive's (it was restored from something older than this backup) |
| holds rows bound to no deployment | refused, unless you pass `--external-ledger-attached` to say it is this deployment's, and then kept |
| holds another deployment's ledger, or rows a newer billet created, tables with no billet migrations, or is another schema | refused |

The dump goes back into the schema it came from, so the connection's `search_path` must resolve to a schema of that name that already exists. The restore replaces the ledger rather than overlaying it. It first drops, in a transaction of its own, any table in that schema the dump does not name (a later migration's, so a ledger restored from before an upgrade does not keep that upgrade's tables beside it). That drop never cascades: if a view, a foreign key or an inheriting table outside those tables depends on one of them, or one is partitioned, the restore refuses and drops nothing, because the copies billet keeps are of this schema alone and could not bring the dependent back. A schema name with whitespace in it, or a dump whose table list cannot be read unambiguously, is refused the same way. It then runs `pg_restore --single-transaction --clean --if-exists --no-owner --no-privileges --schema=<that schema>` on a copy of the dump it has just verified, leaving the schema itself alone. Finally it reads the migrations and the binding back through billet's own connection before it lifts the fence. `pg_restore` sets its own `lock_timeout` to 0, so a session holding a lock on a ledger table makes the restore wait: `pg_stat_activity` names it, and interrupting the command ends the restore's transaction, leaving the schema as the drop left it, which the copy a recovery keeps (or a restore's proof that the schema held nothing) covers. Before reading anything to plan with, it takes the ledger's writer lock for an instant, which proves no billet write transaction from any host is in flight. An abandoned restore removes the files it wrote and leaves the database as it is, saying so: emptying a schema is your decision.

**A restore run as root hands what it wrote to the service account** and prints each path. On a packaged host it must run as root because the App key lands in root-owned `/etc/billet`, and systemd's `StateDirectory=` repairs ownership only when the top directory's owner is wrong, which after a restore it is not. The rehearsal found this: a deployment restored perfectly and the control plane could not open one file of it.

Restoring an older snapshot while compute from after it still exists is destructive: the restored ledger has no lease for those jobs, so node recovery treats their instances as orphans and destroys them, and GitHub does not requeue work that already started. Fence every node first, inventory compute created after the backup, and reconnect nodes only after that compute is proved gone or you have accepted the failures.

## Recover: putting a deployment back over itself

`restore` refuses a commissioned deployment, because its ledger is a live capacity record. `recover` is the operation for the ordinary disaster: the controller is sick, you have yesterday's archive, and the ledger on disk is not one you want to keep.

```bash
billet local recover --from /var/backups/billet/20260902T014500Z --old-controller-fenced
billet local recover --from-backup latest --old-controller-fenced --accept-failing-jobs
billet local recover --abandon
```

The order it acts in is the whole of its safety. It refuses unless the archive is **this host's own deployment**, so it can never relabel a host. It **seals** the deployment and waits for it to hold nothing, and if work is still outstanding it refuses and names every job by tier, node, phase, age and run id unless you pass `--accept-failing-jobs` (those jobs fail; compute whose lease has already gone is not in that list, and it says so). It requires `--old-controller-fenced`. It replaces the ledger and renames the old one to `billet.db.superseded-<taken-at>-<digest>`, never deleted, because it is the only record of the work this operation failed. Then it **seals the deployment again**, because the restored ledger carries the admission it had when the backup was taken, open, and a recovered control plane must not take new work while its nodes hold compute it has never heard of. That seal is an operator's, so `billet local up` does not clear it; `billet resume` is you saying that compute is proved gone.

An interrupted recovery leaves the directory fenced with a journal, and re-running the same command is the way forward from every point it can stop at. `--abandon` puts the superseded ledger back, refuses rather than guessing whenever it cannot reassemble that ledger and its write-ahead log or would leave the directory with no ledger, and refuses an operation that already finished, because undoing a complete sealed recovery would remove the restored credentials and bring back the ledger you chose to replace.

**On a PostgreSQL ledger `recover` takes the same steps over the database.** After sealing and proving the deployment quiet, it dumps the ledger it is about to replace into the identity directory as `ledger.pgdump.superseded-<taken-at>-<digest>`, journals that dump's digest and makes it durable, and only then restores the archive's dump over the ledger in one transaction and seals again. The fence it raises in the identity directory reaches this host's handles only, so `--old-controller-fenced` means every controller, standbys included. It recovers only a ledger bound to this deployment, from a backup bound to it, and journals the database it writes before its first write there: every later step (a resumed run, the seal, lifting the fence, an abandon) proves the connection still reaches that database, bound to this deployment, before it acts. The replacement is whole: a ledger a later migration widened comes back as the archive's, with that migration's tables dropped, so recovering from a backup taken before an upgrade works. `--abandon` restores the ledger from that dump the same way and keeps the dump as `….abandoned`. Before restoring, it proves three things: the dump is the one this recovery took, the connection still reaches the database it was taken from, and that database is bound to this deployment. Afterwards it reads the ledger back and requires exactly the migrations the dump recorded. An abandon interrupted after it put the ledger back finds the dump at its kept name and finishes. A dump the journal does not vouch for, or one the journal says was published and is now gone, is refused, fence and journal standing; so is a resumed `recover` that finds its published dump gone, since the archive's restore may already have begun. Only a journal that never got as far as publishing the dump means the database was not touched. What stands in for a digest of the database is the journaled digest of the dump and the single transaction it is restored in. Archives older billets wrote for such a deployment carry no ledger, and `recover` still refuses them: there is nothing in one to put the ledger back from.

## Moving a control plane without losing its identity

A control plane that starts on an empty state directory does not fail; it creates a **new deployment**, indistinguishable afterwards from a genuine new install, and every node trusting the old CA stops authenticating. The state has to arrive on the new host after its ledger volume is mounted and proved, and before anything starts a server on it. The Ansible host role's `billet_server_prepare_only: true` is that stopping point: it mounts and proves the volume, installs the units with their mount fence, and leaves both services installed, disabled and refused by their own units, so nothing can mint an identity until you have copied one in.

1. Stage the existing App key first (`billet_github_private_key_src`, and `billet_github_target_key_srcs` for every further target); the role refuses a converge without them, and each must be the same key.
2. Converge the new host with `billet_server_prepare_only: true`.
3. Drain the old control plane and wait for the barrier; an interrupted `billet drain --wait` exits 2 and means *not drained*.
4. Stop the old controller and **disable** it, so a reboot cannot resurrect it.
5. Copy the whole stopped state directory, SQLite sidecars included, over what the preflight left. What proves the new host uncommissioned is the absence of an identity and a CA, not an empty directory.
6. Point the nodes at the new endpoint; the certificate must be valid for the new name (`server.node_tls_hosts`).
7. Converge again with `billet_server_prepare_only: false`. To roll back, disable the new controller before re-enabling the old one.

Do not `systemctl mask` here: masking writes a `/dev/null` link exactly where the role renders the unit. On a PostgreSQL ledger a second controller can instead be a standby that promotes itself, and the identity material can be replicated through Parameter Store rather than copied ([PostgreSQL and active-passive controllers](../deploying/postgres-and-active-passive.md)).

## Rehearsed on every pull request

Two legs: an end-to-end test that restores a deployment onto a directory that has never seen it and proves the restored control plane serves the node that trusted the old one, on SQLite and on PostgreSQL (the dump restored into a database that starts empty), and `make restore-rehearsal`, which does the same with the real Debian package and the real service account on a Linux host (`make postgres-restore-rehearsal` for a PostgreSQL deployment: the dump, a restore that keeps the surviving database's ledger, a restore into an empty database, and a recovery). A restored deployment's first start under `billet local up` with a real App is rehearsed by hand (`make restore-first-start-rehearsal`), and so is an interrupted recovery's `--abandon` (`make recover-rehearsal`); see [the rehearsal record](../reference/records/restore-rehearsal.md) and [Host rehearsals](../reference/records/host-rehearsals.md).
