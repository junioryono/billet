# ADR-024: Attaching to a job, and holding a failed job's VM

Accepted, 2026-11-04. Issue #533.

## Context

A job that fails on billet's hardware leaves its operator a log and nothing else: the VM is destroyed as soon as GitHub reports the result, which is usually the moment somebody wants to look around it. Getting into a VM by network would need something listening: on the guest, which then needs an address someone can reach, or on the node, which ADR-017 keeps outbound-only so that no firewall has to be opened for a node.

## Decision

**A terminal reaches a VM over a stream the node dials back.** An operator on the control plane host runs `billet jobs attach`, which asks the control plane over its operator socket. The plane sends the node a command naming the lease, a stream and a single-use token. The node connects to the guest over Firecracker's vsock, through the VMM's socket in the jail, then dials the plane on its own mTLS identity and upgrades that request into the stream. Nothing on the node or in the guest listens on a network, and nothing new is opened in any firewall.

**The node and the plane carry bytes and read none.** The frames are spoken between the operator's command and the guest's server, in `internal/relay`. Neither side in the middle decodes one, so nothing a job writes can reach a billet log, an error or a decision on the way through. A stream is one connection for one terminal, bounded per node on the plane. #511 can multiplex these later without changing what they carry.

**Attaching is the operator's, on a tier that says so.** `tiers[].debug.attach` is off by default. Config refuses it on an untrusted tier, because a person attaching to a VM an outside contributor's job controls would be typing into a machine that job owns, and on any backend but Firecracker. The guest's server is a unit of its own, started only where the node's metadata asks, so every other guest has nothing listening and no vsock device.

**The job's log says how.** On a tier that may be attached to, a job-started hook prints the command and the hold policy into the job's log. It stands in front of a hook already named, and that hook's status stays the answer, so a job does not start failing because it can be attached to.

**A hold delays a destroy and never authorises one.** `tiers[].debug.hold_on_failure` keeps a failed job's VM for at most an hour. The completion is still what authorises the destroy, and when the hold ends the destroy runs the path it always would have, with its proofs. The lease stays in its ordinary phase, charged and renewed, so a hold costs capacity and touches no capacity rule. The runner's registration is withdrawn at once, since GitHub must not route to a runner whose job has finished. A cancelled job or a passed job is not held.

**A hold has one bound, written once.** The row in `lease_holds` fixes when it ends the first time it is written, so a retry, a restart or an edited setting cannot move it. It ends at that bound, when a session's shell has exited and the last session has closed, on `billet jobs release`, or when `billet drain --cut-holds` ends it. A connection that merely goes ends nothing: a dropped link, a node restarting or a controller stopping say nothing of whether anyone has finished looking, and the VM they were looking at must not go with it. The command says the shell exited, since nothing between it and the guest reads the frames; with two sessions open, one shell exiting and the other's link dropping ends the hold, since somebody finished and nobody is left. A ledger that cannot answer means no hold: keeping capacity on a guess is the costlier mistake, and what it loses is one VM someone might have looked at.

**Every destroy of a held VM is covered or deliberate.** A completion journals its pool runner as retiring before anything else, and every reconcile retires what is journaled, so the retirement asks about the hold too, and waits for the completion's own destroy when the hold ends: that one is bound to the node holding the VM and carries the proof a release needs, and a retirement's goes by request id. A shutdown leaves a held entry alone as it leaves a running job. A force operation does not ask: it is an operator ending work, which is what a hold gives way to.

**A drain waits for a hold and says so.** A drain waits for compute, and a held VM is compute. It says how many it is waiting for and until when, and `--cut-holds` ends them and says it did, and goes on ending any a listener opened before it heard of the seal. A tier draining opens no new hold: a job that fails after the seal is not what anyone is waiting for.

## Rejected

- **A listener in the guest or on the node.** It needs an address someone can reach, a firewall opened on every site, and an authentication story of its own, for a feature a dial back gives without any of them. The triggering user's GitHub-key SSH attach would revisit this and is a separate issue.
- **The plane decoding the frames**, to log sessions or to enforce anything in them. The job is root in its VM and can replace the server, so nothing read there would be evidence, and the bytes would have reached billet's processes.
- **Holding the lease in a phase of its own.** It would add a state to the machine for something the running phase already does, and every reader of a phase would have to learn it.
- **Extending a hold while someone is attached.** A person can keep a capacity slot for as long as they like that way. An hour is the operator's few minutes, and a longer look is a job re-run.
- **Running the server as a child of the guest's agent.** A unit's children die with it, and the agent exits when the runner does, so a held VM would serve nothing.

## Consequences

- Only a Linux Firecracker tier can be attached to, and only with an image built after this; an older image has nothing listening, which the attempt says.
- What passes through a session is the guest's. It reaches the operator's terminal as any remote machine's would, and billet labels it no further.
- A held VM keeps its capacity from other jobs for up to the bound. `billet status` lists what is held and for how long.
- A controller restart ends the sessions, whose connections it held, and not the holds, whose rows outlive it. The node's registration adopts what survived and renews it, as any job's, so exactly one party renews a lease across it. A lease quarantined by a controller away for longer than a lease's life is still kept: a node asks about what it adopts, and a runner retiring under a hold that stands answers busy, so the node keeps the VM and asks again until the hold ends and the answer is retired.
