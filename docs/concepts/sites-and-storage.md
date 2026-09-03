# Sites and storage

## A site is where compute and storage share a fast network

```yaml
sites:
  - name: home
    store: ceph
  - name: us-west-2
    store: ebs-s3
```

A node says which site it is at; a tier may insist on one. The site's store is its cache authority, and the control plane refuses a node whose provider cannot use it: a Firecracker node needs Ceph, an EC2 node needs EBS and S3, and a CodeBuild node declares no site because a build uses neither. Cache data is namespaced by deployment and site, so two sites never see each other's bytes, and the same `runs-on` label can fall back from one site to another with the second starting cold. That boundary was measured on real hosts: two nodes at one site reuse one generation, a second site cannot reach the first's store, and a label fails over across sites ([Site acceptance](../reference/records/site-acceptance.md)).

## Why Ceph on the nodes' own NVMe

A snapshot on one machine cannot be mounted on another, so a cache kept in local storage pins every repository to the host that first built it. Ceph RBD gives the same snapshot-and-clone primitive from a pool any node at the site can map, which is what makes a cache a property of a place rather than of a machine. On a single box it is honestly more moving parts than the ZFS pool it replaced; the reason to adopt it anyway is that retrofitting shared storage later means rewriting placement at the same time. [ADR-003](../reference/decisions/adr-003-ceph-rbd.md) records how the reference cluster was built and what it measured, including why clone v2 is a requirement rather than a preference: on the old clone format a snapshot with a live clone can be neither unprotected nor removed, so a cache generation any running job holds would be undeletable, and `billet check` refuses such a cluster.

billet installs nothing: run `cephadm bootstrap`, create two pools, run `ceph osd set-require-min-compat-client mimic`, and point `node.ceph` at them, or let the Ansible host role do exactly those steps after you name the monitor address and every disk it may consume. billet drives Ceph through the `rbd` command rather than a linked library, so the binary stays static.

What it costs: a real `go build` on RBD runs at 1.02x local, fsync-heavy work at 2.36x, and a cold read of forty thousand small files at 4.07x, which is the cache-hydrate path. The baseline that decides whether a persistent cache is worth having is re-fetching over the network, not a local disk a second machine cannot see.

## Generations, clones and publication

Every cache and every guest image is an immutable named **generation**. A job gets a copy-on-write clone of one and the clone is discarded with the compute. Publishing a new generation is a fenced operation: a writer lease is taken, the filesystem is verified clean, a snapshot is made, and the current pointer is advanced by compare-and-swap; an interrupted commit leaves the old pointer or a complete new one, never a pointer to unfinished bookkeeping. Published generations remain for eight inactive days; failures, cancellations, untrusted work and unknown results discard the job's clone without publishing.

On AWS the same contract is EBS snapshots for generations and an S3 object for the pointer, advanced by conditional put. billet never deletes from that path on the hot path; `billet decommission` is what removes the cache store.

## What is cached

- **The guest image** itself: a Firecracker job boots a clone of the golden image's current verified generation ([Guest images](../operating/guest-images.md)).
- **Sticky disks**: an ext4 volume mounted into one job under a key you choose, published in a post step (`actions/stickydisk`).
- **BuildKit state**: a per-job BuildKit daemon whose whole state lives on a sticky disk (`actions/setup-docker-builder`, `build-push-action`), with a per-mount ceiling from the tier's `buildkit_cache_mount_limit`.
- **The Docker image store**: guests request a deployment-, site- and architecture-scoped `/var/lib/docker` volume before the runner starts, so image layers pulled by one job are there for the next.
- **Registry mirrors**: three optional site-local mirrors for Docker Hub, GHCR and Quay, serving public content with no upstream credential.
- **The Actions cache**: `actions/cache` traffic served locally instead of crossing the internet, on Linux Firecracker tiers that opt in ([Transparent Actions cache](../operating/actions-cache.md)).

Firecracker reserves five hot-attach slots per guest; EC2 attaches owned volumes to the one-job instance at workflow runtime. macOS guests have no cache today.
