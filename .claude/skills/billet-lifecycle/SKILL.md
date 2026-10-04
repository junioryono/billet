---
name: billet-lifecycle
description: "Load when touching `billet local up|status|down|uninstall`, `billet drain|resume`, cmd/billet/local*.go or drain.go, internal/lifeops (systemd) or internal/lifeops/launchd (macOS launch agents), deploy/ (the units, plists and package scripts), or anything that decides from what systemctl or launchctl reports; and when a host looks correct from every angle billet checks and is still running something else."
---

# The local service lifecycle

## What this area is

`internal/lifeops` inspects a Linux host (units, files, running processes; it prints nothing) and converges it; `internal/lifeops/launchd` is its sibling for macOS launch agents, deliberately not one abstraction because the vocabularies are not shared. `cmd/billet/local*.go` hold the order the commands act in, which is the whole safety content: `up` is check, server, node, each started, proved, then enabled; `down` is seal, wait, stop node, stop server, disable both; `uninstall` is down plus forgetting the services; `backup`, `restore` and `recover` are in `billet-backup-restore`. `deploy/` holds `billet-server.service`, `billet-node.service`, `billet-backup.service` and `.timer`, `billet-rbd.conf`, the two `sh.billet.*.plist` agents, the packaged `billet.yaml` template and the package scripts; `deploy/units.go` exposes the names as constants.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers. What a controller retirement may do to these services is in `billet-controller-retirement`.

## Invariants

### systemd and the package: [references/systemd.md](references/systemd.md)

- **The units are load-bearing and each line was found by installing a package.**
- **The package enables and starts nothing.**
- **needrestart must never restart a billet service, and the package seeds the exclusion.**
- **`systemctl show` hides the privilege prefix.**
- **Measured systemd facts, each of which reads the other way somewhere.**
- **`billet-upgrade.timer` and `billet-images-refresh.timer` are the one exception to the package enabling nothing.**
- **Every systemctl the inspector runs is bounded, and a stop or a start runs under the caller's deadline from the unit's own bound.**
- **The rehearsals drive the packaged units on real systemd in containers.**

### launchd: [references/launchd.md](references/launchd.md)

- **On a Mac the node is a launch agent and a root daemon cannot do the job.**
- **What launchd loaded is not what its plist says.**

### The local commands and the seal: [references/local-commands.md](references/local-commands.md)

- **`up` writes nothing structural on systemd and does write the service definition on launchd.**
- **`up` enables the timers as a reported last step outside the unit plan.**
- **When a check cannot be built, observe.**
- **The seal is not the authority; the barrier is.**
- **Provenance decides who may clear a seal.**
- **Only states that prove no process remains count as stopped.**
- **One lifecycle command at a time, via a host flock taken immediately before the first mutation.**
- **A root installer prepares the PACKAGED directory, never the calling account's.**
- **A host is moved onto the authority exclusion by an installer, and only an installer.**

Measured facts for both service managers, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A systemd unit change.** Change `deploy/` and the Ansible role's templates together (`make unit-parity` compares them), keep `deploy/units_test.go` green, and run `make systemd-lifecycle`, the only test of the lifecycle against a real service manager and the real package (it skips without a working App credential).

**A plist change.** Nothing else renders a plist, so `deploy/`'s is the only copy; keep `deploy/units_test.go` green and run `internal/lifeops/launchd/reallaunchd_test.go` on a Mac, where every launchd fact was measured.

**Deciding from what systemctl or launchctl reports.** Ask only for the properties the decision reads, so the fake can answer only those; treat every state that does not prove the process gone as not stopped; read [references/measured-facts.md](references/measured-facts.md) for the answers that read the other way.

## Where the tests are

- `internal/lifeops/*_test.go`: the systemctl fake answers only the properties it was asked for (a whole-reply fake makes deleting a property from the production query invisible); fixtures start healthy and break one thing; a healthy host is asserted not refused. `os.SameFile` type-asserts the concrete `FileInfo`, so a fixture varies link counts with a real hard link; `syscall.Stat_t` widths differ per platform, so stats go through a converting helper; `OnFailureJobMode` belongs in `[Unit]` or the fixture proves nothing.
- `internal/lifeops/launchd/*_test.go` and `reallaunchd_test.go` (each test derives its own label).
- `cmd/billet/local_test.go`, `localup_test.go`, `localdown_test.go`, `lifecycle_test.go`, `drain_test.go`, `admission_test.go`, `hostlock_test.go`, `systemd_test.go`; `deploy/units_test.go` (pins `ExitTimeOut` to the unit's 88200).
- `scripts/test-systemd-lifecycle.sh`, `scripts/test-package-lifecycle.sh`.

## Related skills

`billet-capacity` (the drain and the barrier), `billet-backup-restore` (the other `local` commands), `billet-providers-local` (the Mac and the jailer that shape the units), `billet-releases-and-upgrades` (what the packages install and the host transaction). Also `billet-controller-retirement` (what a retirement may do to these services).
