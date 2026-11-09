# A pool host's boot prelude

`prelude.sh` is a `server.pool.boot_prelude` for a pool of Firecracker hosts on AWS metal ([ADR-030](../../../../../docs/reference/decisions/adr-030-rented-bare-metal-pool.md), [the operating page](../../../../../docs/operating/bare-metal-pool.md)). A pool host runs it on its first boot, before the `billet join` the pool appends. It prepares the host with this collection's `host` role in `billet_node_prepare_only` mode and leaves the node's config and its start to the join.

It is an example to copy and render, not a script the collection installs. Where billet and the collection come from, the bucket, the image source and the disks are yours.

## What it does

1. **Installs billet's binary** at `/usr/bin/billet`, checked against the sha256 you render in. It never installs the package: the package's postinstall seeds `/etc/billet/billet.yaml` from its template, and the join leaves a config it did not write alone, so the host could not be joined. The role refuses that seed in this mode.
2. **Fetches the collection** at the commit you pin, and installs the pinned `ansible-core` and `ansible.posix` in a venv.
3. **Chooses its disks once and records them** in `/var/lib/billet-pool-prelude/disks.env`:
   - the OSD takes `PRELUDE_OSD_DEVICE`, else the first whole disk with no partitions, no filesystem and nothing mounted, and not the root's (an instance store's NVMe on metal);
   - a second such disk, when there is one, is formatted and holds Docker's data (cephadm's containers), Ceph's (the monitor warns below 30% free, which an 8 GB root reaches) and the guest image's staging. The staging holds the image's parts, its assembled archive and the decompressed image at once: 97 GB at its peak for the x86_64 image of 2026-11-08. A 40 GB disk filled, and the pull failed.
   - "First" is the kernel's listing order, not the order the disks were attached or their size. When the host's disks differ, set `PRELUDE_OSD_DEVICE`, so the staging lands on a disk large enough for it.
4. **Renders the role's inputs from the join's own config.** `billet init --join` is given the flags `server.pool.init_args` hands the join, and writes to a scratch path, never `/etc/billet/billet.yaml`. So the bridges, the pools and the kernel path the role prepares are the ones the join's config will name. (`--join` cannot be combined with `--emit ansible`.)
5. **Converges the role on localhost.** The settings:
   - `billet_node_prepare_only`, and a converge guard holder;
   - a single-OSD Ceph bootstrap on the recorded disk, pool size 1;
   - the guests' egress limits the operating page recommends;
   - more retries for the role's downloads;
   - uplink shaping off;
   - the guests' bridges on 10.240.0.0/23. The role's default 172.31.0.0/24 would put an AWS default VPC's resolver, 172.31.0.2, behind the first guest bridge.
6. **Acknowledges the single OSD** (`mon_warn_on_pool_no_redundancy false`, `osd_pool_default_size 1`), and restarts Ceph's managers. One of the two that cephadm starts kept the default size it started with and reported `TOO_FEW_OSDS` until it was restarted. Until a pool site has a store, a pool host keeps only golden images, which are pulled again from the signed source, and per-job clones.
7. **Installs the guest kernel** the node's config names, from a pull that is retried. A pull installs an image's paired kernel and then refuses to import until the node has registered and recorded what its control plane accepts (#584). This pull fetches and decompresses the whole image first, so it takes as long, and as much staging, as the import itself.
8. **Leaves a transient unit,** `billet-pool-image-pull`, that waits for that record and then pulls and verifies the image.

It may be run again on the same host. The disks come from the record, and a mounted scratch disk is reused. The preview config is rendered afresh. The role's converge is its own, and the image unit is started once.

## Rendering it

Prepend exports to the script's body (everything after its first line) and point `server.pool.boot_prelude` at the result:

```bash
# A release's archive (or a bare binary); its sha256 is the archive's line in the
# release's checksums.txt. v0.36.0 is the first release whose role has this mode's fixes.
export BILLET_URL='https://github.com/junioryono/billet/releases/download/v0.36.0/billet_0.36.0_linux_amd64.tar.gz'
export BILLET_SHA256='…'
export COLLECTION_URL='https://github.com/junioryono/billet/archive/refs/tags/v0.36.0.tar.gz'
export COLLECTION_SHA256='…'
export PRELUDE_JOIN_SERVER='10.0.0.5:7717'   # server.pool.join_server
export PRELUDE_IMAGE='ubuntu-2404-x64'       # the tier's image name
```

The collection tarball may hold `ansible_collections/` at its top (a tarball of that directory alone, as the run used) or one directory down (GitHub's source archive of a tag or commit).

Or, for artifacts in a private bucket, set `ARTIFACT_BUCKET`, `ARTIFACT_REGION`, `BILLET_KEY` and `COLLECTION_KEY` instead of the URLs. The script then reads them with the instance profile's own credentials, signed by curl, and the pool's instance profile needs `s3:GetObject` on those keys.

**Do not render presigned links signed by a person's session.** The control plane reads `boot_prelude` once, when it starts. Every order after the session expires fails with 400.

Also set `server.pool.init_args` to the same three flags the script gives `billet init --join`:
- `--kernel-image=/var/lib/billet/kernels/vmlinux`
- `--ceph-user=billet`
- `--ceph-keyring=/etc/ceph/ceph.client.billet.keyring`

The whole payload carries a join token, so keep it to the source's API, and within EC2's 16384 bytes of user data. Rendered with presigned links it was about 11 KB.

## What has been proven, and what has not

- **On AWS metal, 2026-11-09** ([the bare-metal hosts record](../../../../../docs/reference/records/bare-metal-hosts.md), "The pool on AWS metal, end to end"), a version of this script prepared a `z1d.metal` that was admitted with no person, registered, imported the guest image and ran jobs. **Not unchanged:** on that host, the OSD was added by hand, the payload was re-run after GitHub refused downloads, the image's assets were fetched with curl's retries and imported with `--from`, and the packaged node unit was installed before `local up`. The role (#816, #810) and this script now carry the fixes for each.
- **This version** was run twice on a nested-virtualisation `m8i.2xlarge` with two spare volumes, the second run reusing the first's disks. See the pull request that added it.
- **Not proven:** a phoenixNAP host. It has no instance metadata service, so the address and the credential fetch here are AWS's.

