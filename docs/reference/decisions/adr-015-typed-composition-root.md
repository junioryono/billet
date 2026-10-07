# ADR-015: A typed composition root, not a dependency-injection container

Accepted, 2026-10-07. Issue #356 (Phase 3), superseding issue #80 and draft PR #85.

## Context

billet assembled itself in three places. `cmd/billet`'s `runServer` and `cmdNode` built the control plane and the node; `internal/e2e`'s `newStackIn` and `wireUp` built them again for the end-to-end suite; `internal/replay`'s `buildStack` built them a third time for the replay harness. Only the scale-set targets and the node wire's handler were shared, so the house rule "assemble in tests the way the CLI does" was true only in part. The same collaborators were built by hand at many sites: per #80's count, the ledger opened at 34 sites in 13 files, `config.Load` at 44 and `alloc.New` at 11.

The control plane's startup order is a safety property, and nothing but parsed source held it. Everything authoritative must happen after the controller claim; the fleet's liveness must be forgotten and the shared authority adopted before the node wire serves (a promoted standby that read the authority first would mint a rival and drop the fleet); the authority must be published after the wire, which creates it on a first controller. `cmd/billet/promotionorder_test.go` asserted this with go/ast, and `state.ControllerClaim`, documented as proof of the claim, was a plain struct anyone could build and `becomeController` discarded.

#80 proposed a dependency-injection container, godi, to give billet one assembly, to make the order structural, to ban construction outside the assembly and to assemble tests the way production is. #85 carried phases 1–3 of its nine and stalled 795 commits behind main.

## Decision

**One assembly, in `internal/app`, written as typed functions.** It is the only package that builds the control plane and the node from a config; `cmd/billet` invokes it and the harnesses do too.

**The control plane's order is its types.** `app.OpenControlPlane` reads the identity, opens the ledger in the mode the config and invocation call for (`app.LedgerMode`, `app.OpenLedger`: control plane, standby, maintenance), builds the allocator, which validates the config, and does nothing authoritative. `(*ControlPlane).BecomeController` is the only way to have a `*Controller`, and it starts the fence that stops a replaced controller. A `Controller`'s steps hand each other proofs: `ForgetFleet` returns a `FleetForgotten` and `AdoptAuthority` an `AdoptedAuthority`, which `ServeWire` takes; `PublishAuthority` takes the `*ServingWire` and returns an `AuthorityPublished`, which `Schedule` takes. Each proof holds an unexported pointer to the controller that made it, each proof type its own field name (so no proof converts into another), and every step refuses a zero proof, another controller's, and a controller that is not held. A `Controller` and its `ControlPlane` record the addresses they were made at, so a copy, or a value written over another, holds no claim. Outside the package, skipping a step does not compile.

**Inside the package, a test holds what the compiler cannot.** `TestOnlyTheProvingStepsMakeTheirProofs` type-checks the package's production files against their dependencies' export data and holds every way to make a proof (a literal however its type is spelled or elided, a conversion, `new`, `copy`, a field written or addressed, an in-place overwrite, a returned type that carries one; a proof being the type, an unnamed or declared type of its shape, or a type parameter admitting one) to the one function that proves it. `TestEachMakerDoesItsStepBeforeItsProof` holds each maker to the operation the type checker resolves, called once, its error tested in a branch that returns it, and no proof before the test. It stops an ordinary edit from handing out a proof without its step; it is not a sandbox against code written to defeat it, and what it cannot read (`reflect`, `unsafe`, a generic helper over any `T`) the steps refuse at run time where they can.

**The node's order is OpenNode's.** `app.OpenNode` reads the certificate, claims the identity it names with the host-wide lock, and only then builds the client and the provider, releasing the lock if anything after the claim fails; `(*Node).Run` starts the guest cache, the sampler, the runner and the loop. The runtime the node runs (`app.NewNodeRunner`, `app.RunNodeLoop`) is the one the harnesses' simulated and docker hosts run.

**The harnesses assemble through it.** The end-to-end suite's stacks and the replay harness open their control plane with `app.OpenControlPlane` and walk the same steps `runServer` does, steering only what a test must (a clock, a lease TTL, pacing, an ordered provisioner, an in-process runner) through `app.Steering` and `ScheduleOptions.NodeRunner`, each applied after what the assembly sets. Scenarios whose subject is the wire itself (enrollment, mTLS, revocation) still build a node plane alone, on purpose.

**Loops are supervised.** `internal/supervise` runs every long-lived loop of the control plane: an essential task's error stops the others, a background task stops nothing and runs until the essential ones have returned, and every task is joined before its group returns. No panic recovery: billet does not panic, and a crash and a service-manager restart is the designed recovery.

**Boundaries are lint.** The depguard rule `appconsumers` lets only `cmd/billet`, `internal/e2e`, `internal/integration` and `internal/replay` import `internal/app`. forbidigo bans, outside `internal/app`, the owning package and tests (and in `cmd/billet` too, whose operator-output exemption names the messages it covers), the constructors that now have no other production caller: `server.New`, `nodeplane.New`, `nodeplane.Handler`, `nodeplane.BootstrapHandler`, `node.New`, `nodeclient.New`, `nodeclient.Run` and `rollout.NewCoordinator`. Both were measured at zero violations before they went in. The constructors cmd/billet's operator commands still call (`state.Open*`, `alloc.New`, the backends', the stores', `awscreds.Default`, `scaleset.New`) join the ban as #356 Phase 4 moves those commands into `internal/ops`.

## Measured

- godi v5.1.0, measured for #80: +19,664 bytes on darwin/arm64 and +24,729 bytes on linux/amd64 against stripped baselines of 48,986,498 and 50,050,289. Its close order is scopes, then the root, then singletons in reverse creation order; its diagnostics render types only; it builds independent singletons in an undefined relative order.
- The proof audit and the maker test answered ten rounds of adversarial review on the control-plane PR (#417); 52 mutants each fail a named test, among them every forgery shape and maker shape the reviews reported.
- The node move (#418) answered nine rounds; 39 mutants each fail a named test, among them the provider built before the claim, the lock kept on a failed open or let go once claimed, and the record path, drain request, readiness or second signal dropped between cmd/billet's host and the loop.
- With the harnesses assembled through `internal/app`, the end-to-end suite's 114 scenarios and the replay suite pass unchanged (2026-10-07, docker daemon present; the live CodeBuild scenario skips without an account), and `server.New` has no caller outside `internal/app` and `internal/server`.

## Consequences

- A step moved above the claim, or the wire served before the fleet is forgotten and the authority adopted, does not compile; `promotionorder_test.go` is gone.
- A new piece of the control plane is added in `internal/app`, as a step of the `Controller` if it is authoritative, with its proof if a later step depends on it, and the audit's table names its maker and operation.
- The identity access a role takes stays in `cmd/billet` and is passed in (`app.IdentityAccess`, `app.Host`), because moving it would change the exit codes `billet server retire` reports; it moves with the host authority in #356 Phase 4b.
- The operator, decision and maintenance assemblies #80 sketched (`OpenOperator`, `OpenDecision`) are not written: those commands keep `cmd/billet/ledger.go`'s openers, which apply the same release-watermark rules, until Phase 4 gives them a home in `internal/ops`.

## Alternatives rejected

- **The container (#80, godi).** billet's critical orderings are side effects, not data dependencies: READY=1 before the standby's wait, the authority published after the wire, `ForgetEveryNode` before any registration. A container builds independent singletons in an undefined relative order, so none of those could live in a constructor, and #80's own design still resolved them by hand in `RunServer`. What the container would have bought (one assembly, construction banned elsewhere, tests assembled the same way) the typed root buys with no reflection, no dependency and no analyzers to keep a container safe.
- **Per-entity repositories under the assembly.** The ledger's transaction boundary is already one function (`DB.Tx`), and a decision spread across repositories is several transactions: moving the headroom check out of escrow's transaction produced 28 grants against a ceiling of 4.
- **Keeping the structural order test.** It parsed one function's statements, so the order held only while every step stayed a statement of `runServer`; any helper extracted from it escaped.

## What carried over from #85

`state.DSN` and `github.AppKey`, the secret types that redact themselves on every rendering path (#377); the ledger modes as a closed set with one opener each; the role-by-role assembly; and `wiringconsumers`, now `appconsumers`.
