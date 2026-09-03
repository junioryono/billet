# Backup, restore and recover

A deployment is four things and they are useless apart: the ledger, the deployment identity, the GitHub App private key, and the node-wire certificate authority. A ledger without its identity is a fresh authority that cannot see the compute the old one launched; an identity without the CA cannot issue a node certificate; a CA without the App key cannot get a token. So `billet local backup` captures all four as one unit, and `billet local restore` puts back all four or none.

## Backup

```bash
billet local backup --out /var/backups/billet/$(date -u +%Y%m%dT%H%M%SZ)
```

It runs against a **live** control plane: the ledger copy is SQLite's `VACUUM INTO`, a consistent snapshot of a database somebody is writing to. It writes a 0700 directory of 0600 files (a manifest with a digest and size for every entry, the ledger, the identity, the App key, the whole node-wire authority, and a reference copy of `billet.yaml`), refuses a destination inside the state directory, and takes the same lock `billet ca rotate` and `billet ca retire` take, so it can never capture a key from one authority generation beside a certificate from another. If a rotation is running, the previous key travels too. An authority missing half a pair is a refusal, not a shorter archive.

The packaged `billet-backup.service` and `billet-backup.timer` run this daily, randomised, catching up after downtime. Neither is enabled by the package, and neither is managed by `billet local up`/`down`; `systemctl enable --now billet-backup.timer` is your decision. Nothing in either unit deletes a local archive, because each holds a copy of the App key GitHub issues exactly once.

**On a PostgreSQL ledger the archive is identity-only**, and says so: your database's own backup is the other half ([PostgreSQL and active-passive controllers](../deploying/postgres-and-active-passive.md)).

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

Five properties to rely on. **billet never deletes**: there is no delete operation in its code and none in the IAM grant the Terraform module attaches, so the credential on the one host that also holds the App key cannot destroy the copies that exist to survive losing that host; retention belongs to the bucket, through versioning and a lifecycle rule on noncurrent versions only. **Every write refuses to replace.** **The manifest is uploaded last**, so an interrupted upload is not something a listing will offer. **The local archive is written and verified first**, always. **The credential is the fleet's**: environment variables or the instance role, nothing else. `billet check` reports the newest archive and its age, because a timer that stopped firing looks exactly like one that works.

## Restore

```bash
billet local restore --from /var/backups/billet/20260903T014500Z --old-controller-fenced
billet local restore --from-backup latest --old-controller-fenced
billet local restore --from … --dry-run
```

It puts the deployment back **as one unit or not at all**. Every piece is absent (installed), byte-identical (already done), or different (preserved and refused). There is no flag that overwrites one: a different App key is never replaced, because GitHub issues it once. The one thing it replaces is the schema-only `billet.db` a preflight `billet check` leaves on a host nobody has commissioned, and only after proving that directory has no identity, no authority and no row in any table a deployment writes to. `--dry-run` reports exactly what would be installed and every reason it would be refused, through the same code path.

**What no lock on the target can establish is that the old controller is gone.** Locally, billet proves three things before publishing anything: no control plane holds the state directory, no handle can transact (a maintenance fence that reaches even a command already open), and every transaction that began before the fence has finished. None of them reaches another machine. Restoring while the controller the backup came from can still start anywhere produces two authoritative controllers sharing one identity, one CA and one App credential, with ledgers that diverge from the moment both are up, so `--old-controller-fenced` is you asserting you have stopped and disabled it everywhere, and without it the restore refuses.

An interrupted restore leaves the directory **fenced** with a journal beside it, so nothing can start a control plane on a half-restored deployment. Re-run the same command to continue, or `--abandon` to remove only what that run created and can still prove it wrote.

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

An interrupted recovery leaves the directory fenced with a journal, and re-running the same command is the way forward from every point it can stop at. `--abandon` puts the superseded ledger back, refuses rather than guessing whenever it cannot reassemble that ledger and its write-ahead log or would leave the directory with no ledger, and refuses an operation that already finished, because undoing a complete sealed recovery would remove the restored credentials and bring back the ledger you chose to replace. On a PostgreSQL ledger `recover` is refused outright: the only thing it moves is the ledger, and billet does not hold that one.

## Moving a control plane without losing its identity

A control plane that starts on an empty state directory does not fail; it creates a **new deployment**, indistinguishable afterwards from a genuine new install, and every node trusting the old CA stops authenticating. The state has to arrive on the new host after its ledger volume is mounted and proved, and before anything starts a server on it. The Ansible host role's `billet_server_prepare_only: true` is that stopping point: it mounts and proves the volume, installs the units with their mount fence, and leaves both services installed, disabled and refused by their own units, so nothing can mint an identity until you have copied one in.

1. Stage the existing App key first (`billet_github_private_key_src`); the role refuses a converge without it, and it must be the same key.
2. Converge the new host with `billet_server_prepare_only: true`.
3. Drain the old control plane and wait for the barrier; an interrupted `billet drain --wait` exits 2 and means *not drained*.
4. Stop the old controller and **disable** it, so a reboot cannot resurrect it.
5. Copy the whole stopped state directory, SQLite sidecars included, over what the preflight left. What proves the new host uncommissioned is the absence of an identity and a CA, not an empty directory.
6. Point the nodes at the new endpoint; the certificate must be valid for the new name (`server.node_tls_hosts`).
7. Converge again with `billet_server_prepare_only: false`. To roll back, disable the new controller before re-enabling the old one.

Do not `systemctl mask` here: masking writes a `/dev/null` link exactly where the role renders the unit. On a PostgreSQL ledger a second controller can instead be a standby that promotes itself, and the identity material can be replicated through Parameter Store rather than copied ([PostgreSQL and active-passive controllers](../deploying/postgres-and-active-passive.md)).

## Rehearsed on every pull request

Two legs: an end-to-end test that restores a deployment onto a directory that has never seen it and proves the restored control plane serves the node that trusted the old one, and `make restore-rehearsal`, which does the same with the real Debian package and the real service account on a Linux host (`make postgres-restore-rehearsal` for the identity-only profile). What neither covers, a real GitHub App and `billet local up` afterwards, is written down in [the rehearsal record](../reference/records/restore-rehearsal.md) rather than assumed.
