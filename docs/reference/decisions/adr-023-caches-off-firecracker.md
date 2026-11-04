# ADR-023: Which backends the node's caches serve, and how EC2 keeps them

Accepted, 2026-11-04. Issue #26. Extends the interception of [ADR-020](adr-020-npm-registry-interception.md) and the publication rule of [ADR-013](adr-013-branch-protected-caches.md) past Firecracker. Measurements are in [EC2 cache probes](../records/ec2-cache-probes.md).

## Context

The Actions cache, the Git proxy, the Go, Bazel and Buck2 caches and the npm cache were all held to one provider, firecracker, on a Linux guest (`Tier.InterceptionErrors`). A job on any other backend used GitHub's cache at GitHub's speed. AWS is capacity people try billet on, so per-job EC2 is the first backend past Firecracker that matters.

Serving a guest takes three pieces: the guest's remap of an origin and trust of the node's authority, a listener the guest can reach, and a store behind that listener. Each backend answers them differently.

## Decision per backend

| Backend | Decision | Why |
|---|---|---|
| Firecracker | **Unchanged.** | Ceph clones are cheap, and a clone is mounted on the host that serves it. |
| EC2 | **Accepted.** One provider, a Linux guest, and a node inside the VPC. | The rest of this record. |
| CodeBuild | **Refused for good.** | The build host is not billet's, and a reserved fleet's instance is not scrubbed between builds: same user, same home directory, contents intact ([AWS acceptance](../records/aws-acceptance.md)). A guest billet cannot prepare, on a host it cannot account for, gets no bearer. |
| Tart (macOS, Linux arm64) | **Deferred until a store exists.** `node.cache` stays refused on a tart node. | There is no site store for a Mac, and a Mac is not a site's storage peer. It would be per-key disk images on the Mac's APFS, cloned with `clonefile` per job, attached with `tart run --disk`, on the generation model the Firecracker store uses, with untrusted jobs reading the trusted baseline and their writes discarded. Attaching a disk that way, and how a macOS guest formats and mounts it (APFS, or ext4 in a Linux guest), are unmeasured and need a probe on a real Mac first. |
| Docker | **Refused.** | The backend exists for trials on a laptop. Its containers share the host's kernel, and nothing about it is a place to keep a cache for a fleet. |
| Several providers on one tier | **Refused, as before.** | A job placed on the fallback cannot reach the first provider's store, and a label that is sometimes cached is worse than one that says what it is. |

A cache a tier turns on explicitly on a refused backend fails config load, naming the reason. A cache on by default that cannot run is off, as it always was.

## EC2: the three pieces

### The guest: the firecracker agent's own text

An EC2 instance boots from user data, not metadata, and has none of the firecracker agent's plumbing. The mechanism is several hundred lines of shell, most with a measured reason, and a second copy would be one that is wrong.

So there is one copy. The agent in `scripts/build-guest-image.sh` marks its shared regions, and `billet ami build` cuts them out (`scripts.GuestImageBlock`), wraps each in a function, and installs the result as `/usr/local/lib/billet/guest-caches.sh`, with `fetch` answered from what the boot script exported. The entry point runs them in the agent's order, around the same `billet-docker-cache prepare`. The files they use are installed from the same assets, and `billet ami verify` proves each one on the booted image by digest. That is `AMIContract` 5. An image below it ignores what the boot script exports and runs the job cold, which `billet check` reports as needing a rebuild.

**The node's authority travels per launch, in user data, and only for a session that has interception.** The AMI is shared by trusted and untrusted tiers, so baking it would make every job trust it. The listener's issuer stays baked (`billet ami build --ca-cert`), because every guest has to reach the listener. A launch's boot script is sent as it is and stays inside EC2's 16384 bytes, which a test proves with the largest registration.

**billet in the guest is the binary that built the image**, or `--guest-billet` where the build runs on another platform. It is staged in the payload, because the AMI's build has no tree to compile.

### The listener: `node.cache`, reached through a relay in the guest

The listener is the one that already serves sticky disks, on a node at an address the instances can dial. The bearer crosses a VPC, so it is HTTPS, as it was.

**A relay in the guest does the TLS.** Everything else in a guest speaks what it speaks on a Firecracker bridge: plain HTTP to the docker gateway, and CONNECT tunnels carrying the client's own TLS to the results origin. A client's TLS nested in a second TLS session cannot be done with Python's sockets, and `node-relay` is a splice that does not care, so on this image the passthroughs tunnel through it. The relay checks the node against the system's roots, where the issuer is baked. A relay that does not start leaves the build caches on the node's own https address, without its patience across a restart, and leaves interception off: the job uses GitHub's cache, which is the fail-open rule at the place it costs least.

**The node is asked before anything names it.** A relay that started proves nothing about the node, and the next steps rewrite `https://github.com/` to it, which `git` does not fall back from. An image built without the listener's issuer, or a security group that drops the listener, would fail every checkout. So the entry point asks with a bounded `curl` that sends no credential, any HTTP answer proves the node, and could-not-tell stops the relay and clears the endpoint: the job runs without the caches that name the node, and a Docker store already attached settles as usual. A node handing over in that moment costs that one job its caches, which is the fail-open rule again. The relay's patience across a node restart covers its TLS handshake as well as its dial, because a restarted node binds its port before it serves, and a certificate that fails verification ends the wait at once.

**TLS is the transport and nothing above it knows.** The listener hides its connections' TLS from `http.Server`, because Bazel's and Buck2's gRPC arrives as HTTP/2 with prior knowledge through the relay, and the server takes that only from a connection that is not a `*tls.Conn`.

### The store: whole objects in the site's bucket

The open question was whether the Actions cache is a store volume on that listener, or something else. Measured from the node ([record](../records/ec2-cache-probes.md)): a volume per archive costs 20 seconds before its first byte, reads lazily at 18 to 47 MiB/s, costs five and a half minutes of snapshot per save, and needs an attachment, of which a node has 32 and one BuildKit job can want 32. S3 through a gateway endpoint has no wait, reads 360 to 730 MiB/s on 8 streams, and has no such limit. GitHub's own store gave the same host about 54 to 64 MiB/s on 8 streams, so a volume per archive would be no acceleration at all.

**So `ebs-s3` gains an object half, through two optional capabilities of the store contract.** Sticky disks and the Docker image store stay EBS: they are filesystems a job mounts, and a snapshot's copy-on-write is what they want.

**`ArchiveStore`, for a cache whose unit is a file.**
- A block streams from the guest straight to a staged object, so the node holds no copy and needs no disk for it. A commit assembles the blocks in the client's order with `UploadPartCopy`, inside S3, and completes with `If-None-Match: *`, so the first writer of a key wins and a loser is told.
- A block below S3's 5 MiB part minimum, anywhere but last, cannot be a part. Such an upload is read back and rejoined through a bounded buffer: the one path that costs the node memory and transfer.
- The commit records its upload on the object as metadata, so one that lost its answer asks the object whose it is rather than guessing. An archive never changes, so there is no pointer, no writer lease and no fence, and a job's save is one object or nothing.
- A download reads through the node. The official client asks for any non-Azure URL on one connection, so only the node can fetch ahead, and a presigned URL would also bypass the kill switch for the life of the URL and need a route from the guest to S3 that a tier's network may not give it. The node fetches four 8 MiB chunks at once and answers in order, for up to 16 reads at a time; a read past that is one plain stream that holds nothing, and is slower and a miss if its body breaks, because a job can open a signed URL as many times as the node takes connections and stop reading.
- A read leaves no trace on an object, so a lookup writes a small marker, and eviction reads the marker. The retention is days and a job is hours, so a download does not normally lose its archive, and no lease is needed to say so. One looked up in the moment an eviction pass takes it fails, which the client reports as a miss, and a read is held to the object it matched, so a key saved again meanwhile cannot hand it half of each.
- `actionsArchiveCount` was what a volume per archive costs. Here an archive costs one entry of the session's record, so a job may hold 512, and BuildKit's `mode=max` stays local.

**`BlobStore`, for a content-addressed cache.**
- An entry is one object, named by its digest or, for an action result, by the action's: first writer wins, either answer valid. Concurrent jobs merge by writing, so there is no generation to publish and no merge.
- A session's writes land in a directory on the node's own disk and upload only when the job's completion authorises them, off the command path, as a volume's publication does. An upload that stopped partway leaves only whole, correct entries, so it is retried inside its window rather than abandoned.
- A read the session's directory cannot answer goes through a read cache on the same disk, shared by the sessions of one key and filled only from the store, so a job's unpublished writes reach no other job, and a cold entry is one request rather than one per read. A listing on the session's first use means a miss costs no request at all. Cache objects are checked against their digest as they are filled.
- Eviction is a mark and a sweep: each job records what it used, and an entry older than the retention that no record within it names is taken. A record that cannot be read stops the sweep, because what it names cannot be told from what nobody uses.
- `max_size` bounds what one job adds, as one allowance across a restart, since there is no volume to fill, and retention bounds what the key holds.

**What this costs.** A request per entry on a first build: at S3's published rates about $0.50 per hundred thousand entries written, and nothing to read them again through the read cache. Storage at S3's rate. The node's disk: a job's unpublished writes and the read cache land under `node.state_dir`, and the node's network carries every job's archive, so an EC2 node that serves these caches is sized for them rather than for an orchestrator's nothing.

**The node may delete what eviction takes.** `s3:DeleteObject` and `s3:AbortMultipartUpload`, on the object half's names only and never on the state objects, so the credential that evicts an archive cannot unpublish a sticky disk. The bucket stays on S3-managed encryption, as it is for the state, and the boundary between deployments sharing a bucket is the prefix the role is scoped to. A per-deployment bucket is the module's default.

## What did not change

The three intercepted calls and everything spliced upstream, the kill switch, the fail-open rule (a cache failure is a miss or a warning, never a failed job), who may publish (ADR-013), and the rule that a job not proved to its repository gets no local archive. An untrusted pool gets interception only under `publish: default-branch`, as on Firecracker, and otherwise no proxy and no authority in its user data.

## An older node

An older EC2 node refuses a launch that names a guest cache, which would fail every job on a tier that merely has them by default. So below `nodeapi.VersionEC2GuestCaches` the plane sends the tier's cache block with them off, and the job runs as it did before.

## Alternatives rejected

- **A per-instance adapter**, serving the three calls from a volume attached to the guest. The EBS costs above stay, and each job's writes need merging somewhere after its instance is gone, which is the node again.
- **Volumes attached to the node's own instance.** The code would have run unchanged, and the numbers above are why it does not.
- **Presigned S3 URLs handed to the guest.** They bypass the kill switch for their life, need the guest's network to reach S3, and a client's one stream reads at about 90 MiB/s where the node's read-ahead gives it what its own disk can take.
- **Baking the interception authority into the AMI.** Every job on the image would trust a certificate that only an interception session needs.
- **A second copy of the agent's logic for EC2.** Every defect found in one would be owed to the other.

## What is not proved here

The live conformance run on EC2, which needs a repository the deployment serves, and what a real `actions/cache` restore costs from GitHub on the same instance. The probes' record says what stands in for it.
