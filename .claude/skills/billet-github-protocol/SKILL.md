---
name: billet-github-protocol
description: "Load when touching internal/scaleset (the only importer of the actions/scaleset client), internal/server's session or message handling, internal/github, JIT registrations, runner groups and workflow allowlists, `billet teardown`, `billet github-app create`, scale-set provisioning, or the proof of a cache's authority from GitHub's record of a run; or when GitHub's behaviour has to be measured rather than read."
---

# The GitHub protocol

## What this area is

billet long-polls GitHub's Runner Scale Set API; GitHub never connects to billet. `github.com/actions/scaleset` (v0.4.0, a public preview whose README says interfaces may change) is consumed only through `internal/scaleset`, which adapts vendor types to `server.Session` and carries no policy (depguard enforces the boundary). `internal/server`'s listener runs one session per tier: `Available → acquire, Assigned → consume escrow, Completed → release`. `internal/github` owns the App: the manifest flow, JWTs, installation resolution, `VerifyAppAt`, and the runner-group policy client. `docs/reference/upstream-references.md` records what billet takes from other people's code and what it does not, checked against source at named versions; read it before reimplementing anything near this protocol.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Sessions, messages and runners: [references/sessions-and-messages.md](references/sessions-and-messages.md)

- **The scale-set client is the answer to most protocol questions, and usually the only one.**
- **A message session is single-holder, GitHub will not hand it over, and eight runs bracketed the wait between 60 and 92 seconds.**
- **An unacknowledged message IS redelivered to a new session, and that is measured rather than assumed.**
- **The message lifecycle is idempotent on billet's durable scheduler id.**
- **A scale-set runner is a pool member, not the job that caused scale-up.**
- **JIT registrations are single-use and cannot update themselves.**
- **One target, one connection, so the connection is health-checked.**
- **A poll that fails past the client's retries is one tier's trouble, and the listener reopens its session rather than stopping the deployment (#207).**
- **The policy client owns its HTTP client, and no request it makes is unbounded.**

### Targets, trust, onboarding and cache authority: [references/targets-and-trust.md](references/targets-and-trust.md)

- **A trusted tier is a runner group GitHub restricts, and billet re-checks it before every mint.**
- **A target is an organization or a repository, and its GitHub path is its identity.**
- **Scale sets are billet's to remove.**
- **Onboarding is the App Manifest flow, so every deployment ends up with provably identical minimal permissions for its scope.**
- **Two issuers.**
- **billet is not actions-runner-controller without Kubernetes.**
- **Two compatibility caveats are permanent until GitHub moves.**
- **A cache's authority is proved from GitHub's record of the run, never from what the job says (#226).**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A rule about GitHub's behaviour.** Pin it to a measurement with its date, never to the documentation; the live tests record a JSON report under `BILLET_LIVE_REPORT_DIR` rather than asserting what GitHub answers (`billet-testing`).

## Where the tests are

- `internal/scaleset/*_test.go` (`endtoend_test.go`, `sessionconflict_test.go`, `teardown_test.go`, `wire_test.go`, `repository_test.go`, `version_test.go`), `internal/integration/repositoryscope_live_test.go` (opt-in, `BILLET_LIVE_REPOSITORY`), `internal/e2e/targets_test.go`, `internal/ops/fleetops/teardowntargets_test.go`, `internal/server/sessionconflict_test.go`, `sessionrecovery_test.go`, `jobresult_test.go`, `scaleset_provenance_test.go`, `runnerless_completion_test.go`, `internal/scaleset/broker_health_test.go`.
- `internal/github/*_test.go` (manifest, permissions, redaction), `internal/ops/setup/githubapp_test.go`, `internal/ops/fleetops/teardown_test.go`, `internal/ops/host/githubaccess_test.go`.
- `internal/integration/sessionreplacement_test.go`, `configboundary_test.go`; `internal/e2e/lifecycle_test.go` (a message is acked only after its work is done; a redelivered message does not start the job twice).

## Related skills

`billet-capacity` (escrow and the message lifecycle), `billet-identity-and-ca` (the App key and onboarding), `billet-security` (trust classes), `billet-config` (runner-group validation).
