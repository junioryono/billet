---
name: billet-security
description: "billet's credential and identity rules — the GitHub App private key, JIT runner registration tokens, the node-wire CA, node identity, and why destruction is scoped by deployment identity rather than node name. Use when touching internal/wirecert, internal/github, cmd/billet/githubapp.go, anything that reads or writes a key or token, the mTLS node wire, or any code path that destroys compute."
---

# Credentials, identity, and destruction scope

billet holds an App private key that can mint tokens for an entire GitHub organization. These rules exist because a quiet mistake here is expensive and invisible.

### A credential GitHub issued once is never deleted, and never rendered

GitHub returns the App private key **exactly once**, from the manifest conversion. There is no re-issue. Every rule here exists because a review found a way to lose or leak it, and several were introduced by the fix for the previous one.

**The reservation never occupies the destination.** This is the shape everything else rests on, and it took four rounds to reach. While the reservation *was* the key path, installing meant unlinking that path first — and a pathname unlink cannot be made safe by any check preceding it, because the check is never atomic with it. Every guard tried (`os.SameFile`, then "and it is still empty") still had an ordering where another run's key was deleted on the way to installing this one.

Reserving a sibling file removes the unlink entirely, and collapses two files into one: the reservation *is* the staging file. The destination is created exactly once, by an `os.Link` that **fails rather than replaces**. There is no rename fallback — `os.Rename` has no no-clobber form in Go, so on a filesystem that cannot hard-link billet reports the staged key and the operator moves it by hand.

**Nothing is deleted by pathname unless it is known not to be a key.**

- The **reservation cleanup is gone.** An aborted run leaves its staging file, and `reserveKeyFile` prints the exact `rm` after inspecting whether it is a leftover or a credential.
- The **staging file is removed only after a successful install** — `os.Link` leaves two names for one private key, so that removal is mandatory — and only after `os.SameFile` confirms the name still refers to this run's file, with the directory synced afterwards so a crash cannot resurrect the entry. A failure to remove it is reported, never swallowed: an unmentioned second copy of an App key is what nobody finds until it matters. **The `SameFile` check narrows this race; it does not close it.** Go unlinks by name, the check cannot be atomic with it, and a file swapped in between the two would be deleted. That residual is accepted and stated rather than claimed away.
- **"Could not tell" is never "no key here."** `inspectKey` returns present / absent / unverifiable, and only *absent* permits a deletion or a "your credential is gone" message. A stat that FAILS is not a mismatch either, so identity answers matches / differs / unknown and path lookups answer present / absent / unknown. **Three-valued types get collapsed back at the call site if you let them** — a `!= identityMatches` undid one of these a line after it was introduced, and a `fileExists` that returned false on EACCES made billet recommend `mv` onto an occupied destination. Callers use `inspectKey` directly — the boolean wrappers over both were deleted, because every one of them collapsed the third state at exactly the call site that needed it. Note that unlink permission comes from the DIRECTORY, so an operator can act on a bad `rm` suggestion for a file billet could not itself read.
- **The staging name is re-inspected after `O_EXCL` fails.** The answer from before the attempt is stale by then: a concurrent run's empty reservation can have become a complete key in between, and printing "it holds no usable key" beside an exact `rm` handed the operator a command that destroys it.
- **"Not a valid key" is not "safe to clobber."** Whether to recommend `mv` asks `lookupPath`, not `inspectKey` — a PEM with trailing junk, a format this build cannot parse, or a file a live writer has not finished are all worth keeping, and `mv` replaces. Every `mv` suggestion in this file checks the destination first, because the operator is following *billet's* advice.

**A pathname is only spoken about once it is tied to a descriptor.** `inspectKey(name)` answers "is there a usable key at that name", which is NOT "this run's key is there" — conflating them let a replaced recovery file be reported as this App's key while the real one sat at a moved path nobody was told about. Identity is established first, everywhere, and an unknown identity yields uncertainty rather than either claim.

**A credential is never declared lost while its bytes are still in memory.** `os.Link` takes a NAME while the run owns a DESCRIPTOR, so the staging name is verified against the descriptor before the link and the destination is verified after it — the second catches the window the first cannot. But what follows a failed check is **not** "your key is gone": `writeKeyAtomically` still holds the complete PEM at every one of those points, so it writes the key to a fresh `O_EXCL` file and reports where it landed — verified against the descriptor and directory-synced before that promise is made. An earlier version reasoned that an unlinked inode cannot be given a name again: true, and irrelevant, because the bytes never depended on that inode. Declaring a credential unrecoverable while it sits in a live variable is the worst mistake available here, because the advice that follows is "delete the App".

The same reasoning applies one level down, and the first version missed it there: a recovery write that REPORTS an error may still have left a usable key, so the file is inspected rather than assumed empty. **Loss is what remains after looking, never what is inferred from a return value.** A recovery that fails also says only what it knows — this directory could not hold it, which is not proof that none could.

**`billet check` proves the key WORKS**, not that it exists — regular file, no group/other permission bits, bounded read, and actually parsed, all from one descriptor opened `O_NONBLOCK` so a FIFO cannot hang it. `os.Stat` alone accepted a directory, an empty file, a truncated PEM and mode 0644.

**The one-time code is removed STRUCTURALLY, and from every error in the chain.** It is still live when the exchange fails, and it reaches a terminal through `*url.Error`, which embeds the whole URL. Two rules, learned separately:

- **Sanitize where the error is created, not at the boundary.** Every wrapper renders the message of the error beneath it, so cleaning the innermost one means no wrapper can carry the code and no later stage has to recognise the encoding it arrived in. A `*url.Error` is *rebuilt* with a fixed path rather than pattern-matched — double-encoding and over-encoding both defeat matching.
- **Redaction has to hold for the whole chain, including nodes with no structure.** Sanitizing `Error()` while `Unwrap` returned the original meant `errors.As(err, &urlErr)` handed back the live URL, and any reporter that walks causes serialized it. The walk handles `errors.Join` trees (`errors.Unwrap` returns nil for one, so a chain-only walk stops dead at the join), cuts cycles, and keeps a depth backstop. Identity is preserved wherever it can be, because `errors.Is` against `context.DeadlineExceeded` depends on it — but **not** at a node whose own text carries the secret. An opaque leaf that built its message from the request URL has nothing to rebuild, so it is replaced; safety beats identity there. **Clean every field**, not just the obvious one: `url.Error` has three, and copying `Op` verbatim let a transport put the endpoint straight through the one path that is supposed to be structurally safe.
- **Never compare two `error` values with `==`.** It panics when the dynamic type is not comparable — an error struct holding a slice is ordinary — so both the cycle guard and the did-this-change check go through `sameError`, which compares pointer identity where identity exists and answers "different" otherwise. That direction is the safe one: it rebuilds a node that did not need it, rather than crashing mid-onboarding and losing the key. A test written for the *first* instance is what found the second.
- **Match the endpoint, not just the code.** A caller-supplied `RoundTripper` composes its own text. The endpoint string is an exact literal billet constructed, so matching it needs none of the encoding guesswork that matching the bare code does. Renderings are captured once per node and reused, rather than calling `Error()` again to test and again to substitute. This narrows the stateful-error hole without closing it — a parent's `Error()` inherently invokes its children's — and billet supplies the transport, so a deliberately non-deterministic error is not in the threat model.

**Nothing derived from the conversion response body is ever rendered.** This is the one endpoint in GitHub's API whose success carries a private key, so an intermediary forwarding that body under a rewritten status would otherwise put the key on the terminal. Filtering the body does not work — a secret out of its field is an opaque string, and `{"message":"whsec-…"}` carries no marker to catch. The status is mapped to text billet writes itself. False positives cost GitHub's explanation; false negatives cost the credential, and that asymmetry decides it. Other endpoints keep `apiError`.

**A code that does not redeem is discarded, not fatal.** The unguessable callback path is handed to `open`/`xdg-open` as a command-line argument, and argv is readable by other local processes — so both the path and the `state` must be assumed known, and only what a caller can *do* with them is bounded. Treating the first code to arrive as final was a kill switch: inject a worthless one, and onboarding ended with the App created and its key unrecoverable.

**Only a status that ESTABLISHES the code is unusable may discard it**, which is a much shorter list than it looks: **404**, and nothing else. Four versions shipped before that. `{404, 422}` left the kill switch open for an injected code drawing a 414. "Every 4xx" swallowed **429** — a rate limit says nothing about the code, so a *valid* code was discarded while the App stayed created. And 422 is the subtle one: GitHub documents it as *"Validation failed, **or the endpoint has been spammed**"*, so an attacker feeding forged codes can trip abuse protection and make the honest code's 422 look like a rejection. 400 is not code-specific either — a proxy returns it for header and policy reasons.

**Everything that is not a definitive rejection is ambiguous**, and ambiguous codes are **retried round-robin** — never one at a time. An enumerated ambiguous list left every unlisted status falling through as fatal, which is how removing 414 from the rejection set preserved the exact failure it was removed to fix. And retrying a single code in a blocking loop reopened the kill switch in slow motion: an injected code that always draws 422 monopolised the exchange while the honest redirect sat in the queue until the window closed. A bounded number of exchanges happen per round, unreached codes rotate to the front of the next one, a new callback interrupts the backoff, and only a 404 drops a code. Making it *fatal* was still credential loss with extra steps — the code lives in a local variable and the loopback listener dies with the flow, so "run the command again" builds a SECOND App rather than recovering the first one's key. Nothing is discarded on a response that never said the code was bad. A forged code is a random string, so GitHub answers 404, and the case that actually needed handling is the one that is unambiguous.

The callback queue is deep, and a callback that does **not** fit is refused rather than dropped — a silent drop plus an "App created" page meant the honest redirect could be discarded while its browser was told it had worked. What remains is a local process being able to *delay* onboarding up to `ManifestTTL` — not fixable while argv is readable. **No cap on which codes are kept can be correct, and three attempts established that.** Unbounded let an attacker accumulate work; bounding admission discarded an honest redirect the handler had already answered "App created"; bounding retention discarded it one ambiguous response later. billet cannot distinguish an injected code from an honest one — only GitHub can, and only a 404 is it saying so — so the bound is on **work per round**, and codes not reached rotate to the front of the next round. Nothing is ever discarded. A transport failure on one code is remembered rather than returned, because returning closes the listener with an acknowledged code unredeemed.

### The residual: argv, and why billet stops here

Everything in the paragraph above exists because of one fact: **the callback URL is passed to `open`/`xdg-open` as a command-line argument, and argv is readable by other local processes** (via `/proc` on Linux, `ps` generally). That is how a local process learns the unguessable path, reads the `state` from the start page, and injects callbacks at all.

This is **documented and accepted, not fixed.** What an attacker with that access can do is bounded:

- **They cannot obtain the key.** The conversion response never passes through anything they control, and the code is redacted from every error.
- **They cannot destroy it.** Every path above either installs the key, preserves it somewhere named, or says honestly that it could not tell — and nothing is deleted that this run did not create.
- **They can delay onboarding**, up to `ManifestTTL`, by injecting codes that stay ambiguous.

The structural fix is to keep the URL out of argv — write it to a 0600 file with a meta-refresh and open that, so the path is protected exactly as the key file is. It is not done because it trades a real risk on the primary happy path (not every browser follows `file://` → `http://`) against a threat that only exists on a multi-user host. **If billet ever targets shared CI hosts as a first-class case, do that first** — it collapses this entire class rather than scheduling around it.

Four review rounds went into scheduling around this before anyone noticed it was downstream of argv. That is the lesson worth keeping: when fixes keep producing adjacent bugs, look for the premise they all share.

**`App` is redacted on every rendering path billet can reach.** `String`/`GoString` on a **value** receiver (a pointer receiver is not consulted when a value is formatted), `Format` so no verb falls back to the raw fields — `%d` printed the key before it existed — and `MarshalJSON` plus `LogValue`, because billet standardizes on `log/slog` and its JSON handler ignores `fmt` entirely. Only marshaling is redirected; decoding GitHub's response still populates every field. Not absolute, and the gaps are known: an `App` reached through an unexported field of another struct, and any serializer that is neither `fmt` nor `encoding/json` nor `slog` — reflection-based dumpers read the fields directly.

### A registration proves who you are; only a command proves what you may do

The JIT endpoint required a registered node and nothing else, which made the README's containment claim false: a host holding a node certificate could mint runner registrations in a loop, for any scale set, under any name, and start runners billet never escrowed capacity for and never tears down.

The entitlement was already in the request and unused. Billet's runner names carry the lease id (`provider.InstanceName`), so a node may mint exactly the registration for a launch command it currently holds. Apply the same shape to anything else the node can ask for: **authentication answers WHICH host, and the command table answers WHAT it was given.**

### An empty CA directory is ambiguous, so something has to remember

"No files" reads as day one, and day one mints a new authority that every issued bundle fails to verify against — the whole fleet drops off at once while the control plane looks healthy. A marker file written at creation is what makes a later absence mean *loss*; deleting it is how an operator starts over on purpose.

Two more rules the same subsystem needs, both of which load cleanly when broken: a CA's subject must name THIS deployment (verifying against the CA is what decides who may connect, so somebody else's silently re-points that decision), and its key must be its certificate's key (unrelated halves sign leaves that fail days later on a node, in an error naming neither file). And a private key is refused unless the file itself is safe: creation's 0600 says nothing about what a backup restored.

### A node's identity is the name in its certificate, and its deployment is too

The wire used to take both from the request — the node named itself in the path, and named its deployment in the registration body. Neither was verified, which is why it refused to serve anywhere but loopback.

Now `internal/wirecert` mints one CA per deployment, held by the control plane, and `billet ca issue <node>` produces the bundle an operator copies to a host. Two rules follow, and both exist to keep ONE authority for one fact:

- **The certificate's common name decides which node a request is from.** A path that disagrees is refused, never reconciled. The check runs after routing (the path variable does not exist until the mux has matched) and is applied in the routing table itself, so a route added without it is visibly missing something.
- **The certificate's organization decides which deployment the node belongs to.** A node's state directory MINTS a random identity when it has none, which is right for a control plane — where an installation begins — and wrong for a node, which joins one. Before this, a freshly enrolled node invented an identity and the control plane refused it forever; nothing an operator could copy would have fixed it. `state.AdoptDeploymentID` writes the certificate's answer down, and REFUSES rather than overwrites when the directory already holds a different one, because the compute that directory is already managing carries the old label.

The server's own certificate is minted per boot and never stored: nothing verifies it except this CA, so persisting it would only add a file that expires, and its expiry would take the whole fleet offline at an hour nobody chose. The CA is the one thing that persists, and a CA directory holding only ONE of its two files is refused rather than repaired — minting a replacement is a new authority, and every node certificate ever issued stops verifying at once.

### Destruction is scoped by DEPLOYMENT identity, never by node name

`state.DeploymentID`: random, minted once per state directory, `O_EXCL`, and the directory is fsynced as well as the file. The node name defaults to the hostname, so two billets on one machine share it while keeping separate state directories — and the process lock does not catch that, because it guards a *directory*. Labelling compute by node name let one installation enumerate the other's containers and act on live jobs it had no relationship with.

**A copied state directory deliberately keeps the original's identity** (the copy's containers are labelled with it), and the directory lock does NOT make that safe — a copy is a different inode, so both directories lock happily. That is what `state.LockDeployment` is for: a SECOND lock keyed by the IDENTITY, so the copy collides and refuses to start.

Three things about it were wrong on the first attempt and are worth not repeating:

- **Never put a lock file in a cache directory.** It was there first, chosen over `/tmp` because `/tmp` is world-writable and a local user could hold the file to keep billet from booting. True, and still the wrong place: a cache directory's contract is that its contents may be deleted at any time. Unlinking a held lock file does not release the flock, but it detaches the PATH from the locked inode, so the next process creates a new file there, locks that, and both run. **An inode check does not fix this** — the newcomer's check passes because it created the file it just locked. The location is the fix; it now lives in the state directory (`$XDG_STATE_HOME`, or Application Support on darwin).
- **Failing to place the lock is an ERROR, not a downgrade.** It used to degrade on the reasoning that a host with nowhere to put a lock is more often one deployment than two. That derives AUTHORIZATION FROM AN I/O FAILURE: a symlink loop, a permissions change, ENOLCK, fd exhaustion, or a service manager with no `HOME` all land there and look identical to the benign case. `server. allow_unlocked_deployment` lets an operator opt in explicitly.
- **The default location is per-user, which the lock cannot fix by itself.** A system service and an operator sharing `/var/run/docker.sock`, or two containers sharing a socket with private filesystems, get different directories and never collide while their containers do. `server.lock_dir` puts them in one collision domain, and the resolved path is logged every boot so which domain a process joined is evidence rather than inference.

- **A shared directory must be SETGID, and mode bits alone never prove sharing works.** `0660` says *a* group may open the file, not *which*. A service account whose primary group is `service` and whose supplemental group is `billet` creates the lock owned by `service` in a non-setgid directory — every permission bit a check could ask for, and still unopenable by the operator it was widened for. Group-writable proves sharing was *intended*; setgid decides *who gets it*. So the directory's gid is captured and the lock file's gid must match. The umask is the same trap one level down: it turns a requested `0660` into `0640`, which cannot be opened `O_RDWR`, so the mode is corrected explicitly and verified by result rather than by intent.
- **`os.Root` confines a path; it does NOT refuse a symlink.** It follows links that stay inside the root, and its Unix implementation applies its own `O_NOFOLLOW` internally, inspects the link on `ELOOP`, then follows — so a caller's `syscall.O_NOFOLLOW` is indistinguishable from its own and is ignored. Measured, not read: a relative `link.lock -> real.lock` inside the directory opened as `os.SameFile` with its target. Use `unix.Openat` against a real directory descriptor, which honours the flag because the kernel does. Opening the directory itself `O_DIRECTORY|O_NOFOLLOW` then removes the separate `os.Lstat` too — one resolution, and no second one that could describe a different directory than the handle holds.
- **Take the flock BEFORE judging metadata.** Validating first meant a group mismatch told the operator to delete a "stale" lock file that nobody had checked was unheld — and after the delete the newcomer creates a fresh inode while the holder keeps the old one, so neither excludes the other. Nothing may be called stale until the lock is held.
- **A gid sentinel of `-1` is not safe.** `Stat_t.Gid` is unsigned, so on a 32-bit host a gid above `MaxInt32` converts to a negative `int` and becomes indistinguishable from "no group owner". Absence gets its own field.

**Claim the identity BEFORE `state.Open`.** It ran after, and `state.Open` applies migrations — so a process about to be refused first migrated the database it was refused the right to use (start an old copied backup beside a live original and the backup is silently upgraded on its way to the error).

**A contention test that runs in one process is not a contention test.** Both of the original ones called `LockDeployment` twice in the same process; a package-level mutex or a PID in the filename satisfies that while two billets start against one daemon. Measured, not assumed — the in-process test really does pass a fake process-local mutex. The real one re-executes the test binary (`deploymentlock_process_test.go`), which is also the only way to assert that SIGKILLing the holder frees the identity.

## Admission: how a server decides to trust a node

**An operator compares a fingerprint and approves it.** Two ends display the same number; a human checks they match. Everything else is transport.

A machine that has nothing runs `billet node --enroll --ca-fingerprint <value from `billet ca show`> --join-token <value from `billet ca token`>`. It fetches the control plane's authority, checks the fingerprint against the one the operator gave it, generates a key, and asks to join — printing its OWN fingerprint. The request sits as `pending`. The operator runs `billet nodes pending`, compares, and runs `billet nodes approve <node> --fingerprint <the value they compared>`. The node picks up its certificate on the next attempt.

**Both directions are verified, and neither is trust-on-first-use.** The node refuses to enroll without a CA fingerprint, because accepting whatever answered on a network an attacker can reach is just trust: they answer first, the node enrolls with them, and every job it runs afterwards is theirs. The operator refuses to approve without a node fingerprint, because approving by name alone approves whatever currently holds the name.

**A name is claimed by the first key to ask.** A second key wanting it is refused rather than replacing it — otherwise an operator who compared a fingerprint yesterday would be approving a different machine today, under a name they already trust.

**The listener verifies a certificate if one is given rather than requiring one**, because an unenrolled machine has none and still has to reach `/v1/ca` and `/v1/enroll`. That is a deliberate hole in exactly two routes; every other one is behind a guard that refuses a connection with no verified chain, and `TestAnUnenrolledConnectionCanReachNothingElse` is what says so. A certificate that IS presented is still verified against the deployment's authority.

**A join token is required to ASK.** Approval is still what admits — an operator compares fingerprints — but without a credential in front of the endpoint, anyone who can reach the port can fill the pending list, and can TAKE A NAME before the machine that should have it. "First key claims the name" protects an operator from approving a substitute; unauthenticated, it also lets a stranger deny a machine its own name. Tokens are short-lived, counted, and stored as a SHA-256 digest rather than verbatim, so a copy of the ledger is not a copy of every credential that still works. The check and the decrement are one statement, so two machines racing on a single-use token cannot both win.

The older path still works and is right for a machine you are provisioning anyway — cloud-init can drop a bundle on it, and no human is standing there to compare a fingerprint: `billet ca issue <name>` on the control plane, copy the bundle out of band. It now RECORDS the admission, marked `issued` rather than `enrolled`, so `billet nodes pending --all` is the single answer to "what has been let into this deployment, and when". Before, a fleet built that way was invisible to that list.

## The CA is a slow cliff

A leaf may not outlive its authority, so once the CA has less than a leaf's lifetime left, every certificate it issues is quietly SHORTER than the last. Renewals keep working and come round faster and faster; nothing errors; and then every node expires on the day the authority does. `billet ca show` warns once that starts, because it is invisible otherwise.

**Rotating is an overlap, not a switch**, and the ordering is the whole design. A node trusts the authority it was given, so the moment the control plane PRESENTS a certificate from a new one, every node that has not yet heard about it fails to verify the server and drops out — over the wire it would need in order to recover. There is no way back from that remotely.

Two phases:

- `billet ca rotate` — the new authority issues node certificates; the OLD one still signs what the control plane presents; both are trusted for clients. Nodes adopt the new one through ordinary renewal, which carries the trust bundle alongside the certificate. Restart the control plane to pick it up.
- `billet ca retire --force` — once every node has renewed. A node that missed the whole overlap has to be re-enrolled, which is why this is a command an operator runs when they can see the fleet has moved rather than something on a timer.

A second rotation while one is running is refused: there is one previous authority, and starting another would drop the one the un-renewed fleet still trusts.

Two implementation details that are load-bearing. `createCA` writes with `O_EXCL` and falls back to LOADING the existing authority when a key is already there — right for two processes racing to initialise, and it silently makes a rotation a no-op, so `Rotate` mints aside and renames into place. And `Rotate` must not clear the CA files and call `LoadOrCreateCA`: that leaves the authority momentarily missing while its marker says one exists here, which is exactly the state `ErrAuthorityLost` refuses.

The control plane is its own CA: on first non-loopback start it creates a per-deployment authority in `server.state_dir`, 10-year life, private key never leaving that machine. `billet ca issue <name>` mints a leaf for that name, 1-year life. The operator copies the bundle to the node out of band. On connect, `tls.RequireAndVerifyClientCert` with that CA as the only client-CA pool means a certificate this deployment did not sign never reaches a handler, and `RequireClientCert` makes the CN authoritative — a request whose PATH names a different node is rejected rather than reconciled. Registration then checks the protocol version, that the deployment id matches, that the site was declared, and that the contribution is non-zero.

**A loopback wire has no certificates at all.** The trust boundary is the machine. Config validation refuses `node.tls` against a loopback server, and the wire refuses to bind anywhere but loopback without `RequireClientCert`, so there is no way to serve the unauthenticated wire to a network.

## Revocation

`billet ca revoke <node>` writes the certificate's SERIAL to the ledger; `billet ca revocations` lists what has been withdrawn.

**Keyed on serial, not on node name.** A name is legitimately re-issued to a replacement machine, and revoking the name would refuse the replacement too.

**Checked on every authenticated request**, not only at registration: a node holds one long poll open for the better part of a minute and re-registers rarely, so a check at registration alone would leave a revoked host working until it happened to restart.

**It fails closed.** An unreadable revocation list refuses the request rather than assuming nothing is revoked — the alternative makes a transient database fault equivalent to switching the check off, silently, at exactly the moment somebody is relying on a revocation having taken effect.

## Renewal

A node replaces its own certificate when less than a third of its life remains, on the same pass as the sweep. Without it a fleet enrolled on one afternoon expires on one afternoon a year later, all at once, and cannot recover on its own.

**Authenticated by the certificate being replaced**, so it grants nothing: a host that can already act as a node asks to keep doing so. A revoked certificate therefore cannot renew — `forNode` has already refused it.

**The private key never crosses the wire.** The node generates a key and sends a CSR; the control plane returns only a signature. **The subject comes from the authenticated identity, never from the CSR** — a CSR's subject is whatever the requester typed, so signing it would let any node with a valid certificate mint one for any name, which is every node able to impersonate every other through the endpoint meant to keep them working.

**The renewal is verified before it is installed**, and written to disk before it is used. A certificate that does not chain to the authority this node trusts leaves the working one in force.

**What this does NOT cover:** a node whose certificate has already expired cannot renew, because renewal is authenticated by that certificate. It has to be re-enrolled by hand. The renewal window is a third of the certificate's life — months — so this only happens to a host that was off for that entire period.
