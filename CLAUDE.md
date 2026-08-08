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
- **Held compute is bounded** (`MaxCustody`, 24h) and warns hourly before that.
  The warning is the mechanism; the bound is a backstop.

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

### Destruction is scoped by DEPLOYMENT identity, never by node name

`state.DeploymentID`: random, minted once per state directory, `O_EXCL`, and the
directory is fsynced as well as the file. The node name defaults to the hostname,
so two billets on one machine share it while keeping separate state directories —
and the process lock does not catch that, because it guards a *directory*. Labelling
compute by node name let one installation enumerate the other's containers and act
on live jobs it had no relationship with.

**A copied state directory deliberately keeps the original's identity** (the copy's
containers are labelled with it). The lock does NOT make that safe — a copy is a
different inode — so two copies can run at once. Closing that needs a host-wide
lock keyed by the identity; see the task list.

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
```

## Git workflow

Feature branches, never commit to `main`. See the `billet-git-flow` skill for branch naming, commit
format, and PR conventions.

## Platform support

Linux (amd64, arm64) and macOS (arm64). The `flock`-based state lock is `//go:build unix`; a Windows
port needs an equivalent, and `make cross` is what catches a build-tag mistake before it reaches a
node.
