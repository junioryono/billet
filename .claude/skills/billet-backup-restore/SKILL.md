---
name: billet-backup-restore
description: "Load when touching `billet local backup`, `local restore`, `local recover` or the off-site S3 hop (internal/deployarchive, internal/archivestore, cmd/billet/local{backup,restore,recover,offsite}.go), internal/durablefile, the kernel installer in images pull, the restore and recover rehearsals, or anything that writes a file a remote record will name."
---

# Backup, restore, recover, and durable files

## What this area is

A deployment is four things and they are useless apart: the ledger, the deployment identity, the GitHub App private key and the node-wire authority. `internal/deployarchive` captures them as one archive (`Manifest`, `Schema` 2, digests and sizes per entry; a PostgreSQL ledger is recorded as `ExternalLedger` because copying rows through billet's own connection would produce something that looks like a backup and is not) and puts them back with `PlanRestore`/`PlanRecover`, `Execute`, `Finish` and `Abandon`. `internal/archivestore` is the S3 hop with no delete. `internal/durablefile` is the one ordering for installing a file something remote will name. The rehearsals are `make restore-rehearsal` and `make postgres-restore-rehearsal`, recorded in `docs/reference/records/restore-rehearsal.md`.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Backup and restore: [references/archive-and-restore.md](references/archive-and-restore.md)

- **The host transaction runs on an external ledger without copying it.**
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
- **The journal is schema 3 with a closed phase set and an explicit intent.**

### The off-site hop and durable files: [references/offsite-and-durable-files.md](references/offsite-and-durable-files.md)

- **An archive on the disk it protects is not a backup, and billet owns both ends of one narrow hop.**
- **A remote record may not name a local file that is not durable yet.**
- **A durable file is not a file nothing else will delete.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**Installing a local file.** Go through `internal/durablefile`, the one fsync ordering, and never let a remote record name a file that is not durable yet ([references/offsite-and-durable-files.md](references/offsite-and-durable-files.md)).

## Where the tests are

- `internal/deployarchive/*_test.go` (planning, execute, finish, abandon, supersede ordering, journal schema, containment, external ledger).
- `cmd/billet/localbackup_test.go`, `localrestore_test.go`, `localrecover_test.go`, `localoffsite_test.go`, `restoreownership_test.go`, `restoreprofile_test.go`, `preservedpaths_test.go`.
- `cmd/billet/imagespulldurable_test.go` (`TestAPullDoesNotPublishAGenerationBeforeTheKernelIsDurable`), `imagespulllock_test.go`, `kernellock_test.go`, `kernelreap_test.go`, `internal/durablefile/*_test.go`.
- `internal/e2e/restore_test.go` (`TestARestoredDeploymentServesTheFleetThatTrustedTheOldOne`); `scripts/restore-rehearsal.sh` and `postgres-restore-rehearsal.sh` are the halves that need a real package.

## Related skills

`billet-state` (the fence, the barrier, `OpenMaintenance`), `billet-identity-and-ca` (the authority files and the App key), `billet-lifecycle` (`local up` and ownership repair), `billet-guest-images` (the kernel the durable installer exists for).
