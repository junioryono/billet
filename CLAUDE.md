# billet

Self-hosted GitHub Actions runners on your own hardware, with a colocated cache.
Go, Apache-2.0, single static binary. Pre-alpha.

## How to Work on This Codebase

### Skills First

Skills encode multi-step workflows and hard-won conventions. Before exploring code or planning an
approach, check which skills apply and load them. Skipping one means rediscovering something that is
already written down.

1. **Load skills before researching.** They contain the context you need.
2. **Create skills for new patterns.** Repeatable multi-step work with no skill → suggest one via
   `skill-creator`.
3. **Keep them current.** If a change invalidates something a skill says, update the skill in the
   same PR.
4. **Keep CLAUDE.md current.** Same rule.

### Codex compatibility (AGENTS.md + .agents/skills are symlinks)

This repo supports OpenAI Codex, which reads the cross-tool standard files. **Claude's files are
canonical**; the standard paths are committed symlinks. Never create a real file at a symlink path,
and never edit `AGENTS.md` (you would be editing the `CLAUDE.md` it points to).

- `AGENTS.md -> CLAUDE.md`
- `.agents/skills/<name> -> ../../.claude/skills/<name>` for every skill. **Add the symlink in the
  same PR that adds a skill**, and keep SKILL.md frontmatter strict YAML — quote any description
  containing `:`, since Codex rejects invalid YAML that Claude tolerates.
- `.codex/config.toml` raises `project_doc_max_bytes`; keep it if you touch Codex config.

### Verify your work

`make check` is the gate. It runs build, vet, gofmt, lint, and `go test -race`. All of it must be
clean before a commit. There is no "the linter is being annoying" — see the lint section below.

### Every commit gets an independent review before it is pushed

`make check` proves the code compiles and its own tests pass. It cannot tell you the design is wrong.
**Every commit is reviewed by Codex before being pushed** — before, because publishing unreviewed
code is the wrong order and billet holds credentials that make a quiet mistake expensive.

The exact invocation, the flags that are load-bearing, and the rule that every finding must be
validated by hand before acting on it are in the `billet-git-flow` skill. Read it rather than
improvising the command; several of its details each cost a debugging session to learn.

---

## Architecture

One binary, two roles. `billet server` is the control plane: it long-polls GitHub's Runner Scale Set
API for assigned jobs, owns the capacity ledger, and tells nodes what to launch. `billet node` is a
compute host: it runs a provider and launches instances. `billet server --dev` runs both in one
process — the single-machine deployment.

```
cmd/billet/          the binary: server | node | dev roles, plus the whole operator CLI
internal/config/     billet.yaml schema + validation (a leaf package — imports nothing of ours)
internal/state/      SQLite control-plane store: capacity ledger, job history, process lock
internal/github/     App Manifest onboarding, App JWT, installation resolution
internal/alloc/      global capacity allocator + lease state machine
internal/server/     scale-set listeners, scheduler                              (P1)
internal/node/       node runtime: provider driver, capacity reporting, mTLS     (P2)
internal/provider/   firecracker | tart | ec2 | docker                            (P1+)
internal/store/      zfs | ebs | apfs — CoW clone, generations, atomic publish   (P3)
internal/cachev2/    GitHub Actions Cache v2 Twirp + conformance suite           (P4)
docs/                reference-hardware.md — the bare-metal host billet is measured against
```

Layering is enforced by `depguard` in `.golangci.yml`, not by convention: `provider` and `store` are
siblings that may not import each other or the scheduler, and `config` may not import any other
billet package.

---

## Decisions written down

`docs/adr-001-control-plane-hosting.md` — where the control plane runs and what stores its state.
Short version: a single small EC2 with SQLite on EBS, recovered by EC2 auto-recovery (NOT an ASG —
an ASG launches a fresh instance that does not reattach the data volume), ~$7-13/mo, no code change.
DynamoDB was considered seriously and rejected FOR NOW — it is feasible, it saves nothing, and it
costs a rewrite of the two most invariant-dense packages. Revisit it when more than one controller is
genuinely wanted, which is the only thing SQLite cannot do at any price.

The fact that resized that decision, and which is easy to forget: **GitHub queues a job for 24 hours
when no matching runner is available.** A dead controller delays CI rather than failing it, so the
requirement is "recovers in minutes", not HA. Do not buy availability machinery the failure mode does
not require.

## Upstream references

`docs/upstream-references.md` records what billet takes from other people's code and what it
deliberately does not. Read it before reimplementing anything that touches the scale-set protocol.

Two things from it that come up constantly:

- **`github.com/actions/scaleset` is the answer to most protocol questions**, and usually the only
  answer — the API is not documented elsewhere. `actions-runner-controller` depends on the same
  module at the same version, so the `listener` package inside it is ARC's, not a generic vendor's.
- **billet is not actions-runner-controller without Kubernetes.** ARC does not track individual jobs
  at all; its whole scaling decision is `min(MinRunners+TotalAssignedJobs, MaxRunners)` and
  Kubernetes absorbs scheduling, queueing and placement. Billet has fixed hardware, one global budget
  across tiers, and placement constraints (CCD locality, the macOS licence cap, guest-OS allowlists)
  that need a lease bound to a specific host. ARC has no cache, no sticky disks, no microVM
  isolation — everything that makes this project worth building.

## Invariants

These are the rules that a change can silently break. Each one exists because the alternative was a
real failure mode, not a preference.

### The control plane has exactly ONE authoritative writer

Corrupting the capacity ledger means double-admitting jobs onto a machine that cannot hold them, and
the failure is quiet — jobs are accepted, then the host OOMs or thrashes.

- **An exclusive `flock` on the state directory is held for the DB's lifetime.** SQLite's own
  single-writer rule prevents simultaneous *writes*; it does not prevent two billet processes both
  long-polling GitHub and taking turns writing conflicting decisions. The second process must not
  start at all. `flock` rather than a PID file, so a crash releases it automatically.
- **Writes go through `DB.Tx` on a one-connection pool.** An allocation decision is read-current,
  decide, record — one transaction, not a read followed by a hopeful write.
- **`DB.Reader()` returns a narrow `Querier`, never `*sql.DB`.** Handing out the pool would let any
  caller write through a connection that is supposed to be read-only.
- **The database MUST be on local storage.** SQLite's WAL is explicitly unsafe on a network
  filesystem. `Open` reads the pragmas back and fails closed if `journal_mode` is not WAL, which is
  what catches an NFS state directory.

### Migrations are append-only and identified by version + checksum

Never edit or reorder an existing migration; add a new one. `migrate` verifies a recorded checksum
against the binary's SQL and refuses on mismatch, and refuses a database carrying a version this
binary has never heard of. Counting applied rows is not a version — a deleted row reruns a
migration, a forged row skips one, and inserting one in the middle silently reruns the tail.

### Cache TLS interception defaults OFF, per tier

`ACTIONS_RESULTS_URL` carries **artifact** metadata as well as cache traffic, so anything in that
path is in the user's release path. `Tier.Intercept` is opt-in, and tiers that publish release
artifacts or hold deployment secrets must not enable it. A mistake here does not slow CI down; it
breaks a deploy.

The protocol is reverse-engineered — GitHub has never published the `.proto` files — so the cache
must **fail open to a miss** on any error, never fail a job, and a conformance suite must run the real
`actions/cache`, `upload-artifact` and `download-artifact` against live GitHub to catch drift. Both
are requirements on the cache when it is built (P4); neither exists yet, and nor does the cache.

### The macOS guest limit is enforced against `guest_os`, never a label

Keying the limit off a label matching `macos` means a tier named `sonoma-arm64` escapes it entirely,
and a Linux tier named `builds-macos-artifacts` gets capped for no reason. `Tier.GuestOS` is the
explicit field, macOS tiers must pin a `node`, and per-node totals are summed at load. Warm
instances count.

The config check is a **guard, not the enforcement point**: the allocator holds a single host-wide
count of running plus warm macOS guests at runtime, because two individually-valid tiers still share
one physical Mac. Both read the effective limit from the same `NodePolicy`, so there is one number
rather than two that drift.

**The limit is per host and configurable; `DefaultMacOSVMLimit` is a default, not a ceiling.** Apple's
standard licence permits two macOS guests per Apple-branded host, which is what a config that says
nothing gets. But what a host may run is a deployment decision, not a fact about the hardware — an
Apple Silicon machine can serve macOS guests, Linux arm64 guests, or both — so `nodes:` carries a
per-host `guest_os` allowlist and `macos_vm_limit`. Raising the limit is permitted because billet
cannot know what licence or hardware agreement an operator has; it is an assertion about their
licence, which is why the diagnostic names Apple only when the limit came from the default.

Two rules keep that from becoming a footgun. A tier pinned to a host that does not permit its guest
OS is a load-time error rather than a job that queues forever with nothing saying why. And
`macos_vm_limit > 0` together with a `guest_os` allowlist excluding macOS is rejected instead of
silently resolving — a config that reads as "two macOS guests" must not schedule none.

**Placement is checked where the host is known, and again at the launch boundary.** Config
validation cannot see every placement: an *unpinned* tier names no host, so nothing ties it to the
allowlist, and a scheduler that simply picked a node with free capacity would put a Linux guest on a
macOS-only Mac. `Bind` is the first point at which the host is known. The load-time guard covers what
it can prove — a pinned tier, or an unpinned one against a host declaring the *same provider*, since
a Firecracker tier can never land on a Tart host and comparing guest OS alone would make one
macOS-only Mac an error for every x64 Linux tier in the deployment.

`Bind` alone is not enough, for two reasons that each looked fine in isolation:

- **Nothing required it.** `assigned → launching` succeeded on an unbound lease, so a caller could
  pick a host outside the allocator and every check inside `Bind` would never run. Every phase that
  presumes a running host — `launching`, `online`, `busy` — now requires a bound node. Gating only
  the `launching` edge is not sufficient either: a row left in `launching` by an older binary would
  walk on to `online` untouched.
- **Binding is not launching.** A lease can be bound while still in `capacity`, so a policy tightened
  in between would let the instance start on a host that no longer permits it. Placement is
  re-checked on entry to those phases against policy in force *then*, making the guarantee "legal
  now" rather than "was legal once". Only a repeated `Bind` is grandfathered, because it changes
  nothing.

**A lease whose placement facts are unverifiable fails closed.** A row predating the `provider`
column records `""`, and tolerating that would be a bypass rather than a compatibility courtesy —
such a lease may still be *unbound*, so it is not old work already placed but unplaced work whose
backend nothing can check. `Reap` and `Release` deliberately do not consult these checks, so a lease
refused this way can always be cleaned up; failing closed on something unrecoverable would just
strand capacity.

That rule is also what protects rows that no migration can classify. `macos_slot` only became
truthful at migration 5, which added it defaulting to `0` without a backfill, so a macOS lease older
than that is indistinguishable from a Linux one. Migration 7 repairs what it can and the rest are
refused rather than guessed at.

The lease's `guest_os` is recorded at reserve time for the same reason as `target_node` and
`macos_slot`: a tier redefined underneath an in-flight lease must not reclassify what that lease is
allowed to bind to.

**`NodePolicy` is deep-copied when the allocator is built.** `GuestOS` is a slice and
`MacOSVMLimit` is a pointer, so copying the map alone still shares both — letting a caller widen an
allowlist or raise a cap after construction, moving a licence limit out from under leases already
counted against it. `NodePolicy.Clone` owns that, so there is one place to get it right.

### Capacity is escrowed BEFORE a listener advertises

Each tier is its own GitHub scale set with its own `maxCapacity`. If listeners advertise
independently, GitHub can fill all of them at once and the host is overcommitted with nothing to
stop it. Reserve against the global ledger first, advertise second. Capacity is a **vector** — CPU,
memory, macOS licence slots, disk — never one integer.

`internal/alloc` owns this, and three details are load-bearing:

- **The headroom check and the insert are ONE transaction.** Checking outside it is a read followed
  by a hopeful write. Measured: moving the check out produced **28 grants against a ceiling of 4**
  under concurrency. `TestConcurrentReservationsNeverOvercommit` is the guard.
- **A lease's `node` stays NULL until a node binds it.** A reservation is *constrained* to a node by
  its tier's config, not *bound* to one — and the column has a foreign key to `nodes(name)`, which at
  reserve time may name a host that has not registered yet. So Apple's per-host limit counts by the
  set of macOS tiers pinned to that node, not by `leases.node`, which would read zero during exactly
  the window the limit exists to cover.
- **The epoch is a fence, and a reclaim bumps it.** Without that, a holder declared dead and replaced
  keeps writing to a lease someone else now owns — an orderly takeover becoming two concurrent owners
  of one slot. Every write presents its epoch; a stale one is refused.

The state machine is written down in `validTransitions` rather than implied by scattered UPDATEs, and
terminal phases have no successors: a lease that released its capacity must never move backwards and
re-acquire it, which is what a double-admit looks like from the inside.

### What is advertised is TOTAL escrowed capacity, and it is renewed on its own clock

The listener holds the escrow in three states — `held` (free), `acquiring` (promised to a request
billet has claimed from GitHub but has not yet been given, keyed by request id), and `running`
(assigned). All three are escrowed and **all three are advertised**, because
`maxCapacity` is the scale set's *total* capacity, not its spare. The vendor's own listener sends a
configured maximum that does not move as jobs are assigned. Sending only the free half shrinks the
advertisement on every assignment, so a tier with room for two tells GitHub "1" the moment the first
job lands and the second slot goes unused. The invariant above is untouched: every lease in all three
came from the allocator, so the sum across listeners is still bounded by the budget.

**`acquiring` exists because a promise is not a count.** Capping acquisitions at "how many free
leases are there right now" is an instantaneous measure, and an acquisition is an obligation that
lasts until the assignment arrives. Leaving the lease in `held` while the claim is in flight let one
lease back two consecutive offers, let a lease promised to one request be consumed by the assignment
of another, and let the heartbeat spend it out from under a claim already on the wire. Reserving it
under the mutex *before* the network call fixes all three at once, which is the tell that they were
one bug rather than three.

**Heartbeats must not be bounded by the poll.** A long poll was assumed to be about 50 seconds
against a 90 second TTL, which reads like ample margin. **Measured against a real organization on the
first poll billet ever made, it ran ~88 seconds** — two seconds inside the TTL — and the vendor's HTTP
client permits far longer once slow responses and retries are counted. Renewal that happens only
*between* polls stops for as long as one poll lasts. The reaper then terminalises the leases, another tier
escrows the capacity, and the poll returns an assignment backed by a lease this listener no longer
holds. Tying renewal to the poll makes the safety of the whole escrow depend on a timeout billet does
not control.

**Derive the cadence from the allocator's actual TTL, never from `DefaultLeaseTTL`.** A cadence
computed from the default is correct only for a default-configured allocator; under a shorter TTL
every lease expires between beats and advertised capacity climbs — measured at six times the budget
before a test caught it. Nothing in the type system will catch this.

That makes heartbeat the only writer concurrent with the poll loop, so the escrow takes a mutex, and
`assign` holds it **across** the allocator write. Releasing it around the write to keep heartbeats
snappy looks obviously right and is not: it opens a window where the write succeeds but a concurrent
heartbeat has already dropped the lease, leaving the assignment durable in the ledger and tracked
nowhere in memory. A transient heartbeat error **keeps** the lease — only `ErrFenced` and
`ErrLeaseNotFound` mean it is genuinely someone else's. Dropping on a busy database removes the lease
from the release path too, so the ledger keeps counting it until the reaper expires it.

The message lifecycle closes: `Available` → acquire, `Assigned` → consume escrow (idempotent by
request id, because an unacknowledged message is redelivered), `Completed` → release. Acknowledging
`Completed` without releasing leaves the lease open until the reaper expires it, withholding capacity
and recording the wrong conclusion against it.

### A commitment made to a remote service cannot be revoked by a local timer

`acquiring` is the one escrow state with no way back on its own: every exit needs GitHub to say
something. That reads exactly like a leak — a lease renewed forever because a message went missing —
and the obvious fix is to age it out. **That fix was written, reviewed, and reverted, and the reason
generalises well beyond this listener.**

`AcquireJobs` is one-way. There is no decline or release endpoint on the session client, and
`DeleteMessage` acknowledges a *notification* rather than refusing a job. So releasing the escrow on
a timer hands nothing back to GitHub. It only means billet has forgotten it owes a runner while
GitHub still expects one — the freed slot goes to another tier, and the assignment, when it arrives,
has nothing behind it. A fix for a capacity leak had created a way to drop live work.

The reasoning that produced it was sound and the premise was not. The question asked was *can this
state end on its own?* The question that mattered was **are we entitled to end it unilaterally?**
When a local record mirrors a commitment held by someone else, only they can release it; the local
timer can report, and that is all. So a stale promise is reported and kept — it is capacity billet
genuinely still owes — and the real remedy is to invalidate the session so GitHub itself redelivers.

**The second attempt is the instructive one, because it looked like it had escaped this.** Instead of
a timer it released a promise when GitHub's own statistics reported the scale set had no job
outstanding of any kind — apparently authorised by the party holding the commitment, with a staleness
check only to avoid racing an in-flight acquisition. It was the same bug. The staleness check bounds
how old *billet's promise* is; it says nothing about how old the *statistics* are, and the response
carries no observation time, generation, or request identity to pin them to. Elapsed local time was
still doing the authorising, now paired with a snapshot of unknown freshness — plus a quiet
assumption that those counters are exhaustive, atomically maintained, and scoped as expected.

So: **a freshness check on your own record is not a causal fence on someone else's snapshot.** To act
on remote state you need something that provably postdates your own action — a version, an
observation time, a per-request status. Absent that, more samples and longer thresholds are the same
guess with more decimal places. When the API does not offer one, the honest answer is to leave the
gap open, report it, and go and measure the real system.

The same distinction decides how loudly to fail. An assignment with no escrow behind it **declines
and carries on**, because that is reachable by ordinary races and stopping the control plane strands
every tier's capacity over one job. A scale-set response that is not a subset of its request
**stops the listener**, because no race can produce it: it means the API broke, billet can no longer
tell which remote commitments are real, and stopping is itself the remedy since a fresh session makes
GitHub redeliver. Match the blast radius to whether the condition is a race or a broken contract.

### A rule about someone else's API is pinned to measured behaviour, not to reasoning

The runner-group validator began as an allowlist of "URL-safe" characters and was wrong in both
directions: it rejected `team=platform`, `who?`, and every non-ASCII name — `Grupo-Ñ`, `研发` — while
missing `;` entirely. The client interpolates the name unescaped into a path, then `url.Parse`s it,
reads `Query()`, and re-`Encode`s it, so the only question that matters is whether a character
survives that round trip. Running it settled it in a minute: `&` `#` `;` `%` `+` do not, everything
else does. `;` is the one no amount of reasoning would have produced (Go's `ParseQuery` has rejected
it as a separator since 1.17).

The test asserts the **property**, not the list: every name the validator accepts is put through the
client's exact transformation and must come out unchanged. When a rule encodes an assumption about
code you do not own, pin it to what that code does — a probe costs a minute, and a plausible-sounding
character list is exactly the kind of thing that is confidently wrong.

### A credential GitHub issued once is never deleted, and never rendered

GitHub returns the App private key **exactly once**, from the manifest conversion. There is no
re-issue. Every rule here exists because a review found a way to lose or leak it, and several were
introduced by the fix for the previous one.

**The reservation never occupies the destination.** This is the shape everything else rests on, and it
took four rounds to reach. While the reservation *was* the key path, installing meant unlinking that
path first — and a pathname unlink cannot be made safe by any check preceding it, because the check is
never atomic with it. Every guard tried (`os.SameFile`, then "and it is still empty") still had an
ordering where another run's key was deleted on the way to installing this one.

Reserving a sibling file removes the unlink entirely, and collapses two files into one: the reservation
*is* the staging file. The destination is created exactly once, by an `os.Link` that **fails rather
than replaces**. There is no rename fallback — `os.Rename` has no no-clobber form in Go, so on a
filesystem that cannot hard-link billet reports the staged key and the operator moves it by hand.

**Nothing is deleted by pathname unless it is known not to be a key.**

- The **reservation cleanup is gone.** An aborted run leaves its staging file, and `reserveKeyFile`
  prints the exact `rm` after inspecting whether it is a leftover or a credential.
- The **staging file is removed only after a successful install** — `os.Link` leaves two names for one
  private key, so that removal is mandatory — and only after `os.SameFile` confirms the name still
  refers to this run's file, with the directory synced afterwards so a crash cannot resurrect the entry.
  A failure to remove it is reported, never swallowed: an unmentioned second copy of an App key is what
  nobody finds until it matters. **The `SameFile` check narrows this race; it does not close it.** Go
  unlinks by name, the check cannot be atomic with it, and a file swapped in between the two would be
  deleted. That residual is accepted and stated rather than claimed away.
- **"Could not tell" is never "no key here."** `inspectKey` returns present / absent / unverifiable, and
  only *absent* permits a deletion or a "your credential is gone" message. A stat that FAILS is not a
  mismatch either, so identity answers matches / differs / unknown and path lookups answer present /
  absent / unknown. **Three-valued types get collapsed back at the call site if you let them** — a
  `!= identityMatches` undid one of these a line after it was introduced, and a `fileExists` that
  returned false on EACCES made billet recommend `mv` onto an occupied destination. Callers use `inspectKey`
  directly — the boolean wrappers over both were deleted, because every one of them collapsed the third state at
  exactly the call site that needed it. Note that unlink permission comes from the DIRECTORY, so an
  operator can act on a bad `rm` suggestion for a file billet could not itself read.
- **The staging name is re-inspected after `O_EXCL` fails.** The answer from before the attempt is
  stale by then: a concurrent run's empty reservation can have become a complete key in between, and
  printing "it holds no usable key" beside an exact `rm` handed the operator a command that destroys it.
- **"Not a valid key" is not "safe to clobber."** Whether to recommend `mv` asks `lookupPath`, not
  `inspectKey` — a PEM with trailing junk, a format this build cannot parse, or a file a live writer
  has not finished are all worth keeping, and `mv` replaces. Every `mv` suggestion in this file checks
  the destination first, because the operator is following *billet's* advice.

**A pathname is only spoken about once it is tied to a descriptor.** `inspectKey(name)` answers "is
there a usable key at that name", which is NOT "this run's key is there" — conflating them let a
replaced recovery file be reported as this App's key while the real one sat at a moved path nobody was
told about. Identity is established first, everywhere, and an unknown identity yields uncertainty
rather than either claim.

**A credential is never declared lost while its bytes are still in memory.** `os.Link` takes a NAME
while the run owns a DESCRIPTOR, so the staging name is verified against the descriptor before the link
and the destination is verified after it — the second catches the window the first cannot. But what
follows a failed check is **not** "your key is gone": `writeKeyAtomically` still holds the complete PEM
at every one of those points, so it writes the key to a fresh `O_EXCL` file and reports where it
landed — verified against the descriptor and directory-synced before that promise is made. An earlier
version reasoned that an unlinked inode cannot be given a name again: true, and irrelevant, because the
bytes never depended on that inode. Declaring a credential unrecoverable while it sits in a live
variable is the worst mistake available here, because the advice that follows is "delete the App".

The same reasoning applies one level down, and the first version missed it there: a recovery write that
REPORTS an error may still have left a usable key, so the file is inspected rather than assumed empty.
**Loss is what remains after looking, never what is inferred from a return value.** A recovery that
fails also says only what it knows — this directory could not hold it, which is not proof that none
could.

**`billet check` proves the key WORKS**, not that it exists — regular file, no group/other permission
bits, bounded read, and actually parsed, all from one descriptor opened `O_NONBLOCK` so a FIFO cannot
hang it. `os.Stat` alone accepted a directory, an empty file, a truncated PEM and mode 0644.

**The one-time code is removed STRUCTURALLY, and from every error in the chain.** It is still live
when the exchange fails, and it reaches a terminal through `*url.Error`, which embeds the whole URL.
Two rules, learned separately:

- **Sanitize where the error is created, not at the boundary.** Every wrapper renders the message of
  the error beneath it, so cleaning the innermost one means no wrapper can carry the code and no later
  stage has to recognise the encoding it arrived in. A `*url.Error` is *rebuilt* with a fixed path
  rather than pattern-matched — double-encoding and over-encoding both defeat matching.
- **Redaction has to hold for the whole chain, including nodes with no structure.** Sanitizing
  `Error()` while `Unwrap` returned the original meant `errors.As(err, &urlErr)` handed back the live
  URL, and any reporter that walks causes serialized it. The walk handles `errors.Join` trees
  (`errors.Unwrap` returns nil for one, so a chain-only walk stops dead at the join), cuts cycles, and
  keeps a depth backstop. Identity is preserved wherever it can be, because `errors.Is` against
  `context.DeadlineExceeded` depends on it — but **not** at a node whose own text carries the secret.
  An opaque leaf that built its message from the request URL has nothing to rebuild, so it is replaced;
  safety beats identity there. **Clean every field**, not just the obvious one: `url.Error` has three,
  and copying `Op` verbatim let a transport put the endpoint straight through the one path that is
  supposed to be structurally safe.
- **Never compare two `error` values with `==`.** It panics when the dynamic type is not comparable —
  an error struct holding a slice is ordinary — so both the cycle guard and the did-this-change check
  go through `sameError`, which compares pointer identity where identity exists and answers "different"
  otherwise. That direction is the safe one: it rebuilds a node that did not need it, rather than
  crashing mid-onboarding and losing the key. A test written for the *first* instance is what found the
  second.
- **Match the endpoint, not just the code.** A caller-supplied `RoundTripper` composes its own text.
  The endpoint string is an exact literal billet constructed, so matching it needs none of the encoding
  guesswork that matching the bare code does. Renderings are captured once per node and reused, rather
  than calling `Error()` again to test and again to substitute. This narrows the stateful-error hole
  without closing it — a parent's `Error()` inherently invokes its children's — and billet supplies the
  transport, so a deliberately non-deterministic error is not in the threat model.

**Nothing derived from the conversion response body is ever rendered.** This is the one endpoint in
GitHub's API whose success carries a private key, so an intermediary forwarding that body under a
rewritten status would otherwise put the key on the terminal. Filtering the body does not work — a
secret out of its field is an opaque string, and `{"message":"whsec-…"}` carries no marker to catch.
The status is mapped to text billet writes itself. False positives cost GitHub's explanation; false
negatives cost the credential, and that asymmetry decides it. Other endpoints keep `apiError`.

**A code that does not redeem is discarded, not fatal.** The unguessable callback path is handed to
`open`/`xdg-open` as a command-line argument, and argv is readable by other local processes — so both
the path and the `state` must be assumed known, and only what a caller can *do* with them is bounded.
Treating the first code to arrive as final was a kill switch: inject a worthless one, and onboarding
ended with the App created and its key unrecoverable.

**Only a status that ESTABLISHES the code is unusable may discard it**, which is a much shorter list
than it looks: **404**, and nothing else. Four versions shipped before that. `{404, 422}` left the kill switch open
for an injected code drawing a 414. "Every 4xx" swallowed **429** — a rate limit says nothing about
the code, so a *valid* code was discarded while the App stayed created. And 422 is the subtle one:
GitHub documents it as *"Validation failed, **or the endpoint has been spammed**"*, so an attacker
feeding forged codes can trip abuse protection and make the honest code's 422 look like a rejection.
400 is not code-specific either — a proxy returns it for header and policy reasons.

**Everything that is not a definitive rejection is ambiguous**, and ambiguous codes are **retried
round-robin** — never one at a time. An enumerated ambiguous list left every unlisted status falling
through as fatal, which is how removing 414 from the rejection set preserved the exact failure it was
removed to fix. And retrying a single code in a blocking loop reopened the kill switch in slow motion:
an injected code that always draws 422 monopolised the exchange while the honest redirect sat in the
queue until the window closed. A bounded number of exchanges happen per round, unreached codes rotate to the
front of the next one, a new callback interrupts the backoff, and only a 404 drops a code. Making it *fatal* was still credential loss with
extra steps — the code lives in a local variable and the loopback listener dies with the flow, so
"run the command again" builds a SECOND App rather than recovering the first one's key. Nothing is
discarded on a response that never said the code was bad. A forged code is a random string, so GitHub
answers 404, and the case that actually needed handling is the one that is unambiguous.

The callback queue is deep, and a callback that does **not** fit is refused rather than dropped — a
silent drop plus an "App created" page meant the honest redirect could be discarded while its browser
was told it had worked. What remains is a local process being able to *delay* onboarding up to
`ManifestTTL` — not fixable while argv is readable. **No cap on which codes are kept can be correct, and three attempts established that.** Unbounded let
an attacker accumulate work; bounding admission discarded an honest redirect the handler had already
answered "App created"; bounding retention discarded it one ambiguous response later. billet cannot
distinguish an injected code from an honest one — only GitHub can, and only a 404 is it saying so — so
the bound is on **work per round**, and codes not reached rotate to the front of the next round.
Nothing is ever discarded. A transport failure on one code is remembered rather than returned, because
returning closes the listener with an acknowledged code unredeemed.

### The residual: argv, and why billet stops here

Everything in the paragraph above exists because of one fact: **the callback URL is passed to
`open`/`xdg-open` as a command-line argument, and argv is readable by other local processes** (via
`/proc` on Linux, `ps` generally). That is how a local process learns the unguessable path, reads the
`state` from the start page, and injects callbacks at all.

This is **documented and accepted, not fixed.** What an attacker with that access can do is bounded:

- **They cannot obtain the key.** The conversion response never passes through anything they control,
  and the code is redacted from every error.
- **They cannot destroy it.** Every path above either installs the key, preserves it somewhere named,
  or says honestly that it could not tell — and nothing is deleted that this run did not create.
- **They can delay onboarding**, up to `ManifestTTL`, by injecting codes that stay ambiguous.

The structural fix is to keep the URL out of argv — write it to a 0600 file with a meta-refresh and
open that, so the path is protected exactly as the key file is. It is not done because it trades a
real risk on the primary happy path (not every browser follows `file://` → `http://`) against a threat
that only exists on a multi-user host. **If billet ever targets shared CI hosts as a first-class case,
do that first** — it collapses this entire class rather than scheduling around it.

Four review rounds went into scheduling around this before anyone noticed it was downstream of argv.
That is the lesson worth keeping: when fixes keep producing adjacent bugs, look for the premise they
all share.

**`App` is redacted on every rendering path billet can reach.** `String`/`GoString` on a **value**
receiver (a pointer receiver is not consulted when a value is formatted), `Format` so no verb falls
back to the raw fields — `%d` printed the key before it existed — and `MarshalJSON` plus `LogValue`,
because billet standardizes on `log/slog` and its JSON handler ignores `fmt` entirely. Only marshaling
is redirected; decoding GitHub's response still populates every field. Not absolute, and the gaps are
known: an `App` reached through an unexported field of another struct, and any serializer that is
neither `fmt` nor `encoding/json` nor `slog` — reflection-based dumpers read the fields directly.

### Compute is unaccounted for until proven otherwise, and that is a state with a name

`internal/node/custody.go`. A **custody** entry is a lease whose compute billet
cannot account for and which nothing else in the process is managing. Two things
produce one and they are the same situation from different sides:

- **Adopted** — a container survived a restart. The runner inside is talking to
  GitHub on its own and may well finish the job.
- **Discarded** — a launch failed ambiguously and its cleanup was not confirmed.

Both heartbeat the lease so the reaper leaves it alone; both release only when the
compute is confirmed gone. The rules that were each learned by getting them wrong:

- **A negative observation is not a causal result.** A `docker ps` issued right
  after a lost `docker run` can overtake the daemon and see nothing. A successful
  `Destroy` proves the compute is gone; an *absence* does not, and has to persist
  through `strayGrace` before it is believed.
- **Adoption renews the lease at the moment it adopts.** Billet may have been down
  longer than a lease TTL, and the control plane reaps BEFORE its first tend — so
  leaving the renewal to the tick let the reaper terminalize the lease that had
  just been adopted.
- **"Could not verify" is not "safe to destroy."** Only `ErrLeaseNotFound` proves
  a lease is gone. Any other read error aborts recovery, having destroyed nothing.
- **Serializing a mutation is not serializing a transition.** Holding the lock for
  the flag write and releasing it before the backend calls is the same race, one
  line down.
- **A cleanup obligation is owed to the COMPUTE, not to the lease.** Only a
  successful destroy discharges a pending completion retry. Losing the lease —
  fenced, reaped — changes who owns the capacity; it does not make the container
  stop running, and GitHub will not redeliver the completion that would ask
  again. If no lease remains, skip the release and keep destroying.

  Written down because the opposite was committed for one round and the argument
  for it was seductive: the capacity really is someone else's, so the record
  looks like litter. Two separate rules were being conflated. No record is
  CREATED for a request this listener never held — a restart loses the map while
  leases live on, and those retries could accomplish nothing. That says nothing
  about a record created when the listener DID hold the lease.
- **"Could not X" usually collapses two different facts, and only one is
  evidence.** A heartbeat that returns ErrFenced is the allocator SAYING the
  lease is not ours; a heartbeat that times out is the database saying nothing at
  all. Returning one boolean for both made a listener that briefly lost its
  ledger forget the containers it had launched. The same shape recurs everywhere
  in this codebase — a failed destroy is not proof the container survived, an
  absent `docker ps` row is not proof it is gone. When a call can fail for
  reasons that mean opposite things, the return type has to be able to say so.
- **A claim and an obligation expire differently.** Losing a lease ends this
  listener's claim on the CAPACITY. It does not end its obligation to destroy the
  container it started, and the two have been conflated three separate times:
  once for pending cleanup records, once for running leases dropped by the
  heartbeat, once at shutdown. Whenever a record is removed because "it is not
  ours any more", ask what was launched under it.
- **A bound shorter than the work it bounds causes the failure it prevents.** The
  shutdown grace was 90 seconds against a node command timeout of TEN MINUTES, so
  an ordinary slow destroy tripped the watchdog, stopped renewal, and let the
  reaper reclaim capacity whose container was still being destroyed — precisely
  what the grace existed to avoid. Before choosing a timeout, find the longest
  legitimate operation underneath it and make the bound larger, or make the work
  smaller.
- **Concurrency against a serial queue can be worse than sequence.** A node runs
  commands one at a time and each command's timeout starts when it is QUEUED, so
  firing twenty destroys at once starts twenty ten-minute clocks against a queue
  that serves them in turn: the later ones expire while the node is working
  happily through the earlier ones, and healthy jobs are recorded as failures.
  Fan-out needs a bound chosen against the SERVER's concurrency, not the client's
  patience.
- **An OPTIONAL capability cannot carry a safety invariant.** The reversed change
  above was defended with "both shipped runners implement `Sweeper`, which
  destroys compute no lease is holding." True, and irrelevant: `Sweeper` is a
  type assertion on `Runner`, so that reasoning makes correctness depend on which
  implementation is plugged in, and billet is meant to be extended by strangers.
  If safety rests on a capability, the interface must require it — otherwise
  assume the implementation without it, and let the capability be a backstop
  rather than the mechanism.
- **Time warns; it does not authorise a teardown.** Held compute has NO bound by
  default (`DefaultMaxCustody = 0`) and warns hourly. Elapsed time is not evidence
  that a job stopped making progress — billet imposes no job limit and self-hosted
  runners run past GitHub's six-hour default — so killing live work must be
  authorised by a completion, an observed exit, or an operator. An operator who
  knows their longest job can set one with `node.WithMaxCustody`; a job killed by
  it is archived as FAILED, not done.

### GitHub does not requeue a job whose runner vanished mid-execution

Checked against the vendored `scaleset` README, after I asserted the opposite in a
commit message and had to retract it. Reassignment is documented for a job
*"assigned to your scale set but not acquired by a runner in time"* — GitHub
cancels and requeues that, up to 3 times. It says **nothing** about a job a runner
has already started.

So force-killing a container that is running a job is a **deliberate job failure**,
not a recovery, and "a graceful shutdown does it too" is not a defence: a graceful
shutdown is a choice the operator made. This is why restart recovery adopts.

### An instance is named for its lease, and the name is the only durable link

`provider.InstanceName` / `provider.LeaseOf`. Nothing writes "this container
belongs to that lease" anywhere, so after a crash the name is all reconciliation
has. Two consequences:

- The instance carries **billet's** name, never GitHub's runner name. They are
  different identities and conflating them made every orphan unattributable.
- Docker's `--filter name=X` is a **SUBSTRING** match — measured, not assumed:
  `billet-abc` really does return `billet-abcdef`. `Find` compares exactly
  afterwards.

### A lease must be renewed by exactly one party, and there are three handovers

Custody exists because a remote launch has three outcomes and one of them is
UNKNOWN. Every defect in this area has been a moment where the count of parties
renewing a lease was zero — never two, which is harmless, because `Heartbeat` is
idempotent and a released lease answers `ErrLeaseNotFound`.

The three moments, each of which cost a review round to find:

- **While the provider is still working.** The plane gives up after the command
  timeout and tells the listener the node has custody, so the listener stops. The
  node is inside `provider.Launch` and adopts nothing until it returns. The node
  now renews from the moment it commits to launching (`r.launching`), which is
  the first instant either side can be sure something may exist.
- **When the report arrives too late.** The plane records a TOMBSTONE for every
  launch it abandons — on timeout and on re-registration — so a late success is
  answered with "this lease is yours" instead of 204. Without the tombstone the
  node files the instance in its ordinary running set, which nothing renews.
- **When the result races the timeout.** Both select branches are live at once.
  `settle` drains the result channel while holding the plane mutex, which is what
  makes the answer exact: `Result` sends under the same lock, so the send has
  either completed or not started.

### A registration proves who you are; only a command proves what you may do

The JIT endpoint required a registered node and nothing else, which made the
README's containment claim false: a host holding a node certificate could mint
runner registrations in a loop, for any scale set, under any name, and start
runners billet never escrowed capacity for and never tears down.

The entitlement was already in the request and unused. Billet's runner names
carry the lease id (`provider.InstanceName`), so a node may mint exactly the
registration for a launch command it currently holds. Apply the same shape to
anything else the node can ask for: **authentication answers WHICH host, and the
command table answers WHAT it was given.**

### An empty CA directory is ambiguous, so something has to remember

"No files" reads as day one, and day one mints a new authority that every issued
bundle fails to verify against — the whole fleet drops off at once while the
control plane looks healthy. A marker file written at creation is what makes a
later absence mean *loss*; deleting it is how an operator starts over on purpose.

Two more rules the same subsystem needs, both of which load cleanly when broken:
a CA's subject must name THIS deployment (verifying against the CA is what
decides who may connect, so somebody else's silently re-points that decision),
and its key must be its certificate's key (unrelated halves sign leaves that fail
days later on a node, in an error naming neither file). And a private key is
refused unless the file itself is safe: creation's 0600 says nothing about what a
backup restored.

### A node's identity is the name in its certificate, and its deployment is too

The wire used to take both from the request — the node named itself in the path,
and named its deployment in the registration body. Neither was verified, which is
why it refused to serve anywhere but loopback.

Now `internal/wirecert` mints one CA per deployment, held by the control plane,
and `billet ca issue <node>` produces the bundle an operator copies to a host.
Two rules follow, and both exist to keep ONE authority for one fact:

- **The certificate's common name decides which node a request is from.** A path
  that disagrees is refused, never reconciled. The check runs after routing (the
  path variable does not exist until the mux has matched) and is applied in the
  routing table itself, so a route added without it is visibly missing something.
- **The certificate's organization decides which deployment the node belongs
  to.** A node's state directory MINTS a random identity when it has none, which
  is right for a control plane — where an installation begins — and wrong for a
  node, which joins one. Before this, a freshly enrolled node invented an
  identity and the control plane refused it forever; nothing an operator could
  copy would have fixed it. `state.AdoptDeploymentID` writes the certificate's
  answer down, and REFUSES rather than overwrites when the directory already
  holds a different one, because the compute that directory is already managing
  carries the old label.

The server's own certificate is minted per boot and never stored: nothing
verifies it except this CA, so persisting it would only add a file that expires,
and its expiry would take the whole fleet offline at an hour nobody chose. The CA
is the one thing that persists, and a CA directory holding only ONE of its two
files is refused rather than repaired — minting a replacement is a new authority,
and every node certificate ever issued stops verifying at once.

### Destruction is scoped by DEPLOYMENT identity, never by node name

`state.DeploymentID`: random, minted once per state directory, `O_EXCL`, and the
directory is fsynced as well as the file. The node name defaults to the hostname,
so two billets on one machine share it while keeping separate state directories —
and the process lock does not catch that, because it guards a *directory*. Labelling
compute by node name let one installation enumerate the other's containers and act
on live jobs it had no relationship with.

**A copied state directory deliberately keeps the original's identity** (the copy's
containers are labelled with it), and the directory lock does NOT make that safe —
a copy is a different inode, so both directories lock happily. That is what
`state.LockDeployment` is for: a SECOND lock keyed by the IDENTITY, so the copy
collides and refuses to start.

Three things about it were wrong on the first attempt and are worth not repeating:

- **Never put a lock file in a cache directory.** It was there first, chosen over
  `/tmp` because `/tmp` is world-writable and a local user could hold the file to
  keep billet from booting. True, and still the wrong place: a cache directory's
  contract is that its contents may be deleted at any time. Unlinking a held lock
  file does not release the flock, but it detaches the PATH from the locked inode,
  so the next process creates a new file there, locks that, and both run. **An
  inode check does not fix this** — the newcomer's check passes because it created
  the file it just locked. The location is the fix; it now lives in the state
  directory (`$XDG_STATE_HOME`, or Application Support on darwin).
- **Failing to place the lock is an ERROR, not a downgrade.** It used to degrade
  on the reasoning that a host with nowhere to put a lock is more often one
  deployment than two. That derives AUTHORIZATION FROM AN I/O FAILURE: a symlink
  loop, a permissions change, ENOLCK, fd exhaustion, or a service manager with no
  `HOME` all land there and look identical to the benign case. `server.
  allow_unlocked_deployment` lets an operator opt in explicitly.
- **The default location is per-user, which the lock cannot fix by itself.** A
  system service and an operator sharing `/var/run/docker.sock`, or two containers
  sharing a socket with private filesystems, get different directories and never
  collide while their containers do. `server.lock_dir` puts them in one collision
  domain, and the resolved path is logged every boot so which domain a process
  joined is evidence rather than inference.

- **A shared directory must be SETGID, and mode bits alone never prove sharing
  works.** `0660` says *a* group may open the file, not *which*. A service account
  whose primary group is `service` and whose supplemental group is `billet`
  creates the lock owned by `service` in a non-setgid directory — every permission
  bit a check could ask for, and still unopenable by the operator it was widened
  for. Group-writable proves sharing was *intended*; setgid decides *who gets it*.
  So the directory's gid is captured and the lock file's gid must match. The umask
  is the same trap one level down: it turns a requested `0660` into `0640`, which
  cannot be opened `O_RDWR`, so the mode is corrected explicitly and verified by
  result rather than by intent.
- **`os.Root` confines a path; it does NOT refuse a symlink.** It follows links
  that stay inside the root, and its Unix implementation applies its own
  `O_NOFOLLOW` internally, inspects the link on `ELOOP`, then follows — so a
  caller's `syscall.O_NOFOLLOW` is indistinguishable from its own and is ignored.
  Measured, not read: a relative `link.lock -> real.lock` inside the directory
  opened as `os.SameFile` with its target. Use `unix.Openat` against a real
  directory descriptor, which honours the flag because the kernel does. Opening
  the directory itself `O_DIRECTORY|O_NOFOLLOW` then removes the separate
  `os.Lstat` too — one resolution, and no second one that could describe a
  different directory than the handle holds.
- **Take the flock BEFORE judging metadata.** Validating first meant a group
  mismatch told the operator to delete a "stale" lock file that nobody had checked
  was unheld — and after the delete the newcomer creates a fresh inode while the
  holder keeps the old one, so neither excludes the other. Nothing may be called
  stale until the lock is held.
- **A gid sentinel of `-1` is not safe.** `Stat_t.Gid` is unsigned, so on a 32-bit
  host a gid above `MaxInt32` converts to a negative `int` and becomes
  indistinguishable from "no group owner". Absence gets its own field.

**Claim the identity BEFORE `state.Open`.** It ran after, and `state.Open` applies
migrations — so a process about to be refused first migrated the database it was
refused the right to use (start an old copied backup beside a live original and
the backup is silently upgraded on its way to the error).

**A contention test that runs in one process is not a contention test.** Both of
the original ones called `LockDeployment` twice in the same process; a
package-level mutex or a PID in the filename satisfies that while two billets
start against one daemon. Measured, not assumed — the in-process test really does
pass a fake process-local mutex. The real one re-executes the test binary
(`deploymentlock_process_test.go`), which is also the only way to assert that
SIGKILLing the holder frees the identity.

### The test written to prove a fix tends to prove the adjacent thing

Four consecutive review rounds on the deployment lock found tests that passed
while the check they named did nothing. They are worth listing together, because
the shape is the same every time and it is not carelessness — each test was
**about** the right subject and **exercised** something else:

| Meant to prove | Actually proved |
|---|---|
| the lock file's gid matches the directory's | `20 == 20` — a `t.TempDir` is owned by the primary group |
| ...then, with a borrowed group | the *kernel* supplied the gid — setgid was set before the file was created, so the comparison was never reached |
| `O_NOFOLLOW` refuses a symlinked lock file | `os.Root` refuses an *escape* — the target was an absolute path outside the directory |
| a second *process* is refused | flock works within one process; a package-local mutex passes identically |

**The check that catches this is mutation, not review.** Delete or neuter the
production line the test names and re-run only that test: if it stays green, the
test is about something else. Every one of the four above was found that way, three
of them after a human-style reading had already called them correct. A test whose
mutation survives is not necessarily wrong, but it must be shown to be redundant
rather than assumed to be — and the redundancy said out loud.

A related habit that paid off repeatedly: when a platform behaviour matters
(`os.Root` and symlinks, umask stripping mode bits, BSD versus Linux gid
inheritance, `os.UserCacheDir`'s environment dependence), **write a throwaway probe
that prints what actually happens** instead of reasoning from the documentation.
Three of the defects above were documented behaviour that read the other way.

**A mutation harness must prove it CHANGED the file.** Verify by hash, not by
grepping for a string that may already be absent — a substitution that matches
nothing reports SURVIVED, which is indistinguishable from a vacuous test and sends
you to fix a test that was fine. This produced three false verdicts in one
session, one of them because a route is registered as `root+"/installed"` rather
than the literal the pattern looked for.

**A clean `-race` run is evidence, not proof.** It only sees the interleavings
that actually occurred. A concurrent slice read in the onboarding fake survived
six clean `-race` runs because the racing append only happens while a request from
the *previous* visit is still in flight. If two goroutines can reach the same
field, fix it because they can, not because the detector complained.

**A catch-all route means a deleted route still answers 200.** The onboarding
loopback mux registers `root+"/"`, so removing `/installed` sends the callback to
the start page rather than to a 404 — every status-based assertion reads that as
success. Where a route matters, assert something only its handler produces.

**A fallback path can make the thing it backs up untestable.** The same fake
marked the installation visible as soon as the install page opened, so the
authenticated poller completed the flow whether or not a callback was ever issued
— deleting the callback, its route, or its address all left the tests green. When
a fast path and a fallback both lead to success, the test has to withhold the
fallback or it is only ever testing the fallback.

### `created` is not `running`

`docker ps` reports created/running/paused/restarting/removing/exited/dead. A
container that exists but was never started never will be — whatever would have
started it is gone — so adopting it holds a lease open forever for a job that
cannot begin. It is the one state that looks alive and is not.

An **unrecognised** state still counts as running. The asymmetry is deliberate: the
caller destroys what is not running, and a state billet has never heard of is not
evidence that a job is over.

### Never guess at a byte size

`config.ByteSize` parses with exact integer arithmetic on a restricted grammar. It used
`strconv.ParseFloat`, which accepts `NaN`, `Inf`, hex and exponents, and loses precision above 2^53 —
and converting any of those to `int64` is implementation-defined and can come out **negative**, which
silently disables the capacity ceiling. Reject what cannot be represented exactly.

---

## Linting

`.golangci.yml` is the source of truth for Go conventions, and the *why* for each non-stdlib rule is
a comment next to that rule. The version is **pinned to v2.12.2** and CI uses the same one:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
```

`nolintlint` rejects a bare `//nolint`. An exception is written `//nolint:linter-name // reason`,
and the reason is mandatory — so "0 issues" means "0 unexplained issues".

**When you fix a bug, ask whether it should be a lint rule.** If the bug is a *class* something could
reintroduce, and it is mechanically checkable, encode it:

1. **Can golangci-lint express it?** A `forbidigo` identifier ban, a `depguard` layering rule, a
   linter setting. Cheapest — do this first.
2. **Project-specific but deterministic?** A `go/analysis` analyzer, wired into CI only if it
   measures ~zero current violations. A noisy analyzer erodes trust and gets ignored.
3. **Needs judgment?** Document it in the Invariants section above.

Rules already carrying real weight here:

- `depguard` bans `mattn/go-sqlite3` (cgo breaks the single-static-binary goal and the cross-build
  matrix) and confines `modernc.org/sqlite` to `internal/state`, so nothing else can open a
  connection without the durability pragmas.
- `forbidigo` bans `time.After` (leaks its timer until it fires), `panic` (a control plane that
  panics drops every in-flight lease), and `context.Background` outside `cmd/billet`.

---

## Testing

`make test` runs `-race`; `make cover` writes and opens an HTML profile.

- **A test must fail when the code is wrong.** Two of the original state tests did not: one read
  pragmas through the *reader* pool, where `journal_mode` reports `wal` from persistent file state
  regardless of the writer's DSN; the other exercised only the single-connection writer, which makes
  serialization tautological. If a test asserts an invariant, break the invariant once and confirm
  the test fails.
- **Assert the diagnostic, not the shape.** Counting error lines passes for the wrong reasons;
  asserting the specific messages does not.
- Tests use `t.Context()` and `t.TempDir()` (enforced by `usetesting`).
- `paralleltest` is deliberately off: these tests open real SQLite files, and `t.Parallel()`
  everywhere buys nothing and invites flakes.

### Tests that could not have failed

Every one of these passed against the exact bug it existed to catch. Mutation
testing is what found them, and it is not optional for anything load-bearing.

- **A fake that ignores `context`** cannot distinguish "cleanup ran on a cancelled
  context" from "cleanup ran on a fresh one" — which was the entire subject of the
  test. Fakes honour `ctx.Err()`.
- **Cancelling before the call** made `Bind` fail first, so the launch path was
  never reached and the test passed because *nothing happened*. The fake cancels
  from inside `Launch` now, as a real timeout does, and the test asserts the
  provider was actually called.
- **A race whose window is nanoseconds** is not tested by two goroutines and a
  `WaitGroup`: the mutation survived five runs under `-race`. The fake can delay
  inside `Find`, which holds both callers inside the transition long enough to
  genuinely overlap.
- **Counting containers instead of RUNNING containers** let the headline
  end-to-end test pass while the "runner" exited instantly — proving container
  creation and removal, not that a job ran.
- **A fake that pops a message on read** cannot model redelivery, so a test
  "asserting" acknowledgement passed against a billet that acked before doing any
  work. The queue holds the head until its exact id is acked.
- **A mutation that does not compile is caught by the compiler, not by a test.**
  Three of them looked like passes until the failure count read zero. Keep every
  mutation compiling, and print the failing-test count so a zero is visible.
- **A mutation that never APPLIED looks exactly like a surviving one.** A `perl`
  substitution with three tabs of indentation against a line that has two matches
  nothing, reports "SURVIVED", and sends you off to write a test for behaviour
  that is already covered. Assert the substitution changed the file — `grep -c`
  the original text and expect zero — before believing the result.

- **A test satisfied by "an error" cannot tell a refusal from a panic.** Deleting
  the guard that checks a request carries a client certificate makes the code
  after it dereference a nil `r.TLS` — the handler panics, the client sees an
  error, and a test asserting `err != nil` passes. The mutation survived because
  the assertion was too weak, not because the guard was covered. Assert the
  SPECIFIC refusal: a sentinel error, or a status code.
- **Every test dialling an `httptest` server shares an assumption no production
  caller makes.** Its `URL` carries a scheme. `billet node` handed the client a
  bare `host:port` from config and could not construct a single request, and the
  whole suite was green — the one code path that builds a base URL from
  configuration had no test at all.

- **A wait that something else already satisfied is not a wait.** A test meant to
  let the janitor renew once before changing the TTL waited for `heartbeats > 0`
  — and `Recover` had already heartbeated while adopting, so it returned
  instantly and the race it was added to remove was still there. Count from a
  baseline taken before the thing under test exists.
- **A fake that cannot be slow cannot model the bug.** The window where nobody
  renews a lease only exists while a provider is working, so the fake provider
  needs to block inside `Launch` and say when it has — a delay plus a channel,
  never a sleep in the test.

- **A test that manufactures the concurrency it is checking proves only the
  narrow half.** The starvation test started `retryCleanup` in one goroutine and
  `heartbeatHeld` in another, then asserted the second could run while the first
  was stuck. That proves a stuck destroy does not hold `l.mu` — and nothing else.
  The property it was NAMED for is that the two run on separate clocks, and
  moving cleanup back onto the heartbeat's tick passed it unchanged. When the
  property is about scheduling, the test has to use the scheduler under test:
  drive `Run`, and assert the CONSEQUENCE (a lease the reaper took) rather than
  the mechanism.
- **A shutdown-time worker must not run on the caller's context.** Renewal was
  started on a child of `ctx`, so the caller cancelling to shut down stopped it
  at that instant — before the session close, before the release, before every
  slow remote destroy the release performs. Stopping it "last" in the deferred
  teardown was decoration: it had already stopped. Anything that must stay alive
  DURING shutdown gets `context.WithCancel(context.WithoutCancel(ctx))` and is
  stopped explicitly by the function that owns the teardown.

  The general form: if a goroutine's job is to protect the teardown, inheriting
  the cancellation that triggers the teardown is exactly backwards.

  The discriminator, having audited the other two sites — `Server.Run`'s sweeper
  `KeepAlive` and `nodeclient`'s janitor — is whether the function does
  meaningful work AFTER its context is cancelled. Both of those simply return, so
  a child context is right there and nothing needs changing: on process exit
  their leases are reaped and restart recovery re-adopts. Only the listener keeps
  working after cancellation, because its teardown destroys compute and releases
  capacity, and that work is what has to be protected.
- **Cancelling a goroutine is not stopping it.** A cleanup retry blocked in a
  remote `Destroy` outlived `Run` and came back afterwards to release against a
  database the caller was entitled to have closed. Cancel AND join, and be
  explicit about the order when two workers must stop at different times —
  cleanup before the release that would race it, renewal after.
- **A test whose observation can also be produced by shutdown proves nothing
  about the loop.** The first version of the cleanup-loop wiring test let the
  context expire and then asked whether a destroy had happened. `releaseAll`
  destroys everything still running, so it produced one — the test passed with
  the loop deleted, and the mutation run reported a kill only because `Run`
  incidentally returned `DeadlineExceeded`. Observe the effect while the system
  is still running, and enumerate every other path that could produce it.
- **A mutant that applies but changes no behaviour reports SURVIVED, and that is
  indistinguishable from a real gap.** Inserting `_ = id` next to a `delete` left
  the delete in place; the harness verified the file hash changed, so every
  existing guard passed, and the output said the property was uncovered when it
  was not. Hash-verification catches an edit that did not apply, not an edit that
  did nothing. A mutant must remove or invert behaviour — if you cannot say which
  assertion it should break, it is not a mutant.
- **Run the suite the way CI runs it, instrumented.** `-covermode=atomic` is not
  a reporting flag; the counters change timing enough to reorder goroutines that
  a plain `-race` build schedules identically every time. A launch in progress
  being handed to teardown was invisible under `make check` and reliable under
  coverage. `make check` now carries the flags, because a local gate weaker than
  CI trains you to trust it.

### Four ways silence has looked like success, and the guards for each

Every one of these produced a green gate and an untrue conclusion. They are the
same failure wearing different clothes, and the pattern is worth recognising
before the fifth one: **the thing that would have objected was itself missing.**

| What went missing | What it looked like | Guard |
|---|---|---|
| A scripted substitution matched nothing | Build and tests pass, bug untouched | `assert old in s` before replacing, and verify the file hash changed |
| A mutant applied but changed no behaviour | `SURVIVED` — identical to a real coverage gap | A mutant must remove or invert behaviour; if you cannot name the assertion it should break, it is not a mutant |
| A review prompt file did not exist | `codex exec` exit 0, no findings — identical to a clean round | `run_round.sh` refuses to launch without a non-empty prompt |
| A scripted edit deleted a whole test | Suite green; a deleted test cannot fail | `make tests-kept` — compares Test function names against HEAD |
| A killed mutation run left its mutant in the file | Compiles, mostly passes; an earlier green gate says nothing because the mutation landed after it | `make no-mutants` — runs FIRST in `check`; a stranded `.bak` is the only evidence |

The last one was found only because a mutation run happened to name that test and
reported `NO SUCH TEST`. Nothing else in the toolchain noticed, and nothing else
would have.

### An edit that did not apply looks exactly like an edit that did

Twice in one session a scripted `replace()` matched nothing and reported success:
once because the anchor said HANDOVER where the file said HANDOFF, once because a
comment had been reworded a round earlier. The build passed, the tests passed,
and the bug the edit was meant to fix was untouched — the only reason it surfaced
was a test written against the behaviour rather than the code.

**Assert every substitution.** `assert old in s` before replacing, and check the
file hash changed afterwards. This is the same rule already written down for
mutation testing, and it applies to every scripted edit for the same reason: the
failure mode is silence.

**And use `-F` for every commit message.** Backticks in `git commit -m` are
command substitution: three messages this session lost a phrase to it, in a
project whose commit messages are the design record. A file cannot misfire.

### Two things Go gets right and reviewers get wrong

- **`url.Parse` accepts `"127.0.0.1:7717"`**, reading the host as a scheme. A
  validation that only calls it therefore cannot fail on the input it exists to
  reject. Check the parts you actually need — a scheme you recognise, a non-empty
  `Host`.
- **Deferred calls run last-in, first-out**, so

  ```go
  defer stopJanitor()   // runs SECOND
  defer janitor.Wait()  // runs FIRST — waits for a goroutine nothing has stopped
  ```

  deadlocks on every exit path the parent context did not cause. Two defers whose
  order matters belong in one defer, in the order written.

### The end-to-end suite

`internal/e2e` runs the real control plane and a real container runtime against
`internal/fakeactions`, a scripted stand-in for GitHub. It exists because every
other suite tests one seam, and this project's worst defect — acquiring jobs by the
wrong id — lived in the relationship between billet and the wire, where billet's
own types agreed with billet's own mistake.

Two things it must keep doing:

- **Assemble billet the way `cmd/billet` does**, through `internal/wiring`. The
  hand-copied adapter it started with had already drifted — it dereferenced a
  scale set the client returns as nil — and a test that assembles a different
  program is testing a different program.
- **Use an image that stays running.** busybox exits immediately, so recovery
  correctly saw a finished job every time and adoption was untestable.

## Coverage

Codecov is wired in CI. Current: **`internal/config` ~80%, `internal/state` ~79%**.

The project target is **`auto`** — do not regress from the base commit — not an absolute number, and
that is deliberate. A hard 85% on a project sitting at 79% fails every PR from day one, and a check
that always fails is a check everyone learns to ignore. New code carries an 80% patch target, so the
overall figure ratchets up as the project grows rather than being declared true in advance.

Coverage is a signal, not a goal: a test that exercises a line without asserting anything raises the
number and catches nothing. See the `billet-testing` skill for the mutation-test discipline that
separates the two.

## Commands

```bash
make check     # build + vet + fmt-check + lint + race test — the pre-commit gate
make build     # ./bin/billet
make test      # go test -race -count=1 ./...
make cover     # coverage profile + HTML report
make lint      # golangci-lint run
make lint-fix  # auto-fix what can be auto-fixed
make fmt       # gofmt -s -w .
make tidy      # go mod tidy
make cross     # build every target a node can run on
make dist      # build the release artifacts locally, exactly as a tag would
```

## Releases

Semantic version tags, because Go gives no choice: a module version must be
`vX.Y.Z`, so a date-stamped tag is not a version Go will ever resolve. Staying on
`v0` also keeps the module path free of a `/vN` suffix, which is required from
major version 2 onwards.

One branch per MINOR version, carrying every patch on it:

```
main ──●──●──●──●──●──●──▶
        \              \
         \              release/v0.4 ── v0.4.0
          release/v0.3 ─┬─ v0.3.0   (cut)
                        ├─ v0.3.1   (hotfix)
                        └─ v0.3.2   (hotfix)
```

Cut with the **Cut Release** workflow (Actions → Run workflow). Blank version
cuts the next minor; supply one to bump deliberately. A hotfix is a commit on the
existing `release/vX.Y` branch and a press of the same button with the patch
version typed in — then **merge that branch back into main**, or the next release
reverts the fix.

`cut-release.yml` creates the tag and CALLS `release.yml` rather than relying on
the tag push to trigger it. A ref pushed with `GITHUB_TOKEN` does not start
another workflow — GitHub's recursion guard — which is why a release button
usually needs a PAT or an App. A `workflow_call` is not an event, so billet needs
no repository secrets.

## Deployment

`deploy/` holds the systemd units and the packaged config; GoReleaser's `nfpms`
section turns them into `.deb`, `.rpm` and `.apk`. Three things there are
load-bearing and were each found by installing a package rather than by reading
one:

- **`--config /etc/billet/billet.yaml` is explicit in both units.** billet's
  default config path is per-user and deliberately never reads the working
  directory; a unit that relied on the default would find nothing.
- **`lock_dir` is set in the packaged config.** billet derives its default from
  `HOME`, which a service does not have, so without this billet refuses to start
  rather than run without the lock that stops two processes managing one host's
  containers.
- **`TimeoutStopSec` is sized from `drain_timeout` plus the teardown.** systemd's
  expiry is a SIGKILL through the middle of the shutdown. Lower `drain_timeout`
  first, then the unit; never the unit alone.

The package **does not enable or start anything**. `/var/lib/billet` is created
by postinstall rather than shipped, so a package removal cannot delete the
deployment identity and the mTLS CA.

## Git workflow

Feature branches, never commit to `main`. See the `billet-git-flow` skill for branch naming, commit
format, and PR conventions.

## Platform support

Linux (amd64, arm64) and macOS (arm64). The `flock`-based state lock is `//go:build unix`; a Windows
port needs an equivalent, and `make cross` is what catches a build-tag mistake before it reaches a
node.
