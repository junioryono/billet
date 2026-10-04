---
name: billet-node-wire
description: "Load when adding a route, a command kind or a field to a wire type; when bumping the protocol version; when touching internal/nodeapi, internal/nodeclient, internal/nodeplane or the two listeners in cmd/billet; when a node cannot register or is fenced; or when touching enrollment, certificate renewal, the bootstrap listener, connection budgets or handshake timeouts."
---

# The node wire

## What this area is

A node dials out and never listens. `internal/nodeapi` declares the request and response types, the command kinds (`CommandLaunch`, `CommandDestroy`, `CommandSweep`, `CommandTend`, `CommandUpgrade`, `CommandInventory`) and the version range. `internal/nodeclient` is the node side (`Register`, `Poll`, `Report`, `Withdraw`, the lease calls, `Renew`, enrollment; `loop.go` drives `Poll → execute → Report` one command at a time). `internal/nodeplane` is the plane side: `Handler` (mTLS routes under `/v1/register` and `/v1/nodes/{node}/…`), `BootstrapHandler` (`/v1/ca` and `/v1/enroll`, no client certificate), `Plane` (dispatch, registrations, the barrier loop), and `Runner`, which implements `server.Runner` so the listener cannot tell whether compute is a goroutine away or a continent away. `cmd/billet/handshakelistener.go` bounds connections on the real wire.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### The version range: [references/versions.md](references/versions.md)

- **The wire is a range, and the bridge runs one way.**
- **`MinVersion` is a promise about meaning, not about whether fields parse.**
- **What each version added, and whether an older peer is refused or reported.**
- **The release string is reduced on ingest.**

### Authentication, commands and fences: [references/authentication-and-fences.md](references/authentication-and-fences.md)

- **A registration proves who you are; only a command proves what you may do.**
- **An inventory is an observation, not permission to claim another node's lease.**
- **`/v1/register` authenticates before it reads a body.**
- **A registration discards the host's barrier run before the request is judged.**
- **Incarnation and epoch are different fences.**
- **Commands are a queue with a ten-minute timeout that starts when queued, and only launches run beside each other.**
- **Placement reads do not hold the shared plane mutex.**

### Listeners, enrollment and renewal: [references/listeners-and-enrollment.md](references/listeners-and-enrollment.md)

- **Two listeners, because a budget taken before the handshake cannot separate callers.**
- **The permit is charged only to what verified.**
- **Enrollment is two fingerprints compared by a person.**
- **Renewal is authenticated by the certificate being replaced, and the subject comes from the identity.**
- **A loopback wire has no certificates.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A field or a route.** The plane decodes bodies strictly and a node decodes responses leniently, so decide whether an older peer is refused or reported, gate the new behaviour on its version, and record it in the version table ([references/versions.md](references/versions.md)). Upgrade the server first. A node upgraded first is refused at registration only when its registration carries a field the older plane does not know or the ranges do not overlap; otherwise it registers at the highest version both speak, so the gate on the negotiated version is what protects everything after registration.

## Where the tests are

- `internal/nodeplane/*_test.go` (guards, dispatch fences, barrier loop), `internal/nodeapi/*_test.go` (range negotiation), `internal/nodeclient/*_test.go`.
- `cmd/billet/handshakelistener_test.go`, `limitedlistener_test.go`, `bootstrapwire_test.go`, `wirewindow_test.go`.
- `internal/e2e/wire_test.go`, `enroll_test.go`, `revocation_test.go`, `mtls_test.go`: `TestAnUnenrolledConnectionCanReachNothingElse` and the superseded-incarnation scenarios.

## Related skills

`billet-identity-and-ca` (the authority the wire trusts), `billet-capacity` (what the commands and fences protect), `billet-security` (what a node may and may not ask for), `billet-releases-and-upgrades` (server-first rollouts).
