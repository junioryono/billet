# What is proven

billet is pre-alpha. This page says what has actually run, by backend, against real GitHub and real infrastructure, with the date where there is one. Everything not listed here is either tested against fakes or not built. The acceptance records under [Records](records/index.md) are the evidence.

## Backends

| Backend | Proven | Not yet |
|---|---|---|
| Docker | jobs run end to end in the end-to-end suite against a scripted Actions service, on a real container runtime; crash recovery adopts a real container; `billet init` writes a config that launches an allowed job | never against a real organization; trials only |
| Firecracker | a real private-repository workflow completed on the reference host; jailer lifecycle (launch, find, list, destroy) on a real VMM; two nodes at one site reused one generation and a second site started cold and failed over under one label (2026-09-03); guest images pulled with signature verification, verified by boot and promoted; the compute barrier observed real compute whose lease was gone | interception on a release-bearing tier is still gated on the conformance workflow per candidate |
| Tart (macOS) | a real Xcode job ran to `** BUILD SUCCEEDED **` in a billet-launched guest and the VM was destroyed; a SIGKILLed node re-adopted its guest and the job finished green; SIGTERM drained; softnet isolation and the resolver proof measured in real macOS and Linux guests; a launchd-managed node ran a real job (macOS 26, tart 2.36.0, softnet 0.23.0) | a fork pull request end to end; billet-built guest images with `@verified`; a cache on a Mac |
| Tart (Linux arm64) | a real job built a Docker image asserting `aarch64` and used a PostgreSQL service container in a native arm64 guest (Docker 29.7.2) | as above |
| EC2 | three cold private-repository jobs on 2026-08-18: JIT registration, Docker builds, a service container, runtime toolchain installation, every instance destroyed; the same unchanged label completed first on local capacity and then on EC2 after local capacity was withdrawn; a live FIS spot interruption; AMI build and boot verification (2026-08-28); cold start 47.6 to 58.7 seconds to the first job step | warm pools (refused by design); untrusted fork work end to end |
| CodeBuild | real Linux builds on demand (31 builds, 31 distinct hosts, 2026-09-02); a real Xcode job on a reserved `MAC_ARM` fleet; the holder-gone scenario re-run against real CodeBuild; the registration sweep, the fleet-starvation behaviour and the account queue cap measured; three macOS jobs through one label across an owned Mac and the fleet (2026-09-03) | untrusted work on a shared node (refused by design; a node named by both trust classes is refused at load and at registration); the live fork-PR network-isolation step remains a human step |

## Storage and state

| Area | Proven | Not yet |
|---|---|---|
| Ceph RBD | the reference cluster built and measured (ADR-003); real Ceph growth becomes Firecracker guest capacity; same-site sharing and cross-site isolation on real hosts | |
| EBS + S3 | first real publication on 2026-09-03 found and fixed two refusals every fake accepted; warm hits clone from snapshots | the live warm-workflow, failed-job and power-cut proofs for sticky disks |
| Actions cache interception | the live conformance matrix (host and container save/restore, BuildKit through two drivers, kill switch, pinned and embedded clients, poisoned clients) | |
| SQLite ledger | every invariant tested; migrations frozen since the first release | |
| PostgreSQL ledger | the suite runs against PostgreSQL 18 in CI; the allocator re-run on it; the identity-only backup and restore rehearsed with the real package | |
| Active-passive controllers | the fence and the election under test; promotion order asserted structurally | a promotion measured under a real partition |

## Operations

| Area | Proven | Not yet |
|---|---|---|
| Backup and restore | rehearsed on every pull request with the real Debian package and service account; a restored control plane serves the fleet that trusted the old one | `billet local up` after a restore with a real App, on a real host |
| Recover | tested end to end in-process, every abandon and resume state | on a real host |
| Rollouts | the coordinator's ordering, cohorts and failure budget under test; the upgrade transaction's ordering asserted structurally; every corrupt, unsigned, expired or incompatible manifest refused; two packaged hosts under real systemd reached `billet rollout start` and its refusal found that no binary through v0.6.0 verifies a published manifest ([Host rehearsals](records/host-rehearsals.md), 2026-09-04) | `cmd/billet`'s real host actions (stop a service, replace a binary, migrate) on a real host, which the rollout rehearsal reaches once a release the deployed policy accepts exists; whether GitHub redelivers an unacknowledged message to a new session |
| Drain and the compute barrier | a real job, a real stop, a real stray container observed and blocked | |
| Host lifecycle | `billet local up`/`down` against real systemd and the real package (`make systemd-lifecycle`); launchd facts measured on macOS 26 | |
| CA rotation | every state of the file machine under test; enrollment across a rotation in the end-to-end suite | a rotation across a real fleet |
| Node enrollment | the whole flow in the end-to-end suite over real mTLS; certless connections refused at the listener | |

## Not built

Observability and a dashboard; SSH into a job; a Windows port (needs a `flock` equivalent); a Terraform provider (deferred by [ADR-004](decisions/adr-004-terraform-provider.md)); billet-built macOS guest images; an Apple Silicon cache.

## Compatibility caveats

- There is no supported way to point `actions/cache` at your own server ([actions/toolkit#1051](https://github.com/actions/toolkit/issues/1051), open since April 2022), which is why transparent caching is interception and stays off by default.
- The Actions Cache v2 protocol is reverse-engineered; GitHub has never published its `.proto` files ([actions/toolkit#1931](https://github.com/actions/toolkit/issues/1931)). GitHub can change it without notice, so a cache failure is a miss or a warning, never a failed job, and a candidate image is not promoted until it passes the live conformance suite.
- Apple Silicon support requires [Tart](https://tart.run), licensed FSL-1.1-ALv2 (each release converts to Apache-2.0 after two years). billet treats it as an external dependency you install, like Docker or Ceph; billet itself is Apache-2.0 throughout.
- A Firecracker compute host needs Ceph on the nodes' own disks. A cache kept in local storage pins every repository to the host that built it.
- Every CodeBuild job inherits CodeBuild's 36-hour build ceiling and 8-hour queued ceiling, and a reserved fleet is a 24-hour minimum commitment.
