---
name: billet-storage-and-cache
description: "Load when touching sites, internal/store (generations, writer leases, fences, pointer CAS), the Ceph RBD store or the EBS+S3 store, any cache the node serves (Docker store, sticky disks, the Actions cache, the Git mirror, the Bazel/Buck2 remote cache, the Go build cache), tiers[].cache.publish, the actions in actions/, the conformance workflow, `node.ceph`, `node.ebs_s3`, `node.cache` or `billet cache *`; or when a cache is stale, a clone fails, a publication is refused, or a job cannot resolve names."
---

# Sites, stores and caches

## What this area is

A site is a place where compute and its storage share a fast network (`sites[]`, `store: ceph | ebs-s3`). A node reports its site at registration, a tier may pin one, and the control plane refuses a node whose provider cannot use the site's store. `internal/store` is the contract every cache implementation satisfies: immutable named generations, copy-on-write clones held under active leases, a writer lease for publication and a fenced pointer advanced by compare-and-swap. `internal/store/ceph` drives RBD through the `rbd` command for firecracker sites; `internal/store/ebss3` uses EBS snapshots and an S3 pointer for EC2 sites. On the node, `internal/node/actions_cache.go`, `actions_proxy.go` and `actions_volume.go` intercept `actions/cache` traffic for Linux firecracker tiers. `actions/stickydisk`, `setup-docker-builder`, `stop-docker-builder` and `build-push-action` are the published actions. `docs/reference/decisions/adr-003-ceph-rbd.md` records the cluster and its measurements; `docs/operating/actions-cache.md` is the operator page.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Sites, stores and discards: [references/stores.md](references/stores.md)

- **Storage is a property of the site, not of the compute backend.**
- **A cache writer lease is not a capacity lease.**
- **A writer lease that will not publish is released, and a release is exact.**
- **Ceph is driven through the `rbd` command, and every naming rule is pinned to what rbd does.**
- **Clone v2 is a requirement, and two settings decide it.**
- **A real build on RBD costs 2%, and the fio number that said otherwise measured a machine doing nothing but IO.**
- **The ebs-s3 store's first real publication was refused twice by things every fake accepted.**
- **A discard is storage, not compute, and never runs on the node's command path (#223).**
- **On Ceph, a discard moves to the trash and the node's sweep deletes (#280, #292).**
- **An intact orphan volume is reclaimed by an operator, on proof, through the trash (#301).**

### The Actions cache, the kill switch and publication: [references/actions-cache-and-publication.md](references/actions-cache-and-publication.md)

- **The Actions cache is a DNS remap of one origin, serving three methods.**
- **A store and a listener are halves of one thing, and `billet check` is where the mirror is caught.**
- **The kill switch is central, per cache, and consulted before every local operation.**
- **What the cache did for a job is recorded as an observation, from a closed vocabulary, first observation kept.**
- **The conformance workflow is consumer-owned and pinned.**
- **Caches are a deliberate cross-job channel, and `tiers[].cache.publish` gates publication (#226, ADR-013).**
- **Guest volumes never become host file authority.**
- **Every cache is on by default where the tier can have it, and a default that cannot run is off, never refused.**
- **Default-branch publication is deferred, journaled and off the command path.**

### Sticky disks, the Go and Bazel caches and the Git proxy: [references/build-caches.md](references/build-caches.md)

- **Sticky disks and the builder actions.**
- **Go and Bazel share a content-addressed host volume per session (`casvolume.go`).**
- **The Go helper and the Bazel credential helper are billet itself, in the guest.**
- **The Git proxy serves github.com fetches from a mirror, and only what GitHub would give that job (`gitproxy.go`).**
- **What each build cache did is observed like the image store.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A new cache or a change to what one publishes.** Publication is gated by `tiers[].cache.publish` and the ref GitHub proves (ADR-013), and `billet-security` holds the bearer and interception rules; read both references before changing either. What the cache did is an observation from a closed vocabulary, first observation kept.

**A change to how a store talks to Ceph or S3.** Every rbd naming rule is pinned to what rbd does, and the ebs-s3 store's first real publication was refused twice by things every fake accepted, so prove it against the real service, not only the fake.

## Where the tests are

- `internal/store/ceph/*_test.go` and `internal/store/ebss3/*_test.go` (including each `writer_test.go`, the conformance shared by both backends), `internal/integration/firecracker_ceph_test.go` (real Ceph growth becomes guest capacity).
- `internal/node/actions_*_test.go`, `cmd/billet/cache_test.go`, `cacheconformance_test.go`, `cephcheck_test.go`, `sitecheck_test.go`, `cachecheck_test.go`.
- `actions/actions_test.go`, `siblingrefs_test.go`, `scripts/cache_conformance_cached_test.go`; the live matrix in `.github/workflows/cache-conformance.yml`.

## Related skills

`billet-config` (sites and store pairing), `billet-providers-local` and `billet-providers-aws` (which backend uses which store), `billet-security` (cache trust), `billet-guest-images` (the golden-image generations that share the Ceph client).
