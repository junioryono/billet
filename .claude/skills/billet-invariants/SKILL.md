---
name: billet-invariants
description: "Cross-cutting invariants for billet that are not obvious from the code — the single authoritative writer, append-only migrations, cache TLS interception defaults, how Apple's macOS guest limit is enforced, pinning rules about third-party APIs to measured behaviour, and byte-size handling. Use when adding a migration, changing the state store, touching tier or node policy validation, or writing a rule about an API billet does not own."
---

# Invariants

Rules that hold across billet, each of which cost something to learn.

### The control plane has exactly ONE authoritative writer

Corrupting the capacity ledger means double-admitting jobs onto a machine that cannot hold them, and the failure is quiet — jobs are accepted, then the host OOMs or thrashes.

- **An exclusive `flock` on the state directory is held for the DB's lifetime.** SQLite's own single-writer rule prevents simultaneous *writes*; it does not prevent two billet processes both long-polling GitHub and taking turns writing conflicting decisions. The second process must not start at all. `flock` rather than a PID file, so a crash releases it automatically.
- **Writes go through `DB.Tx` on a one-connection pool.** An allocation decision is read-current, decide, record — one transaction, not a read followed by a hopeful write.
- **`DB.Reader()` returns a narrow `Querier`, never `*sql.DB`.** Handing out the pool would let any caller write through a connection that is supposed to be read-only.
- **The database MUST be on local storage.** SQLite's WAL is explicitly unsafe on a network filesystem. `Open` reads the pragmas back and fails closed if `journal_mode` is not WAL, which is what catches an NFS state directory.

### Migrations are append-only and identified by version + checksum

Never edit or reorder an existing migration; add a new one. `migrate` verifies a recorded checksum against the binary's SQL and refuses on mismatch, and refuses a database carrying a version this binary has never heard of. Counting applied rows is not a version — a deleted row reruns a migration, a forged row skips one, and inserting one in the middle silently reruns the tail.

### Cache TLS interception defaults OFF, per tier

`ACTIONS_RESULTS_URL` carries **artifact** metadata as well as cache traffic, so anything in that path is in the user's release path. `Tier.Intercept` is opt-in, and tiers that publish release artifacts or hold deployment secrets must not enable it. A mistake here does not slow CI down; it breaks a deploy.

The protocol is reverse-engineered — GitHub has never published the `.proto` files — so the cache must **fail open to a miss** on any error, never fail a job, and a conformance suite must run the real `actions/cache`, `upload-artifact` and `download-artifact` against live GitHub to catch drift. Both are requirements on the cache when it is built (P4); neither exists yet, and nor does the cache.

### The macOS guest limit is enforced against `guest_os`, never a label

Keying the limit off a label matching `macos` means a tier named `sonoma-arm64` escapes it entirely, and a Linux tier named `builds-macos-artifacts` gets capped for no reason. `Tier.GuestOS` is the explicit field, macOS tiers must pin a `node`, and per-node totals are summed at load. Warm instances count.

The config check is a **guard, not the enforcement point**: the allocator holds a single host-wide count of running plus warm macOS guests at runtime, because two individually-valid tiers still share one physical Mac. Both read the effective limit from the same `NodePolicy`, so there is one number rather than two that drift.

**The limit is per host and configurable; `DefaultMacOSVMLimit` is a default, not a ceiling.** Apple's standard licence permits two macOS guests per Apple-branded host, which is what a config that says nothing gets. But what a host may run is a deployment decision, not a fact about the hardware — an Apple Silicon machine can serve macOS guests, Linux arm64 guests, or both — so `nodes:` carries a per-host `guest_os` allowlist and `macos_vm_limit`. Raising the limit is permitted because billet cannot know what licence or hardware agreement an operator has; it is an assertion about their licence, which is why the diagnostic names Apple only when the limit came from the default.

Two rules keep that from becoming a footgun. A tier pinned to a host that does not permit its guest OS is a load-time error rather than a job that queues forever with nothing saying why. And `macos_vm_limit > 0` together with a `guest_os` allowlist excluding macOS is rejected instead of silently resolving — a config that reads as "two macOS guests" must not schedule none.

**Placement is checked where the host is known, and again at the launch boundary.** Config validation cannot see every placement: an *unpinned* tier names no host, so nothing ties it to the allowlist, and a scheduler that simply picked a node with free capacity would put a Linux guest on a macOS-only Mac. `nodeplane.pick` is where a host is first chosen and it checks the tier's allowlist there; `Bind` is where the allocator durably validates that choice, and it is the one that has to hold, because a node cannot route around it. The load-time guard covers what it can prove — a pinned tier, or an unpinned one against a host declaring the *same provider*, since a Firecracker tier can never land on a Tart host and comparing guest OS alone would make one macOS-only Mac an error for every x64 Linux tier in the deployment.

`Bind` alone is not enough, for two reasons that each looked fine in isolation:

- **Nothing required it.** `assigned → launching` succeeded on an unbound lease, so a caller could pick a host outside the allocator and every check inside `Bind` would never run. Every phase that presumes a running host — `launching`, `online`, `busy` — now requires a bound node. Gating only the `launching` edge is not sufficient either: a row left in `launching` by an older binary would walk on to `online` untouched.
- **Binding is not launching.** A lease can be bound while still in `capacity`, so a policy tightened in between would let the instance start on a host that no longer permits it. Placement is re-checked on entry to those phases against policy in force *then*, making the guarantee "legal now" rather than "was legal once". Only a repeated `Bind` is grandfathered, because it changes nothing.

**A lease whose placement facts are unverifiable fails closed.** A row predating the `provider` column records `""`, and tolerating that would be a bypass rather than a compatibility courtesy — such a lease may still be *unbound*, so it is not old work already placed but unplaced work whose backend nothing can check. `Reap` and `Release` deliberately do not consult these checks, so a lease refused this way can always be cleaned up; failing closed on something unrecoverable would just strand capacity.

That rule is also what protects rows that no migration can classify. `macos_slot` only became truthful at migration 5, which added it defaulting to `0` without a backfill, so a macOS lease older than that is indistinguishable from a Linux one. Migration 7 repairs what it can and the rest are refused rather than guessed at.

The lease's `guest_os` is recorded at reserve time for the same reason as `target_node` and `macos_slot`: a tier redefined underneath an in-flight lease must not reclassify what that lease is allowed to bind to.

**`NodePolicy` is deep-copied when the allocator is built.** `GuestOS` is a slice and `MacOSVMLimit` is a pointer, so copying the map alone still shares both — letting a caller widen an allowlist or raise a cap after construction, moving a licence limit out from under leases already counted against it. `NodePolicy.Clone` owns that, so there is one place to get it right.

### A rule about someone else's API is pinned to measured behaviour, not to reasoning

The runner-group validator began as an allowlist of "URL-safe" characters and was wrong in both directions: it rejected `team=platform`, `who?`, and every non-ASCII name — `Grupo-Ñ`, `研发` — while missing `;` entirely. The client interpolates the name unescaped into a path, then `url.Parse`s it, reads `Query()`, and re-`Encode`s it, so the only question that matters is whether a character survives that round trip. Running it settled it in a minute: `&` `#` `;` `%` `+` do not, everything else does. `;` is the one no amount of reasoning would have produced (Go's `ParseQuery` has rejected it as a separator since 1.17).

The test asserts the **property**, not the list: every name the validator accepts is put through the client's exact transformation and must come out unchanged. When a rule encodes an assumption about code you do not own, pin it to what that code does — a probe costs a minute, and a plausible-sounding character list is exactly the kind of thing that is confidently wrong.

### `created` is not `running`

`docker ps` reports created/running/paused/restarting/removing/exited/dead. A container that exists but was never started never will be — whatever would have started it is gone — so adopting it holds a lease open forever for a job that cannot begin. It is the one state that looks alive and is not.

An **unrecognised** state still counts as running. The asymmetry is deliberate: the caller destroys what is not running, and a state billet has never heard of is not evidence that a job is over.

### Never guess at a byte size

`config.ByteSize` parses with exact integer arithmetic on a restricted grammar. It used `strconv.ParseFloat`, which accepts `NaN`, `Inf`, hex and exponents, and loses precision above 2^53 — and converting any of those to `int64` is implementation-defined and can come out **negative**, which silently disables the capacity ceiling. Reject what cannot be represented exactly.

---
