# ADR-001: Where the control plane runs, and what stores its state

**Status:** accepted, Aug 2026
**Decision:** a single small EC2 instance with SQLite on EBS, recovered by **EC2 auto-recovery**.
**Rejected for now:** DynamoDB, Fargate, Aurora, Lambda.

## The requirement, and how it changed

The original plan (§0.4) treated hosting as a **cost** question and made co-location with a node the
default. That was wrong, and the operator said so plainly:

> "What if one of my servers is down at my house? That's the one that is the controller. Then none of
> my actions work, and I can't have that happening."

A controller in a house has the availability of that house — power, ISP, hardware, a cable. So the
control plane belongs somewhere with better odds than that, and cost is not the driver. Stated
tolerance is **$20–50/month**; the thing to avoid is the *expensive shape* (ECS-on-EC2 plus RDS), not
spend as such.

## The fact that resized the problem

**GitHub queues a job for 24 hours when no matching runner is available.** From its own self-hosted
runner docs: a job labelled for a runner type with none available "does not immediately fail at the
time of queueing. Instead, the job will remain queued until the 24 hour timeout period expires."

So a dead controller does not fail anybody's CI. It delays it. The requirement is **"recovers in
minutes"**, not "never goes down" — and that is a far cheaper thing to buy. Any design here that
purchases high availability is buying something the failure mode does not require.

## Options, with numbers

All us-east-1, Linux, on-demand, checked Aug 2026. Rates move; re-check before acting.

| Option | ~$/mo | Survives instance death | Multi-AZ | Code change |
|---|---|---|---|---|
| **t4g.small + SQLite on EBS** | **~$13** | yes, reattach volume | no | **none** |
| t4g.micro + SQLite on EBS | ~$7 | yes | no | none |
| t4g.nano + SQLite on EBS | ~$4 | yes | no | none |
| Fargate ARM 0.25 vCPU + DynamoDB | ~$7 | yes | yes | **state rewrite** |
| Lambda + DynamoDB | ~$0 | yes | yes | rewrite **and** webhook mode |
| Aurora Serverless v2 (0.5 ACU min) | ~$43 | yes | yes | Postgres port |

Inputs: `t4g.nano` $0.0042/hr (~$3.07/mo), `t4g.micro` $0.0084/hr, `t4g.small` ~$0.0168/hr; gp3 EBS
$0.08/GB-month; Fargate ARM $0.03238/vCPU-hr and $0.00356/GB-hr, minimum task 0.25 vCPU / 0.5 GB.

**Fargate at minimum size is ~$7.21/month** — `(0.25 × $0.03238 + 0.5 × $0.00356) × 730` — which is
cheaper than a `t4g.small` and about the same as a `t4g.micro`. An earlier draft claimed Fargate was
not cheaper than a small EC2; that contradicted this very table and is corrected here.

The defensible claim is narrower: **Fargate plus the state rewrite it forces saves nothing** against a
correctly-sized `t4g.micro`, and a burstable instance amortises idle in a way a fixed task size does
not. The rewrite, not the compute, is what decides this.

## Why not DynamoDB

It was considered seriously, and it is **technically feasible** — this is not a dismissal.

- Fencing epochs map cleanly onto conditional writes (`ConditionExpression: epoch = :expected`).
- The allocator's central invariant is that the **headroom check and the insert are ONE transaction**
  — moving the check outside it measured **28 grants against a ceiling of 4** under concurrency. That
  maps onto `TransactWriteItems`: a conditional update of a usage-counter item plus the lease `Put`,
  atomically. The counter is a hot item, which at tens of writes per minute does not matter.
- The reaper's expiry scan is the awkward one, and "a GSI on `expires_at`" — which an earlier draft
  said — **is not sufficient**. DynamoDB queries require partition-key equality, so `expires_at` as a
  GSI key does not answer "every lease expiring before now". It needs a constant or sharded partition
  key with `expires_at` as the sort key, or a low-volume scan. Worth stating so an implementation
  does not start from a range query DynamoDB cannot express.

Three reasons it is not worth doing yet:

1. **It saves nothing.** ~$7/mo on Fargate + DynamoDB against ~$4–13 for EC2 + EBS. The rewrite buys
   durability, not budget.
2. **It costs the riskiest rewrite available.** `internal/state` is SQLite-specific (migrations, WAL,
   `synchronous=FULL`, a process lock) and `internal/alloc` is SQL transactions throughout. Those two
   packages are the most heavily reviewed in the repository and their invariants took many review
   rounds to settle. Rewriting them before billet has run a single job is the wrong order.
3. **It breaks an invariant without replacing it.** "Exactly one authoritative writer" is currently
   enforced by a `flock` on the state directory. On DynamoDB that becomes leader election — real,
   solvable, and new machinery in exactly the place where new machinery has been most expensive.

**Revisit when you genuinely want more than one controller.** That is the thing SQLite cannot do at
any price, and it is the only reason worth paying the rewrite for.

Two DynamoDB facts worth keeping regardless:

- **The free tier is provisioned-mode only** — 25 WCU / 25 RCU, never expiring, but on-demand tables
  get *no* free requests. billet's write volume is a handful per job, so provisioned mode would be
  genuinely free.
- **Transactional writes cost 2× WRUs**, and every GSI multiplies write cost.

## The decision

**A single `t4g.micro` or `t4g.small` running `billet server`, SQLite on a gp3 EBS volume, recovered
by EC2 auto-recovery.** Combined with the 24-hour queue grace, an outage costs minutes of queueing
and no jobs.

**EC2 auto-recovery, NOT an Auto Scaling group.** This distinction is the whole recovery story and an
earlier draft of this ADR got it wrong. Auto-recovery restarts *the same instance*, on new underlying
hardware, keeping its instance id, its EBS volumes and their attachments — SQLite comes back to the
data it left. An ASG does not do that: replacement launches a *new* instance from a launch template,
which has no knowledge of the terminated instance's data volume, so the controller would come up with
an empty or missing disk. "The volume reattaches" is true of auto-recovery and false of an ASG.

If an ASG is wanted anyway (for the health-check behaviour), the volume has to be handled explicitly:
an AZ-pinned volume with deletion disabled, plus lifecycle-hook or EventBridge automation that
detaches, attaches and mounts it, and **fails closed** rather than starting on an empty disk. That is
real machinery and should not be hand-waved. Auto-recovery covers less (it handles host failure, not
a wedged process) but needs none of it.

`t4g.micro` at ~$7 is probably sufficient — the controller should idle in tens of MB. Size after
measuring rather than before.

### What this does NOT give you, stated plainly

- **It is not HA.** One instance, one AZ. If the AZ fails you are down until it returns.
- **EBS is single-AZ.** The volume does not follow you to another one.
- **Never put SQLite on EFS or any NFS.** That is a hard rule in this project and it corrupts. If a
  design ever seems to need shared filesystem state, that is the signal to reach for DynamoDB or
  Postgres, not for NFS.

Moving from a house to one AWS instance is a large availability improvement. It is not solved, and
this document exists partly so nobody later reads "it's on AWS now" as though it were.

## Consequences

- **No inbound FROM GITHUB.** The scale-set API is outbound long-poll, so hosting on AWS does not
  require webhook mode — those two questions got conflated early and are unrelated.

  This is NOT "no inbound at all", and an earlier draft said so carelessly. Nodes dial out to the
  controller, which means the controller *accepts* those connections: it needs a reachable listen
  address and a path for node-originated mTLS — restricted security-group ingress, a VPN or overlay
  (Tailscale/Headscale/WireGuard), or a reverse tunnel. Taken literally, "no inbound" would mean the
  EPYC box could never connect and nothing would ever run. The listen address also cannot stay on
  loopback as it is in the dev config.
- mTLS bootstrap for nodes now matters more: they are no longer on the same LAN. Enrollment,
  authorization, rotation, revocation and CA-key backup move onto the critical path (task #16).
- Backups: SQLite's backup API to S3, with a **rehearsed** restore. An untested backup is not one.
- Terraform for this belongs with P11; do not block the manual version on it.
