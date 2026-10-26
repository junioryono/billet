# Guest images

A Firecracker job boots a **golden image**: a root filesystem based on Ubuntu 24.04, carrying Docker, the GitHub Actions runner, and a small agent that reads the runner registration out of the metadata service and starts the runner with it. It lives in Ceph as an RBD image with immutable named snapshots called **generations**, and every job gets a copy-on-write clone of one, grown to the tier's `disk` before boot and discarded with the guest. billet builds the image once, centrally, and a deployment pulls it.

## What is in it

GitHub does not publish the image its hosted runners boot; it publishes the recipe. billet vendors GitHub's own `toolset-2404.json`, pinned by digest, and builds from it, so a workflow that assumes something a hosted runner has (`openssh-client`, a particular Python minor, five JDKs, the node/go/Python/PyPy/Ruby toolcache) finds it here, on x64 and arm64 alike. The image leaves out what billet may not redistribute (the Android SDK, the CodeQL bundle, Firefox, Google Chrome and Microsoft Edge), which a deployment that accepts them installs before it imports the image ([below](#components-the-image-leaves-out)), and carries what GitHub installs outside its declaration (Git LFS, yq, the AWS and Kubernetes tools, Podman, MySQL, PostgreSQL, Composer and the rest ADR-005 lists), as a hosted runner does, and is about 27 GiB used; [ADR-005](../reference/decisions/adr-005-runner-image-parity.md) records what parity costs, and [ADR-014](../reference/decisions/adr-014-guest-image-from-githubs-template.md) how the guest image builds from GitHub's own template, browsers and their drivers included.

## Pull, verify, promote

```bash
billet images refresh                       # pull, verify and reap only if the channel is newer than what is imported
billet images pull ubuntu-2404-x64          # signed manifest, staged and verified, then imported
billet images verify ubuntu-2404-x64@<gen>  # boot one, make the guest prove it, record it
billet images compatible                    # every configured image speaks this binary's contract
billet images list                          # what exists, what is verified, what tiers boot
billet images due                           # is the image old enough to rebuild (exit 2: no)
billet images reap --keep 3                 # remove generations nothing needs
billet images promote|unpromote <image>@<gen>
```

**A node takes up a new image by itself.** The package enables `billet-images-refresh.timer`, which runs `billet images refresh` daily: it reads the same signed channel a pull does, compares the image's build time with the newest generation imported (a generation is named for the moment it was imported, which is always after the build it came from, so nothing new has to be recorded to answer), and only when the channel's image is newer pulls, boot-verifies and promotes a generation and then reaps to three verified generations per guest contract. The same image is never imported twice. The comparison is by time, not by identity: an older image sideloaded on purpose after the channel's build reads as current until the channel moves again, which is the cost of needing no new cluster metadata. A host upgrade does the other half: with the services stopped and before the ledger is fenced, the candidate asks which configured images speak its guest contract and pulls a generation for each that does not, so a release that moves the contract never leaves `@verified` resolving to nothing. `release: {automatic: false}` in `billet.yaml` stops the refresh, and `--dry-run` says what it would do.

**A pull verifies the signature before it parses anything.** Every asset is checked against a digest the manifest names, which is worth nothing on its own: a manifest somebody else serves names digests of bytes they chose. So the pull first verifies the Sigstore signature over the manifest bytes against a pinned identity, billet's own publication workflow on its main branch (pinned to the workflow and the ref, not the repository, because pinning the repository alone would accept a signature from a workflow a pull request added). The trust root is embedded in the binary rather than fetched, because a node may be air-gapped. It stages to disk and verifies before importing, because streaming unverified bytes into shared storage is a cluster operation to undo. A pull also installs the paired kernel durably before it publishes the generation, so a power loss cannot leave a published generation whose kernel vanished.

**A source that is not billet's must say what makes it trustworthy.** Pointing at your own mirror and configuring nothing is refused rather than silently unverified: set `images.signing_identity` and `images.signing_issuer`, or pass `--skip-signature-verification` deliberately. Sideloading with `--from <dir>` applies the same policy; a directory is not more trustworthy than a download.

**Verification grows the disk the way a launch does.** A launch grows each job's clone to the tier's `disk` with resize2fs before boot, so a filesystem resize2fs refuses boots nowhere on the fleet. `billet images verify` therefore launches its probe through the same grow: to the largest `disk` any firecracker tier in the config asks for, to 80GiB when none does (a node config with no tiers), or to what `--disk` names. The guest reports the size of the root filesystem it booted, and a verification whose filesystem is not the grown one fails. A generation that booted and reported on an ungrown disk, and then failed every launch for about ninety minutes, is why (2026-09-26).

**`@verified` is what lets a fleet take up a new image with no config edit.** A tier names one of two things:

```yaml
image: ubuntu-2404-x64@g20260814145813   # exactly this, forever
image: ubuntu-2404-x64@verified          # the newest one proved to boot
```

Verification records itself, the next launch resolves to it, and rollback is `billet images unpromote`, one command against the cluster rather than an edit on every node. A bare image name is refused: naming `@verified` is the decision. If nothing has passed verification, `@verified` refuses rather than booting something unproven, and a generation whose snapshot is gone is never resolved. The launch log names the generation, not the word. `@verified` is contract-relative, so during a rolling upgrade an older binary keeps resolving its newest compatible generation while the new binary selects the one it just proved.

**A generation records the machine it was built for, and an image holds one.** A pull records the architecture its manifest names on the generation it imports (x86_64 or aarch64, as `uname -m` spells it), and refuses to import into an image unless every generation already in it is known to be of the same one, so one site's x64 and arm64 nodes never share an image by naming the same one: an arm64 node's image is `ubuntu-2404-arm64`. `@verified` never resolves to a generation of another machine than the node's, and a tier that pins one exactly is refused at launch rather than booted. A generation imported before billet recorded this records nothing and resolves as it always did; the next pull reads its architecture from the generation itself, mapping its snapshot read-only under the host's lock in `node.lock_dir` and reading the ELF header of `/usr/bin/env` with `debugfs` (from e2fsprogs) without mounting it, and records it. One that cannot be read stays unknown, the pull says why, and it refuses before it downloads anything: an import into that image waits until the cause is fixed, or the generation is unpromoted and reaped (reap keeps one a tier pins exactly until the pin moves), or the image is given a new name. `billet images compatible` refuses an exact pin of another machine's generation, so a host upgrade stops there rather than after it. `billet check` names every generation that records no architecture or another machine's.

**Verification records which kernel proved it**, under the same cluster lock reaping takes, because a generation the whole fleet takes up with no kernel recorded would boot against whatever each node happens to be configured with. A pull keeps the kernel in `/var/lib/billet/kernels` named by version and digest; `images reap` collects kernels no surviving generation names and refuses while any generation's kernel is unknown.

## Keeping images current

Nothing has to be pulled by hand on a host that follows the packaged units. `billet images refresh` reads the newest generation's import time off its name for every image this host's firecracker tiers boot, compares it with the build time the signed channel publishes, and when the channel is newer runs the same `billet images pull --verify` an operator would: import, boot, promote, under the kernel lock and the runner-deadline proof, then `billet images reap --keep 3`. When nothing newer is published it prints why and exits 0, so a daily tick costs one signed statement and one question of the cluster. `billet-images-refresh.timer` runs it daily with a randomised delay and `Persistent=true`. The package enables the timer, `billet local up` enables and arms it again on a firecracker node after the services are up and proved (a host converged from a tarball ran no postinstall), and the Ansible host role renders and enables it on every node that boots guests; on a host set up some other way, `systemctl enable --now billet-images-refresh.timer` is the one command, and `systemctl disable --now` the opt-out.

`billet check` on a firecracker node reports each image's newest generation and its age against the weekly cadence, and says plainly when the timer is not enabled, because a timer that was never enabled looks exactly like one that is working right up until GitHub refuses the runner baked into a stale image. `billet images due` asks the same question by age alone, for an operator deciding whether to build.

A host upgrade converges images on its own, separately from the timer: the transaction asks the candidate binary whether this host's images are compatible with it and pulls a generation for each that is not, after the services are stopped and before anything is fenced. See [upgrades](upgrades.md).

## Components the image leaves out

Five parts of GitHub's runner image come under licenses that forbid billet redistributing them, so billet's published guest image is to leave them out, and each deployment installs the ones it accepts, from their vendors, on its own nodes (#584). billet's build skips their installers, ships GitHub's installers and tests for them inside the image at `/opt/billet/complete`, and before it publishes completes a copy of the image with each one on the builder, which proves each completes and measures the space each needs; the signed manifest declares them (schema 3). An image built before the build began leaving them out carries all five and declares none. A deployment that has accepted nothing boots an image without them.

| Name | What it installs | A job without it | License |
|---|---|---|---|
| `android-sdk` | the Android SDK (command-line tools, platform tools, platforms, build tools, NDKs) at `/usr/local/lib/android/sdk`, with `ANDROID_HOME` set | an Android build fails unless it installs the SDK itself | [Android SDK terms](https://developer.android.com/studio/terms) |
| `codeql` | the CodeQL bundle in the toolcache | the CodeQL Action downloads the bundle in each job: slower, not a failure | [CodeQL terms](https://github.com/github/codeql-cli-binaries/blob/main/LICENSE.md) |
| `firefox` | Firefox from the mozillateam PPA, and geckodriver | a browser test that drives Firefox fails | [Firefox terms](https://www.mozilla.org/en-US/about/legal/terms/firefox/) |
| `google-chrome` | Google Chrome, ChromeDriver and Chromium | a browser test that drives Chrome fails | [Google Chrome terms](https://www.google.com/chrome/terms/) |
| `microsoft-edge` | Microsoft Edge and msedgedriver | a browser test that drives Edge fails | [Microsoft Edge WebDriver terms](https://developer.microsoft.com/en-us/microsoft-edge/tools/webdriver/eula) |

**Accepting one is one line of `billet.yaml`, and nothing else.** Writing a name accepts that component's license for this deployment. Write it in the control plane's file and every node takes it up: the control plane sends the list to each node when it registers, so a node needs no line of its own. A node also installs what its own file names, and takes up a change to the control plane's list at its next registration, which restarting `billet server` causes on every node. Until a node has registered once it cannot tell what its deployment accepts: a pull then installs only the image's kernel and imports nothing, and verify and promote refuse, so a fresh node's first pull gives it the kernel it needs to start, and the first pull after it registers imports, completes and promotes the image. Nothing is accepted unless it is named, and a name billet does not know is refused when the file loads. `billet init` writes `images.accept: []` on a Firecracker host, with each name and a link to its license above it. The packaged refresh timer does the rest; there is no other command to run.

```yaml
images:
  accept: [google-chrome, codeql]
```

**A new image is used only once it is complete.** When a pull imports an image that left components out, it installs each accepted one before the image becomes a generation. It boots a Firecracker microVM from the newest verified generation of the image, or, on a site's first pull, from the image as published, imported as a completion root that no tier can boot. It attaches the new image as the microVM's second disk, and inside the guest runs GitHub's own installer and tests for each component. The image is imported only if every accepted component installed, its tests passed and the filesystem then checked clean. Otherwise nothing is imported, and every tier keeps the generation it has. Vendor code never runs on the node itself, and the node never mounts a filesystem vendor code wrote. `billet images verify` and `promote` refuse a generation that lacks an accepted component, so `@verified` never moves onto one.

**Nothing is downloaded again for nothing.** Completion runs only when a generation is imported: when the channel publishes a newer image, or when the accepted set differs from what the newest generation was completed with. A week in which the channel does not move imports and installs nothing. Accepting a component later, or withdrawing one, makes the next refresh import the current image again with the new set, without waiting for the next weekly image. Each generation records what its image left out and the version of each component installed into it.

**What the versions are.** The installers run at the runner-images commit the image was built from and take each vendor's current release, as GitHub's own builds do: Google's apt repository serves only the current Chrome (one version, measured 2026-10-18), while Microsoft's keeps about two hundred versions of Edge. A browser and its driver install together, and GitHub's own tests, run before anything is imported, prove their major versions match. An image declaring its components through a completion contract this billet cannot run, or written in a manifest schema it cannot read, is refused before anything is downloaded, and the tiers keep booting what they have until a newer billet takes it up. With `release.automatic: false` neither billet nor its images update by themselves.

**What `billet check` says.** On a Firecracker node, for each image the tiers boot, it reports:

- the generation they boot, and the versions it was completed with;
- what its image left out that this deployment has not accepted, which a job will not find;
- what it lacks that the deployment accepts (a component accepted since it was imported), and what will complete it: the refresh timer, a pull by hand when `release.automatic` is false, moving the pin of a tier that names an exact generation, or verifying a newer generation that already matches, which a refresh does not retry while the channel is unchanged (when that generation speaks a guest contract this billet does not, it says a billet that speaks it is what boots it);
- a tier that names a generation that does not exist;
- with its network probes, whether the channel's image is one this billet can import, and if it needs a newer billet, whether this deployment installs one by itself (not with `release.automatic: false`, and not while `release.version` pins it).

**A node behind a firewall.** The completion microVM leaves through the same egress as a trusted job. GitHub's installers download from these hosts, read from the installers at the pinned runner-images commit on 2026-10-20 rather than measured from a completion. Every component also needs the Ubuntu archive for the packages it depends on.

| Component | Hosts |
|---|---|
| `android-sdk` | `dl.google.com` |
| `codeql` | `api.github.com`, `raw.githubusercontent.com`, `github.com`, and the `githubusercontent.com` hosts its release downloads redirect to |
| `firefox` | Launchpad, for the mozillateam PPA and the key `add-apt-repository` fetches (`ppa.launchpadcontent.net`, `api.launchpad.net`); `api.github.com` and `github.com` for geckodriver |
| `google-chrome` | `dl.google.com`, `googlechromelabs.github.io` (the Chrome for Testing list), the ChromeDriver download that list names, and `www.googleapis.com` (Chromium) |
| `microsoft-edge` | `packages.microsoft.com`, `msedgedriver.microsoft.com` |

## Notices in the image

The image redistributes what GitHub's template installs, so it carries the license of each tool whose license asks for its text with every copy. `/opt/billet/IMAGE_NOTICES` names each tool installed outside apt that asks this, and where its license is in the image:

| Tool | License | Where its text is |
|---|---|---|
| The GitHub Copilot CLI | GitHub Copilot CLI License | `LICENSE.md` beside the binary in the toolcache (`copilot-cli/<version>/x64/`), fetched by the build from the tag of the version installed, because the release tarball holds only the binary |
| The agentic workflow firewall bundle | MIT | `LICENSE` beside each `awf-bundle.js` in the toolcache, fetched from the tag of its version |
| Miniconda | Miniconda End User License Agreement | `/usr/share/miniconda/LICENSE.txt`, where its installer puts it |
| Packer | Business Source License 1.1 | `/usr/local/bin/LICENSE.txt`, where its zip puts it |

A package installed with apt carries its copyright and license in `/usr/share/doc/<package>/copyright`, billet's own third-party notices are in `/opt/billet/THIRD_PARTY_NOTICES`, and the container hook's are beside it in `/usr/local/lib/billet/container-hook/`. The publication gate refuses an image that carries one of these tools without its license, or without the notices file naming it.

**The source manifest.** `/opt/billet/IMAGE_SOURCES.tsv`, published beside each release as `image-sources.tsv`, lists every package installed with apt: its version, the source package and source version it was built from, each site apt could fetch that version from when the image was built (`none` when only dpkg's own record has it, as for git from its PPA, which GitHub's template removes after installing), and `gnu` when its copyright file names a GNU license, `other` when it names none, or `unclassified` when the package has no readable copyright file. The build writes it inside the image while apt still has every source's lists, and the publication gate refuses an image whose manifest does not list exactly the packages dpkg installed, at their versions. It is what corresponding source for a GNU-licensed package has to be found from: a version from Ubuntu's archive stays downloadable from Launchpad after the archive supersedes it, and one from a PPA does not (measured 2026-10-21).

**The corresponding source.** Each guest release carries, as its own assets, the source of everything the image redistributes under a GNU license ([ADR-019](../reference/decisions/adr-019-guest-image-corresponding-source.md)). `image-sources.tar`, in the parts `image-sources.parts` lists with their sha256 (joined in order, as the image's parts are), holds two things:

- every source package `image-sources.tsv` marks `gnu` or `unclassified`, at the exact version the image installed, as its `.dsc` and the files it names;
- the Linux tarball and the configuration the guest kernel was built from.

`image-sources-index.tsv` lists every file in it with its sha256 and size. The build fetches each from where that version is kept: Ubuntu's archive and the template's PPAs through Launchpad, PostgreSQL's packages from `apt-archive.postgresql.org`, and each Temurin JDK's source tarball from Adoptium's release. It checks each against the digest its own description gives, and a source it cannot fetch and verify fails the build. The scripts that built the image are the repository at the commit the release names. The six vendor packages that ship no copyright file are reviewed by hand in `scripts/runner-images/reviewed-licenses.tsv` (Apache-2.0 or MIT, each with its evidence) and need no source. About 3.4 GB of source accompanies each release (measured 2026-10-21).

`/opt/billet/IMAGE_NOTICES` also records two settings each deployment should know about. `ACCEPT_EULA=Y` is set for every job, as on GitHub's hosted runners, so a package that asks for its license to be accepted when it installs is accepted on the job's behalf. And Anaconda's terms of service govern its own package repository, the `defaults` channel conda uses unless told otherwise; read them before an organisation uses it.

## Where images come from

The built-in source reads a signed pointer from billet's `release-channel` branch, verifies that the main publication workflow signed a still-current pointer (it expires after ten days, so rewriting the branch cannot pin a fleet to an old genuine channel) naming an immutable dated `guest-YYYYMMDD-HHMMSS` prerelease, then downloads that release. The rootfs is published in parts because a release asset caps at 2 GiB. Every image is built weekly by `.github/workflows/guest-image.yml` from the tree of the billet release the stable channel names (never from `main`, which a fleet should not inherit unreviewed), at whatever `actions/runner` GitHub has published, and passes two gates before it is published: its **contents**, by loop-mounting the filesystem (the runner is the version the manifest claims, Docker with Buildx and Compose, the agent's contract, the units enabled, root locked), and that it **boots** under Firecracker on the runner that built it, proving the kernel, systemd, the network, the metadata service and the agent in one sentence on the console.

**The kernel and the filesystem are a matched pair.** A guest booted with a different kernel fails in the middle of somebody's job, so they are published together and the generation records which kernel it was paired with. The kernel is built from `scripts/guest-kernel.config`, every option in `scripts/kernel/required-builtins.txt` built in (a microVM has no initramfs), and checked against moby's vendored `check-config.sh` before publication.

`scripts/build-guest-image.sh` remains, for a custom image or an air-gapped build. It is no longer the normal path; a per-node rebuild timer was removed because it made N machines do N builds of a byte-identical artifact.

## The runner-release deadline

GitHub refuses a self-hosted runner about thirty days after a newer `actions/runner` release, and billet's runners cannot update themselves (every JIT registration disables auto-update). `billet runner check` reports how close the runner on your image is to being refused: exit 0 while there is nothing to do, 2 once a rebuild is due, 3 once GitHub is already refusing. Failing to reach GitHub is an error, not one of the three.

**The clock starts at the first release newer than yours, not at the newest one.** Counting from the newest moves a deadline that has already passed every time something else ships: 2.334.0 went out of date when 2.335.0 was published on 2026-06-08, and counting from 2.336.0 on 2026-07-20 described a fleet GitHub had refused for six weeks as having a month in hand. The check reads the runner version the image itself recorded, because the image is the only thing that knows what the fleet is running. It never states a deadline it cannot place, and thirty days is the ordinary window rather than a promise, since GitHub may enforce a critical release at once. `images pull` asks the same question about the image it imports and refuses only a proved expiry (`--allow-stale` overrides); image age is reported as maintenance information and decides nothing. A daily workflow opens a pull request when a runner release lands, and merging it is the gate.

## On EC2

The `ec2` backend boots an AMI `billet ami build` makes by running the same template on an EC2 builder, completed with the components `images.accept` names, verified by booting it, and stamped with a contract tag only after the boot proved the properties. An AMI is region-scoped. [AWS with EC2](../deploying/aws-ec2.md) covers it; `billet images` does not manage AMIs.

## On a Mac

A tart node pulls what its tiers name with `billet images pull`, into tart's own per-user store; a launch refuses an image that is not present rather than fetching tens of gigabytes inside a job. Guest images billet builds for a Mac are still open ([Run jobs on a Mac](../deploying/mac-tart.md)).
