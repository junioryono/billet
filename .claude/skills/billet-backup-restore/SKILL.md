---
name: billet-backup-restore
description: "Load when touching `billet local backup`, `local restore`, `local recover` or the off-site S3 hop (internal/deployarchive, internal/archivestore, internal/ops/host/local{backup,restore,recover,offsite}.go), internal/durablefile, the kernel installer in images pull, the restore and recover rehearsals, or anything that writes a file a remote record will name."
---

# Backup, restore, recover, and durable files

## What this area is

A deployment is four things and they are useless apart: the ledger, the deployment identity, the GitHub App private key and the node-wire authority. `internal/deployarchive` captures them as one archive (`Manifest`, written at schema 3, or 4 when a PostgreSQL ledger travels as `pg_dump`'s copy, digests and sizes per entry; schema 2 and 3 archives of a PostgreSQL ledger are identity-only and still read) and puts them back with `PlanRestore`/`PlanRecover`, `Execute`, `Finish` and `Abandon`. `internal/pgdump` runs `pg_dump` and `pg_restore`; the archive package is handed it and runs no subprocess. `internal/archivestore` is the S3 hop with no delete. `internal/durablefile` is the one ordering for installing a file something remote will name. The rehearsals are `make restore-rehearsal` and `make postgres-restore-rehearsal`, recorded in `docs/reference/records/restore-rehearsal.md`.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Backup and restore: [references/archive-and-restore.md](references/archive-and-restore.md)

- **The host transaction runs on an external ledger without copying it.**
- **A PostgreSQL ledger travels as `pg_dump`'s copy of the snapshot billet read it in (archive schema 4, #19).**
- **What the database holds decides a restore, and a database still holding this deployment's ledger is kept.**
- **All four pieces or none, and nothing is ever overwritten.**
- **Backup, rotate and retire share `wirecert.LockAuthority`.**
- **Restore's exclusion is three local mechanisms and one human assertion.**
- **The plan is re-derived inside the exclusion and only that one is acted on.**
- **Publication goes file by file, hashes through a descriptor, and the identity check precedes the close.**
- **A failed restore leaves the ledger fenced with a journal.**
- **A restore runs as root and hands back what it wrote.**

### Recover: [references/recover.md](references/recover.md)

- **`local recover` puts a deployment back over itself, and every step exists because the previous one cannot promise the next.**
- **The fence outlives the publication.**
- **Order of a supersede: sidecars, sync, ledger.**
- **An abandon reconciles the whole set by digest, and refuses whatever it cannot vouch for.**
- **A PostgreSQL recovery keeps a dump of what it replaces, journaled before the archive's restore runs.**
- **The journal is schema 3 with a closed phase set and an explicit intent.**

### The off-site hop and durable files: [references/offsite-and-durable-files.md](references/offsite-and-durable-files.md)

- **An archive on the disk it protects is not a backup, and billet owns both ends of one narrow hop.**
- **The packaged backup directory keeps seven archives, and an older one goes only when the bucket lists it complete.**
- **A remote record may not name a local file that is not durable yet.**
- **A durable file is not a file nothing else will delete.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**Installing a local file.** Go through `internal/durablefile`, the one fsync ordering, and never let a remote record name a file that is not durable yet ([references/offsite-and-durable-files.md](references/offsite-and-durable-files.md)).

## Where the tests are

- `internal/deployarchive/*_test.go` (planning, execute, finish, abandon, supersede ordering, journal schema, containment, external ledger; `dump_test.go` for schema 4 against a fake database that records every call).
- `internal/pgdump/pgdump_test.go` (executed fake programs prove argv and environment; the real round trip and the snapshot need `BILLET_TEST_POSTGRES_DSN` and the client); `internal/state/libpq_test.go`, `pgpeek_test.go`; `internal/ops/host/localpostgres_test.go` (backup, restore, recover and abandon through the commands on PostgreSQL).
- `internal/ops/host/localbackup_test.go`, `localrestore_test.go`, `localrecover_test.go`, `localoffsite_test.go`, `restoreownership_test.go`, `restoreprofile_test.go`, `preservedpaths_test.go`.
- `internal/ops/images/imagespulldurable_test.go` (`TestAPullDoesNotPublishAGenerationBeforeTheKernelIsDurable`), `imagespulllock_test.go`, `kernellock_test.go`, `kernelreap_test.go`, `internal/durablefile/*_test.go`.
- `internal/e2e/restore_test.go` (`TestARestoredDeploymentServesTheFleetThatTrustedTheOldOne`); `scripts/restore-rehearsal.sh` and `postgres-restore-rehearsal.sh` are the halves that need a real package.

## Related skills

`billet-state` (the fence, the barrier, `OpenMaintenance`), `billet-identity-and-ca` (the authority files and the App key), `billet-lifecycle` (`local up` and ownership repair), `billet-guest-images` (the kernel the durable installer exists for).
