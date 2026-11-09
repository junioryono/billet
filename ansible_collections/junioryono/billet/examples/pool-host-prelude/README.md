# A pool host's boot prelude

`prelude.sh` is a `server.pool.boot_prelude` for a pool of Firecracker hosts on AWS metal ([ADR-030](../../../../../docs/reference/decisions/adr-030-rented-bare-metal-pool.md), [the operating page](../../../../../docs/operating/bare-metal-pool.md)). A pool host runs it on its first boot, before the `billet join` the pool appends. It prepares the host with this collection's `host` role in `billet_node_prepare_only` mode and leaves the node's config and its start to the join.

It is an example to copy and render, not a script the collection installs. Where billet and the collection come from, the bucket, the image source and the disks are yours.

## What it does

1. **Installs billet's binary** at `/usr/bin/billet`, checked against the sha256 you render in. It never installs the package: the package's postinstall seeds `/etc/billet/billet.yaml` from its template, and the join leaves a config it did not write alone, so the host could not be joined. The role refuses that seed in this mode.
2. **Fetches the collection** at the commit you pin, and installs the pinned `ansible-core` and `ansible.posix` in a venv.
3. **Chooses its disks once and records them** in `/var/lib/billet-pool-prelude/disks.env`:
   - the record names each disk by a `/dev/disk/by-id` name, resolved again on every run, since a kernel name like `/dev/nvme1n1` is only the order the disks were found in at that boot. A disk with no such name is refused rather than recorded by its kernel name, and the record is written only after the whole scan and every name succeeded. It is synced to disk and renamed into place before either disk is changed;
   - the OSD takes `PRELUDE_OSD_DEVICE` (an alias is compared by the disk it names), else the first whole disk with no partitions, nothing mounted, no signature, and not the root's (an instance store's NVMe on metal). Blank means `wipefs --no-act` read the disk and listed nothing, the role's own test for a ledger volume. `blkid`'s "nothing found" is also its answer when it could not read the disk. Any probe that fails, a `wipefs` read included, fails the whole scan, which is tried again (five times), so a disk that could not be read just then is never left out of the record for good;
   - a second such disk, when there is one, is formatted only when it is blank by that test. It is used as it is when it holds exactly one ext4 filesystem (this prelude's, from a run cut short before the mount), and refused otherwise. It is mounted by its filesystem's UUID from `/etc/fstab`, with Docker's and Ceph's directories bound onto it there. The entries are `nofail`, so a disk gone after a reboot does not stop the boot. A drop-in makes Docker require both mounts, so neither Docker nor Ceph's daemons, which are its containers, starts on the root's empty directories. The disk holds Docker's data (cephadm's containers), Ceph's (the monitor warns below 30% free, which an 8 GB root reaches) and the guest image's staging. The staging holds the image's parts, its assembled archive and the decompressed image at once: 97 GB at its peak for the x86_64 image of 2026-11-08. A 40 GB disk filled, and the pull failed.
   - "First" is the kernel's listing order, not the order the disks were attached or their size. When the host's disks differ, set `PRELUDE_OSD_DEVICE`, so the staging lands on a disk large enough for it.
4. **Renders the role's inputs from the join's own config.** `billet init --join` is given the flags `server.pool.init_args` hands the join, and writes to a scratch path, never `/etc/billet/billet.yaml`. So the bridges, the pools and the kernel path the role prepares are the ones the join's config will name. (`--join` cannot be combined with `--emit ansible`.)
5. **Converges the role on localhost.** The settings:
   - `billet_node_prepare_only`, and the converge guard holder `pool-prelude`. The role takes the guard and never releases it, and a guard never expires, so the prelude releases it after the converge, however the converge ended. A host left holding it would refuse every upgrade for its life. After a converge that succeeded, a release that fails stops the boot;
   - a single-OSD Ceph bootstrap on the recorded disk, pool size 1;
   - the guests' egress limits the operating page recommends;
   - more retries for the role's downloads;
   - uplink shaping off;
   - the guests' bridges on 10.240.0.0/23. The role's default 172.31.0.0/24 would put an AWS default VPC's resolver, 172.31.0.2, behind the first guest bridge.
6. **Acknowledges the single OSD** (`mon_warn_on_pool_no_redundancy false`, `osd_pool_default_size 1`), and restarts Ceph's managers. One of the two that cephadm starts kept the default size it started with and reported `TOO_FEW_OSDS` until it was restarted. Until a pool site has a store, a pool host keeps only golden images, which are pulled again from the signed source, and per-job clones.
7. **Installs the guest kernel** the node's config names, from a pull that is retried. A pull installs an image's paired kernel and then refuses to import until the node has registered and recorded what its control plane accepts (#584). This pull fetches and decompresses the whole image first, so it takes as long, and as much staging, as the import itself.
8. **Leaves a transient unit,** `billet-pool-image-pull`, that waits for that record and then pulls and verifies the image.

Its output reaches the console the host source reads for the join's line, so no command that talks to a network writes there. What curl, apt, pip, Galaxy, the role, Ceph and the pulls say goes to logs in `/var/lib/billet-pool-prelude`. The script's own lines start `prelude:`, and a failure is reported by the line it stopped on; a local command that fails (a mount, say) may add its own error first.

Retries:
- each of these is retried, so one refused answer does not skip the join: the instance metadata requests, apt (update and install, whole, since its lock timeout does not cover the package lists' lock), pip (whole), Galaxy, the downloads, the disk scan, the role's converge (three times, whole), Ceph's settings and the kernel pull;
- a download attempt that falls under 1 KB/s for a minute, or runs fifteen minutes, is abandoned and asked again, and none begins after twenty;
- the kernel pull has no deadline of its own, so its attempts share a budget: `PRELUDE_BUDGET_SECONDS` from the prelude's start (default 5400, ninety minutes). Each attempt is cut off at what is left, and none begins with under a minute left. The join's token lives two hours from the order, and the host still has to enroll;
- before the pull, the staging directory's free room is checked against `PRELUDE_STAGING_GB` (default 100, in units of 10⁹ bytes, for the 97 GB the x86_64 image of 2026-11-08 needed). Too little is refused at once rather than retried until the budget is spent. Set it for another image or architecture. A `pull-*` directory a pull left when it was killed (a reboot, say) is reclaimed before the check, unless the image unit is running, since it is the other pull into that staging.

The S3 credentials reach curl on its standard input, never its arguments. They are written into its configuration only when every character is one a credential is made of, so no answer can add a configuration line of its own.

An image name with `@` (a tier's `ubuntu-2404-x64@verified`) is refused at the start. A pull assigns the generation itself and refuses a name that names one.

It may be run again on the same host:
- the disks come from the record, and a mounted scratch disk is reused;
- the preview config is rendered afresh;
- the role's converge is its own;
- an image unit that is running is left alone, and one that failed or finished is started again. The unit is transient, so a reboot before the node registers ends it, and running the prelude again starts it.

## Rendering it

Write the exports below to a file, append the script's body without its comment lines, and point `server.pool.boot_prelude` at the result:

```bash
{ cat exports.sh; tail -n +2 prelude.sh | sed '/^[[:space:]]*#/d'; } >/etc/billet/pool-prelude.sh
```

The comments are why each step is there, and they cost room the payload does not have. The script with them is about 21 KB, past the 16384 bytes the control plane carries (it refuses a longer boot, naming `boot_prelude`). Without them it is about 11.4 KB. No line the script needs starts with `#`: the full-line comments inside its two YAML heredocs are YAML's own.

The exports:

```bash
# A release's archive (or a bare binary); its sha256 is the archive's line in the
# release's checksums.txt. v0.36.0 is the first release whose role has this mode's fixes.
export BILLET_URL='https://github.com/junioryono/billet/releases/download/v0.36.0/billet_0.36.0_linux_amd64.tar.gz'
export BILLET_SHA256='…'
export COLLECTION_URL='https://github.com/junioryono/billet/archive/refs/tags/v0.36.0.tar.gz'
export COLLECTION_SHA256='…'
export PRELUDE_JOIN_SERVER='10.0.0.5:7717'   # server.pool.join_server
export PRELUDE_IMAGE='ubuntu-2404-x64'       # the tier's image, by its name alone (no @verified)
```

The collection tarball may hold `ansible_collections/` at its top (a tarball of that directory alone, as the run used) or one directory down (GitHub's source archive of a tag or commit).

Or, for artifacts in a private bucket, set `ARTIFACT_BUCKET`, `ARTIFACT_REGION`, `BILLET_KEY` and `COLLECTION_KEY` instead of the URLs. The script then reads them with the instance profile's own credentials, signed by curl, and the pool's instance profile needs `s3:GetObject` on those keys.

**Do not render presigned links signed by a person's session.** The control plane reads `boot_prelude` once, when it starts. Every order after the session expires fails with 400.

Also set `server.pool.init_args` to the same three flags the script gives `billet init --join`:
- `--kernel-image=/var/lib/billet/kernels/vmlinux`
- `--ceph-user=billet`
- `--ceph-keyring=/etc/ceph/ceph.client.billet.keyring`

The whole payload carries a join token, so keep it to the source's API, and within EC2's 16384 bytes of user data. The control plane adds the join's line and a few lines around the prelude, under 1 KB. The sizes:
- **The body without comments** is about 11.4 KB.
- **Exports naming release URLs or S3 keys** add a few hundred bytes.
- **A presigned link** adds about 1.9 KB. Two of them leave about half a kilobyte of room once the control plane's lines are added, so prefer S3 keys.

Check the rendered file's size (`wc -c`) before pointing the pool at it.

## What has been proven, and what has not

- **On AWS metal, 2026-11-09** ([the bare-metal hosts record](../../../../../docs/reference/records/bare-metal-hosts.md), "The pool on AWS metal, end to end"), a version of this script prepared a `z1d.metal` that was admitted with no person, registered, imported the guest image and ran jobs. **Not unchanged:** on that host, the OSD was added by hand, the payload was re-run after GitHub refused downloads, the image's assets were fetched with curl's retries and imported with `--from`, and the packaged node unit was installed before `local up`. The role (#816, #810) and this script now carry the fixes for each.
- **On nested-virtualisation `m8i.2xlarge` hosts, 2026-11-09** (Ubuntu 24.04, two spare EBS volumes, no control plane to join).

  Earlier versions, run five times on one host, found what the script now handles:
  - a `head` that ended a pipeline under `pipefail` (141);
  - a kernel lookup that stopped the boot on the first failed pull;
  - a 40 GB scratch disk the image's staging filled;
  - a Ceph manager that kept the old pool size;
  - the converge guard left held.

  The last two runs there fetched from S3 with the instance's own credentials, and after a reboot the mounts and Ceph came back.

  **The version before this one** (rendered the same way, 13.5 KB, its disks blank by `blkid`) ran twice on a fresh host. Each run printed only `prelude: host prepared`, and after a reboot Docker started after both mounts and Ceph was `HEALTH_OK`. On its second run GitHub reset the kernel pull's connection, and the run took the kernel the first had installed.

  **This version** was rendered as above (comments stripped, presigned links, 14.4 KB) on another fresh host, as its user data. `PRELUDE_OSD_DEVICE` named the smaller volume by its `/dev/disk/by-id` alias. It ran twice, then the host was rebooted, and it ran a third time:
  - **The console.** Each run exited 0, with `prelude: host prepared` the only line on the console.
  - **The disks.** The scan excluded the root disk, which booted as `nvme1n1` with the volumes on either side of it. The alias's disk took the OSD and the larger disk the staging, and the record kept both by their `by-id` names.
  - **The renumbering.** After the reboot the kernel had renumbered them: root `nvme0n1`, the OSD `nvme1n1`. The third run resolved the record to the OSD's new name and handed the role that.
  - **The rest.** The guard was released, Ceph was `HEALTH_OK` after every run and after the reboot, the kernel was installed within the budget's first attempt, and the image unit was left waiting for the node's acceptance record.

  **Not part of these runs:** a join, the node's registration, and the image's import.
- **Not proven:** a phoenixNAP host. It has no instance metadata service, so the address and the credential fetch here are AWS's.

