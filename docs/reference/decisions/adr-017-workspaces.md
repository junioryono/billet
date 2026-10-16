# ADR-017: Workspaces, long-lived environments forked from a warm snapshot

Proposed, 2026-10-16. Nothing here is implemented, and nothing here is measured yet: every fact below about Firecracker is a reading of its documentation, which a house rule says is not enough, so each one is listed under Measure before this is accepted. Relates to issue #28: accepting this record revises the exposure rule #28 states for HTTP views (see The edge), and its attach transport is meant to serve #28's SSH item too.

## Context

A coding agent now opens many changes in parallel, and each one is only verifiable with the whole application running: its services, its database with data in it, and the URLs a browser or a phone reaches it on. On one developer's machine that is a fixed number of local stacks, each minutes to boot and gigabytes of memory, and the machine runs out before the agent does. billet already runs compute behind a real boundary on its own hardware: jailed Firecracker microVMs, Ceph copy-on-write root disks, an outbound-only node wire, and secrets delivered through MMDS rather than argv. What it runs is one shape, a GitHub job: a lease bought when a runner starts, a VM that cold-boots, and compute released on proof it is gone. A workspace is a second shape on the same machinery, and most of this record is where the two differ.

## Decision

### What a workspace is

**A workspace is a VM for one task.** It holds an ordinary git checkout of one repository on a fresh branch, the application's whole stack running, and the agent process started with the task. It is reached by a person (attach, previews, an editor) and by the agent's own tools. It pushes a branch and opens a pull request through GitHub like any other client. billet adds no version-control system.

**A workspace is a resource of its own, not a lease with a longer TTL.** It is owned by one user in one organisation, it lives until it is removed, and it is never charged to a tier's escrow or advertised to GitHub. It is still bought before compute exists and released only on proof the compute is gone, through the same placement, capacity vector and custody rules a lease follows. A workspace's compute is a set of new command kinds on the node wire (launch from a golden, fork, suspend, resume, destroy), so a node that does not speak them is never sent one.

**Only explicit requests stop or delete a workspace.** Idle time produces a warning to the owner and a suspension recommendation; it never stops the VM, because a timer never authorises a teardown and an idle observation does not prove that a background agent or an outside operation has finished. Suspending and deleting are requests that name the workspace, made by its owner or an operator. Any automatic policy would be a change to that invariant, made in a record of its own.

### Goldens, and making a fork unique

**A workspace starts from a warm golden snapshot.** A golden is built by booting the application's image, bringing its stack to a healthy state, quiescing it (below), and snapshotting it. The VM that produced the golden is terminated and never resumed. Each workspace is a restore of that snapshot into its own jail with its own Ceph clone of the golden's system disk, so it pays for the pages it touches rather than for a boot.

**A golden restores only with its own system disk; a workspace's checkout is on a volume the golden never saw.** A restore brings back the guest kernel's view of every mounted filesystem, so putting a different disk under a restored mount risks corrupting it. So a workspace has two disks:
- **The system disk:** a clone of the golden's, holding the OS, the dependencies and the warmed services with their seeded data.
- **The workspace volume:** created for that workspace and never present in any golden, holding the git checkout.

How the volume reaches the guest: Firecracker creates drives only before boot and afterwards updates an existing one, so the golden carries an unused drive slot backed by a disposable placeholder, with no filesystem mounted on it and no outstanding I/O. billet already attaches cache volumes this way, through pre-declared `cacheN` slots. After restore, billet replaces that slot's backing store with the workspace volume through the existing-drive update, and the guest mounts it only after the replacement and its cache invalidation are proved. This is conditional on a dated probe.

Nothing billet carries from one workspace into another goes on the system disk.

**A golden is pinned to a compatibility fingerprint, and billet refuses a mismatch before loading.** Firecracker requires a restore on the software and hardware configuration a snapshot was taken on; that is a requirement on the integrator, not a refusal Firecracker promises to make. So every golden records a fingerprint (CPU model and configuration, host kernel, Firecracker build, guest kernel), and billet compares it with the host's before invoking a load; a mismatch, or a fingerprint it cannot read, refuses. A host whose fingerprint changes builds new goldens.

**Nothing in a warmed stack is trusted to be unique, so nothing runs until it is made so.** Firecracker's documentation calls resuming one state more than once insecure: identifiers, seeds, the entropy pool and any token in memory are replicated into every restore, VMGenID reseeds only the guest kernel, and userspace can run before the reseed lands. So:
- **Before capture:** every application process is stopped at a restore barrier the guest agent holds, the guest's network is down, and the golden holds no credential, key or token of any kind.
- **After restore:**
  - the barrier holds until the agent has proved the kernel reseeded from the VMGenID notification;
  - the agent sets the guest's wall clock to the current time through a measured mechanism, because a restored clock continues from snapshot time (an aged golden or a long suspension would otherwise start TLS and credential checks in the past), and an unknown result holds the barrier;
  - the agent then regenerates per-fork identity (machine id, host keys, hostname, the Docker daemon's identity);
  - only then do services start.
- **Services:** a service with clone-sensitive state (a database's system identifier, a TLS server or client session, a persistent random seed, a cached token) is reinitialised through a procedure measured for that service, or is not in the golden at all and starts fresh after restore.
- **No guess:** a golden whose services include one billet cannot classify is refused at publication.

TLS for previews terminates at the edge, so no fork serves with a private key a golden held.

**Per-fork state is delivered after load and before resume.** The MMDS data store does not survive a snapshot, so a fork is loaded paused, given its MMDS document (its identity, its repository token, its model-credential source, its task), and only then resumed. Metrics and logging configuration are not in a snapshot either and are configured on every load.

**A fork's network is its own namespace.** A restored guest keeps the address and tap name the golden had, so every workspace runs in a network namespace of its own with that tap name inside it, reached from the host through a veth pair and NAT, following Firecracker's network-for-clones recommendation. Egress is an allowlist per workspace, on top of the deny rules the host already applies to guests.

### Suspension and forks of a running workspace

**A suspended workspace is private, and it is never a source for anyone else.** A running workspace's memory holds its owner's repository token and model credentials, so a suspension snapshot is confidential user state, bound to one deployment, one owner and one workspace. It is never a golden and never the source of a fork. It lives in storage only its node and its owner's restore can reach, and a resume is authorised the same way as attach.

**A suspension is captured behind the same barrier a golden is.** Before the VM pauses:
- the agent process stops, and the credential home's writes are drained and the home is unmounted, so no filesystem client state or credential cache for the home sits in the captured memory;
- every application process is parked behind the guest agent's restore barrier, and its external traffic is blocked.

If any of these cannot be proved, the suspend is refused and the workspace keeps running. After restore, applications stay parked until the clock is corrected, the home is mounted again and the network is restored; an unknown result holds the barrier. A resumed suspension keeps its own identities, because it is the same workspace continuing, so golden-fork identity regeneration does not apply. The agent is started again with its session's own continuation.

**Durability is proved before the VMM stops.** A suspend, behind that barrier:
1. pauses the VM;
2. writes a recovery generation (memory, VM state, the system disk and the workspace volume, as one consistent set);
3. durably records that the generation is complete.

Only then is the deployment-owned VMM stopped, through the existing pid proof and SIGKILL path, and capacity is released only on positive proof the VMM is gone. A generation becomes eligible for restore only once its source VMM is proved gone and unable to execute further.

If the generation fails, or billet cannot tell whether it is complete, billet first durably and exclusively marks it unusable for restore, under the same fence restore claims use, and only then resumes the original VMM. If even that mark's outcome is unknown, the VMM stays paused and keeps its capacity.

**A suspension generation runs at most once.** Firecracker warns that executing one state twice duplicates it even when the two runs are sequential, and that includes the original VM continuing after its own snapshot is restored. So a resume first takes a durable, fenced claim on the generation, and only then loads it. Once execution may have begun, the generation is never loaded again: recovery reconciles that incarnation, even after it stops. A lost acknowledgement or a restart between any durable decision and the VMM action that follows it therefore never runs the captured state twice.

**A suspended workspace pins its configuration.** A resume needs the fingerprint the suspension was taken on. An upgrade may not retire that configuration while a suspended workspace depends on it, unless its owner resumes it first or explicitly chooses the fallback. The fallback is a new workspace from a current golden with the old workspace volume attached after the barrier. That volume is treated as crash-consistent: it is mounted only after its journal is recovered. It keeps the checkout and loses process memory and anything the old system disk held, the database's changes included. Rebuilding a golden never replaces a workspace's own snapshot.

**`fork` is a new workspace for the same owner with a copy of the source's checkout files.** It restores a current golden like any create, then copies the files of the source's checkout into the new workspace volume, file by file. It does not clone a block device, so deleted contents do not come along. It never copies memory, the system disk or a credential home.

The traversal is confined to the checkout through verified directory descriptors, following billet's rule that privileged operations go through a descriptor, never a pathname:
- it never follows a symlink, crosses a mount point or opens a special file;
- symlinks are copied as their link text;
- no source inode is shared with the destination;
- a path replaced while the copy runs cannot lead outside either root. Whatever the source's agent wrote into its checkout, a token in an untracked file included, comes along, which is why a fork belongs to the same owner only. The new workspace gets its own identity and its own repository token.

### Access

**Previews and attach need no new externally reachable listener on a node host.** The node initiates authenticated relay connections to the control plane and serves nothing outward. Attach is a terminal the guest agent serves over vsock, relayed node to control plane on a multiplexed stream beside the long-poll. A snapshot resets established vsock connections, so the agent reconnects after every restore and the relay treats a reset as normal. Previews are HTTPS requests to the control plane's edge, routed to a workspace and down the same stream to the fork's address inside its namespace.

**Every request is authorised for the workspace it names, not only authenticated.** Every preview request, attach stream, resume and MCP call authenticates the caller and authorises that caller against the destination organisation, workspace and current assignment before a relay opens. Missing, unknown or unreadable authorisation refuses. The edge consumes billet's session cookies and authorisation headers and never forwards them into a guest.

**A browser cannot be made to send a command.** A preview is application content its owner's browser runs, so being signed in and owning the workspace does not prove a request was intended:
- every cookie-authenticated mutation validates `Origin` and carries a session-bound CSRF token;
- a preview's session is scoped to its own workspace and cannot authorise any control-plane operation;
- an attach upgrade validates `Origin`;
- MCP clients authenticate with scoped bearer tokens, never ambient cookies, and those tokens are never handed to preview content.

Only the start of the sign-in flow is reachable without a session.

**Each workspace has its own origin, never reassigned.** A reused preview hostname would carry a browser's cookies, storage and service workers from one workspace into the next. So every workspace's preview hostnames are unique to it, on a domain that serves nothing but previews (on the Public Suffix List, so one workspace's origin cannot set cookies for another's). An application whose OAuth provider needs a redirect URL registered in advance points it at a fixed callback on billet's edge, which binds the callback to the user, the workspace and its assignment generation and forwards it to the workspace's own origin.

**The edge is a new public listener, and this record is what permits it.** Today billet's HTTP views stay behind the loopback or mTLS listeners (#28), and a browser can present neither. Accepting this record revises that rule for exactly these public, session-authenticated endpoints of the control plane:
- the sign-in flow;
- the preview edge;
- the OAuth callback broker;
- the MCP endpoint.

Everything else stays where it is. A team that wants all four private puts the control plane behind its own network. An MCP client that connects from its own cloud cannot then reach the MCP endpoint, which the team accepts or exposes alone.

**Workspaces are trusted-only and Firecracker-only.** The code in a workspace is written live by an agent and pulls dependencies from the internet, and a person attaches to it, so it needs the jailer's boundary and a trusted classification. A workspace is refused on a backend or tier that cannot give both. Attach is never offered on an untrusted tier, which is also #28's rule for SSH into a job.

### GitHub and model credentials

**Repository access is a separate GitHub App, and the App alone entitles no one.** A runner App holds only what registering runners needs, and billet refuses an installation holding a permission it did not request, in both directions. Workspaces need `contents: write` and `pull_requests: write`, so they use an App of their own per target, created and verified the same way, rather than widening the runner App. Before creating a workspace, and before minting or renewing its repository token, billet proves that the workspace's owner may write to that repository under the target's policy; the installation covering a repository says nothing about who may use it, and could-not-tell refuses. The same check applies to the golden a workspace starts from and the data it carries. Tokens are scoped to the one repository, short-lived, and delivered through MMDS, never argv.

**The agent's model credentials are the user's own, and billet never handles a sign-in credential.** A workspace runs the agent's binary unmodified. A user either authenticates with their own API key or cloud-provider credential (delivered through MMDS to a key helper), or signs in to their own subscription through the agent's own sign-in flow. For the latter, each user has a home directory on a shared filesystem with proved concurrent-client semantics (CephFS is the candidate, since the nodes already run Ceph), mounted into that user's workspaces only.
- **How it's used:** the agent writes its own login there, as several sessions on one laptop share one home, and billet never reads, copies or special-cases it.
- **Never a snapshot:** it is never an ordinary block filesystem mounted by two guests. Its storage is outside every recovery generation and every fork, and it is unmounted before a suspension is captured, so no client session or cached page for it is in a snapshot either.
- **Never in a golden:** it is never part of a golden, and the agent starts after a restore, so a golden's memory holds no credential.

**Agents reach billet through MCP.** The control plane's MCP endpoint offers tools to create, list, fork, inspect, suspend and remove the caller's own workspaces, each call authorised as above.

## Measure before accepting

Each is a probe on the reference Firecracker host, recorded here with the Firecracker version, the guest kernel and the date:
- **Speed and memory:** the time from a create request to a healthy stack, at 1, 5, 10 and 20 concurrent workspaces; memory per workspace and how much is shared.
- **Reseeding:**
  - the restore barrier holds every application process until the guest kernel reseeds from VMGenID with the kernel billet ships;
  - the reseed is observable to the agent;
  - each service in the reference golden either reinitialises its clone-sensitive state by a named procedure or is excluded.
- **MMDS and configuration:** the MMDS data store is empty after load and accepts a document before resume; metrics and logging configuration must be set again after load.
- **Network:** a workspace in its own namespace with the golden's tap name reaches the network through its veth and is reachable from the relay.
- **vsock:**
  - a vsock listener in the guest survives restore with its CID updated, as documented;
  - an established attach stream is closed by the restore;
  - the agent and the relay recover it.
- **The memory file in a jail:** how it reaches each jail without handing a hard link to a jailed uid (a read-only bind mount, or a copy, measured for cost).
- **Compatibility:** a golden with a mismatched or unreadable fingerprint is refused by billet before load, and a matching one restores.
- **Clock:** after restoring an aged golden and a long suspension, the guest's wall clock is current before the barrier releases, with whatever the chosen Firecracker clock mechanism requires of the host.
- **Disks:**
  - replacing a restored golden's placeholder slot with a workspace volume through the existing-drive update works, covering device sizing, cached reads and writes and repeated restores;
  - an old workspace volume mounts after journal recovery for the fallback.
- **Fork:**
  - a file-level copy of a checkout carries the files and no deleted contents;
  - a symlink pointing out of the checkout, a special file and a rename during the copy all fail to escape either root.
- **Shared home:**
  - two workspaces of one user share the home concurrently;
  - suspending one (agent stopped, writes drained, home unmounted) and resuming it while the other writes, and while the other rotates the login, loses nothing and rolls nothing back.
- **Suspension barrier:** with an application busy at the moment of capture, a resumed suspension runs none of that application's code before the clock is corrected and the home and network are back.
- **Suspend and resume:**
  - a suspend writes one consistent generation of memory, VM state and both disks;
  - **process crashes:** a crash of the billet node or controller process, injected at every suspend step while the host and VMM survive, leaves either the original VMM or a complete generation;
  - **host or storage failure:** an acknowledged suspension stays recoverable, and an interrupted, unacknowledged one may lose volatile state but is never reported complete;
  - **at most once:** a resume whose acknowledgement is lost, and a restart in the middle of a resume, never load one generation twice;
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
