# EC2 cache probes

What was measured on 2026-11-04 to decide where the caches a node serves keep their bytes on per-job EC2 ([ADR-023](../decisions/adr-023-caches-off-firecracker.md)). A node had to keep each Actions archive in one of two places: a store volume per archive, as a Firecracker node does on Ceph, or a whole object in the site's S3 bucket. These are the numbers that chose.

## Where it ran

One `c7i.2xlarge` in `us-west-2a` (8 vCPU, EBS baseline 312.5 MB/s, network "up to 12.5 Gigabit"), in a VPC with an S3 gateway endpoint, built with billet's own Terraform module. The archives were incompressible: `head -c` from `/dev/urandom`, which is what a zstd tarball looks like to a store. Every reading is wall-clock from the host. The probe scripts made everything under a `b1t22-probe` name and removed it, and the account held no probe volume or snapshot afterwards.

Reading to `/dev/null` measures the store and the network, not the host's disk. A job's root disk is the next limit: gp3 at its default 125 MB/s took the 1 GiB S3 download in 18.07 s and the 4 GiB one in 72.69 s, where `/dev/null` took 11 s and 63 to 77 s on one stream.

## A store volume per archive

A 22 GiB gp3 volume at the store's settings, encrypted with the deployment's KMS key, which is what `actionsVolumeSize` asks for on Ceph.

| Step | Seconds |
|---|---|
| Create the empty volume, to `available` | 17.62 |
| Attach, to the device present | 2.24 |
| Write 1 GiB, with `sync` | 8.93 |
| Write 4 GiB, with `sync` | 34.65 |
| Snapshot the volume with 5 GiB used, first in its lineage | 332.54 |

Then three clones of that snapshot, each attached and mounted read-only (`ro,noload`, as the Actions cache mounts one) and read with `dd iflag=direct` after dropping the page cache:

| Run | Clone to `available` | Attach | Before the first byte | Read 1 GiB | Read 4 GiB | Detach |
|---|---|---|---|---|---|---|
| 1 | 18.16 | 2.32 | 20.53 | 55.14 | 152.87 | 16.86 |
| 2 | 17.94 | 2.26 | 20.30 | 57.41 | 87.33 | 16.81 |
| 3 | 18.27 | 2.13 | 20.47 | 27.64 | 209.25 | 16.85 |

The cost is in the shape, not the tuning. **Every restore pays 20 seconds before its first byte**, and a clone reads lazily from the snapshot at 18 to 47 MiB/s. **Every save pays a snapshot**: five and a half minutes here, before a job's finalize can answer. And a volume needs an attachment: `DescribeInstanceTypes` says `MaximumEbsAttachments: 32`, `AttachmentLimitType: dedicated`, on `c7i.large`, `c7i.2xlarge`, `c7i.4xlarge` and `m7i.xlarge`. One job may hold 32 archives at once (`actionsArchiveCount`), and BuildKit's `mode=max` writes one per layer, so one such job fills the node's attachments on its own.

## Whole objects in S3, through the gateway endpoint

One object, read with `curl` to `/dev/null` through a presigned URL. Ranged runs read equal ranges at once with separate processes.

| Read | 1 GiB | 4 GiB |
|---|---|---|
| One stream | 11.32, 11.50, 20.84 | 62.72, 76.60, 76.21 |
| 4 ranged streams | 3.66, 2.72 | 24.18, 39.77 |
| 8 ranged streams | 1.40, 1.41 | 11.27, 9.42 |
| 16 ranged streams | 0.88, 0.99 | 5.14, 4.52 |

A put with the AWS CLI's own multipart took 4.86 s for 1 GiB and 45.58 s for 4 GiB. Sixteen 64 MiB blocks staged one at a time, each a new CLI process, took 16.60 s, and assembling them with `UploadPartCopy` in a chosen order, one at a time and each a new CLI process, took 35.30 s; that second figure is mostly the CLI starting, and an assembly moves no bytes through the host.

## GitHub's own store, as this subnet sees it

**This is a stand-in, and it is labelled as one.** The bar is `actions/cache` restoring from GitHub on a billet EC2 job, and that run needs a repository the deployment serves. What was measured instead is GitHub's blob store from the same host: ranges of a 19 GB Actions artifact of billet's own, through its signed URL on `productionresultssa15.blob.core.windows.net`, which is the kind of URL a cache call answers with. The official client reads such a URL with 8 concurrent ranges by default, and any other host with one plain stream (below).

| Read | 1 GiB | 4 GiB |
|---|---|---|
| One stream | 118.88, 106.61, 108.49 | 419.20 |
| 8 ranged streams | 18.78, 19.14 | 64.34, 72.65 |
| 16 ranged streams | 20.92, 12.64 | 41.39, 47.51 |

Six further 4 GiB readings from the first attempt are not here. The signed URL expired after the first of them, and they finished in about 0.35 s each because `curl` without `-f` writes an error page to `/dev/null` and exits 0. The ranged 4 GiB rows are a second attempt with `-f` and a fresh URL, each process's status checked.

## What the official client does

Read from `actions/toolkit`'s `packages/cache` on 2026-11-04, and to be confirmed on a live run:

- `downloadCache` (`src/internal/cacheHttpClient.ts`) reads a URL whose host ends in `.blob.core.windows.net` with its concurrent client, 8 at once by default (`src/options.ts`), and **any other host with one plain stream**. So a restore served by anything but Azure is one connection whatever the store behind it can do, and only what answers that connection can fetch ahead.
- `uploadCacheArchiveSDK` (`src/internal/uploadUtils.ts`) always uploads with Azure's block-blob client, with a 128 MiB single-shot limit, so an archive up to that size is one `PUT` and a larger one is blocks and a block list. `saveCacheV2` (`src/cache.ts`) sets 64 MiB blocks and 8 workers, well above S3's 5 MiB part minimum.

## What this decided

GitHub's store gave this host about 54 to 64 MiB/s on 8 streams. A volume per archive reads no faster than that, starts 20 seconds later, and saves more than five minutes slower. S3 read 7 to 13 times faster on the same 8 streams, at 360 to 730 MiB/s, with no wait before the first byte and no attachment to run out of. So on this store an archive is an object.

## The object half against real S3

`TestRealArchiveStore` ran on the same host on 2026-11-04, under the node's own role with exactly the policy the module renders, through billet's own signer, in 2.25 s. What a fake accepts and S3 might not, and did not refuse:

- `UNSIGNED-PAYLOAD` over TLS on a streamed put, and on a part and a completion.
- A commit whose declared size differs, refused before S3 is asked for anything.
- `UploadPartCopy` of a staged block above the part minimum followed by a smaller last one, and `CompleteMultipartUpload` with `If-None-Match: *` over a free key.
- The same completion over a taken key, which S3 answers with a status billet reads as the second writer having lost, and the metadata on the object read back with `HeadObject` to tell whose it is.
- `AbortMultipartUpload`, twice, the second of an upload already gone.
- A key holding a space, a percent sign, a `+`, `?`, `#` and non-ASCII, matched back through a restore prefix.
- Three 7-byte blocks rejoined into one archive, in the order committed rather than staged.
- The eviction pass's own listings and reads, multipart listing included, under the grant the node holds: `s3:ListBucketMultipartUploads` carries no prefix condition, and the object half's `s3:DeleteObject` is on its own names and never on a state object.

## What this does not prove

- That a real restore is faster than GitHub's. The stand-in above is GitHub's storage, not GitHub's cache call; the live run is owed and belongs to the conformance record.
- Anything about a node under load, or several restores at once. The node's network is the shared limit once it serves every job's archive, and `c7i.2xlarge`'s "up to 12.5 Gigabit" is a burst.
- Any region but `us-west-2`, or any zone but `us-west-2a`.
