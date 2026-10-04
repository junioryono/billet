---
name: billet-security
description: "Load when touching anything that reads or writes a key, token or credential (the App key, the DSN, JIT registrations, connector tokens, cache bearers, AWS credentials), any code path that destroys compute or signals a process, any provider's untrusted-work gate, a redaction method, the upgrade root or a root-owned evidence file, or the systemd units' hardening."
---

# Security

## What this area is

billet holds a GitHub App private key per target that can mint tokens for a whole organization, or for one repository through `administration: write`, the only permission GitHub offers for registering a repository's runners and a far wider grant than the organization one (used for registration and nothing else, disclosed at creation, and installed on the one repository rather than every one the owner has); a node-wire CA, per-job runner registrations (JIT configs), per-guest cache bearers, an optional TLS interception CA, and on cloud nodes AWS credentials that can create and destroy machines. The threat model is stated in the README: billet is not a sandbox for untrusted code, and a job can affect the host it runs on unless the backend is a real boundary and the network is its own. Everything here exists because a quiet mistake with one of those credentials is expensive and invisible.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Privilege, trust and destruction: [references/privilege-and-trust.md](references/privilege-and-trust.md)

- **The server is unprivileged; the node is a root custodian.**
- **Trust belongs to a pool, not to the assignment that scaled it up.**
- **Untrusted work needs a real boundary and a network of its own, and each backend answers differently.**
- **A registration proves who you are; only a command proves what you may do.**
- **Destruction is scoped by deployment identity.**
- **Never signal a pid you have not proved is still yours.**
- **Giving away a hard link gives away the inode.**
- **Privileged operations go through a descriptor, never a pathname.**
- **The IAM grants have no delete where the credential would sit beside what it could destroy.**
- **Root-created identity artefacts are handed back to the service account by descriptor, named, never walked.**

### Secrets: [references/secrets.md](references/secrets.md)

- **The JIT registration is never in argv and never in a log.**
- **Nothing the guest wrote is ever quoted into a billet error.**
- **Every type that holds a credential redacts itself, on a value receiver, and the table proves it by mutation.**
- **A failed fetch names the URL it was on, and after a redirect that URL is signed.**
- **A typed nil satisfies an interface and panics on use.**
- **A connector token is a bearer credential and travels in the environment, never argv.**
- **The converge action's credentials land in files or the environment, never argv, and its cleanup removes only what the run created.**

### Cache credentials: [references/cache-credentials.md](references/cache-credentials.md)

- **A cache bearer lives exactly as long as the compute it belongs to.**
- **Interception terminates TLS for one host and serves three methods.**
- **A cache publishes only what the ref GitHub proves may write, and reads are the pool's (#226, ADR-013).**
- **The Git proxy holds a job's GitHub credentials for exactly one request.**
- **No cache bearer is written where a job can read it after the fact.**

### Root-owned evidence: [references/host-evidence.md](references/host-evidence.md)

- **The upgrade root is a trust boundary, proved through descriptors before any lock, and the executable a guard records is verified by digest and never run.**
- **The role runs a guard's recorded executable only after the fallback module proved it, and holds under a name the driver chose.**
- **The node's registration record is root-owned evidence under the node unit's own runtime directory, and it is judged on the descriptor that is read.**
- **The endpoint receipt is root-owned evidence, and the migration signals only its own subprocess.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A new type that holds a credential.** It redacts itself on every rendering path, on a value receiver, and joins the mutation-verified redaction table ([references/secrets.md](references/secrets.md)). It never reaches argv, a log or a file the guest controls.

**A path that destroys compute or signals a process.** Scope it by deployment identity and signal only a pid proved still yours ([references/privilege-and-trust.md](references/privilege-and-trust.md)).

## Where the tests are

- `internal/awscreds/*_test.go` and the provider clients' redaction tables; `internal/github`'s `App` redaction tests.
- `internal/provider/firecracker/*_test.go` (pid proof, chown scope), `internal/lifeops/*_test.go` (descriptor rules, link counts).
- `internal/nodeplane/*_test.go` (JIT entitlement, register-before-decode with a counting body), `internal/e2e/wire_test.go`.
- `actions/convergefleet_test.go` (credentials 0600 and exported, environment lines never in argv, pins appended, `ssh-keyscan` never run, cleanup scoped to the run's marker).
- `internal/provider/codebuild/*_test.go` (never echoes its registration, `TestTheSweepNeverDecodesAValue`), `internal/provider/tart/*_test.go` (`Accepts` and the flag builder), `internal/provider/ec2/*_test.go` (metadata options, endpoint scheme).

## Related skills

`billet-identity-and-ca` (the App key and the CA in detail), `billet-providers-local` and `billet-providers-aws` (each backend's boundary), `billet-storage-and-cache` (cache trust), `billet-lifecycle` (the units' hardening and ownership repair).
