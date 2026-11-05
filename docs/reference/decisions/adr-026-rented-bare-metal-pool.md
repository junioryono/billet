# ADR-026: A pool of rented bare metal

Accepted, 2026-11-05, except where a section says what it still owes. Issue #553.

## Context

A Firecracker node starts a job in seconds and serves every cache, and it needs KVM, so it needs bare metal. Per-job EC2 is the fallback when nothing else has room, and it is slower to start (47.6 to 58.7 s to the first step). An operator who wants capacity past the machines it owns can rent bare metal from two kinds of source:

- **A dedicated-server provider.** phoenixNAP's Bare Metal Cloud is the first one billet speaks to: it orders by API, and bills by the hour or by a reservation of one, twelve, twenty-four or thirty-six months.
- **AWS bare-metal instances**, billed by the second. Four launches on 2026-11-05 reached their first script 21 to 47 s after the call.

What was measured of both, with dates, is in the [bare-metal hosts record](../records/bare-metal-hosts.md). This record decides how billet orders, admits, grows, shrinks and polices such a pool.

## Decision

### Host sources behind one contract

A new package, `internal/hostsource`, holds the contract a source implements, beside `internal/provider` and below the scheduler: a source imports neither `server`, `node`, `nodeplane` nor `cmd`, and writes no ledger, which depguard holds (`hostsource`). A source answers:

- **Order** a host of a named shape in a named location, carrying the payload its first boot runs and the pool's order identifier, which the source records on the host (an EC2 `ClientToken` and tag, a phoenixNAP hostname). It answers with the source's identifier for the host, with `ErrNoStock` or `ErrQuota`, which the scaler reports and never waits on, or with `ErrRefused`, a refusal an operator acts on (phoenixNAP's 403 to an API client that may not order, measured). Any other error is could-not-tell.
- **Find** the host an order placed, by that identifier, so an order whose answer was lost is read back rather than placed again.
- **Describe** a host: provisioning, running, failed or releasing, as the source reports it. An unread or unknown state is could-not-tell, which is the zero value, never provisioning.
- **Gone**, the answer that frees a host's row, in three values with could-not-tell as the zero. It says gone only on the source's own affirmative terminal answer for that exact host, read after a release (EC2's `terminated`, which `DescribeInstances` keeps showing for a while), never on not-found or on elapsed time: AWS documents that a not-found can coexist with a running instance. A source that has no terminal answer, as phoenixNAP has none measured yet, never says gone, and its host leaves the ledger by an operator's word.
- **Release** a host, only with a proof (below).
- **Terms**: lead time, billing granularity and minimum term, per shape, which decide whether a source covers the floor or bursts, and whether an order is idempotent, which decides whether a lost answer may be ordered again. A shape that is preemptible is refused.

The AWS source, `internal/hostsource/awsmetal`, launches, describes and terminates through the same EC2 client the `ec2` provider uses, with its retry ladder, its error codes, its `ClientToken` idempotency and its redaction, so there is one EC2 client as there is one signer; where that client lives so both may import it is the AWS source's change to make. The phoenixNAP source, `internal/hostsource/phoenixnap`, uses the BMC API with a client-credentials token. Each source's credentials are redacting types read from the environment or the identity store, never from argv, and never logged.

### Shape

About 4 GiB of memory per vCPU, so a 2 vCPU job can have 8 GiB, and local NVMe for guest disks and the read caches. On AWS that rules out `c7i.metal-24xl` and `c7g.metal`, which have 2 GiB per vCPU and no instance store. Of the types that fit, `m7gd.metal` (arm64, 64 vCPU) and `z1d.metal` (x86, 48 vCPU, which AWS rates at a sustained 4.0 GHz) were launched; `m6id.metal`, `m8gd.metal-24xl` and `m8id.metal-48xl` fit on paper and are owed a launch. Which x86 type a pool names is owed the in-guest single-thread measurement under full load that #553 asks for: on the host, a `z1d.metal` thread lost 48% of its speed with every other cpu busy, and that run did not separate its causes. On phoenixNAP the `s1`, `s2` and `d` types carry NVMe; the 6-core E-2276G and the 8-core E-2388G (64 GiB each, 12 and 16 threads) both fit the ratio, and the E-2388G is preferred for its threads.

### Storage

A pool must shrink to its floor without losing a cache generation, so a generation's only copy is never on an elastic host.

- **On AWS metal**, Firecracker uses the `ebs-s3` store: the Actions, Go and Bazel caches as the objects [ADR-024](adr-024-caches-off-firecracker.md) keeps in S3, and a sticky disk or image store as a volume the node attaches to its own instance and hands its guest as a block device. The instance's NVMe holds guest disks and read caches only. This changes a site rule: today a Firecracker node needs `node.ceph` (`internal/config/ceph.go`), and the config change that accepts `node.ebs_s3` for a Firecracker node on AWS metal is owed with the probe that proves the path, since no billet node attaches a store volume to itself today.
- **On phoenixNAP**, the site's store is an always-on tier the pool never shrinks: a Ceph cluster on three hosts on a monthly reservation, with the elastic hosts as Ceph clients. Its replication and failure-domain layout are owed a measurement on real servers, and until then a phoenixNAP pool runs with its caches off.
- **Failure domains.** An EBS volume lives in one availability zone, so an AWS pool host is ordered in the zone of the site whose volumes it attaches, and a site spread across zones is a site per zone. A phoenixNAP pool orders in its storage tier's location.

### Orders that survive a lost answer

The scaler's orders and hosts are ledger rows. An order's row, with its identifier, is committed before the source is called, and the call carries that identifier as the source's idempotency key or marker. A restarted or uncertain scaler reads the order back with Find before it does anything else. When Find cannot say (a source error) the order is held as could-not-tell and not placed again. Find listing no host lets the scaler order once more, under the same identifier, only on a source whose Terms say it is idempotent, as EC2's `ClientToken` makes it; on any other source, phoenixNAP among them, an order whose answer was lost waits for a person, because an order still in flight can land after a list that did not show it.

### Growing and shrinking

The control plane runs the scaler, because it holds the demand, in a package of its own (`internal/pool`) assembled through `internal/app`.

- **A floor, then bursts.** Each architecture keeps a floor of ready hosts that the operator configures, from sources whose lead time is long and whose term is cheap; bursts come from sources that answer in minutes and bill by the hour or less. A floor sized from measured usage is a recommendation billet can make once the job statistics of #535 exist; it is not computed here.
- **Growth starts ahead of demand,** from the queue wait the listener records (#525), purchases the allocator refused, and, once #524 lands, predicted runtimes, which the scaler takes through a seam rather than waiting for.
- **Shrink only by drain, and release only on proof.** A timer may propose a drain, near the end of a billed hour for a source that bills by the hour; it never releases a host. The proof is ledger facts, read and sealed in one transaction so no lease can be placed between the check and the release: the host is sealed against placement, and it holds no lease, no quarantine and no custody, and its node's last inventory reported no compute. The seal is one way: the transaction moves the host to `releasing`, which nothing moves back, and a resume or a placement contends for the same row and is refused once a release has sealed it, so a proof never outlives its seal. The node's own drained ending is not on the wire today, so it is not part of the proof; if it is added, it joins it.
- **A host that never joined** (its first boot failed, or its node never enrolled) has no drain to prove. It is released on a different proof: its order is sealed against enrollment and its join token revoked in one transaction, and the ledger records that no enrollment was ever admitted for it, so it never held anything a job could have used.
- **A host that dies holding work** stays charged and in quarantine, with its leases and custody, until the existing force paths (`billet leases release --force`, `billet force-destroy`) settle it on an operator's word, never on a timer.
- **No spot or preemptible capacity for pool hosts.** A spot host can be taken with two minutes' warning, which ends every job running on it, and GitHub does not requeue a job whose runner vanished, so each would need a rerun by hand. Per-job EC2 may still use spot, where an interruption costs one job's rerun rather than a host's worth.
- **Each source's limits are inputs, not surprises**: AWS's running-instance vCPU quota through `awsquota`, phoenixNAP's server quota and stock through its own API (one server on a new account, measured), each read before an order.

### Joining without a person

Enrollment today is a key fingerprint a person compares. A pool host is admitted without one only when the plane can read that fingerprint from the host through a channel the source authenticates for that exact host; a token and an address are not enough, because both can be copied or reassigned.

- **On AWS**, the host generates its node key on first boot and writes the key's fingerprint to its serial console. The plane reads the console of the exact instance the order launched (`GetConsoleOutput` with `Latest`, which returned the bare-metal probes' console lines within two minutes of launch), which only that instance can write and only the account can read, and admits an enrollment only when its key's fingerprint is the one there, the instance identity document names that instance, account, region and launch, and the enrollment's source address is that instance's private address. The host's launch keeps the `ec2` provider's metadata settings (IMDSv2 required, hop limit 1), and the host's NAT drops guest traffic to the metadata addresses, so no guest can read the host's token, identity document or role.
- **On phoenixNAP**, no source-authenticated channel from the host is known, so a pool host there waits for a person's fingerprint comparison, as a node does today, with the order-bound token and the API's record of the server's address shown beside it. Automated admission there is owed a channel.
- **The admission is one transaction**: it checks the order is open at the generation the enrollment names, spends the order's single-use token (`spendJoinTokenTx` is already transaction-scoped), records the admitted key fingerprint and certificate serial on the host's row, and names the node from the order, never from the request. Cancelling or releasing an order fences it in the same way, so an admission and a cancellation cannot both succeed. A retried enrollment with the same key finds the identity already admitted. A token presented with a fingerprint, document or address that does not match is not spent; the order is flagged for a person and admits nothing until one acts.
- **Release revokes first.** The certificate recorded on the host's row is revoked before the source is asked to take the host back.

The rules of `billet-identity-and-ca` and `billet-security` apply in full: the token is a request to enroll, never an identity, and the certificate a node then holds is what proves it afterwards.

### What a returned host keeps

A phoenixNAP server goes to its next tenant with its disks. The node's key, its guest disks and its read caches live on a volume encrypted with a key the host generates at boot and never writes down, so a released host's disks hold nothing readable once its power goes. billet relies on no source's wiping, so an AWS pool host uses the same encryption, and the two sources keep one layout.

### A host its provider polices

A hosting provider locks a server that scans, mines or sends packets at a high rate, and one job could cost a whole host that way. On a pool host every guest's traffic is NATed through the host with per-tap limits on packet rate, new connections per second and egress bandwidth, outbound SMTP is refused, and the metadata addresses are dropped. These are nftables rules the host role and the Firecracker provider install, one rule set with the per-tier egress policy of #727 rather than a second; their defaults are owed a measurement against a real build's traffic before they ship, so they never throttle a job billet exists to run.

## Consequences

- An operator can rent capacity from two kinds of source behind one contract, and a third source is a package that implements it.
- A pool on phoenixNAP needs a monthly storage tier before its caches turn on, a person at each enrollment until a source-authenticated channel exists, and a raised server quota before it can grow past one host.
- A Firecracker node on AWS metal needs the site rule that requires Ceph relaxed for `ebs-s3`, and a store volume attached to a metal instance and handed to a guest proved, before its caches turn on.
- The metal source shares the `ec2` provider's EC2 client rather than growing its own.
- The ledger grows two tables, orders and hosts, through the migration rules of `billet-state`.

## What was rejected

- **Spot or preemptible pool hosts,** for the reason above.
- **Ceph across the elastic hosts' own NVMe.** Shrinking to the floor would have to move every placement group off a host before releasing it, which turns a drain of minutes into one of hours and makes a host the source reclaims a data-loss event.
- **A timer that removes idle hosts, or that turns not-found into gone.** Release is on proof, and gone is the source's affirmative answer.
- **Admitting on a token, an identity document or an address.** Each can be copied or reassigned, and none names the key that enrolls.
