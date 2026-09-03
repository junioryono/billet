# Site acceptance

The site boundary — two nodes in one declared site reuse one cache generation, and a node at a different site cannot — was proved on real hosts on **2026-09-03**. This records the topology, the commands, the evidence and what it does not prove, so the acceptance can be repeated.

## Topology

One deployment (identity `86565b95…`), three nodes at two sites. The home nodes and the control plane ran a build of `f1faffc3` (v0.3.27-0.20260902060226-36c885437c87), the tree this acceptance started from. The aws node first ran that tree plus the two fixes below as uncommitted edits — which is what found them — and then, once they were committed, the aws legs were **re-run from the committed tree `ce21705f`** (`billet v0.3.27-0.20260903015826-068851350181`); the seed and warm-reuse evidence quoted for aws below is from that re-run.

| node | where | provider | site | store |
|---|---|---|---|---|
| `home-a` | the reference host (`ubuntu-01`, EPYC 7763), a second `billet node` process beside the production one | firecracker | `home` | Ceph, pools `accept-images` / `accept-cache` |
| `home-b` | a KVM virtual machine on `ubuntu-01` (Ubuntu 24.04, kernel 6.8, 16 vCPU, nested KVM), mapping the same pools over the network as a second kernel client | firecracker | `home` | the same pools |
| `aws-1` | `t3.small` in the CI account (`810711872940`, us-west-2a), which also runs the control plane | ec2 | `aws` | EBS snapshots and an S3 prefix (`billet-20-cache-810711872940/accept20`) |

**`home-b` is a virtual machine on the same chassis as `home-a`, not a second physical server**, because the office has one Linux host and a Mac cannot map RBD or run Firecracker. What the VM shares with a second physical node is everything the site boundary is about — its own kernel, its own RBD client identity, its own jailer, its own bridges and its own node identity and certificate, reaching the mons and OSDs over the network rather than through a local socket. What it does not share is a second power supply and a second NIC; nothing here measures those.

The control plane listens on `0.0.0.0:7717` under the node-wire CA with `node_tls_hosts: [52.42.222.206, 10.3.2.50]`; the home nodes dial the Elastic IP and `aws-1` dials the private address. Bundles were issued with `billet ca issue` on the control plane and copied to each host.

The isolation from the production deployment on the same office host is by pools and identity: `client.billet-accept` holds `profile rbd` on `accept-images` and `accept-cache` only, the golden image was copied into `accept-images` with `rbd export --export-format 2 | rbd import` plus its 41 `image-meta` keys (the `@verified` resolution lives in those keys, and `rbd deep cp` does not carry them), and the acceptance node on `ubuntu-01` listens on `172.31.0.1:7728` with `image_verify_port: 7729`, two ports opened on the production `billet_guard` chain for the duration.

The probe is one workflow, `site-cache.yml` in the private consumer repository: `seed` pulls `alpine` and tags it `billet-site-cache:<tag>`; `reuse` asks whether that tag is in the runner's Docker image store and fails the run when the answer differs from `expect`. The image store is the per-site Docker image-store generation the guest requests from its node's cache API before the runner starts, keyed `<deployment>/<site>/docker-images/<arch>`.

## Two nodes, one site

Run `33702304224`, with `home-b` stopped so the job could only land on `home-a`:

```
01:06:34Z  home-a: launched a microVM runner=billet-cd761f9e…  image=ubuntu-2404-x64@g20260901032644
           guest:  /var/lib/docker on /dev/vdb ext4 97.9G, driver=overlay2
           guest:  seeded billet-site-cache:t1  (image store after: alpine b66e0ce64844, billet-site-cache:t1)
01:06:54Z  home-a: settling a Docker image store from a completed job result=succeeded
01:07:07Z  home-a: destroyed a microVM
           accept-cache: cache-g-1788397617-5057459ce9cf526713eaee4f  100 GiB  parent cache-v-1788397600-…@fadb3984-…
```

Then `home-b` was started and `home-a` stopped, and run `33702380035` asked the same tier for the same tag with `expect: warm`:

```
01:12:08Z  home-b: launched a microVM runner=billet-83cc0b33…
           guest:  /var/lib/docker on /dev/vdb ext4 97.9G
           guest:  image store BEFORE any step: alpine b66e0ce64844, billet-site-cache:t1 b66e0ce64844
           guest:  result=warm — billet-site-cache:t1 is warm on billet-guest
01:12:37Z  home-b: settling a Docker image store from a completed job result=succeeded
```

So the second host mounted the exact generation the first host published — same image id `b66e0ce64844`, on a volume cloned from `cache-g-1788397617-…` — and no second generation was published, because the reuse job changed nothing.

**Two things the run showed that are not about the cache.** First, `systemctl stop` on `home-a` (a SIGTERM, which is a drain) exited the process in under a second with nothing running — and the control plane went on placing on it: the reuse job was assigned to `home-a` at 01:07:36, sat until the plane forgot the silent node at 01:12:04 (`forgetting a node that stopped polling … silent_for=4m34s`, then `could not start the compute for an assigned job; handing the capacity back`), and only then was placed on `home-b`. A node that exits cleanly does not tell the control plane it is leaving, so withdrawing a host costs one `staleAfter` window before placement stops choosing it. Second, `home-b`'s first teardown failed once with `remove /sys/fs/cgroup/firecracker-v1.16.1/…: device or resource busy`; the plane kept the lease held and retried, and the retry succeeded (`billet leases`: nothing held) — which is the custody rule doing what it says.

## Two sites

The `aws` site is a genuinely different place — a different failure domain, a different provider, and a store of its own kind — and it had never published a cache generation on real AWS before this. The Actions-cache interception is Ceph-only, so what an EC2 site shares between jobs is the Docker image-store generation: an EBS volume the node creates or clones, hot-attaches to the instance before the runner starts, and snapshots after a clean success, with the fenced per-key state in S3.

**The home tag is cold at aws.** Run `33704997398`, `reuse t1 expect: cold` on `billet-20-aws`:

```
01:46:29Z  aws-1: launched instance i-0438d35e9edde2765 c7i.xlarge image=ami-0ca372f538fcbe5b6
           guest ip-10-3-2-203: /var/lib/docker on /dev/nvme1n1 ext4 97.9G   (a fresh 100 GiB volume — the key had no generation)
           guest: result=cold — billet-site-cache:t1 is cold on ip-10-3-2-203
           S3: accept20/owners/…/state/….json written (the key's first state object)
```

The key is `<deployment>/aws/docker-images/amd64`; home's generation lives under `<deployment>/home/docker-images/amd64` in the Ceph pool, and the aws node holds no Ceph identity, no `rbd`, and no route to the cluster. Nothing at aws can name it.

**An aws generation is reused warm at aws, from a different instance.** Two defects had to be fixed first, both found by this run and neither reachable by a fake (below); the first warm hit (`t3`, runs `33705811488` and `33707462397`) ran on the uncommitted fixes. From the committed tree `ce21705f`, run `33709515126` seeded `t5` on `ip-10-3-2-99`; the settlement snapshotted the volume (`snap-08b44016467d17ae4`, tagged `sh.billet.owner`, `sh.billet.cache-owner=…/aws` and the request token `c5103ac9…`) at 02:59:02Z, and the S3 state's pointer moved to it at fence 3 by 02:59:46Z — fifty-five seconds from the job finishing to the generation being current. Run `33709710968`, `reuse t5 expect: warm`:

```
02:59:56Z  aws-1: launched instance i-0288803c710732024
           guest ip-10-3-2-239: /var/lib/docker on /dev/nvme1n1 ext4 97.9G   (cloned from snap-08b44016467d17ae4)
           guest: image store before any step: alpine b66e0ce64844, billet-site-cache:t5 b66e0ce64844
           guest: result=warm — billet-site-cache:t5 is warm on ip-10-3-2-239
```

A different EC2 instance (whether AWS placed it on a different physical host is not something an instance id says), the same image id — the ebs-s3 store's first warm hits on real AWS. Both volumes were gone from the account afterwards; the snapshot is the generation.

**The aws tag is cold at home.** Run `33707216894`, `reuse t3 expect: cold` on `billet-20-home`, landed on a microVM at home with its own generation mounted (`t1` present, `t3` absent) — `billet-site-cache:t3 is cold on billet-guest`. The Ceph pool has no `t3`; the aws snapshot is not addressable from a Ceph node, which has no AWS credentials and no ebs-s3 store.

### Two defects that only a real AWS could show

Both were in code that had never run against the real service, and both had a fake pinning the wrong belief.

**EC2's `CreateSnapshot` has no `ClientToken`.** `CreateVolume` and `RunInstances` take one; `CreateSnapshot` answers `UnknownParameter` (HTTP 400). Every settlement therefore failed at the snapshot, the image store was discarded, and every job at the site ran cold — with the node's own test asserting the parameter was sent. The token now travels as a tag (`sh.billet.snapshot-token`) and idempotency is billet's: an attempt looks for a snapshot carrying its token before creating one, and again after a failed call, because AWS may have acted before the answer was lost.

**`CreateVolume` from a snapshot authorizes the snapshot too.** The policy `billet init iam` generates granted `ec2:CreateVolume` on `*` conditioned on the new volume's request tag, and its own comment said the parent snapshot was not authorized. It is: the clone answered `UnauthorizedOperation`, the job ran cold (run `33707063227`), and a second statement scoped to `arn:aws:ec2:*::snapshot/*` and conditioned on the snapshot's owner tag is what let the next clone mount the generation warm. The committed terraform renderings of the node policy carry it now.

**And one the fix uncovered**: the failed snapshot left the key's writer lease standing for its full 15 minutes, during which the next settlement waited silently, retrying the acquisition every 100ms against S3. Fixed since: a publication that fails after acquiring the writer now gives the lease back (`Store.ReleaseWriter`, on both backends), and the wait for a live writer is an announced back-off bounded by the holder's expiry rather than a silent 100ms poll.

## Fallback: the first site withdrawn

`billet-20-any` names no site and lists `providers: [firecracker, ec2]`, so home fills first and aws is the overflow. Both home nodes were stopped, and run `33707655235` asked that label for the home tag with `expect: cold`:

```
02:26:38Z  dispatched; home-a and home-b stopped at 02:26:2xZ
02:30:24Z  plane: forgetting a node that stopped polling node=home-a silent_for=3m54s   (and home-b)
           plane: could not start the compute for an assigned job; handing the capacity back tier=billet-20-any
02:30:29Z  aws-1: launched instance i-006953e7722932792 for the assignment
02:31:40Z  plane: received a completed job … result=canceled     (GitHub withdrew the assignment as unacquired)
02:31:46Z  aws-1: launched instance i-03d7d3b5064fe3fdd for the requeued job
02:33:28Z  job running on ip-10-3-2-245: result=cold — billet-site-cache:t1 is cold on ip-10-3-2-245
```

So the same label ran at the second site, cold, without pretending its cache was warm — and it took seven minutes to get there, four of them the plane still believing the stopped home nodes were live and the rest GitHub's pickup deadline cancelling the assignment that finally reached aws and requeuing it. The fallback is an availability mechanism; nothing in it is fast.

Then both home nodes were started again. The FIRST job after that (run `33708137612`) still went to aws and ran cold: the tier's escrow had been made while home was absent, and an assignment lands on the slot that was escrowed for it, not on the best host at the moment it arrives. The next one (run `33708440963`, `expect: warm`) went home, on a microVM, and found `t1` warm:

```
02:39:42Z  guest billet-guest (Linux 6.1.155): image store before any step: alpine b66e0ce64844, billet-site-cache:t1
           result=warm — billet-site-cache:t1 is warm on billet-guest
```

## `billet status` across both sites

`billet status` on the control plane names every node with its site and what it last reported running; `billet leases` showed nothing held once each teardown had completed. The `rbd` listing of `accept-cache` and the S3 state object are what say which generation is current at each site, and neither can be read from the other site's node.

## What this does not prove

- `home-b` is a VM on `home-a`'s chassis. Power, NIC and disk failure domains are not separated; only the software boundary is.
- The Actions-cache interception (`intercept: true`) was not exercised; the generation reused here is the Docker image store, which is the same per-site generation mechanism through the same fenced index.
- Eviction was not exercised at either site. Every generation this run published is still there.
- The aws store's failure recovery — a settlement interrupted between the snapshot and the pointer write, a lost conditional PUT — was not exercised. The one failure it did see (the standing writer lease) is a liveness cost, not a correctness one.
