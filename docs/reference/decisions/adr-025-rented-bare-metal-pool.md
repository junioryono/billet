# ADR-025: A pool of rented bare metal

Accepted, 2026-11-05, except where a section says it is owed. Issue #553.

## Context

A Firecracker node starts a job in seconds and serves every cache, and it needs KVM, so it needs bare metal. Per-job EC2 is the fallback when nothing else has room, and it is slower to start (47.6 to 58.7 s to the first step) and, until ADR-024, had fewer caches. An operator who wants capacity past the machines it owns can rent bare metal from two kinds of source:

- **A dedicated-server provider.** phoenixNAP's Bare Metal Cloud is the first one billet speaks to: it orders by API, bills by the hour or by a reservation of one to thirty-six months, and its servers carry desktop and server CPUs with local NVMe.
- **AWS bare-metal instances.** They boot in under a minute of the launch call, and bill by the second.

What was measured of both, with dates, is in the [bare-metal hosts record](../records/bare-metal-hosts.md). This record decides how billet orders, admits, grows, shrinks and polices such a pool.

## Decision

### Host sources behind one contract

A new package, `internal/hostsource`, holds the contract a source implements, beside `internal/provider` and below the scheduler: a source imports neither `server`, `node`, `nodeplane` nor `cmd`, and writes no ledger, which depguard holds (`hostsource`). A source answers five questions:

- **Order** a host of a named shape in a named location, carrying the payload that host must run on its first boot. It answers with the source's identifier for the host, or with `ErrNoStock` or `ErrQuota` when the source says so, which the scaler reports and never waits on.
- **Describe** a host: provisioning, running, failed, or gone, as the source reports it, or could-not-tell.
- **Gone**, the one answer that releases anything, in three values: proved gone, proved present, could not tell. "Not found" from a source that is eventually consistent is could-not-tell until the source's own documented settle time has passed and a second read agrees.
- **Release** a host, which the scaler asks only after the node's drain is proved and the host holds no lease and no custody.
- **Terms**: lead time, billing granularity and minimum term, per shape, which decide whether a source covers the floor or bursts.

The AWS source, `internal/hostsource/awsmetal`, launches, describes and terminates through the same EC2 client the `ec2` provider uses, with its retry ladder, its error codes and its redaction, so there is one EC2 client as there is one signer; a second copy is one that is wrong. The phoenixNAP source, `internal/hostsource/phoenixnap`, uses the BMC API with a client-credentials token. Each source's credentials are redacting types read from the environment or the identity store, never from argv, and never logged.

### Shape

About 4 GiB of memory per vCPU, so a 2 vCPU job can have 8 GiB, and local NVMe for guest disks and the read caches. On AWS that rules out the types that boot fastest and are cheapest per hour: `c7i.metal-24xl` and `c7g.metal` have 2 GiB per vCPU and no instance store. The candidates that fit are listed in the record; the first ones billet names are `m7gd.metal` for arm64 and `z1d.metal` for x86 when single-thread speed matters, `m6id.metal` when it does not. On phoenixNAP the `s1`, `s2` and `d` types carry NVMe (the oldest, `s0`, carry SATA SSD), and the Xeon E-2388G type (8 cores, 16 threads, 64 GiB) fits the ratio.

### Storage

A pool must shrink to its floor without losing a cache generation, so a generation's only copy is never on an elastic host.

- **On AWS metal**, Firecracker uses the `ebs-s3` store: the Actions, Go and Bazel caches as the objects ADR-024 keeps in S3, and a sticky disk or image store as a volume the node attaches to its own instance and hands its guest as a block device, where a per-job EC2 guest has the volume attached to its own instance. The instance's NVMe holds guest disks and read caches only. A volume attached to a metal instance and handed to a Firecracker guest is owed a probe before this ships: no billet node attaches a store volume to itself today.
- **On phoenixNAP**, the site's store is an always-on tier the pool never shrinks: a Ceph cluster on three hosts on a monthly reservation, with the elastic hosts as Ceph clients. Its replication and failure-domain layout are owed a measurement on real servers before this ships, and until then a phoenixNAP pool runs with its caches off.

### Growing and shrinking

The control plane runs the scaler, because it holds the demand, in a package of its own (`internal/pool`) assembled through `internal/app`. Its orders and hosts are ledger rows, so a restarted controller resumes an order rather than placing a second.

- **A floor, then bursts.** Each architecture keeps a floor of ready hosts from sources whose lead time is long and whose term is cheap; bursts come from sources that answer in minutes and bill by the hour or less.
- **Growth starts ahead of demand,** from the queue wait the listener records (#525), purchases the allocator refused, and, once #524 lands, predicted runtimes, which the scaler takes through a seam rather than waiting for.
- **Shrink only by drain.** A host leaves through billet's drain, and the source is asked to release it only on proof: the node's own ending says drained, and the ledger holds no lease and no custody on it. A timer may propose a drain, near the end of a billed hour for a source that bills by the hour; it never releases a host.
- **No spot or preemptible capacity** for a host that runs jobs. A spot host can be taken with two minutes' warning, which kills its running jobs, and a CI job is never the price of cheaper capacity. Per-job EC2 may still use spot, where a lost instance is one job retried.
- **Each source's limits are inputs, not surprises**: AWS's running-instance vCPU quota through `awsquota`, phoenixNAP's server quota through its own API (one server on a new account, measured), and stock per location as each source reports it.

### Joining without a person

Enrollment today is a fingerprint a person compares. A host the pool ordered proves it is that host with three things together, and the plane admits its one enrollment only when all three agree:

- **An order-bound join token**: minted per order, single use, living no longer than the source's lead time plus a margin, and recorded against the order. It travels in the host's first-boot payload (EC2 user data, phoenixNAP's cloud-init), so it is never in a command line.
- **The source's own word** that the host exists: on AWS, the instance identity document, signed by AWS and verified against AWS's published certificate, naming the instance the order launched in the account the deployment runs in; on phoenixNAP, the BMC API's record of the ordered server, whose public address must be the one the enrollment came from.
- **The order record**, still open, for that source and that host.

A token that arrives without the other two waits for a person, as today. The rules of `billet-identity-and-ca` and `billet-security` apply in full: the token is a request to enroll, never an identity, and the certificate a node then holds is what proves it afterwards.

### A host its provider polices

A hosting provider locks a server that scans, mines or sends packets at a high rate, and one job could cost a whole host that way. On a pool host every guest's traffic is NATed through the host with per-tap limits on packet rate, new connections per second and egress bandwidth, and outbound SMTP is refused. The limits are nftables rules the host role and the Firecracker provider install together; their defaults are owed a measurement against a real build's traffic before they ship, so they never throttle a job billet exists to run.

## Consequences

- An operator can rent capacity from two kinds of source behind one contract, and a third source is a package that implements it.
- A pool on phoenixNAP needs a monthly storage tier before its caches turn on, and a new account's one-server quota must be raised before it can grow.
- The metal source shares the `ec2` provider's EC2 client rather than growing its own.
- The ledger grows two tables, orders and hosts, through the migration rules of `billet-state`.

## What was rejected

- **Spot or preemptible pool hosts,** for the reason above.
- **Ceph across the elastic hosts' own NVMe.** Shrinking to the floor would have to move every placement group off a host before releasing it, which turns a drain of minutes into one of hours and makes a host the source reclaims a data-loss event.
- **A timer that removes idle hosts.** It is what releases a host whose job is still being torn down; release is on proof.
- **Admitting on the token alone.** A token copied from a boot payload is a bearer credential; the source's signed or recorded word that the host exists is what ties it to the order.
