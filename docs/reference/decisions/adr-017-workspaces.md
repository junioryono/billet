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

**A workspace starts from a warm golden snapshot.** A golden is built by booting the application's image, bringing its stack to a healthy state, quiescing it (below), and snapshotting it. The VM that produced the golden is terminated and never resumed. Each workspace is a restore of that snapshot into its own jail with its own Ceph clone of the disk, so it pays for the pages it touches rather than for a boot.

**A golden is pinned to a compatibility fingerprint, and billet refuses a mismatch before loading.** Firecracker requires a restore on the software and hardware configuration a snapshot was taken on; that is a requirement on the integrator, not a refusal Firecracker promises to make. So every golden records a fingerprint (CPU model and configuration, host kernel, Firecracker build, guest kernel), and billet compares it with the host's before invoking a load; a mismatch, or a fingerprint it cannot read, refuses. A host whose fingerprint changes builds new goldens.

**Nothing in a warmed stack is trusted to be unique, so nothing runs until it is made so.** Firecracker's documentation calls resuming one state more than once insecure: identifiers, seeds, the entropy pool and any token in memory are replicated into every restore, VMGenID reseeds only the guest kernel, and userspace can run before the reseed lands. So:
- **Before capture:** every application process is stopped at a restore barrier the guest agent holds, the guest's network is down, and the golden holds no credential, key or token of any kind.
- **After restore:**
  - the barrier holds until the agent has proved the kernel reseeded from the VMGenID notification;
  - the agent then regenerates per-fork identity (machine id, host keys, hostname, the Docker daemon's identity);
  - only then do services start.
- **Services:** a service with clone-sensitive state (a database's system identifier, a TLS server or client session, a persistent random seed, a cached token) is reinitialised through a procedure measured for that service, or is not in the golden at all and starts fresh after restore.
- **No guess:** a golden whose services include one billet cannot classify is refused at publication.

TLS for previews terminates at the edge, so no fork serves with a private key a golden held.

**Per-fork state is delivered after load and before resume.** The MMDS data store does not survive a snapshot, so a fork is loaded paused, given its MMDS document (its identity, its repository token, its model-credential source, its task), and only then resumed. Metrics and logging configuration are not in a snapshot either and are configured on every load.

**A fork's network is its own namespace.** A restored guest keeps the address and tap name the golden had, so every workspace runs in a network namespace of its own with that tap name inside it, reached from the host through a veth pair and NAT, following Firecracker's network-for-clones recommendation. Egress is an allowlist per workspace, on top of the deny rules the host already applies to guests.

### Suspension and forks of a running workspace

**A suspended workspace is private, and it is never a source for anyone else.** A running workspace's memory holds its owner's repository token and model credentials, so a suspension snapshot is confidential user state, bound to one deployment, one owner and one workspace. It is never a golden and never the source of a fork. It lives in storage only its node and its owner's restore can reach, and a resume is authorised the same way as attach.

**Durability is proved before the VMM stops.** A suspend pauses the VM, writes a recovery generation (memory, VM state and every disk the workspace uses, as one consistent set), and durably records that the generation is complete. Only then is the deployment-owned VMM stopped, through the existing pid proof and SIGKILL path. Capacity is released only on positive proof the VMM is gone. If the generation fails, or billet cannot tell whether it is complete, the VMM keeps running and keeps its capacity.

**A suspended workspace pins its configuration.** A resume needs the fingerprint the suspension was taken on. An upgrade may not retire that configuration while a suspended workspace depends on it, unless its owner resumes it first or explicitly chooses the fallback: a fresh restore from a current golden with the workspace's disks attached, which keeps the checkout and the database on disk and loses only process memory. Rebuilding a golden never replaces a workspace's own snapshot.

**`fork` never clones live memory.** Forking a workspace takes a consistent snapshot of its disks, then restores a current golden with those disk clones attached, so the new workspace has the source's checkout and data and none of its memory. It gets its own identity, its own repository token and the owner's credentials afresh.

### Access

**Previews and attach need no new externally reachable listener on a node host.** The node initiates authenticated relay connections to the control plane and serves nothing outward. Attach is a terminal the guest agent serves over vsock, relayed node to control plane on a multiplexed stream beside the long-poll. A snapshot resets established vsock connections, so the agent reconnects after every restore and the relay treats a reset as normal. Previews are HTTPS requests to the control plane's edge, routed to a workspace and down the same stream to the fork's address inside its namespace.

**Every request is authorised for the workspace it names, not only authenticated.** Every preview request, attach stream, resume and MCP call authenticates the caller and authorises that caller against the destination organisation, workspace and current assignment before a relay opens. Missing, unknown or unreadable authorisation refuses. The edge consumes billet's session cookies and authorisation headers and never forwards them into a guest.

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

**The agent's model credentials are the user's own, and billet never handles a sign-in credential.** A workspace runs the agent's binary unmodified. A user either authenticates with their own API key or cloud-provider credential (delivered through MMDS to a key helper), or signs in to their own subscription through the agent's own sign-in flow. For the latter, billet persists a per-user home volume mounted into that user's workspaces only. The agent writes its own login there, and billet never reads, copies or special-cases it. The home volume is never part of a golden, and the agent starts after a restore, so a golden's memory holds no credential.

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
- **Suspend and resume:**
  - a suspend writes one consistent generation of memory, VM state and disks;
  - a crash injected at each step of suspend leaves either the running VM or a complete generation, never neither;
  - resume restores it.

## Rejected

**A cold boot per workspace.** It is what a job does today and costs minutes, which is the problem being solved.

**Containers as the boundary.** billet's threat model treats a docker container as no boundary at all, and a workspace is code an agent writes live, with dependencies from the internet and a person attached.

**Widening the runner App.** It would give every runner deployment a code-write grant it never uses, and remove the two-way permission check that catches an App pointed at the wrong job.

**An inbound port on a node, or a third-party tunnel in the product.** The first is the one property billet's node wire was built to avoid. The second makes a workspace depend on an account the operator may not have.

**Fixed preview hostnames reused across workspaces.** They would make OAuth registration simpler and carry one workspace's browser state into the next.

**Suspending or deleting on a timer.** Time warns; its owner decides.

**Forking by cloning a running workspace's memory.** It would duplicate the owner's live credentials and every identity the source holds.

**A new version-control system.** The pain a workspace removes is running the application, not storing its history, and every tool an agent and a reviewer use already speaks git.

**Capturing the agent's subscription token to provision workspaces.** It would make billet the holder of a credential it may not hold; the user signs in through the agent's own flow instead.
