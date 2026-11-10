# Bare-metal hosts

What was measured of the two kinds of rented bare metal [ADR-030](../decisions/adr-030-rented-bare-metal-pool.md) builds a pool from, and when. Every AWS number is from `us-west-2`, every phoenixNAP number from its API on the day named, except the 2026-11-09 server, which was ordered in the portal and measured over SSH.

## AWS bare-metal instances, 2026-11-05

**How.** Each instance was launched from Ubuntu 24.04's own image into a public subnet, with `--instance-initiated-shutdown-behavior terminate` and a user-data script that wrote what it found to the serial console, which `get-console-output` read back, and then shut the instance down. Launch is the instance's `LaunchTime` as `describe-instances` reported it; boot is when the user-data script began, with the kernel's own uptime beside it. The second pair also timed an 8 GiB sequential write and read of the first instance-store disk with `dd` and `O_DIRECT`, and the single-thread speed of `openssl speed sha256` on 16 KiB blocks for 5 s, alone and then with a busy loop on every other cpu. Single-thread speed was measured on the host, not inside a guest, and with no cpu pinned; the guest's is owed with the first Firecracker node on one of these. The instance-store sizes, the sustained clock and the SMT of each type are what `describe-instance-types` says, not what the probe read.

| Type | Launch to boot | Kernel uptime at boot | vCPU, memory | CPU | KVM | Instance store | On demand |
|---|---|---|---|---|---|---|---|
| `c7i.metal-24xl` | 29 s | 12.65 s | 96, 188 GiB | Xeon Platinum 8488C | yes | none | $4.284/h |
| `c7g.metal` | 21 s | 10.52 s | 64, 126 GiB | Neoverse-V1 | yes | none | $2.320/h |
| `z1d.metal` | 47 s | 14.51 s | 48, 378 GiB | Xeon Platinum 8151 | yes | 2 × 900 GB | $4.464/h |
| `m7gd.metal` | 23 s | 10.61 s | 64, 251 GiB | Neoverse-V1 | yes | 2 × 1900 GB | $3.417/h |

| Type | 8 GiB write | 8 GiB read | sha256, one thread alone | with every other cpu busy |
|---|---|---|---|---|
| `z1d.metal` | 8.09 s, 1.0 GiB/s | 3.73 s, 2.1 GiB/s | 516 MB/s | 269 MB/s |
| `m7gd.metal` | 4.37 s, 1.8 GiB/s | 3.00 s, 2.7 GiB/s | 1,629 MB/s | 1,630 MB/s |

- **These four reached their first script 21 to 47 s after launch.** The kernel had been up for 10 to 15 s of that; what took the rest (AWS's allocation, power-on, firmware) was not separated. Four launches set no bound, and this is not the time from an order to a registered node, which is owed.
- **The first two types do not fit the pool's shape.** `c7i.metal-24xl` and `c7g.metal` have 2 GiB per vCPU and no instance store. `describe-instance-types` filtered to bare metal with instance storage and at least 4 GiB per vCPU names, among others, `m7gd.metal` (64 arm64 vCPU, $3.417/h), `m8gd.metal-24xl` (96 arm64, $5.535/h), `z1d.metal` (48 x86, which AWS rates at a sustained 4.0 GHz, $4.464/h), `m6id.metal` (128 x86, $7.594/h) and `m8id.metal-48xl` (192 x86, $12.530/h).
- **On these two hosts, a busy host halved the x86 thread and left the arm64 one alone.** On `z1d.metal` a single thread lost 48% when the other 47 cpus were busy; one of those 47 is its own core's second hyperthread, and the all-core clock and the shared memory system changed too, and this run did not separate them, so the cause is not known. `m7gd.metal`, which AWS lists without SMT, lost nothing. The two architectures' sha256 numbers are not comparable with each other: Neoverse-V1 has SHA-2 instructions and this Xeon generation does not, and no cpu flags were read to confirm either.
- **The quota.** Running on-demand standard instances, the quota every type above draws from, was 256 vCPU on this account after a raise; before it, the quota was below the 96 one `c7i.metal-24xl` needs on its own.
- **The quota refusal.** A launch of one `m8id.metal-96xl` (384 vCPU, past the 256 quota) at 13:36 was refused synchronously with `VcpuLimitExceeded` ("You have requested more vCPU capacity than your current vCPU limit of 256 allows for the instance bucket that the specified instance type belongs to"), and a lookup by its Name tag found no instance, so nothing started. The AWS source reads that code as a quota answer. Every other launch was accepted on the first call; a no-stock answer was not met, and the source reads the capacity codes the `ec2` provider already treats as one.

## phoenixNAP Bare Metal Cloud, 2026-11-05

**How.** An API client created in the portal, its credentials read from files and given to `curl` through a netrc file, and a token from the client-credentials grant at `auth.phoenixnap.com`, whose scope came back as `bmc.read bmc`.

- **The catalogue.** `GET /billing/v1/products?productCategory=SERVER` listed 456 hourly plans across six locations (Ashburn, Chicago, Phoenix, Seattle, Amsterdam, Singapore), each type also offered on a reservation of one, twelve, twenty-four or thirty-six months. The cheapest hourly type was $0.08/h (a 4-core Xeon E3 with SATA SSD); the cheapest with NVMe $0.16/h (`s1.c1.small`, 6-core, 12-thread Xeon E-2276G, 64 GiB, 1 TB NVMe), which already fits the pool's 4 GiB per thread; the 8-core, 16-thread Xeon E-2388G with 64 GiB and 1 TB NVMe (`s2.c2.small`) is $0.31/h.
- **Stock is per location and small.** `GET /billing/v1/product-availability?productCategory=SERVER&showOnlyMinQuantityAvailable=true` reported `s2.c2.small` available in five locations, nine at most (Chicago); the larger types showed one to seven each, mostly in Phoenix and Ashburn. A pool on this source must order where the stock is, and read it before every order.
- **A new account may hold one server.** `GET /bmc/v1/quotas` answered `Servers` with a limit of 1, `Public IPs` 15, `Public IPv6s` 1 and `Storage Networks` 3000 GB. A pool cannot grow past one host here until that is raised.
- **The order was refused.** `POST /bmc/v1/servers` for one hourly `s1.c1.small` in Ashburn answered `403` with `The request failed since this resource cannot be accessed by the provided credentials.`, although the same token read the catalogue, the stock, the quotas and the (empty) server list. Whether the API client lacks permission to create, or the account cannot yet order, was not established; the boot time, the guest's speed and the NVMe of a phoenixNAP server are owed until an order is accepted.

## The console and the identity document, 2026-11-06

One t3.micro (a Nitro instance; `GetConsoleOutput --latest` needs one) in us-west-2, launched from Ubuntu 24.04 with IMDSv2 required and a hop limit of 1, whose user data read its identity document and signatures over IMDSv2, wrote them to `/dev/console`, then opened `/dev/console` itself and wrote one marker line every 15 s for ten minutes, and shut itself down. A reader called `aws ec2 get-console-output --latest` every 10 s of sleep (about every 22 to 44 s with the call's own time) and noted when each line first appeared. The instance terminated itself; nothing was left behind.

- **Every line reached the console.** All 40 lines appeared, each by the reader's next call after it was written: at most 48 s after its writing, with the reader's interval the larger part of that. The first line was readable 70 s after launch. `Latest` returned the newest 65536 bytes.
- **A line written by a process that opens `/dev/console` itself arrives as one written to the boot's own standard output does.** Both kinds of line were read back.
- **The document's `signature` is RSA-SHA256 under the region's RSA certificate.** `openssl dgst -sha256 -verify` against the RSA certificate AWS lists for us-west-2 verified it; as SHA-1, or under the region's RSA-2048 or DSA certificates, it did not. billet's verifier (`internal/awsidentity`, `TestARealDocumentVerifies` given that document) verified it too. The signature is 128 bytes, 172 characters of base64; the document 475 bytes.
- **The document matches `DescribeInstances`.** Its `pendingTime` was the instance's `LaunchTime` (`2026-11-06T14:26:45Z`) and its `privateIp` the instance's private address.
- **AWS's certificates differ by region.** Its page of certificates for identity document signatures listed an RSA certificate for 39 regions, one shared by the two China regions and every other distinct; us-west-2's was issued 2024-04-29 and lapses 2029-04-28.

## The pool's IAM, asked of EC2, 2026-11-08

**How.** `billet init iam --pool --deployment <a test id>` rendered the control plane's grant for a pool in the acceptance VPC (its subnet and security group, `z1d.metal` from the x64 acceptance image and `m7gd.metal` from the arm64 one). A throwaway role held that document and nothing else, and each request below was sent under it with `--dry-run`, which answers the authorization question and starts nothing. A `t4g.nano` carrying this deployment's pool tags, launched as the administrator, stood in for a pool host, and the stopped acceptance host for an instance without them. The role and the instance were deleted at the end.

| Request | EC2's answer |
|---|---|
| billet's own pool launch, arm64 and x64: IMDSv2 required, hop limit 1, the three pool tags on the instance and its volumes | allowed |
| the same with another deployment's pool owner | denied |
| the same with the job instances' owner tag (`sh.billet.owner`) added | denied |
| the pool owner's key spelled `sh.billet.Pool-Owner` | denied |
| no tags | denied |
| an image the pool does not name | denied |
| a subnet and group the pool does not name | denied |
| another image's snapshot in a block-device mapping | denied |
| an existing network interface in another subnet | denied |
| an existing network interface in the pool's subnet with a group the pool does not name | denied |
| the launched image's own backing snapshot in a mapping | allowed |
| terminate, and read the console of, the instance carrying the pool owner | allowed |
| terminate, and read the console of, the instance without it | denied |
| describe instances | allowed |
| stop the pool host, or tag it after its launch | denied |

The grant names every resource a pool launch touches, so a snapshot in a mapping is refused by omission, without the Deny the node's launch carries, and holds the network interface a launch creates or names to the pool's subnet (`ec2:Subnet`); an existing interface in that subnet carrying a group the pool does not name was refused as well, so EC2 evaluates an existing interface's groups too. A customer-managed key's grant (`--kms-key-arn`) is the cache's shape, which the 2026-09-04 simulation measured, and a dry run does not exercise it. The one snapshot it lets through is the launched image's own, which the 2026-09-04 dry runs found EC2 does not evaluate, and which exposes nothing the pool does not already boot.

## The pool on replay, 2026-11-08

**How.** `internal/replay` drove each trace through the real control plane at compressed time, as it does every replay, with a pool beside the fleet (`PoolFleet`, `internal/replay/pool_test.go`): the real scaler (`internal/pool`) ordered from a source the harness played, each order became a node of the stack a lead time later in simulated time, and every job ran on a host the pool ordered. GitHub's protocol was GitHub.com's, direct assignment, since a tier's shortage is read from the work GitHub assigned it. The source and the join are played: a host's order is admitted as the source's word would admit it, without a key or a certificate, because the replay's wire is the loopback one. A release is proved by its node's clearance over five minutes of wall time, so a replay records the pool asking for a release and not the release; the real runs prove that. The scaler stepped every simulated minute, every tier polled at each step as a live listener's long poll answers, each host joined exactly its lead time after its order (the tests assert it), and the floor was joined before the first job, as a running deployment keeps it. Pool hosts count as room only from their join, so utilization is not understated by hosts not yet ordered. One x64 tier, 16-vcpu pool hosts, a lead time of three minutes from order to joined node.

| Trace | Pool | Queue wait p50 | p90 | p99 |
|---|---|---|---|---|
| `Burst`, 24 four-vcpu jobs in ten seconds, a few minutes each | floor 0, max 3 | 9 min 24 s | 12 min 21 s | 13 min 44 s |
| the same | floor 3, max 3 | 30 s | 6 min 33 s | 8 min 14 s |
| `WideShapeMix`, 40 jobs of 2 to 32 vcpu at the load four hosts carry | floor 1, max 4, 64-vcpu hosts | 1 min 43 s | 3 min 28 s | 6 min 14 s |

- **The pool grows one host at a time while one is on its way** (ADR-030), so a burst arriving at a floor of none waits out a lead time for every host it needs: each host was ordered at the step the one before joined at, three minutes apart, and the burst's p99 was five and a half minutes longer than with the same three hosts kept as the floor. The tail at the floor of three is the second wave of jobs behind the twelve slots, not the pool.
- **Every job ran, on no more hosts than the maximum**, and the wide-shape mix filled all four. Two architectures, each with a floor, replayed the same twice: the same orders under the same identifiers, and every job on the same host, since the scaler now walks its architectures in one order.
- **Once quiet for `idle_after` (ten minutes), the pool asked to release its hosts one a step down to its floor**: all three from a floor of none, three of four from a floor of one, and all five of a pool filled by twenty hour-long jobs that ended together.

## A build's outbound traffic, for the limits a provider polices, 2026-11-08

**How.** One build's traffic on the shared Linux test host, inside a privileged `ubuntu:24.04` container so that nothing of the host's own was counted or changed: an nftables output chain in the container's namespace counted every packet leaving it (loopback aside), its bytes, and every new connection, and a sampler read the counters once a second while each phase ran. A guest's traffic is the same traffic; Firecracker carries it, it does not shape it.

| Phase | Peak packets/s out | Peak new connections/s | Peak bytes/s out | Total new connections |
|---|---|---|---|---|
| `apt-get install` git, nodejs, npm, golang, build-essential | 1018 | 3 | 54001 | 10 |
| `git clone --depth 1` of TypeScript | 226 | 0 | 11951 | 0 |
| `npm install` of express, webpack, typescript, eslint, jest, react, next, babel | 1734 | 14 | 285610 | 17 |
| `go mod download` of billet's module graph | 5505 | 18 | 441560 | 27 |
| `pip install` of numpy, pandas, scipy, requests, boto3, django, flask, sqlalchemy, pytest, black | 1518 | 6 | 92445 | 6 |

A build's outbound load is small and bursty: its heaviest second was 5505 packets and 441 KB, from a download fetching modules in parallel, and it opened at most 18 connections in a second. The host role's limits (`billet_guest_refuse_smtp`, `billet_guest_new_connections_per_second`, `billet_guest_packets_per_second`, `billet_guest_egress_bytes_per_second`) cap what every guest on a host sends off it through the host's forwarding, together, and are off unless set. For a pool host the settings this gives, against 32 guests each at its measured peak in the same second (576 connection attempts, 176160 packets, 14129920 bytes), are SMTP refused, 1000 connection attempts a second (1.7 times), 400000 packets a second (2.3 times) and 125000000 bytes a second (a gigabit, 8.8 times, below the port a provider's server has).

What they do not cover, said plainly. They are a budget for the whole host, so a flooding job is cut off with its neighbours rather than alone; a cap per guest needs ADR-028's per-interface chain and is not built. The connection limit counts every packet conntrack calls new, a first SYN and its retransmissions and each packet of a one-way UDP flow, not each flow once, so one guest sending a one-way UDP stream spends it for every guest. The packet limit counts what the host forwards before a guest's TCP segmentation offload is undone: a flood of small packets, the scanner's kind a provider polices, is counted packet for packet, while a bulk transfer's large buffers count once each and are bounded by the byte limit, so the packets on the uplink can exceed the packet setting while a guest sends something large. And a guest's requests to the host's own services, its DNS and the registry mirrors, end at the host, whose own upstream traffic leaves through its output and is not counted here: a provider sees it, and these limits do not cap it.

## The pool on AWS metal, end to end, 2026-11-08 and 2026-11-09

**How.** One control plane ran `billet server` from a build of main with this branch's role, on the shared Linux test host. The test GitHub App served one Firecracker tier, `b1t22-pool-fc-4`: four vcpu, every cache off and no site. Its pool named `z1d.metal` in the host's own subnet, at floor 0, a maximum of 2 (3 for the last attempt) and `idle_after` 5m. Its credential was an IAM user holding exactly `billet init iam --pool`'s policy, attached as a managed policy, because at 2,210 bytes minified it is past an IAM user's 2,048-byte inline limit. Jobs were queued from the test repository. Each pool host booted Ubuntu 24.04's own image with a boot prelude:
- billet's binary from the release archive, fetched from S3 with the instance profile's own credentials;
- the host role converged locally in `billet_node_prepare_only`, its config the one `billet init --join` renders for this host, so the bridges, pools and kernel path it prepared were the ones the join's config names;
- a single OSD on the first instance-store NVMe;
- Docker's data, Ceph's and the guest image's staging on the second;
- the guests' bridges moved off 172.31.0.0/16, because an AWS default VPC's resolver is 172.31.0.2, inside the role's default first bridge;
- the guest kernel pulled, and a unit left waiting to import the guest image once the node had registered.

| Step | Time (UTC) |
|---|---|
| A job queued with no host able to place it; the pool ordered one | 05:17:47 (on the first attempt, 17:33:30, in the job's own second) |
| `RunInstances` returned the instance | 05:17:49 |
| The host enrolled and was admitted on its source's word: "the console, the identity document, the address and EC2 agree" | 06:17:10 |
| The node registered | 06:19:21 |
| The guest image imported and verified: a microVM booted, took its registration from the metadata service, ran runner 2.338.2 and a container | 06:34:58 |
| Three jobs ran and succeeded on the host | 06:36:05 to 06:40:36 |
| Idle past `idle_after`, the pool asked for its release by drain | 06:45:47 |
| Its node answered empty twice across five minutes | 06:45:50 to 06:50:50 |
| Sealed, its certificates revoked, termination requested | 06:51:17 |

- **Growth and admission worked as ADR-030 says.** The pool ordered within a second of a shortage, grew one host at a time while one was on its way, and admitted both hosts that reached their join with no person: the first at 19:21:32 on 2026-11-08, the second at 06:17:10. A held order did what the operating page says. Every one of its join tokens lapsed with nothing joined, so it counted against the maximum, was listed as held for a person and was not counted as on its way.
- **Shrink worked on proof.** The release was asked 5 min 11 s after the host's last job ended, waited on "what the host holds could not be read" until its node had answered empty twice across five minutes, and sealed then. Each of seven released or sealed hosts was read back as gone by its source about 20 minutes after its termination was requested. A host stays shutting down for most of that, and the order holds its place under the maximum until then.
- **The order to a registered node is not measured clean.** The second host's boot spent most of its hour on GitHub. github.com answered its release downloads (Firecracker's, the guest image's manifest and parts) with 504 on two requests in three from that instance, while the same assets answered 206 from elsewhere. The prelude was re-run, and the image's assets fetched with curl's own retries and imported with `billet images pull --from`.
- **Four things stopped a host on the way, each now fixed or known:**
  - The host role's Ceph bootstrap ran a newer point release than the host's client could read. The device inventory was read as a flat list. On a fresh cluster the inventory did not yet list the NVMe, so no OSD was added and the pools' initialisation waited over an hour. All three are fixed in the role (#816).
  - The node unit the role rendered in this mode was one `billet local up`, the join's last step, refuses: exec rather than notify, and no declared state directory. The first host's node therefore never registered, and the second's did once the packaged unit was installed by hand. The role now renders the package's shape in this mode.
  - A pull refuses to import a guest image until the node has registered and recorded what its control plane accepts (#584). So a prelude can install the kernel, and the image waits for the registration.
  - The control plane reads `server.pool.boot_prelude` when it starts. A prelude rendered with presigned links signed by a session's credentials kept failing with 400 once the session expired, until the control plane was restarted on a prelude that fetches with the instance profile.
- **The shared test host stops itself after two idle hours** unless a detached job holds its keepalive lock. The control plane was restarted once for that. GitHub had kept the old message session, so the new one opened 31 s later, and nothing queued was lost.
- **Cost.** Eight `z1d.metal` orders across both days, at $4.464 an hour on demand; Cost Explorer's figure is in #553.

## A phoenixNAP server ordered from the portal, 2026-11-09

**How.** The API client still cannot order (above), so one hourly server was ordered in the portal:
- an `s4.x6.c6.large` in Phoenix: Xeon 6 6731E, 96 cores and no SMT, 256 GB, two 1.92 TB NVMe;
- Ubuntu Noble, a public /31, and a throwaway SSH key;
- $1.72 an hour, plus $0.007 for the IPs.

The `s1.c1.small` of 2026-11-05 is no longer in the catalogue, and this was the cheapest type with stock in Phoenix that day. The host was measured over SSH and deleted in the portal without keeping its IPs. The quota page reads 0/1 servers.

| Step | Time (UTC) |
|---|---|
| The last clock reading before the order was placed (the click came a few form steps later; its own time was not recorded) | 15:48:09 |
| The portal's "Created On" | 15:51:44 |
| SSH answered as `ubuntu` | 15:52:06 |
| The last clock reading before the delete was confirmed (the confirmation's own time was not recorded) | 15:55:41 |
| SSH refused | by 15:56:41 |
| The portal's server list empty, when next looked at | 15:58 |

- **The kernel had been running for days.** At 15:52:21 `/proc/uptime` read 275,665 s (3 days and 4 hours) and `/dev/kvm` was dated two days earlier. Yet the journal listed one boot, whose first retained entry was 15:51:23. So the server was not booted for this order. How phoenixNAP prepares such a host, and why it advertises "typically deploys in 60 seconds" for this image, was not observed. From the last clock reading before the order to SSH answering was under four minutes.
- **One thread's speed was unchanged beside 95 busy processes.** `openssl speed -seconds 5 -bytes 16384 sha256` hashed 1,383 MB/s alone (no affinity set). Pinned to cpu 0 with `taskset` beside 95 busy-loop shells, whose placement was not recorded, it hashed 1,401 MB/s. The AWS method (above) differs, so compare within a row, not across.
- **A Firecracker guest's hashing pipeline ran as fast as the host's in these runs.** Firecracker v1.13.1 booted its CI's 6.1.141 kernel and Ubuntu 24.04 rootfs with one vcpu and 1 GiB. The same pipeline, `head -c 2147483648 /dev/zero | sha256sum`, ran three times each on the host and in the guest, alone and beside 95 busy-loop shells:

  | Where | Alone | Beside 95 busy-loop shells |
  |---|---|---|
  | Host | 2,348 to 2,419 ms | 2,579 to 2,608 ms |
  | Guest | 2,396 to 2,451 ms | 2,398 to 2,403 ms |

  Alone, the guest's times overlapped the host's, their mean about 2% higher (2,417 against 2,376 ms). Beside the busy shells the guest's did not change, while the host's mean rose about 9% (2,589 ms). Three runs of one pipeline on one host, so this characterises that workload here, not virtualisation's cost in general.
- **NVMe.** An 8 GiB direct write (`dd oflag=direct`, 1 MiB blocks, after one warm-up write) took 3.25 s, 2.5 GiB/s. The direct read back (`dd iflag=direct`) took 3.09 s, 2.6 GiB/s. The root filesystem is LVM over the first NVMe.
- **What the API answers after a delete is still not measured.** These measurements had no API credentials at hand. The API client's order refusal is with phoenixNAP (#828): on 2026-11-09 its live chat, with Sales (the only group online; technical support is offered through their Slack), was asked to check that client's permissions now that a portal order works.

## The guests' caches on AWS metal, 2026-11-09 and 2026-11-10

ADR-030 puts a metal pool's caches on the site's ebs-s3 store: the Actions, Go and Bazel caches as S3 objects, and a sticky disk or Docker image store as an EBS volume the node attaches to its own instance. Guest root disks and golden images stay on the host's single-OSD Ceph. No billet node had attached a volume to itself before, so the path was probed first, then run end to end.

**The probe, 2026-11-09**, on a `t3.small` in us-west-2a, with the AWS CLI:
- **The device.** After `AttachVolume` was accepted, the volume's `/dev/disk/by-id` link (`nvme-Amazon_Elastic_Block_Store_vol<id without the dash>`, and the same with `_1`) appeared in 0.26 to 0.54 s. That was before EC2 described the attachment as `attached`. billet finds the device by the serial the volume's NVMe controller reports, never by the name it asked for.
- **Availability.** An empty volume was `available` in 2.7 s, a clone of a snapshot in 3.1 s. A snapshot of 1 GiB took about 62 s.
- **EBS detached a mounted ext4 volume without refusing**, in 4.4 s, and the kernel aborted the filesystem's journal. So billet detaches a volume only on proof that nothing on the host holds it: an exclusive open of the device, which a mount or another opener refuses, and a scan of every process's open descriptors for its device number. A check that cannot tell refuses.
- **Refusals, as EC2 worded them:**
  - `IncorrectState` (a volume still being created);
  - `InvalidParameterValue` "Attachment point /dev/sdf is already in use", the only refusal billet retries, under the next name;
  - `VolumeInUse`;
  - `InvalidParameterValue` "/dev/nvme5n1 is not a valid EBS device name";
  - `InvalidVolume.ZoneMismatch` for a volume in another zone.
- **The attachment limit.** `DescribeInstanceTypes` reports `MaximumEbsAttachments` 31, "shared", for `z1d.metal` and `m7gd.metal`, and 27 for `t3.small`.

**The real test under the rendered role** (`TestRealSelfAttach`, `BILLET_LIVE_EBSS3_SELF_ATTACH=1`) ran the store's own create, publish, clone and discard through billet's signer. Its credential was an instance profile holding exactly what `billet init iam` prints for a Firecracker node with `node.ebs_s3`, on an instance tagged as a pool host of the test's deployment.
- **On the `t3.small`, 2026-11-09:** create and attach 3.5 s; verify, detach and snapshot 55 s; clone and attach 4.0 s; mount and first read 38 ms; detach and delete 3.1 s. A held volume's detach was refused (`ErrHeld`).
- **Its first run was refused `UnauthorizedOperation` by `CreateVolume`.** The role lacked `ec2:CreateTags` for the tags a create carries. `billet init iam` now grants it, conditioned on the deployment's owner tag and on `CreateVolume` and `CreateSnapshot` alone (`BilletCacheTag`).
- **Again on a `t3.small`, 2026-11-10,** once review had held the cache's attach grant to volume ARNs (on `*` it also allowed any instance carrying billet's owner tag, whatever the self-attach grant said): create and attach 3.6 s; verify, detach and snapshot 38.3 s; clone and attach 3.7 s; mount and first read 39 ms; detach and delete 3.3 s.
- **And once more, 2026-11-10,** with the detach proof refusing a device with a partition, reading every thread's descriptors and every process's mappings, and asking the kernel each loop device's backing: the first run was refused could-not-tell at the partition check, because `/sys/dev/block/<major>:<minor>` holds attribute files (`alignment_offset`) under which a `partition` lookup answers ENOTDIR, which the fakes had no file to show; with only directories read as partitions it passed (create and attach 3.6 s, verify, detach and snapshot 46.5 s, clone and attach 3.7 s, detach and delete 5.2 s), the host's snap loop devices answered by the kernel.
- **On the pool's `z1d.metal`, 2026-11-10,** under the host's own instance profile, at a site name of its own so nothing of the run's cache was touched: create and attach 3.9 s; verify, detach and snapshot 59.2 s; clone and attach 3.7 s; mount and first read 19 ms; detach and delete 11.6 s.

**End to end, 2026-11-10.** One control plane on the shared Linux test host served a Firecracker tier, `b1t22-metal-fc-4`: trusted, with a runner group restricted to the matrix's workflows, interception on, and every cache publishing `trusted-only`. It had site `aws-us-west-2a`, whose store is `ebs-s3`, and a pool of `z1d.metal` at floor 0 and maximum 1. The pool's `init_args` named the site, the store and the cache listener on the guests' first bridge, and the prelude handed the same words to the config the role was converged from (`PRELUDE_INIT_ARGS`). The node registered at wire version 48, saying it uses its site's ebs-s3 store, and the plane accepted a Firecracker node at an ebs-s3 site on that word. The acceptance matrix's cold workflow was dispatched first, then its warm one, each job asserting what it found:

| Step | Time (UTC) |
|---|---|
| The cold run queued its seven jobs; the pool ordered a host | 02:03:45 |
| `RunInstances` returned the instance | 02:03:46 |
| The host role converged (prepare-only) | 02:13 |
| Admitted on its source's word, and the node registered | 02:26:07, 02:26:10 |
| The first launch that found a verified guest image | 02:49:14 |
| The cold run's seven jobs succeeded; the last publication settled | 02:51:41, 02:53:46 |
| The warm run's seven jobs succeeded | 02:54:19 to 02:56:15 |

- **Every cache was cold, then warm, as the ledger observed it**, never only as a job's log said. The cold jobs recorded the image store and the cache each one exercises as `cold`: sticky disk, builder, Git, Go, Bazel and npm, with Actions `served`. The warm jobs recorded each of them `warm`, and each asserted the cold job's content. The image store was a volume the node attached to itself: 3.8 to 11.3 s from the request to the guest's device on the cold run, and 3.8 to 6.4 s, cloned from the cold run's snapshot, for seven launches at once on the warm one.
- **A publication is a snapshot, and the job waits for it.** A sticky disk's or builder's write is published by the action's post step, which took 58 and 55 s cold and 74 and 79 s warm, against 59 s for the store's own verify, detach and snapshot of 1 GiB above. Attaching took the sticky-disk action 6 s cold and 24 s warm.
- **What waiting for the image cost.** The node registered 23 minutes before a launch could find a verified image, because a pull imports one only after the registration has recorded what its control plane accepts (#584). Launches dispatched meanwhile failed, and each held its lease's capacity until the node proved no compute existed. Up to 11 leases were held at once, and the tier's work waited for them to be released and bought again.
- **The attachment limit, measured.** The idle host was given 1 GiB volumes until EC2 refused. It took 30 beside its root, named `/dev/sdd` to `/dev/sdz` and then `/dev/xvdba` to `/dev/xvdbg`. The 31st was refused `AttachmentLimitExceeded`, "You may attach up to 31 devices for this instance type". The instance store held `/dev/sdb` and `/dev/sdc` ("already in use") without counting against the 31. billet asked only for `/dev/sdf` to `/dev/sdz`, which would have stopped it at 21, so it now continues with `/dev/xvdba` to `/dev/xvdbz`, and EC2's limit refuses first. Each Firecracker guest holds its image store and, while a job uses one, a sticky disk, so 30 attachments serve 15 such jobs at once.
- **Against the host's own Ceph.** On the same host, an RBD clone of a 1 GiB ext4 image took 0.04 to 0.06 s, its kernel map 0.06 to 0.07 s, and a mount and unmount 0.02 to 0.03 s, three times each. An EBS clone and attach took 3.7 s. A cache volume on EBS costs a launch about 4 s more than one on Ceph would, and its publication about a minute.
- **An S3 object read.** A 1 GiB object read with one stream by curl from the host took 20.5 to 22.3 s, 48 to 52 MB/s, three times. The host's default VPC had no S3 gateway endpoint, so the reads went through its internet gateway. On 2026-11-04, the EC2 cache probes' one stream through an endpoint took 11.3 to 20.8 s.
- **Decommission.** `billet decommission --yes` was run from the node's config on another machine, with an operator's credentials in its environment and the deployment's id in the node's state directory. It purged what the run had published: 5 snapshots, 3 state objects and 173 cache objects. It listed no instances, since a metal node's host is its pool's. A sweep by the deployment's tags then found no volume, snapshot or object.
- **Cost.** One `z1d.metal` for about 1 hour 20 minutes at $4.464 an hour on demand, about $6, and less than $1 of EBS and S3.

## Still to measure

- A no-stock and a quota refusal from each source, read as the scaler will read them.
- The time from an order to a registered node with nothing in the way: on 2026-11-09 GitHub's 504s took most of the hour.
- Single-thread speed inside a Firecracker guest under full load, on each candidate, with cpus pinned so a sibling hyperthread is told apart from the rest of the load.
- `m6id.metal`, `m8gd.metal-24xl` and `m8id.metal-48xl`, which fit the shape on paper and were not launched.
- What `DescribeInstances` answers for a terminated metal instance, and for how long: billet read each released host as gone about 20 minutes after asking, and the answers in between were not recorded.
- `m7gd.metal`'s attachment limit by attaching until refused; only `DescribeInstanceTypes`' 31 is recorded.
- A metal host's cache reads through an S3 gateway endpoint in the pool's own VPC.
- phoenixNAP: an order through the API, which is still refused (#828), and with it the join's person-approved admission and the API's answer after a delete.
