# ADR-017: Workspaces, long-lived environments forked from a warm snapshot

Proposed, 2026-10-16. Nothing here is implemented, and nothing here is measured yet: every fact below about Firecracker is a reading of its documentation, which a house rule says is not enough, so each one is listed under Measure before this is accepted. Relates to issue #28, whose SSH item this record's attach transport is meant to serve.

## Context

A coding agent now opens many changes in parallel, and each one is only verifiable with the whole application running: its services, its database with data in it, and the URLs a browser or a phone reaches it on. On one developer's machine that is a fixed number of local stacks, each minutes to boot and gigabytes of memory, and the machine runs out before the agent does. billet already runs compute behind a real boundary on its own hardware: jailed Firecracker microVMs, Ceph copy-on-write root disks, an outbound-only node wire, and secrets delivered through MMDS rather than argv. What it runs is one shape, a GitHub job: a lease bought when a runner starts, a VM that cold-boots, and compute released on proof it is gone. A workspace is a second shape on the same machinery, and most of this record is where the two differ.

## Decision

**A workspace is a VM for one task.** It holds an ordinary git checkout of one repository on a fresh branch, the application's whole stack already running, and the agent process started with the task. It is reached by a person (attach, previews, an editor) and by the agent's own tools. It pushes a branch and opens a pull request through GitHub like any other client. billet adds no version-control system.

**A workspace is a resource of its own, not a lease with a longer TTL.** It is owned by a user and an organisation, it lives until it is removed, and it is never charged to a tier's escrow or advertised to GitHub. It is still bought before compute exists and released only on proof the compute is gone, through the same placement, capacity vector and custody rules a lease follows. A workspace's compute is a new command kind on the node wire (launch from a snapshot, fork, suspend, destroy), so a node that does not speak it is never sent one.

**Time warns; it does not delete.** An idle workspace is suspended: its VM is snapshotted to disk and stopped, and its capacity comes back when the node proves both that the snapshot is durable and that the VMM is gone. Deleting a workspace's disk and snapshot is only ever an explicit request from its owner or an operator. This is the existing rule that a timer never authorises a teardown, applied to the one resource whose loss is someone's unpushed work.

**Fork from a warm golden snapshot, once per golden.** A golden is built by booting the application's image, waiting until its stack is healthy, and snapshotting it. The VM that produced the golden is terminated and never resumed. Each workspace is a restore of that snapshot into its own jail, with its own Ceph clone of the disk, so a fork pays for the pages it touches rather than for a boot. A golden is valid only on the host configuration it was taken on (CPU model, host kernel, Firecracker version), so it is built per node and keyed to that configuration, and a node whose configuration changes rebuilds rather than restores.

**A fork is unique before anything trusts it.** Firecracker's documentation calls resuming one state more than once insecure: identifiers, random seeds, the entropy pool and any token in memory are replicated into every fork. So:
- the golden holds no credential, key or token of any kind;
- the guest kernel must reseed from the VMGenID notification Firecracker injects on restore;
- the guest agent regenerates per-fork identity (machine id, host keys, hostname) before the workspace is reported ready.

TLS for previews terminates at the edge, not inside the guest, so the golden carries no private key that a fork would share.

**Per-fork state is delivered after load and before resume.** The MMDS data store does not survive a snapshot, so each fork is loaded paused, given its MMDS document (its identity, its repository token, its agent credential source, its task), and only then resumed. Metrics and logging configuration are not in a snapshot either and are configured again on every load.

**A fork's network is its own namespace.** A restored guest keeps the address and tap name the golden had, so every fork runs in a network namespace of its own with the same tap name inside it, reached from the host through a veth pair and NAT, following Firecracker's network-for-clones recommendation. Egress is an allowlist per workspace, on top of the deny rules the host already applies to guests.

**Nothing on a node accepts a connection.** Attach and previews ride the connection the node already holds to the control plane:
- **Attach** is a terminal the guest agent serves over vsock, relayed node to control plane on a multiplexed stream beside the long-poll. A snapshot resets vsock, so a fork's agent reconnects after every restore, and the relay treats a reset as normal.
- **Previews** are HTTPS requests to an edge in the control plane, routed by hostname to a workspace and down the same stream to the fork's address.

**The edge is a new listener, and it is authenticated.** It is the one exception to keeping HTTP views behind the loopback or mTLS listeners. Every request carries a billet session established by signing in, checked before anything is proxied; there is no unauthenticated path to a workspace. Preview hostnames come from a fixed per-organisation slot pool on a domain that serves nothing else, so OAuth redirect URLs can be registered once and preview content never shares a site with the sign-in that protects it.

**Workspaces are trusted-only and Firecracker-only.** A workspace runs code its owner's agent writes, with network access, and a person attaches to it. That needs a real boundary (the jailer) and the trusted classification, so a workspace is refused on a tier or backend that cannot give both. Attach is never offered on an untrusted tier, which is also #28's rule for SSH into a job.

**Repository access is a separate GitHub App.** A runner App holds only what registering runners needs, and billet refuses an installation holding a permission it did not request, in both directions. A workspace needs `contents: write` and `pull_requests: write` on the repositories it works on. Widening the runner App would break that refusal for every deployment, so workspaces use an App of their own per target, created and verified the same way. Its installation tokens are scoped to the one repository and delivered through MMDS, never argv.

**The agent's model credentials are the user's own, and billet never handles a sign-in credential.** A workspace runs the agent's binary unmodified. A user authenticates with their own API key or cloud-provider credential (delivered like any other secret, through MMDS to a key helper), or signs in to their own subscription through the agent's own sign-in flow. For the last, billet persists a per-user home volume mounted into that user's workspaces only. The agent writes its own login there, and billet never reads, copies or special-cases it. A home volume is never part of a golden or of any snapshot shared across users, and the agent starts after a fork is restored, so no credential sits in snapshot memory.

**Agents reach billet through MCP.** The control plane serves an MCP endpoint whose tools create, list, fork, inspect and remove the caller's own workspaces. A client that connects from outside a self-hosted network (a hosted assistant reaching the endpoint from its own cloud) needs that endpoint reachable from the internet. Only that endpoint is exposed, authenticated by the same sign-in as the edge, and each call acts as the signed-in user over their own workspaces alone.

## Measure before accepting

Each is a probe on the reference Firecracker host, recorded here with the version and the date:
- the time from a fork request to a healthy stack, at 1, 5, 10 and 20 concurrent forks;
- memory per fork, and how much is shared;
- that a restored guest's RNG reseeds on VMGenID with the guest kernel billet ships;
- that the MMDS data store is empty after load and accepts a document before resume;
- that a fork in its own namespace with the golden's tap name reaches the network and is reachable through its veth;
- that a vsock listener in the guest survives restore with its CID updated, as documented;
- how the memory file reaches each jail without handing a hard link to a jailed uid (a read-only bind mount, or a copy, measured for cost);
- that a golden restored on a host with a different CPU model or kernel is refused rather than run.

## Rejected

**A cold boot per workspace.** It is what a job does today and costs minutes, which is the problem being solved.

**Containers as the boundary.** A workspace's pool is trusted, but the code inside it is written live by an agent and pulls dependencies from the internet, and a person attaches to it. billet's threat model treats a docker container as no boundary at all, so a workspace host needs the jailer even for trusted work.

**Widening the runner App.** It would give every runner deployment a code-write grant it never uses, and remove the two-way permission check that catches an App pointed at the wrong job.

**An inbound port on a node, or a third-party tunnel in the product.** The first is the one property billet's node wire was built to avoid. The second makes a workspace depend on an account the operator may not have. A team that wants its edge private can put the control plane behind its own VPN.

**Deleting idle workspaces on a timer.** Time warns; a suspended workspace costs disk, not compute, and its owner decides when it goes.

**A new version-control system.** The pain a workspace removes is running the application, not storing its history, and every tool an agent and a reviewer use already speaks git.

**Capturing the agent's subscription token to provision workspaces.** It would make billet the holder of a credential it may not hold; the user signs in through the agent's own flow instead.
