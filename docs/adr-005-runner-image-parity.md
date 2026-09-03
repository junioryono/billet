# ADR-005 — Parity with GitHub's runner image is a rebuild, not a copy

Status: accepted, and being implemented in stages. On the Firecracker guest: the apt set, every declared toolcache line, the five JDKs and the image environment have landed and are validated by a real build that boots. The EC2 AMI now builds from the same declaration and runs the same installers — one file, sourced by the guest build and carried to the builder — with the apt set validated by a real build and the toolcache not yet. The remaining heavyweight software has landed on neither.

## The question that started it

A workflow that runs on `ubuntu-latest` and then on a billet tier should behave the same way. It did not, and the gap was not a bug anywhere — it was that billet's guest image carried about twenty-five apt packages and three toolchains against GitHub's seventy-four and six. A workflow cloning over ssh, verifying a signature, reading a timezone or unpacking a 7z archive worked on a hosted runner and failed here, and none of those failures says anything about the image.

The obvious fix — boot GitHub's image — is not available, and establishing that is what decided the design.

## GitHub publishes the recipe and the manifest, never the bits

`actions/runner-images` is **source**. Its templates are HCL2 for Packer, and their only builder is `azure-arm`: running them produces an Azure managed image in the builder's own subscription. There is no VHD, qcow2 or rootfs anywhere.

Checked directly against the releases API rather than inferred: **every image release's only asset is a ~50KB `internal.<image>.json`.** Not a disk image; a description of one.

What *is* published is enough to rebuild:

- **`images/ubuntu/toolsets/toolset-2404.json`** — machine-readable, and the load-bearing artifact for this work. Every apt package, toolcache entry, JDK, NDK, Docker pin and tool version.
- **About eighty plain bash installers** under `images/ubuntu/scripts/build/`, which Packer only orchestrates.
- **A `releases/<image>/<date>` branch per release**, carrying the full software manifest at that exact version.

All MIT.

So anyone matching that image is rebuilding it. **Blacksmith included** — their docs say the Windows image is *"based on GitHub's official runner image but with some exclusions"* (no Visual Studio, no EdgeDriver, no Cosmos DB emulator), and you can only exclude from a build you run yourself. Their Linux claim of "the same image" is parity-by-rebuild, and their technical page says the accurate thing: *"pre-installed with the same dependencies supported by GitHub's official runner image."*

## The decision

**Vendor the declaration, pin it by digest, and build from it — in both backends.**

`internal/runnerimages` holds `toolset-2404.json` at a named upstream commit, with the commit and the file's digest on one line of `pinned.txt`. Both the Go side and `scripts/build-guest-image.sh` read that same file, the shell through `jq`.

Three properties follow, and each replaced something worse:

**Pinned, not tracked.** An image is a thing you reproduce; reading upstream `main` at build time would make two runs of the same command produce different images. This is the argument `billet ami` already makes about the runner version.

**One declaration, two backends.** The guest image and the AMI carried two hand-maintained package lists in two languages. That is the shape of the bug `runnerrelease` exists to prevent — a runner bump that updated one and not the other leaves exactly one stale fleet, found on the day GitHub stops queueing to it.

**The digest is checked on both paths that read it.** The Go side verifies before parsing; the shell verifies before building. A check on only one leaves the thing that actually produces the image trusting whatever is on disk, and an edit to this file is an edit to every image built afterwards.

### What billet deliberately does differently

- **`--no-install-recommends`.** runner-images installs with recommends onto a full cloud image. Here every recommended package is permanent size in a file every node downloads and every job clones.
- **billet's own packages are a separate list.** Docker, `iproute2`, `netplan`, `dnsmasq-base`, `libicu74`, `e2fsprogs` — the guest *mechanism*, none of it on GitHub's list because GitHub's image is not a microVM guest, and none of it removable when the declaration changes.
- **Lines from the declaration, patches from the vendor.** A toolcache is a cache rather than a contract: `node-version: 22` does not care which patch it finds, so pinning would ship stale runtimes for a reproducibility nobody wants. Every download stays checksum-verified against what the vendor published for the version resolved.

## What this costs, and where it lands

Stated plainly because these are the decisions an operator inherits.

**Storage is the dominant cost, and the artifact sizes are measured on both backends.** Each usage figure below was read from a machine booted off the artifact, or from the build's own report. The comparisons and retention totals that follow are arithmetic on those figures, not separate measurements — and the units are binary throughout, since the build reports MiB.

| | measured used | toolcache |
|---|---|---|
| EC2 image, x64 | 26.8GiB | 5.2GiB |
| EC2 image, arm64 | 24.9GiB | 3.1GiB |
| Guest image | 15.0GiB (15392MB) | — |

Two of those differences are worth reading rather than skimming. The **2.1GiB between the architectures** is not waste: it is CodeQL, which publishes no arm64 bundle at all, and PyPy 3.9 and 3.10, which ship aarch64 tarballs their own checksums page does not cover. Both are recorded in `.billet-unpublished` rather than silently absent, which is what makes the gap legible instead of alarming. The **~12GiB between the guest and the AMI** is the Android SDK and its three NDKs, which the guest deliberately does not carry — `BILLET_TC_ANDROID_ACCEPT_LICENSES` is set by the AMI build and not by the guest build, because building in the operator's own account is use and publishing this image as a release asset is redistribution.

For retention, the guest figure is the one that matters, since that is what Ceph holds. `cmd/billet/images.go` defaults to `--keep 3` and the reference host's pools are `size=2`, so three generations is about **90GiB** of content (15392 MiB × 3 × 2). That is **per guest contract, not per cluster**: `PlanReap` counts kept generations with `keptByContract[contract] < keep.Keep`, and pinned generations survive on top of that. During a contract migration two contracts are live at once, so the figure to size against is nearer 180GiB plus pins — which is the number that matters and the one a single-contract reading understates.

`SIZE_MB` is 22528, so the declared upper bound is 132GiB per contract; RBD is thin-provisioned and zstd compresses the unused remainder, so real occupancy tracks the 90GiB rather than the declaration. Either way the conclusion the first version of this section drew now has a number behind it: retention stops being an afterthought, and `--keep 3` on a host also carrying a colocated cache is a decision to make deliberately rather than inherit.

**Every concurrent job occupies at least the image size.** `growRoot` is grow-never-shrink, so a tier asking for less than the image already works — it silently gets a clone the size of the image. That is a capacity-accounting change rather than a failure, but it is real.

**Cold reads are the measured expensive case.** ADR-003 recorded a cold read of 40,000 small files at 4.07x on RBD versus local. Hydrating a parity-sized image is exactly that shape.

**Publication no longer fits in one release asset.** GitHub caps a single asset at 2 GiB (1000 assets per release, no total limit), and a parity image packs well past it, so the root filesystem is published as ordered parts. Manifest schema 2 describes them.

The migration is deliberately not a flag day: **the reader learned schema 2 and shipped a release before the writer emits it.** A reader accepts exactly the schemas it knows, so publishing a new layout in the same change that teaches the reader about it makes the next release unreadable to every deployed binary — and the thing that would fix them is the image they can no longer pull. The schema also follows the artifact rather than a switch: an image that fits in one asset stays schema 1 and stays readable by everything already in the field.

**The whole-file digest is what makes reassembly safe**, and per-part digests do not replace it. Each part's digest proves that piece is one the publisher signed for and says nothing about ORDER — an off-by-one, a listing where `part10` sorts before `part2`, or a retry that appended twice all produce a file made entirely of signed bytes that is a different filesystem.

**The build host had to move.** The workflow ran on a hosted `ubuntu-24.04`, asserted 20GB free, and reclaimed space by deleting `/usr/share/dotnet`, `/usr/local/lib/android` and `/opt/ghc` — precisely what parity installs. The build also writes into a mounted filesystem now instead of assembling a tree and copying it in, which removes a second full copy of the image.

## What is not covered

- **Windows and macOS.** GitHub publishes toolsets for them; billet's guest image is Linux only, and `need_tools` refuses to build anywhere else.
- **arm64 on the GUEST image.** `check_host_arch` refuses a non-x86_64 host because the pinned runner is `linux-x64` and debootstrap takes the host's architecture — a mismatch boots perfectly and fails when the agent execs the runner, inside a guest nobody has a console for. Teaching it to select the runner and debootstrap architecture together also touches the kernel build, and is not done.

An arm64 **AMI** does carry the toolcache, and what that cost is worth recording, because the naive version of it is wrong six times over. Every vendor spells the architecture differently and none of the spellings is derivable from another:

| | x64 | arm64 | |
|---|---|---|---|
| node, Python | `x64` | `arm64` | the `@actions/tool-cache` spelling |
| go, temurin | `amd64` | `arm64` | dpkg's, one in a filesystem path |
| pypy | `x64` | `aarch64` | the kernel's name, for one arch |
| ruby | *(bare)* | `-arm64` | x64 carries no suffix at all |
| codeql | `linux64` | — | no arm64 bundle is published |

Two of those are traps in opposite directions: pypy calls arm64 `aarch64` where every neighbour says `arm64`, and ruby-builder suffixes arm64 while leaving x64 bare — which is why upstream's own `-${arch}` pattern resolves nothing for x64.

**Two lines are absent on arm64 for reasons that belong to the vendor**, and both are recorded rather than silently skipped, so the gates still require every declared line to be installed or named in `/opt/hostedtoolcache/.billet-unpublished` exactly:

- **CodeQL** publishes no arm64 bundle at all.
- **PyPy 3.9 and 3.10** ship aarch64 binaries their checksums page does not cover. Measured: the page carries 115 aarch64 lines and none is the release billet resolves for those two, while 3.11's is. Nothing unverified is baked, so the entry is absent either way; recording it is what stops that being invisible, and refusing outright would mean no arm64 image could be built for a gap that is not billet's.

A page that yields **no** digests at all is still fatal, because recording every line would ship an image with no PyPy past a gate that accepts the record. That three-way distinction — published, published-but-unfindable, and unpublished — is the same one Ruby's asset resolution needs, and it has now been required three times in this file.
- **The remaining heavyweight software** is now Swift, Rust, Haskell, and browsers with their drivers. .NET, PowerShell and its four modules, CodeQL, cmake, the compiler sections and the Android SDK with three NDKs have all landed, alongside the five JDKs with Ant, Maven and Gradle. Android is installed on the EC2 image only: `BILLET_TC_ANDROID_ACCEPT_LICENSES` is set by that build and deliberately not by the guest build, because building in the operator's own account is use and publishing the guest image as a release asset is redistribution. The gate reports what is missing rather than passing silently.
- **Licensing has not been resolved and is a human decision.** Chrome, Edge and the Android SDK carry redistribution and click-through terms. GitHub builds them into images it runs itself; billet would publish them as a downloadable release asset, which is a materially different act. This must be settled per component before that software is added, and the outcome recorded here.

## One implementation, carried two ways

The four toolcache installers are ~650 lines of bash where nearly every line is a trap that cost a debugging session: `x64.complete` is a *sibling* of the arch directory and its absence makes a complete entry invisible; `setup-python` looks for `Python` while `setup-node` looks for `node`; go publishes an initial release as `go1.26` rather than `go1.26.0` and `@actions/tool-cache` skips a directory that is not an explicit semver; Temurin's field is `SEMANTIC_VERSION` and three of its five releases are not valid semver; python's tarball has no pip until `ensurepip` runs offline from the wheel it bundles.

Writing a second copy for the EC2 backend would have copied every one of them, which is the two-pins problem `internal/runnerimages` exists to prevent. So there is one file — `internal/runnerimages/install-toolcache.sh` — which `scripts/build-guest-image.sh` sources from disk and which `billet ami build` carries to the builder in the provisioning script.

**The seam is a single function.** `billet_tc_run` is `chroot "$BILLET_TC_ROOT" "$@"` for the guest build, which assembles a filesystem it is not running, and `"$@"` for the AMI build, which *is* the target. The rest of the contract is six paths the caller states, and the entry point refuses an incomplete one rather than writing several GiB of runtimes somewhere nobody chose.

**How it travels was measured, not estimated.** EC2 caps user data at 16384 bytes. base64 of the installers and the declaration gzips to 27235 — over by more than ten kilobytes, because base64 costs 33% *before* gzip and destroys the redundancy gzip lives on. The same bytes in quoted heredocs, with whole-line comments stripped, land at 11327. The strip has to be the whole-line one: `s/#.*$//` eats `${v#v}` and `${version#go}`, which are parameter expansions, and the result does not parse.

That budget is shared, which is why `maxCACertPEM` came down from 32 KiB to 8. With the toolcache present a 32652-byte bundle takes the payload to 24239 compressed — the old bound promised something every such build would refuse.

## Why the gates iterate the declaration

`check-guest-image.sh` walks the **expected** set — the pinned toolset — and names what is absent.

The direction is the whole value. A gate that walks what is installed can report what is there and can never report what is missing, and "missing" is the entire failure mode here: a workflow pinning `python-version: '3.10'` finds nothing, downloads an interpreter, and succeeds. Counting entries does not help either, because the other four lines make the count non-zero.

An empty declaration is its own failure rather than a pass, because a toolset that parsed to nothing would otherwise make the gate report success over an image containing no packages at all. This project has twice shipped a check that passed against the exact bug it was written for, and both had that shape.

### Inspecting an image is not a filesystem read, and that was measured

The gate mounts the image and executes things inside it, and **a chroot without `/proc` cannot run the two toolchains it most needs to check**. `go env GOVERSION` reports *"'go' binary is trimmed and GOROOT is not set"* and the JDK launcher fails to load `libjli.so` — both resolve their own installation through `/proc/self/exe`, so neither can find files sitting correctly beside it.

The first version of the gate had no `/proc` and reported every go entry and every JDK as broken **on an image where all of them worked**. That is the worst kind of gate: one that fails correct artifacts, because the fix people reach for is deleting the check.

Two rules follow. `/proc` is bind-mounted read-only, `nosuid,nodev,noexec` — exposed only so a binary can read its own path — and it is unmounted **before** the image, since a nested mount makes the outer `umount` fail with *"target is busy"* and leaves the image file held open for whatever wants to upload or delete it next. The same reasoning is why the JVM check passes `-XX:-UsePerfData` and the python check runs `--version` rather than `pip cache dir`: the mount is deliberately read-only, and a test that depends on writing is a test about the mount rather than about the image.

This is the same shape as the jailer's chroot path and `tart list` in `CLAUDE.md` — a property that reads as obviously true until it is run against a real artifact. It only surfaced on the first image that got far enough to be checked.

**It lives here rather than in `CLAUDE.md` because it is currently about one artifact.** That file's gotcha list is for traps that cut across the repo, and this one is specific to inspecting a mounted guest image. The AMI gate below did NOT generalise it — it boots the image instead of mounting it, so none of the chroot reasoning transfers — which is the outcome that paragraph was written to make somebody decide rather than drift into.

## Verifying the AMI is a boot, not a mount

Everything the build asserts about an AMI runs **on the builder, before `CreateImage`** — on a machine that has been apt-installed, part-configured and never rebooted. That is not the machine the image produces, and the difference is not academic. The Docker gate asserted `docker info -f '{{.Driver}}'` on the builder and read `overlayfs` where the image reports `overlay2` on a fresh boot: apt leaves the daemon running, so `systemctl start` was a no-op and the daemon never read the `daemon.json` the build had just written. The image was correct the whole time and the gate failed every build. Restarting fixed it, and the general form is what matters: anything a service reads at start, anything cloud-init does at first boot, and anything a job's own `env -i` can or cannot see are all invisible from the builder.

So `billet ami build` boots what it made. `RunInstances` from the produced AMI with user data that asserts and reports, `GetConsoleOutput`, terminate — the same shape of signalling the build already uses to know provisioning finished, and it needs no key pair, no SSM agent and no inbound access. `billet ami verify <ami-id>` is the same function against an image that already exists, which is what makes a failed verification recoverable: billet speaks no `DeregisterImage`, so without it the only retry is another paid builder.

**The contract tag is the promotion.** `CreateImage` stamps the owner and the billet that built the image — both true the instant it returns. Whether the image *meets* `AMIContract` is a fact about the artifact, so it is written afterwards with `CreateTags`, once billet has booted the image and asserted on it. An unverified or failed image carries no contract tag, which both readers already treat as "no answer, rebuild"; stamping the previous contract instead would have asserted the Docker property that is the most likely thing to be wrong.

**What the verifier asks is scoped to the contract, and to the architecture the image reports.** Free space, because `growpart` is a first-boot property; the Docker driver and data root, because that is what contract 1 says; and at contract 2 the whole toolcache gate, with every binary executed **through the same `setpriv … env -i` the image's own entry point uses**. That last one is the assertion the builder structurally cannot make: it runs as root with an inherited environment, and present on disk is not the same as findable by a job.

**The console lags, and that is the trap.** EC2 posts buffered console output around a state transition rather than continuously, and the live read (`Latest`) is documented as Nitro-only. Two hand-run probes powered off before AWS had flushed — and an empty console is indistinguishable from a boot that printed nothing, so the failure is intermittent and teaches whoever meets it to re-run rather than to look. The verifier therefore reprints its report on a dwell and billet terminates it the moment a complete block is read, so the instance is still running while the console is being asked. The report is bracketed by a per-run nonce, so a block billet reads is this run's rather than a stale or echoed one.

### What the first live run measured

`billet ami build` ran end to end in us-west-2 on 2026-08-28, from Canonical's current noble AMI, and produced `ami-0af6ca1a9ff63a09a`. **That run predates `--payload-bucket` becoming required**, so the command as invoked then no longer works: the installers and the pinned declaration render to 17077 bytes compressed against EC2's 16384-byte cap, the embedded delivery path is gone, and a build now needs a bucket to stage them in. The timings below still describe the work; the invocation needs `--payload-bucket <bucket>` added.

| | |
|---|---|
| provisioning (launch → self-stop) | 3m54s |
| `CreateImage` → image available | 11m06s |
| verifier launch → a complete report | 4m40s |
| standalone `billet ami verify` on the same image | 4m09s |

The verifier reported, from a machine booted off the artifact:

```
verdict=ok step=done
docker_driver=overlay2  docker_root=/var/lib/docker  docker_server=29.1.3
root_total_kib=29378688  root_used_kib=10785452  root_free_kib=18576852
runner=2.336.0  toolcache_kib=5360256
node 22.23.2 24.20.0 | go 1.24.13 1.25.14 1.26.7
Python 3.10.21 3.11.16 3.12.14 3.13.15 3.14.7
Java_Temurin-Hotspot_jdk 8.0.504-1 11.0.32-9 17.0.20-1 21.0.12-1 25.0.4-1
PyPy 3.9.19 3.10.16 3.11.15 | Ruby 3.2.9 3.3.9 3.4.6 | CodeQL 2.26.4
```

Four things that were inferences before that run, and are not now. **`docker_driver=overlay2` on a fresh boot** is the property the whole exercise is about — the in-build gate read `overlayfs` off the builder's own daemon for this exact image shape. **`Latest=true` was accepted** on the `c7i.large` the verifier picks, so nothing fell back to the buffered read. **The contract tag is written by the promotion**: the finished AMI carries `sh.billet.owner`, `sh.billet.built-by` and `sh.billet.ami-contract=2`, the last added after the boot. And **both machines terminated** — `describe-instances` on the owner tag shows the builder and the verifier terminated and nothing else.

The 4m40s is not console latency. The script runs about twenty-five tool invocations through the privilege drop and a `du` over 5.1GiB of toolcache before it prints anything, so most of it is work; the console's own lag is still unseparated and still unmeasured.

**The toolcache has nearly doubled since the figures elsewhere in this file.** 10.3GiB used of 28GiB usable against the 7GiB recorded when only node, go and Python were baked, with the toolcache alone now at 5.1GiB. The disk defaults still hold with 17.7GiB free, but the margin is smaller than the older comments imply.

**The build did not settle the IAM finding, so it was measured separately.** That run used an SSO admin role, which never evaluates the bundled builder policy. The measurement, on 2026-08-29, was two experiments rather than one, because the claim has two halves and a simulation can only reach the first.

The policy half, with `iam:SimulateCustomPolicy` against the document `billet init iam --builder` emits:

| | own resource | foreign resource |
|---|---|---|
| `ec2:CreateTags`, `ec2:CreateAction=CreateImage` | allowed **with** the grant, implicitDeny **without** | — |
| `ec2:CreateTags`, `ec2:CreateAction=RunInstances` | allowed either way (the control) | — |
| `ec2:GetConsoleOutput` on an instance | allowed | implicitDeny |
| `ec2:CreateTags` on an image (the promotion) | allowed | implicitDeny |

The service half — whether EC2 *asks* that question at all, which no simulation can show — with `create-image --dry-run` under a role holding the policy, against a real instance carrying the builder owner tag:

```
without a TagSpecification     DryRunOperation        "Request would have succeeded"
with one, grant absent         UnauthorizedOperation  "not authorized to perform: ec2:CreateTags"
with one, grant present        DryRunOperation
```

Same policy, same instance; the only variable is whether tags were sent, and EC2 names `ec2:CreateTags` in the refusal. So the separate create-time tag check is real, the bundled policy did deny every build before the fix, and the grant is what satisfies it. Nothing was created: `--dry-run` answers the authorization question and stops.

The `GetConsoleOutput` row is worth reading twice. A review round graded its instance-scoped ARN a P0 on the grounds that the action supports no resource type, and proposed widening it to `"*"` — which would hand the builder role every instance's console in the account. An unscopable action would have denied the own-resource case too; it allowed it and denied the foreign one.

**What it does not prove**, stated rather than discovered: the nonce makes a block this run's, not true. An image that prints `verdict=ok` without doing anything passes — exactly as a base image whose own policy powers the machine off satisfies the build's success signal. This is a check against mistake, and the trust it rests on is the operator's own image, which is the trust the build already rests on. So nothing off the console is quoted verbatim: billet reads a bounded number of `key=value` lines whose keys it emitted and whose values are printable ASCII, and the step label it renders back comes from a closed set billet wrote into the script.

**One observability note belongs here.** A successful build usually ends in a deregister, so success and failure are indistinguishable from `describe-images` alone — two separate sessions misread an empty listing on one day, one as a failed build and one as nothing having happened. The instance names are the discriminator: a `*-builder` alone means the build never reached `CreateImage`; a `*-builder` plus a `*-verify` means it did.
