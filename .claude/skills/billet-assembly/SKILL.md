---
name: billet-assembly
description: "Load when touching internal/app or anything that builds the control plane or a node: runServer or cmdNode, OpenControlPlane and the Controller's steps, the proof types and their audit (proofs_test.go), OpenNode and the node runtime, app.Steering, the e2e or replay harness's assembly, the appconsumers depguard rule, or the forbidigo ban on role constructors; and before adding a step to startup."
---

# The assembly

## What this area is

`internal/app` is billet's composition root (ADR-015): the one package that builds the control plane and the node from a config. `cmd/billet`'s `runServer` and `cmdNode` invoke it, and so do the end-to-end suite and the replay harness. The control plane's startup order is held as types: `OpenControlPlane` → `BecomeController` → `AdoptAuthority` and `ForgetFleet` (whose proofs `ServeWire` takes) → `PublishAuthority` (whose proof `Schedule` takes) → `Scheduler.Run`. The node's is `OpenNode` (certificate, claim and lock, then client and provider) → `Node.Run`. `internal/supervise` runs every long-lived loop. Identity access stays in `cmd/billet`, passed in through `app.Host`, until #356 Phase 4b.

Each invariant below is one line here and stated in full, with the failure behind it, in the reference file. Read it before changing what an invariant covers.

## Invariants: [references/assembly.md](references/assembly.md)

- **A step that only the controller may take is a method of the Controller, and a step a later one depends on returns a proof the later one takes.**
- **Only the step that proves a thing makes its proof, and the audit is the type checker's, not a reading of the syntax.**
- **Each maker calls its operation once, tests that call's error, and builds its proof after; a maker stays plain.**
- **A Controller and its ControlPlane are bound to the addresses they were made at.**
- **The node's provider is built only under its deployment lock, and the lock is released on every path.**
- **The harnesses assemble through internal/app and steer only through app.Steering and ScheduleOptions.NodeRunner.**
- **The roles are built in one place, and lint says so.**

## Checklists

**A new piece of startup.** Put it in `internal/app`. If only the controller may do it, make it a method of `Controller`; if a later step depends on it, return a proof (a struct with one unexported field of its own name pointing at the controller) and take it in the later step, then add the proof to `proofMakers` in `proofs_test.go` with its maker and operation as the type checker names them. Add a mutant that skips the step and confirm the audit or the step-by-step test fails.

**A new constructor of a role's piece.** If nothing outside `internal/app` calls it in production, add it to the forbidigo ban in `.golangci.yml` and the exclusion beside it, measured at zero first.

## Where the tests are

- `internal/app/proofs_test.go` (the audit, the makers, `TestTheControlPlaneAssemblesStepByStep`, `TestAForeignOrZeroProofIsRefused`, `TestAFailedAdoptionMakesNoProof`), `roles_test.go` (`TestEveryRoleAssembles`), `node_test.go`, `nodecache_test.go`, `leadershipwiring_test.go`.
- `cmd/billet/nodedrainreport_test.go` (the node command's order and host), `standbystop_test.go`, `internal/nodeclient/record_drain_test.go` (R6, the record path's links).

## Related skills

`billet-state` (the claim and the fence), `billet-identity-and-ca` (the authority adopted and published), `billet-node-wire` (what `ServeWire` serves), `billet-testing` (the harnesses), `billet-checks-and-lint` (depguard and forbidigo).
