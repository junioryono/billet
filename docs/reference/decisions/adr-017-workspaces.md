# ADR-017: Workspaces, long-lived environments forked from a warm snapshot

Proposed, 2026-10-16. Being implemented in stages, of which only the snapshot primitive (#508) has begun: Firecracker can capture a running microVM into a published snapshot and restore one through the launch path, and every restore refuses a snapshot taken where it cannot run (#521, which carried #551). No workspace resource, edge, credential home or command exists yet. The Firecracker facts it rests on were first a reading of the documentation; those the [workspace snapshots record](../records/workspace-snapshots.md) has since measured (2026-10-16, v1.16.1) are marked so under Measure before accepting, and the rest are still to be measured before this is accepted. Relates to issue #28: accepting this record revises the exposure rule #28 states for HTTP views (see The edge), and its attach transport is meant to serve #28's SSH item too.

## Context

A coding agent now opens many changes in parallel, and each one is only verifiable with the whole application running: its services, its database with data in it, and the URLs a browser or a phone reaches it on. On one developer's machine that is a fixed number of local stacks, each minutes to boot and gigabytes of memory, and the machine runs out before the agent does. billet already runs compute behind a real boundary on its own hardware: jailed Firecracker microVMs, Ceph copy-on-write root disks, an outbound-only node wire, and secrets delivered through MMDS rather than argv. What it runs is one shape, a GitHub job: a lease bought when a runner starts, a VM that cold-boots, and compute released on proof it is gone. A workspace is a second shape on the same machinery, and most of this record is where the two differ.

## Decision

### What a workspace is

**A workspace is a VM for one task.** It holds an ordinary git checkout of one repository on a fresh branch, the application's whole stack running, and the agent process started with the task. It is reached by a person (attach, previews, an editor) and by the agent's own tools. It pushes a branch and opens a pull request through GitHub like any other client. billet adds no version-control system.

**A workspace is a resource of its own, not a lease with a longer TTL.** It is owned by one user in one organisation, it lives until it is removed, and it is never charged to a tier's escrow or advertised to GitHub. It is still bought before compute exists and released only on proof the compute is gone, through the same placement, capacity vector and custody rules a lease follows. A workspace's compute is a set of new command kinds on the node wire (launch from a golden, fork, suspend, resume, destroy), so a node that does not speak them is never sent one.

**Storage is admitted separately from compute.** A suspension releases compute and keeps its snapshot and disks, so compute capacity alone bounds nothing on storage. Persistent storage is therefore admitted under deployment and owner limits:
- create, fork and suspend each reserve the storage they need, temporary capture space included, before writing;
- a reservation covers the maximum growth its writable disks and the owner's home are allowed, copy-on-write growth while older generations are retained included, and the storage enforces that limit against the guest's own writes, so growing it is itself an admitted operation;
- a suspension keeps its storage charge after its compute is released;
- golden builds reserve compute and their maximum temporary and persistent storage under deployment admission before they start. A builder's compute stays charged until it is proved gone. Every artifact a build writes stays charged, failed and unpublished output included, until its removal is proved, and an unknown cleanup keeps the charge. Publication transfers the charge with no gap, and a published golden's storage stays charged while it is retained, including while any workspace pins it. A rebuild needs a reservation of its own and never overwrites a retained generation;
- insufficient or unknown headroom refuses the operation and never deletes existing work.

**Only explicit requests stop or delete a workspace.** Idle time produces a warning to the owner and a suspension recommendation; it never stops the VM, because a timer never authorises a teardown and an idle observation does not prove that a background agent or an outside operation has finished. Suspending and deleting are requests that name the workspace, made by its owner or an operator. Any automatic policy would be a change to that invariant, made in a record of its own.

### Goldens, and making a fork unique

**A workspace starts from a warm golden snapshot.** A golden is built by booting the application's image, bringing its stack to a healthy state, quiescing it (below), and snapshotting it. The VM that produced the golden is terminated and never resumed. Each workspace is a restore of that snapshot into its own jail with its own Ceph clone of the golden's system disk, so it pays for the pages it touches rather than for a boot.

**A golden restores only with its own system disk; a workspace's checkout is on a volume the golden never saw.** A restore brings back the guest kernel's view of every mounted filesystem, so putting a different disk under a restored mount risks corrupting it. So a workspace has two disks:
- **The system disk:** a clone of the golden's, holding the OS, the dependencies and the warmed services with their seeded data.
- **The workspace volume:** created for that workspace and never present in any golden, holding the git checkout.

How the volume reaches the guest: Firecracker creates drives only before boot and afterwards updates an existing one, so the golden carries an unused drive slot backed by a disposable placeholder, with no filesystem mounted on it and no outstanding I/O. billet already attaches cache volumes this way, through pre-declared `cacheN` slots. After restore, billet replaces that slot's backing store with the workspace volume through the existing-drive update, and the guest mounts it only after the replacement and its cache invalidation are proved. This is conditional on a dated probe.

Nothing billet carries from one workspace into another goes on the system disk.

**Only what does not depend on the checkout stays warm.** A process the golden warmed holds the golden's code, open files, working directories and filesystem watches, and none of that follows a checkout copied in later. So only checkout-independent services (the database, the cache, the queue) stay warm across a restore. Checkout-dependent processes start afresh from the workspace's checkout once it is mounted and finalised, unless a measured procedure proves that all of their checkout-derived state has been reinitialised from it: loaded code, open files, working directories and watches, but also configuration, environment and in-memory caches such as parsed templates. That list is illustrative, not exhaustive, and an incomplete or unknown proof means the process restarts. Readiness is judged after that.

**A golden is pinned to a compatibility fingerprint, and billet refuses a mismatch before loading.** Firecracker requires a restore on the software and hardware configuration a snapshot was taken on; that is a requirement on the integrator, not a refusal Firecracker promises to make. So every golden records a fingerprint (CPU model and configuration, host kernel, Firecracker build, guest kernel), and billet compares it with the host's before invoking a load; a mismatch, or a fingerprint it cannot read, refuses. A host whose fingerprint changes builds new goldens.

**A snapshot is authenticated, not only compatible.** Firecracker treats snapshot files as trusted input, and a compatible fingerprint says nothing about who wrote the bytes. Before any load, billet verifies an authenticated manifest that binds every component (memory, VM state, disks) to:
- its generation;
- its fingerprint;
- its deployment;
- its permitted scope (a golden's repository, or one workspace).

Only an authorised golden builder, or the custodian assigned to that workspace, may publish one. Missing, mismatched or unreadable evidence refuses the load.

**A memory file is immutable and pinned while anything runs from it.** A restored VMM keeps faulting pages from its memory file for as long as it runs, so a consumed suspension's file is not expendable. Published memory files are immutable to the host and to every jailed process. Every loaded VMM pins its backing generation until there is positive proof the VMM is gone, and an unknown outcome keeps the pin. A rebuild or a new capture writes new files and never overwrites, truncates or repurposes a pinned one.

**Nothing in a warmed stack is trusted to be unique, so nothing runs until it is made so.** Firecracker's documentation calls resuming one state more than once insecure: identifiers, seeds, the entropy pool and any token in memory are replicated into every restore, VMGenID reseeds only the guest kernel, and userspace can run before the reseed lands. So:
- **Before capture:** every application process is stopped at a restore barrier the guest agent holds, the guest's network is down, and the golden holds no credential, key or token of any kind.
- **After restore:**
  - the barrier holds until the agent has proved the kernel reseeded from the VMGenID notification;
  - the guest's wall clock is made current before anything trusts it. Sampled forks restored about a minute after capture were 64 to 75 s behind the host, and an aged golden or a long suspension would start TLS and credential checks in the past. The mechanism (measured):
    - the golden boots with `clocksource=kvm-clock`, and every load passes `clock_realtime`, on a host with a TSC clock source and Linux 5.16 or later;
    - a fork restored more than a minute after capture was then within 0.02 s of the host, whereas with the guest's default `tsc` clock source the option changed nothing;
    - the agent still checks the clock against the host time in its MMDS document, and an unknown or wrong result holds the barrier;
  - the agent then regenerates per-fork identity (machine id, host keys, hostname, the Docker daemon's identity). `boot_id` was identical in the base and every sampled fork (measured), so nothing may rely on the inherited value to tell forks apart;
  - only then do services start.
- **Services:** a service with clone-sensitive state (a database's system identifier, a TLS server or client session, a persistent random seed, a cached token) is reinitialised through a procedure measured for that service, or is not in the golden at all and starts fresh after restore.
- **No guess:** a golden whose services include one billet cannot classify is refused at publication.

TLS for previews terminates at the edge, so no fork serves with a private key a golden held.

**Every load follows one sequence, a suspension's included.**
1. Logging and metrics are configured on the fresh VMM before the snapshot is loaded, because they are not in a snapshot and Firecracker accepts that configuration only before load.
2. The snapshot is loaded paused.
3. Current, authorised MMDS data is installed (identity, a freshly minted repository token, the model-credential source, the task), since the MMDS data store does not survive any load.
4. Only then is the VM resumed.

A fork gets a new identity; a resumed suspension keeps its own but still gets fresh credentials and metadata. A missing or unknown step refuses the resume.

**A fork's network is its own namespace.** A restored guest keeps the address the golden had, so every workspace runs in a network namespace of its own, reached from the host through a veth pair and NAT, following Firecracker's network-for-clones recommendation. The tap inside it need not carry the golden's name: `network_overrides` on load points the interface at whatever tap billet allocated, and that was measured to work. `vsock_override` likewise gives each fork its own socket path. Egress is an allowlist per workspace, on top of the deny rules the host already applies to guests. Ingress is denied too, and the host enforces both:
- no new inbound connection reaches a workspace's services except one the authenticated relay opens, to a port explicitly permitted for previews;
- other workspaces and peers on the physical network cannot reach those ports directly;
- the guest cannot alter this policy, and missing or unproved enforcement refuses the launch.

Firecracker's network-for-clones example forwards freely and disclaims its own security, so it is followed for addressing only.

### Suspension and forks of a running workspace

**A suspended workspace is private, and it is never a source for anyone else.** A running workspace's memory holds its owner's repository token and model credentials, so a suspension snapshot is confidential user state, bound to one deployment, one owner and one workspace. It is never a golden and never the source of a fork. It lives in storage only its node and its owner's restore can reach, and a resume is authorised the same way as attach.

**A suspension is captured behind the same barrier a golden is.** Before the VM pauses:
- the agent process exits, and the credential home's writes are drained before its unmount completes, so a restore never continues using that mounted client. This does not erase credential bytes from guest memory: freed pages can still hold them, so every suspension generation remains credential-bearing confidential state, as above;
- every application process is parked behind the guest agent's restore barrier, and its external traffic is blocked.

If preparation fails at any step, the suspend is refused and billet undoes what it did:
1. restores the home and the network;
2. restarts the agent;
3. releases the barrier only once those steps are proved. After restore, applications stay parked until the clock is corrected, the home is mounted again and the network is restored; an unknown result holds the barrier. A resumed suspension keeps its own identities, because it is the same workspace continuing, so golden-fork identity regeneration does not apply. Restore mounts the current home and starts a fresh agent process, which continues its session through the agent's own mechanism.

**Durability is proved before the VMM stops.** A suspend, behind that barrier:
1. pauses the VM;
2. writes a recovery generation (memory, VM state, the system disk and the workspace volume, as one consistent set);
3. durably records that the generation is complete.

Only then is the deployment-owned VMM stopped, through the existing pid proof and SIGKILL path, and capacity is released only on positive proof the VMM is gone. A generation becomes eligible for restore only once its source VMM is proved gone and unable to execute further.

If the generation fails, or billet cannot tell whether it is complete, billet follows a recoverable abort:
1. It durably and exclusively marks the generation unusable for restore, under the same fence restore claims use.
2. It resumes the original VMM.
3. It restores the credential home's mount and the network.
4. It recovers the vsock connections that the snapshot attempt reset.
5. It restarts the agent.
6. It releases the application barrier only after all of that is proved.

Any unknown outcome keeps the capacity and reports the workspace as needing recovery, never as running.

**A suspension generation runs at most once.** Firecracker warns that executing one state twice duplicates it even when the two runs are sequential, and that includes the original VM continuing after its own snapshot is restored. So a resume first takes a durable, fenced claim on the generation, and only then loads it. Once execution may have begun, the generation is never loaded again: recovery reconciles that incarnation, even after it stops. A lost acknowledgement or a restart between any durable decision and the VMM action that follows it therefore never runs the captured state twice.

**A suspended workspace pins its configuration.** A resume needs the fingerprint the suspension was taken on. A configuration stays pinned while any live VMM uses it or any eligible suspension generation depends on it, and resuming does not release the pin, because a restored VMM still runs on the configuration it was taken on and its next suspension needs that configuration too. Retirement requires positive proof that every dependent VMM is gone and no eligible generation remains. Discarding a dependency takes the owner's explicit fallback or deletion. The fallback is a new workspace from a current golden with the old workspace volume attached after the barrier. It is an exclusive transition under the workspace's restore fence. Before the volume is transferred or attached, billet proves that every previous VMM incarnation that could reach it is gone and that its outstanding storage I/O is drained. A live incarnation is stopped through the deployment-scoped pid-proof path under the owner's explicit fallback request, and missing or unknown proof keeps ownership and capacity where they are and refuses the attachment. Also before the volume moves, billet durably makes every old suspension generation ineligible for restore and transfers exclusive ownership of the volume to the new workspace, and an unknown outcome permits neither the attachment nor a resume. That volume is treated as crash-consistent: it is mounted only after its journal is recovered. It keeps the checkout and loses process memory and anything the old system disk held, the database's changes included. Rebuilding a golden never replaces a workspace's own snapshot.

**`fork` is a new workspace for the same owner with a copy of the source's checkout files.** It restores a current golden like any create, then copies the files of the source's checkout into the new workspace volume, file by file. The source is an immutable view of the checkout, captured while every writer to it is quiesced, so a commit or rewrite in progress cannot produce a repository whose refs name objects that were never copied. The new workspace stays behind its barrier until the copy is complete, and an incomplete or unreadable copy is never published. It does not clone a block device, so unallocated filesystem blocks do not come along. Deleted content still held in a live file does: git objects, reflogs and anything else the checkout keeps. The same-owner restriction covers that. It never copies memory, the system disk or a credential home.

A guest-controlled volume is never parsed by the host kernel. Mounting it, recovering its journal, validating it and walking it all happen inside a microVM, never in the host kernel or a privileged host utility, because guest root can shape filesystem metadata to attack whatever parses it. The host only carries bounded file records between the two VMs, and the destination applies them.

The traversal is confined to the checkout through verified directory descriptors, following billet's rule that privileged operations go through a descriptor, never a pathname:
- it never follows a symlink, crosses a mount point or opens a special file;
- symlinks are copied as their link text;
- no source inode is shared with the destination;
- a path replaced while the copy runs cannot lead outside either root.

Git metadata that points outside the checkout is refused: a `.git` file, a `commondir` or an object alternate never authorises traversal outside the source view. Before a fork is published, billet proves that its git metadata and every object its refs need are inside the copy. A linked worktree or an alternate store must first be materialised into the confined view.

No code from the checkout runs during a fork. Git validation, materialisation and branch creation never run on the host. They run inside the destination microVM as the workspace user, after the identity and reseeding checks, with:
- a clean environment;
- allowlisted configuration (no `core.fsmonitor`, no `core.hooksPath`);
- repository hooks and executable helpers disabled.

Nothing the checkout controls executes before the barrier releases.

The fork gets a branch of its own. After the copy, and before the barrier releases, billet creates and checks out a fresh branch at the copied HEAD, keeping staged and unstaged changes, and removes any inherited push configuration that targets the source branch. Failure leaves the fork unpublished.

Whatever the source's agent wrote into its checkout, a token in an untracked file included, comes along, which is why a fork belongs to the same owner only. The new workspace gets its own identity and its own repository token.

### Access

**Previews and attach need no new externally reachable listener on a node host.** The node initiates authenticated relay connections to the control plane and serves nothing outward. Attach is a terminal the guest agent serves over vsock, relayed node to control plane on a multiplexed stream beside the long-poll. The sampled snapshot-and-resume sequence closed an established vsock connection, so the design requires the agent to reconnect after every restore and the relay to treat a reset as normal. Previews are HTTPS requests to the control plane's edge, routed to a workspace and down the same stream to the fork's address inside its namespace.

**Every request is authorised for the workspace it names, not only authenticated.** Every preview request, attach stream, resume and MCP call authenticates the caller and authorises that caller against the destination organisation, workspace and current assignment before a relay opens. Missing, unknown or unreadable authorisation refuses. The edge consumes billet's session cookies and authorisation headers and never forwards them into a guest.

**Authorisation lasts as long as it holds, not as long as a connection does.** Every relay stream (an attach terminal, a preview WebSocket, a long-lived request) is bound to its authorisation grant and to the workspace's assignment generation. Revocation, the grant's expiry or a change of assignment ends the stream, and forwarding continues only while authorisation is current; an unknown result stops forwarding. Closing a relay neither stops the workspace nor releases its capacity.

**A browser cannot be made to send a command.** A preview is application content its owner's browser runs, so being signed in and owning the workspace does not prove a request was intended:
- every cookie-authenticated mutation validates `Origin` and carries a session-bound CSRF token;
- a preview's session is scoped to its own workspace and cannot authorise any control-plane operation;
- an attach upgrade validates `Origin`;
- MCP clients authenticate with scoped bearer tokens, never ambient cookies, and those tokens are never handed to preview content.

Only the start of sign-in is anonymous. Every other public request carries the authentication its endpoint specifies: MCP accepts scoped bearer tokens and refuses session cookies.

**Each workspace has its own origin, never reassigned.** A reused preview hostname would carry a browser's cookies, storage and service workers from one workspace into the next. So every workspace's preview hostnames are unique to it, on a domain that serves nothing but previews (on the Public Suffix List, so one workspace's origin cannot set cookies for another's). An application whose OAuth provider needs a redirect URL registered in advance points it at a fixed callback on billet's edge, which binds the callback to the user, the workspace and its assignment generation and forwards it to the workspace's own origin.

**The edge is a new public listener, and this record is what permits it.** Today billet's HTTP views stay behind the loopback or mTLS listeners (#28), and a browser can present neither. Accepting this record revises that rule for exactly these public, authenticated endpoints of the control plane:
- the sign-in flow;
- the preview edge;
- the OAuth callback broker;
- the MCP endpoint;
- attach, which requires control-plane authentication, workspace authorisation and an allowlisted control-plane `Origin`, and which a preview session can never authorise.

Everything else stays where it is. A team that wants all five private puts the control plane behind its own network. An MCP client that connects from its own cloud cannot then reach the MCP endpoint, which the team accepts or exposes alone.

**Workspaces are trusted-only and Firecracker-only.** The code in a workspace is written live by an agent and pulls dependencies from the internet, and a person attaches to it, so it needs the jailer's boundary and a trusted classification. A workspace is refused on a backend or tier that cannot give both. Attach is never offered on an untrusted tier, which is also #28's rule for SSH into a job.

### GitHub and model credentials

**Repository access is a separate GitHub App, and the App alone entitles no one.** A runner App holds only what registering runners needs, and billet refuses an installation holding a permission it did not request, in both directions. Workspaces need `contents: write` and `pull_requests: write`, so they use an App of their own per target, created and verified the same way, rather than widening the runner App. Before creating a workspace, and before minting or renewing its repository token, billet proves that the workspace's owner may write to that repository under the target's policy; the installation covering a repository says nothing about who may use it, and could-not-tell refuses. The same check applies to the golden a workspace starts from and the data it carries. Tokens are scoped to the one repository, short-lived, and delivered through MMDS, never argv.

**The agent's model credentials are the user's own, and billet never handles a sign-in credential.** A workspace runs the agent's binary unmodified. A user either authenticates with their own API key or cloud-provider credential (delivered through MMDS to a key helper), or signs in to their own subscription through the agent's own sign-in flow. For the latter, each user has a home directory on a shared filesystem with proved concurrent-client semantics (CephFS is the candidate, since the nodes already run Ceph), mounted into that user's workspaces only. Isolation between users is enforced by the storage service against a client the guest controls, never by a mount path or a guest uid:
- any storage credential inside a workspace authorises only its owner's home;
- for CephFS, both the MDS path and the underlying RADOS access are restricted;
- a node-wide storage credential never enters a guest;
- missing or unproved isolation refuses the mount.
- **How it's used:** the agent writes its own login there, as several sessions on one laptop share one home, and billet never reads, copies or special-cases it.
- **Never a snapshot:** it is never an ordinary block filesystem mounted by two guests. Its storage is outside every recovery generation and every fork, and it is unmounted before a suspension is captured, so a restore never resumes a stale client session. (Bytes it once held may still be in a suspension's memory; that is why a suspension is confidential.)
- **Never in a golden:** it is never part of a golden, and the agent starts after a restore, so a golden's memory holds no credential.

**Agents reach billet through MCP.** The control plane's MCP endpoint offers tools to create, list, fork, inspect, suspend and remove the caller's own workspaces, each call authorised as above.

## Measure before accepting

Each is a probe on the reference Firecracker host, recorded here with the Firecracker version, the guest kernel and the date:
- **Speed and memory:** *mechanics measured:*
  - load takes 4 to 5 ms;
  - a fork's agent answers 31 to 38 ms after its process starts, and 10 concurrent forks take 0.52 s;
  - each fork dirties about 11.5 MiB, and 10 forks were 227 MiB PSS together.

  Still to measure: the time from a create request to a healthy application stack at 1, 5, 10 and 20 concurrent workspaces.
- **Reseeding:** *partly measured:*
  - the guest kernel billet ships binds Firecracker's VMGenID device, and kernel random samples differed in 10 of 10 forks (that VMGenID caused the reseed is not established);
  - a userspace RNG seeded before capture returned the same value in every fork, which is the case the barrier exists for.

  Still to measure:
  - the restore barrier holds every application process until the guest kernel reseeds from VMGenID;
  - the reseed is observable to the agent;
  - each service in the reference golden either reinitialises its clone-sensitive state by a named procedure or is excluded.
- **MMDS and configuration:**
  - *measured:* logging and metrics configured on a fresh VMM before load are accepted (`204`), and after load both are refused with `400`;
  - *measured:* the MMDS data store is `{}` after a paused load and accepts a document before resume;
  - a suspension restored after its previous repository token expired runs with a fresh one.
- **Integrity:** a substituted component, a mix of two generations and a bundle from another deployment are each refused before load.
- **Pinning:** *the mechanism measured:* replacing a running fork's memory file by rename leaves the fork intact, and truncating it in place ends its VMM within a second. Still to measure: billet's own rebuild and cleanup leave running workspaces intact.
- **Storage admission:**
  - create, fork and suspend at the storage limit are refused without touching existing work;
  - a running guest filling its disks or its home stops at its reservation;
  - repeated golden rebuilds while older goldens stay pinned are refused before compute or storage is overcommitted.
- **Fresh code:** with the golden, the source workspace and the fork each answering with a distinguishable response (an uncommitted change in the source included), each workspace's preview serves its own checkout.
- **Revocation:** revoking access while an attach terminal and a preview WebSocket are open ends both.
- **Network:**
  - *measured within the namespace:* the baseline forks used the golden's tap name and address and answered HTTP; a separate fork answered HTTP through a differently named tap selected by `network_overrides`. Still to measure: reachability through its veth from the relay;
  - a direct connection to its services from another workspace or from a physical-network peer fails.
- **vsock:**
  - *measured:* a new connection to the guest's vsock listener was answered after a snapshot and resume. Still to measure: the listener works after a restore with an updated guest CID;
  - *measured:* in the sampled snapshot-and-resume sequence, the next read on an established vsock connection returned end of file;
  - the agent and the relay recover it.
- **The memory file in a jail:** *measured.* A hard link into the chroot, root-owned and mode 0444 as the kernel is, restores under the jailer, and the jailed uid can read it but not write it. A golden's memory file reaches a jail that way. A suspension's is confidential, so it needs a mode only its own VM can read; that is still to be measured.
- **Compatibility:** a golden with a mismatched or unreadable fingerprint is refused by billet before load, and a matching one restores.
- **Clock:** *measured:*
  - without a mechanism, forks restored about a minute after capture were 64 to 75 s behind the host;
  - with a `clocksource=kvm-clock` guest and `clock_realtime` on load, a fork was within 0.02 s after more than a minute.

  Still to measure: the same after a long suspension and an aged golden (hours to days), and the agent's check holding the barrier when the clock is wrong.
- **Disks:**
  - replacing a restored golden's placeholder slot with a workspace volume through the existing-drive update works, covering device sizing, cached reads and writes and repeated restores;
  - an old workspace volume mounts after journal recovery for the fallback.
- **Fork:**
  - a file-level copy of a checkout carries the files and none of the volume's unallocated blocks, while content retained in git objects and other live files is present, as stated;
  - with commits and file rewrites running in the source, the copy is a consistent repository;
  - a symlink pointing out of the checkout, a special file and a rename during the copy all fail to escape either root.
- **Home isolation:** from one user's workspace, as guest root, reading another user's home fails both through the filesystem and through direct object access.
- **Aborted suspension:** after a failure during preparation and during capture, the workspace is usable again (home mounted, network up, agent running, applications released) or reported as needing recovery.
- **Fork executes nothing:** an executable `post-checkout` hook and a `core.fsmonitor` setting in the source run neither on the host nor in the guest during finalisation.
- **Fork git integrity:** a linked worktree and an alternate object store in the source are either materialised or refused, never left dangling; the fork is on a fresh branch with no push configuration targeting the source.
- **Shared home:**
  - two workspaces of one user share the home concurrently;
  - suspending one (agent stopped, writes drained, home unmounted) and resuming it while the other writes, and while the other rotates the login, loses nothing and rolls nothing back.
- **Suspension barrier:** with an application busy at the moment of capture, a resumed suspension runs none of that application's code before the clock is corrected and the home and network are back.
- **Suspend and resume:**
  - a suspend writes one consistent generation of memory, VM state and both disks;
  - **process crashes:** a crash of the billet node or controller process, injected at every suspend step while the host and VMM survive, leaves either the original VMM or a complete generation;
  - **host or storage failure:** an acknowledged suspension stays recoverable, and an interrupted, unacknowledged one may lose volatile state but is never reported complete;
  - **at most once:** a resume whose acknowledgement is lost, and a restart in the middle of a resume, never load one generation twice;
  - **fallback versus resume:** a fallback racing a resume, and a fallback after a completed resume (the old node unreachable with its VMM still alive included), with a crash at each step, never leaves the old volume attached to two workspaces or a generation restorable after its volume moved;
  - **pin survives a resume:** resume, then an attempted retirement of its configuration, then suspend again, then resume, all work;
  - **between decision and action:** a crash between every durable decision and the VMM action it permits, a failed suspend's invalidation and the original's resume included, never runs one captured state twice.

## Rejected

**A cold boot per workspace.** It is what a job does today and costs minutes, which is the problem being solved.

**Containers as the boundary.** billet's threat model treats a docker container as no boundary at all, and a workspace is code an agent writes live, with dependencies from the internet and a person attached.

**Widening the runner App.** It would give every runner deployment a code-write grant it never uses, and remove the two-way permission check that catches an App pointed at the wrong job.

**An inbound port on a node, or a third-party tunnel in the product.** The first is the one property billet's node wire was built to avoid. The second makes a workspace depend on an account the operator may not have.

**Fixed preview hostnames reused across workspaces.** They would make OAuth registration simpler and carry one workspace's browser state into the next.

**Suspending or deleting on a timer.** Time warns; its owner decides.

**Forking by cloning a running workspace's memory or disks.** Memory would duplicate the owner's live credentials and every identity the source holds. A block clone would carry deleted contents and service identities. And a golden's restored memory over another workspace's disk risks corrupting it.

**A credential home on an ordinary block volume.** Several workspaces use it at once, and a snapshot of one would roll back the others.

**A new version-control system.** The pain a workspace removes is running the application, not storing its history, and every tool an agent and a reviewer use already speaks git.

**Capturing the agent's subscription token to provision workspaces.** It would make billet the holder of a credential it may not hold; the user signs in through the agent's own flow instead.
