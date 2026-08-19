# billet

Self-hosted GitHub Actions runners on your own hardware, with a colocated cache.

A *billet* is a bar of metal prepared for forging — raw material shaped into something useful, which
is roughly what CI does to source code.

> **Status: pre-alpha.** Nothing here is production-ready yet. Do not point release or deploy
> pipelines at it. See [Status](#status) for what actually works today.

## The idea

**Your own hardware for the builds, the cloud for when it is not there.**

A box under your desk is the cheapest fast CI you will ever have, and the reason people do not rely
on it is that houses lose power and ISPs go down. billet is built so that one `runs-on` label can
mean "the machine at home if it is up, EC2 if it is not" — the control plane lives somewhere always
on, and the compute is wherever you have it.

That combination is the thing billet is FOR. Kubernetes-based autoscalers do not span bare metal and
cloud; the AWS-based projects are AWS-only; the microVM products are commercial. See
[Alternatives](#alternatives) for an honest comparison, including cases where you should use
something else.

> The cloud path has run real GitHub Actions jobs on EC2, including Docker builds, service containers and runtime toolchain installation, and destroyed every instance afterward. The same unchanged `runs-on` label has also completed first on preferred bare-metal capacity and then on EC2 after that local contribution was withdrawn ([#32](https://github.com/junioryono/billet/issues/32)). A tier can name several backends, capacity is measured per machine, and the control plane picks the host when the job is admitted — in the tier's own order of preference, so `[firecracker, ec2]` means the box at home before the cloud. See [Status](#status) and [AWS acceptance](docs/aws-acceptance.md).

## What it is

`billet` is **being built** to run your GitHub Actions jobs on machines you control — a server under
your desk, a Mac mini, or EC2 — with the accelerations that make self-hosting worth the trouble.
Most of the following does not work yet; see [Status](#status) for what does.

- **Ephemeral microVM per job.** Firecracker on bare metal, one job per VM, destroyed after. Stronger
  isolation than container-based runners.
- **Colocated Actions cache.** Cache traffic served from the same box as the runner instead of
  crossing the internet.
- **Persistent build caches** — Docker layers, package managers, git mirrors — kept on copy-on-write
  volumes instead of being re-downloaded every run.
- **Observability** — job history, machine metrics, logs, and test analytics, without shipping your
  CI data to a third party.

There is **no hosted control plane and no SaaS component.** You run the whole thing. `billet` talks
to GitHub over outbound long-poll, so **GitHub never connects to you** — no public IP, no webhook
endpoint, no tunnel. A single-box deployment opens nothing at all. A fleet is the one exception, and
it is a local one: nodes dial the control plane, so it has to listen somewhere they can reach —
normally a private network or a VPN rather than the internet.

## What it is not

- **Not a way to run untrusted code cheaply.** See [Security](#security) — this matters.
- **Not a managed service.** If you want someone else to run it, use
  [Blacksmith](https://blacksmith.sh), [Actuated](https://actuated.com), [Namespace](https://namespace.so),
  or [WarpBuild](https://warpbuild.com). They are good products; this is for people who want to own it.
- **Not free of operational burden.** One machine is one failure domain. Budget for that.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | sh
```

Downloads the latest release for your platform, verifies its checksum, and puts the binary in `/usr/local/bin`. It does not create users, write config, or start anything.

When one machine prepares another, set the target explicitly. This is useful for an Ansible control machine on macOS provisioning a Linux server, or for preparing a second Mac without running the installer there:

```bash
billet_stage=$(mktemp -d)
curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | \
  BILLET_OS=linux BILLET_ARCH=amd64 BILLET_INSTALL_DIR="$billet_stage" sh
```

`BILLET_OS` and `BILLET_ARCH` must be set together. Supported targets are `linux/amd64`, `linux/arm64`, and `darwin/arm64`. A cross-target install verifies and places the binary but does not execute it on the control machine.

**For a machine that should run jobs across reboots, install the package instead** — it ships the systemd units. Pick the file for your platform from the [latest release](https://github.com/junioryono/billet/releases/latest):

```bash
sudo dpkg -i billet_*_linux_amd64.deb    # Debian / Ubuntu
sudo rpm -i  billet_*_linux_amd64.rpm    # Fedora / RHEL
```

The package installs the units and **does not enable or start them**. Installing
billet should not connect a machine to GitHub and begin accepting jobs; that is
your decision, and it cannot be made before `/etc/billet/billet.yaml` says
something true.

The package prepares the RBD kernel client before the hardened node unit can run and records it for subsequent boots. Removing the package removes that boot policy and the binaries and units, but deliberately preserves `/etc/billet`, `/var/lib/billet`, and `/srv/jailer`; configuration, deployment identity, credentials, and any recoverable guest state are operator data rather than package-manager data.

**For a reproducible Firecracker host**, use the `junioryono.billet.host` Ansible role under `ansible_collections/`. It installs the supplied billet binary, the service identity and units, verified Firecracker binaries, Ceph client and optional single-host bootstrap, isolated DHCP/NAT guest bridges, the scoped Ceph identity, and periodic Ceph health monitoring. You still supply the facts only the deployment can know: its capacity, GitHub App key and ids, network ranges, and—only for a new cluster—the exact disks it may consume. The role refuses to infer disks or start either billet service before `billet check` passes. An installed service-account change is also refused because moving the ledger and credentials is an explicit offline migration, not an ownership side effect of convergence, and transient units or effective service drop-ins are refused because their in-memory definitions or overlays cannot be journaled and replaced safely for rollback and quiescent probing. The role reloads systemd before that drop-in preflight and repeats the effective-overlay check after loading candidate units, immediately before validation and readiness probing. When the supplied binary changes, the same converge binds a unique recovery journal to its inspected digest, claims the host upgrade atomically, resolves certificate-derived node identity before selecting node-pinned tier images, proves only the selected Firecracker images this node can run, reuses and boot-verifies an already imported generation with the required guest contract before downloading another, drains node then server, preserves a recovery copy, migrates with the new binary while the ledger has one writer, and runs quiescent server and node probes that cannot accept work. Transactional candidate units explicitly notify systemd only after initialization, while an ordinary converge preserves each installed unit's readiness type for legacy compatibility and upgraded-host idempotence. After readiness, the role durably commits the complete host state, opens the fence, replaces the probes with full services in server-then-node order, and retains the recovery pointer until their stability is proved; an uncommitted failure first removes candidate enablement links and then restores the prior binary, exact supported service enablement, and stopped ledger without replacing the GitHub App key.

**For a development machine beside the runners**, the same collection provides `junioryono.billet.development_host`. It supports Debian-family systemd hosts and macOS, installs verified Caddy and Terraform packages plus mkcert, creates a local CA and caller-defined certificate, and keeps a caller-supplied Caddyfile running across reboots. Billet knows the reusable mechanism; the consuming repository supplies its own domain SANs, routes and environment. The role supports `ansible-playbook --check --diff` before the first converge as well as on an existing machine. It is deliberately separate from `junioryono.billet.host`, because a macOS development machine is useful even though it cannot run the Linux/Firecracker backend.

**A control plane** — talks to GitHub, owns the capacity ledger:

```bash
# Creates the App and prints the github: block to paste in. The key goes
# somewhere the service user can write; /etc/billet is root-owned.
sudo -H -u billet billet github-app create \
    --org YOUR-ORG --key-path /var/lib/billet/app-private-key.pem

sudoedit /etc/billet/billet.yaml     # paste that block; set max_vcpu/max_memory
sudo -H -u billet billet check --config /etc/billet/billet.yaml
sudo systemctl enable --now billet-server
```

**A compute host** — runs the containers. It needs a `node:` section naming the control plane, and a certificate: either enroll it and approve its fingerprint, or issue one directly (see [Adding a second machine](#adding-a-second-machine)):

```bash
sudoedit /etc/billet/billet.yaml     # uncomment and fill in the node: section
sudo -H -u billet billet check --config /etc/billet/billet.yaml
sudo systemctl enable --now billet-node
```

> **`billet-node` is root on that host.** It joins the `docker` group, and
> anything that can reach a rootful Docker socket can start a privileged
> container or mount the filesystem. Prefer rootless Docker where the workload
> allows it.

## Updating

**If you built from source:**

```bash
git pull && go build ./cmd/billet
sudo mkdir -p /usr/local/bin && sudo install -m 0755 billet /usr/local/bin/billet
sudo systemctl restart billet-server                # if you wrote your own unit
```

**If you installed the package:**

```bash
sudo dpkg -i billet_*_linux_amd64.deb        # or rpm -U
sudo systemctl restart billet-server         # and/or billet-node
```

**If you used the install script**, re-run it. Note that it writes
`/usr/local/bin/billet` while the packaged units run `/usr/bin/billet` — so if
this machine has the package installed, update the package rather than the
script, or the units keep running the old binary.

**The restart is safe because billet drains.** SIGTERM stops it taking new work
and waits for the jobs already running before tearing anything down, so an update
does not fail somebody's CI. That is the whole point of the drain, and the reason
this is two commands rather than a maintenance window.

**It is not instant.** The restart takes as long as the longest job still
running, up to `drain_timeout` (6h by default, which is how long GitHub lets a
job run). If you do not want to wait:

```bash
sudo systemctl kill --kill-whom=main --signal=SIGTERM billet-server
```

That stops the waiting and tears down properly. **The jobs still running are
destroyed, and destroying them FAILS those builds** — GitHub requeues a job that
was assigned but never picked up, and does not requeue one a runner has already
started. A third signal gives up where it stands and leaves containers behind for
the reaper.

`--kill-whom=main` matters: without it systemctl signals every process in the
service, including a container CLI billet has in flight.

### Capacity that does not come back

A lease whose holder stops heartbeating is reclaimed by the reaper — but only if nothing was running behind it. If there was, the capacity stays charged to its host and the lease is **quarantined**, because expiry proves the control plane stopped hearing from something, never that the container stopped. Freeing it immediately would let another tier take that slot while the container is still there, and two jobs would land on a machine sized for one.

It resolves itself in the ordinary case: the host destroys the container and says so, or reports what it is actually running — every sweep, not only when it reconnects, because quarantine happens on the reaper's clock and a node that reconnects after an outage usually does so before the leases it was holding have expired. A quarantined lease missing from that report has no container by definition. `billet leases` shows every custody, teardown, and quarantine hold with its node and age. `billet leases release <lease> --force` records your assertion that the compute is gone: quarantine is resolved immediately because it has no holder, while a healthy custody holder receives the request through its next heartbeat, drops its local obligation, and releases the lease itself. Force, because nothing has confirmed anything: if you are wrong that slot is sold twice.

## Guest images

Firecracker root disks are grown per job to the tier's requested `disk` capacity, then the host expands ext4 on the unmounted clone before the guest boots. This also works with already-published images. The immutable golden generation remains small and shared, and teardown discards the enlarged clone with the microVM. A zero `disk` keeps the generation's size as the backend default.

A Firecracker job boots a **golden image**: an Ubuntu 24.04 rootfs carrying Docker, the
GitHub Actions runner, and a small agent that reads the runner registration out of the
metadata service and starts the runner with it. It lives in Ceph as an RBD image with
immutable named snapshots called **generations**, and every job gets a copy-on-write
clone of one, discarded when the guest is.

The guest agent drops from root to the dedicated `runner` account and establishes `HOME=/home/runner`, `USER=runner`, and `LOGNAME=runner` explicitly. A systemd service is not a login session, and `setpriv` changes ids without constructing that account environment; leaving these implicit lets setup actions install a toolchain and then fail when the tool first asks for its per-user cache. The image contents gate checks this contract before publication.

```bash
sudo scripts/build-guest-image.sh          # build and publish a new generation
billet images verify <image>@<generation>  # boot one, make the guest prove it, record it
billet images list                         # what exists, what is verified, what tiers boot
billet images reap                         # remove generations nothing needs
billet runner check                        # how close the runner is to being refused
```

### Promotion, and why a tier should say `@verified`

A tier names one of two things:

```yaml
image: ubuntu-2404-x64@g20260814145813   # exactly this, forever
image: ubuntu-2404-x64@verified          # the newest one proved to boot
```

`@verified` is what lets a fleet take up a new image with **no config edit and no
restart**: verification records itself, and the next launch resolves to it. Rollback is
`billet images unpromote <image>@<generation>`, one command against the cluster rather
than an edit on every node — which matters because it is what you reach for while a bad
image is in front of every job.

A bare image name stays refused. Choosing a generation for somebody who did not choose one
is how a job boots something nobody decided on; naming `@verified` *is* the decision. And
a launch resolves the alias before it does anything, so the log line names the generation
rather than the word — "which image did this job actually run" has to be answerable
afterwards.

If nothing has passed verification, `@verified` **refuses** rather than booting something unproven. It also refuses a generation whose snapshot is gone: the alias is resolved from verification records *intersected with the generations that still exist*, so a record that outlived its snapshot names nothing rather than pointing every launch at a corpse.

Verification records **which kernel proved it**, and does so under the same cluster lock that reaping takes. The two have to exclude each other: a reap landing between "this booted" and "this is verified" would leave a generation the whole fleet takes up with no kernel recorded, and each node would boot it against whatever it happens to be configured with. Note that a probe boots either way — a clone outlives the snapshot it came from — so a verification can genuinely succeed against a generation somebody deleted while it ran.

### Where images come from

Every deployment needs the same guest: Ubuntu, Docker, the Actions runner, billet's agent.
Nothing in it is specific to an organisation, so billet builds it **once, centrally**, and
a deployment pulls it:

```
billet images pull ubuntu-2404-x64
billet images verify ubuntu-2404-x64@<generation>
billet images compatible                 # prove every configured Firecracker image speaks this binary's contract
```

The built-in source reads `current.json` and its Sigstore bundle from billet's `guest-channel` branch, verifies that the main publication workflow signed a still-current pointer attesting to an immutable dated `guest-YYYYMMDD-HHMMSS` prerelease, then downloads that release. The pointer expires after ten days, so rewriting the mutable branch with an older genuine channel cannot silently pin a fleet there. There is deliberately no rolling `guest-latest` release: repository release immutability locks a published tag and its assets, while GitHub's repository-wide `latest` alias belongs to billet's binary installer. The raw pointer avoids spending GitHub's anonymous REST budget when many nodes share one egress address. A configured mirror remains a direct asset base URL and bypasses first-party discovery.

`billet images pull --verify` combines import, a real guest boot and promotion, and `--result-file` records the exact imported generation only after every requested verification succeeds. A verified generation is fleet-wide shared state and is deliberately left published if a later host upgrade fails: another upgraded host may already depend on it, while immutable unused generations remain safe for ordinary retention to collect. Reaping replans under the same publication lock and refuses to remove a generation whose verification or guest contract changed after the operator's preview. With no explicit image, `billet images compatible` checks every distinct Firecracker image in the catalogue and `--result-file` writes the bare names of floating images that need replacement; automatic image selection resolves an omitted `node.name` from the node TLS certificate before applying node-pinned tier constraints. A matching already imported generation is boot-verified before another multi-gigabyte copy is downloaded, and a generation published before contract metadata existed is boot-verified once and backfilled instead of being replaced speculatively. Exact incompatible pins are refused rather than silently redirected. `@verified` and generation retention are contract-relative: during a rolling upgrade, an older binary keeps resolving and retaining its newest compatible verified generation while the new binary may select the newer contract it just proved.

The pull fetches a signed manifest, refuses anything this build cannot use, checks each
asset against the digest the manifest names, unpacks, and publishes the result as a
generation. It stages to disk and verifies **before** importing — streaming straight into
the cluster would put unverified bytes into shared storage, where undoing it is a cluster
operation rather than deleting a file.

### Who published this, and why the signature is the load-bearing check

Every asset is verified against a digest **the manifest names**. That is worth nothing on
its own: a manifest somebody else serves names digests of bytes they chose, and every one
of those checks passes. The signature is the only thing binding the manifest to the
workflow that produced it, so without it the rest is a checksum against itself.

So a pull verifies the signature over the bytes that arrived, before parsing anything out
of them, against a pinned identity:

```
https://github.com/junioryono/billet/.github/workflows/guest-image.yml@refs/heads/main
```

Pinned to the **workflow and the ref**, not the repository. A certificate's identity is
whatever workflow requested it on whatever ref it ran on, so pinning the repository alone
would accept a signature from any workflow in it — including one added by a pull request,
which is a far lower bar to clear than compromising the release process.

Sigstore's trust root is embedded in the binary rather than fetched. A node that may be
air-gapped cannot reach sigstore's CDN, and the verification library refreshes its TUF
cache whenever it expires — so relying on TUF at verification time means depending on the
one thing an air-gapped node does not have.

**A source that is not billet's own must say what would make it trustworthy.** Pointing at
your own mirror and configuring nothing is refused rather than silently unverified:

```yaml
images:
  source: https://mirror.internal/billet
  signing_identity: ^https://github\.com/acme/images/.*$
  signing_issuer: https://token.actions.githubusercontent.com
```

or `--skip-signature-verification`, which is deliberate rather than what happens by
default.

**Sideloading is verified the same way.** `--from <dir>` exists for a deployment with no route to the internet, and it applies the same policy to the same signature — a directory is not more trustworthy than a download, it is less, because nothing about how it arrived is even in principle observable. Its assets are copied into billet's own staging *while being hashed*, rather than checked where they sit: a file verified in place can be replaced before it is read, and then the bytes reaching the cluster are bytes nothing checked.

**This used to be a per-node timer that rebuilt the image on every machine.** That is
gone. It required root, debootstrap and an hour on every node, it had every operator
independently discover GitHub's thirty-day rule, and it made N machines do N builds of a
byte-identical artifact. Building centrally makes the expiry one project's problem instead
of everybody's.

`scripts/build-guest-image.sh` remains, for a custom image or an air-gapped build. It is
no longer the normal path.

### What is checked before an image is published

Nothing reaches a release without passing both halves of a gate, because an image that
reaches a release is one every deployment pulls — a bad one fans out to everybody on the
next refresh, and the thing that would rebuild it is itself a guest booting the image.

**Its contents**, by loop-mounting the filesystem read-only: the runner is installed and is the version the manifest claims, Docker and its Buildx and Compose plugins are there, the agent's contract matches what the manifest advertises, the units that must start are *enabled* rather than merely present, and root is locked rather than passwordless.

**That it boots**, under Firecracker, on the runner that built it. The guest is served a
metadata contract it is required to refuse, and its refusal on the console proves the
whole chain: the kernel booted this filesystem, systemd reached its target, the network
came up, the metadata service answered, and the agent ran and parsed what it got. A pass
is that sentence — not a clean exit, because Firecracker exits 0 on some guest-side
failures.

The two catch different things and neither replaces the other. Contents cannot see an
integration failure; a boot cannot see that a unit was installed but never enabled.

### The kernel and the filesystem are a matched pair

A guest booted with a different kernel fails in the middle of somebody's job, so they are
published together and the generation records which kernel it was paired with. A pull
keeps the kernel it fetched in `/var/lib/billet/kernels`, named by version *and* digest —
version alone does not identify a file, since two builds can produce the same version from
different sources.

`billet images reap` collects kernels no surviving generation names. It refuses while any
generation's kernel is unknown: such a generation still boots something on disk, unnamed
and indistinguishable from an orphan, and deleting it breaks the generation that boots it.

### Taking up a new runner release

`billet runner check` exits **0** while there is nothing to do, **2** once a rebuild is
due, and **3** once GitHub is already refusing — distinct because the second is a task and
the third is an outage. Failing to *reach* GitHub is none of the three: it is an error,
because a machine with no egress cannot find out, and reporting that as an expiring fleet
is the false alarm that teaches people to ignore the true one.

The version and its checksum live on one line in `internal/runnerrelease/pinned.txt`,
because a checksum is only true of its version — and a daily workflow watches
`actions/runner` and opens a pull request when a release lands, so keeping source current
is a review rather than a reminder. Merging is deliberately the gate: a bad runner release
should not reach a fleet without somebody agreeing to it.

The scheduled refresh does not wait for that. It builds at **whatever GitHub has
published**, and records the version it installed on the image itself — which is what
`billet runner check` reads, because the image is the only thing that knows what the fleet
is actually running. The compiled-in pin says what a build *would* install, and the two
part company the moment a scheduled rebuild takes up a newer release.

## Status

billet is pre-alpha. **Jobs run end to end through the Docker, Firecracker and EC2 providers**, and the same-label local-to-cloud failover path has completed against real GitHub and AWS infrastructure. What works **today**:

| | |
|---|---|
| `billet github-app create` | Creates and installs the GitHub App via the manifest flow |
| `billet check` | Validates the config, the App private key, and the state database. For an EC2 node it also reports that node's conservative compute-only peak implied by its declared shape prices and resource ceilings |
| `billet server --dry-run` | Connects to a real org, reconciles scale sets, polls — accepts nothing |
| `billet server` | The control plane, serving the node wire. It runs no compute of its own — a machine that should also run jobs runs `billet node` beside it. A fleet with no live node advertises zero, so an empty fleet is told to GitHub rather than discovered when a job fails to launch |
| `billet node` | A compute host: dials the control plane, never listens. One per machine, including the machine the server is on |
| `billet node --enroll` | Asks a control plane to admit this machine, printing the fingerprint an operator compares |
| `billet nodes pending` | Shows what is waiting to be let in, with the fingerprint to check |
| `billet nodes approve <node> --fingerprint <fp>` | Admits the machine whose fingerprint you compared |
| `billet ca token` | Mints the short-lived credential a machine needs to ask |
| `billet ca show` | The authority's fingerprint and expiry, and a warning once it is close enough to expiry to be shortening every certificate it issues. It does **not** report whether a rotation is running |
| `billet ca issue <node>` | Mints a certificate directly, for a machine you are provisioning anyway |
| `billet nodes revoke <node>` | Withdraws every credential that machine holds, renewals included |
| `billet ca revoke <node> --cert <path>` | Withdraws one specific certificate |
| `billet ca rotate` / `retire` | Replaces the authority as an overlap, so no node is cut off |
| `billet leases` | Every custody, teardown, and quarantine hold, with its node and age |
| `billet leases quarantined` | The compatibility view limited to holders that vanished |
| `billet leases release <lease> --force` | Hands capacity back on your assertion that its compute is gone; a live holder is told to drop custody first |
| `billet ami build` | Builds the AWS machine image the `ec2` backend launches: the GitHub Actions runner, Docker, git. Deliberately minimal — `actions/setup-*` downloads toolchains at runtime and works, while a workflow assuming a preinstalled toolchain fails loudly. **An AMI id is region-scoped**, so a tier's `image:` or `launch.ec2.image` only works in the region it was built for |
| `billet teardown` | Removes the scale sets billet created |
| Capacity ledger | Lease state machine, fencing epochs, placement enforcement, escrow before advertising |
| Docker provider | One container per job, JIT registration delivered off argv. **Trials only** — shares the host kernel, so it refuses anything not established as trusted |
| EC2 provider | One instance per job, in one subnet. The instance IS the isolation boundary, so unlike Docker it may run fork pull-request code — but only once `untrusted_security_group_ids` describes a network for it, because an instance isolates the kernel and not the VPC. A launch is idempotent by lease id, so an ambiguous retry cannot start two machines for one job. Real private-repository jobs have exercised JIT registration, Docker builds, service containers, runtime toolchain installation, same-label local-to-cloud failover and a live FIS Spot interruption, with every instance destroyed afterward. Three cold launches reached the first job step in 47.6–58.7 seconds ([AWS acceptance](docs/aws-acceptance.md)) |
| Crash recovery | A job running when the controller dies is adopted and left to finish, not killed; its capacity stays held |
| Per-machine capacity | Each node reports what it contributes; a tier advertises the smaller of the deployment ceiling and what its machines can hold. An EC2 node also reports its ordered purchasable shapes, so placement charges the selected shape rather than the usually smaller tier request. Each shape carries an operator-audited hourly price: `billet check` reports one node's conservative peak before credentials are tested, and `billet status` reports the deployment-wide peak across registered EC2 nodes under the shared ceiling. A host nothing can reach stops backing advertisements |
| Placement | The control plane chooses the machine when the work is admitted, by provider preference, then packing (`placement: spread` to even the load instead), then name. Reserved floors are held against the machines that could keep them |
| Sites | A node says where it is; a tier may insist on a place. Cache implementations namespace data by site, and the control plane rejects a split-config node whose provider cannot use that site's declared store. The cross-node/cross-site behavior still needs a real integration proof ([#20](https://github.com/junioryono/billet/issues/20)) |
| Graceful drain | SIGTERM stops it taking new work and waits for the jobs already running, so `systemctl restart` does not fail somebody's build. See [Updating](#updating) |
| Release pipeline | GitHub-immutable tagged releases with attestations, checksums, `.deb`/`.rpm` packages with systemd units, an install script that verifies the published checksum, and immutable internal action references on every release tag |
| Multi-backend tiers | One label can name several providers, and the preference ORDER decides: the control plane picks the host when the job is admitted, walking the tier's list most-preferred-first. Each backend gets its own `launch` entry because a Firecracker generation, an EC2 AMI and their runner commands are backend-specific. `[firecracker, ec2]` means the bare-metal box before the cloud, and live same-label acceptance has completed the same unchanged workflow locally and on EC2 after local capacity was removed ([#32](https://github.com/junioryono/billet/issues/32)) |
| Firecracker microVMs | One job, one guest kernel, on bare metal. Under the jailer always: chrooted, dropped to its own unprivileged uid, in a cgroup, with a seccomp filter. The root disk is a copy-on-write RBD clone of a golden image, discarded with the guest, and the runner registration is delivered through the metadata service so it is never in argv and never on a disk. The guest image exists too: `scripts/build-guest-image.sh` builds and publishes it, and a guest boots, takes its registration and runs a container in about ten seconds. See [Guest images](#guest-images) |

**Not built:** the Apple Silicon provider, transparent interception of `actions/cache`, observability, and the dashboard. Both cache stores have site-local immutable generations, active-clone leases, ext4 verification, fenced CAS publication and inactivity eviction: Ceph uses RBD clones, while AWS uses encrypted EBS volumes and snapshots with the authoritative pointer in S3. AWS state objects are namespaced by deployment and site, and every targeted EBS deletion rechecks both ownership tags before acting; IAM conditions on those same tags and the owner-specific S3 prefix remain the account-level backstop. Firecracker reserves five hot-attach slots per guest; EC2 resolves its one-job instance and attaches owned volumes to it at workflow runtime. The sticky-disk and BuildKit actions are implemented against those paths, but still need their issues' real warm-workflow, failed-job, and power-cut proofs. Firecracker and EC2 guests now request a deployment-, site-, and architecture-scoped `/var/lib/docker` volume before the runner starts. Fresh Docker 29 installations are explicitly kept on the supported `overlay2` backend because its default containerd image store keeps image content in `/var/lib/containerd`, outside that independently fenced volume. A changed store becomes eligible only after the pinned runner reports a clean one-job result and the guest unmounts and checks it; GitHub's independent `succeeded` completion opens the bounded settlement window. The readiness call is cooperative coordination rather than an intra-guest security boundary—workflow code has passwordless sudo and Docker-root equivalence—so node-side trust classification and GitHub's result remain the publication authority. The control plane records that authoritative result, fenced lease identity and bound holder in SQLite before acknowledging GitHub. Settlement retires the record without replaying teardown, and physical removal waits until both settlement and the source acknowledgement are durable, so either ordering survives restart without exposing a reused request ID to stale teardown. Published generations remain for eight inactive days, while failures, cancellations, no change, untrusted workloads and unknown results discard the job's clone ([#28](https://github.com/junioryono/billet/issues/28)). Untrusted work may read the deployment's trusted baseline but never changes it, so a deployment that pulls private images treats those image layers as visible to every job admitted to that deployment. A tier bounds each persistent BuildKit mount independently, and three optional site-local registry mirrors cover BuildKit's Docker Hub, GHCR, and Quay pulls while Docker Engine uses the Docker Hub mirror it supports. Cache availability and commit errors degrade to warnings, and untrusted jobs discard their writes. Transparent `actions/cache` interception remains refused: the GitHub results origin also carries artifact traffic, and enabling a partial proxy before per-VM TLS identity, CA propagation into job containers, exact path passthrough, and live action-version conformance would turn a cache miss into a workflow failure. An EC2 node's `max_vcpu` and `max_memory` are hard resource budgets: the allocator charges the selected purchasable shape, and a fallback is authorised against both the node and deployment ceilings before its AWS request is sent. The enforceable price policy is the shape allowlist—do not declare one you are unwilling to buy—while each required `price_usd_per_hour` lets `billet check` report one node's conservative compute-only peak and `billet status` report the deployment-wide peak across registered EC2 nodes. The report is not an admission gate: copied prices can go stale, so AWS Budgets is the account-wide backstop. Spot remains off by default; when enabled it requires an EventBridge-to-SQS warning queue in the same region, records the reclaim reason durably, and starts teardown without waiting for lease expiry. A live FIS interruption has exercised that full path ([AWS acceptance](docs/aws-acceptance.md)).

**billet runs a fleet, with one thing still missing before it is worth having one.** Capacity is a
figure per machine, so hosts of different sizes can be described and a tier advertises only what its
machines can actually hold. The control plane chooses the host when the work is ADMITTED — which is
what finally makes `providers: [firecracker, ec2]` mean "the machine at home first, the cloud if you
must" — and a destroy goes to the machine holding the container rather than to everyone
([#21](https://github.com/junioryono/billet/issues/21),
[#30](https://github.com/junioryono/billet/issues/30),
[#31](https://github.com/junioryono/billet/issues/31)).

Within a correctly configured site, **cache bytes live in shared storage rather than on the machine that built them**: a Ceph node can clone an RBD generation, and an EC2 node can create an EBS volume from a snapshot. The server's declared store is authoritative for every registering node; the remaining #20 work is the real same-site sharing and cross-site cold integration proof.

**A terminate request is not a stopped guest**, and billet no longer pretends otherwise ([#46](https://github.com/junioryono/billet/issues/46)). `TerminateInstances` returns when the request is *accepted* while the machine keeps running for a minute or two, so this backend reports its teardown as requested rather than confirmed and the node keeps that lease charged. A targeted lookup retains EC2's explicit `terminated` record as causal proof even though fleet inventory excludes historical instances; if that record is unavailable, absence must remain uninterrupted through the full remote-consistency window. Waiting inside the teardown was never the fix — a node runs one command at a time, so it would stall every launch queued behind it. A wedged teardown stays charged rather than being released on elapsed time, appears in `billet status` and `billet leases`, and can be explicitly forced without stopping a healthy node.

**A shape AWS has none of falls through to the next one you declared** ([#49](https://github.com/junioryono/billet/issues/49)). `InsufficientInstanceCapacity` is the likeliest way a cloud launch fails for a reason retrying cannot fix, so billet walks `instance_types` in your order until one starts. It only does that after a refusal AWS returns synchronously, having launched nothing; after an *ambiguous* failure it stops, because a second attempt could leave two machines carrying one job's name.

**One ec2 node is a serial launch queue**, because a node executes one command at a time. That is invisible for a backend where a node is one machine's worth of jobs and visible for one where a single node can stand for sixty — so a large cloud fleet wants several ec2 nodes, each registered separately with its own budget, rather than one with a large one.

Spot warnings are routed one queue per node. The router resolves the warned instance's `sh.billet.node` tag and targets only that node's queue; the queue name must equal the node's effective identity, taken from `node.name` or its certificate. A shared SQS queue is refused as a deployment shape because one consumer hides a message for the visibility timeout, which can consume the whole two-minute warning before the owner sees it.

**The cloud runner path and same-label multi-provider failover are proven end to end.** Real private-repository jobs have launched on EC2, registered through JIT configuration, used Docker and a service container, installed Go at runtime, completed green, and left no live instance behind. The same unchanged workflow also completed first on preferred local capacity and then on EC2 after that local contribution was withdrawn. EBS/S3 cache behavior still uses fake AWS boundaries. See [AWS acceptance](docs/aws-acceptance.md); [#33](https://github.com/junioryono/billet/issues/33) tracks the caching plane.

The owner namespace is the EC2 cache isolation boundary. Pre-release S3 state objects written without a deployment-and-site owner are deliberately ignored and are never migrated automatically: their key cannot prove which deployment owns the referenced snapshots. An operator upgrading an experimental deployment from that layout must drain its EC2 nodes, remove its old unnamespaced state, and let the cache repopulate under the owner-specific prefix.

### Adding a second machine

A control plane bound to a network address requires client certificates, and mints its own authority to issue them. There is no CA to run and nothing to install.

**The machine asks, and you approve a fingerprint.** Two ends display the same number and you check they match — that comparison is the trust decision, and everything else is transport.

```bash
# on the control plane
billet ca show                      # prints the authority's fingerprint
billet ca token                     # prints a short-lived join token

# on the new machine
billet node --enroll \
  --ca-fingerprint SHA256:...  \
  --join-token h7q2...              # prints THIS machine's fingerprint, then waits

# back on the control plane
billet nodes pending                # shows the same fingerprint, if nothing is in the way
billet nodes approve mac-mini-1 --fingerprint SHA256:...
```

Neither side accepts on faith. The node refuses to enroll without the authority's fingerprint, because its first connection has nothing to verify against — accepting whatever answered would let anyone who replies first own every job that node runs. And approval refuses without the node's fingerprint, because approving by name alone approves whatever currently holds the name.

The join token is what stops a stranger who can reach the port filling that pending list, or taking a name before the machine that should have it. It is short-lived, counted, and stored as a hash.

**Or issue a certificate directly**, which is right for a machine you are provisioning anyway — cloud-init can drop a bundle on it, and no human is standing there to compare a fingerprint:

```bash
# on the control plane
billet ca issue mac-mini-1          # writes ./mac-mini-1-billet-tls/
scp -r mac-mini-1-billet-tls mac-mini-1:/etc/billet/tls

# in that host's billet.yaml — node.name comes from the certificate
node:
  server_addr: billet.example:7717
  tls:
    cert: /etc/billet/tls/node.crt
    key:  /etc/billet/tls/node.key
    ca:   /etc/billet/tls/ca.crt
```

Both paths are recorded, so `billet nodes pending --all` is the single answer to what has been admitted and when.

**A node registers itself, and the fleet is not something you edit.** Registration is dynamic and never asks whether a host was declared anywhere: it checks the protocol version, a non-empty name, the deployment identity, that the contribution is non-zero, and that the site is one this deployment declares. The allocator then requires a provider, and refuses to move a host to a different provider or site while leases are still outstanding against it. So `nodes:` is policy *about* hosts rather than a roster *of* them. The one config fact registration does enforce is `sites:` — a node claiming a site the control plane has never heard of is refused rather than recorded, because a typo would otherwise become a place of its own with a cache that is always empty.

**And the operator commands run against a live control plane**, which is when you actually need them. `billet nodes pending|approve|deny|revoke`, `ca token|issue|revoke|revocations`, `status`, `leases|release` and `check` reach the ledger without taking the exclusive lock the server holds: that lock exists to stop two control planes writing conflicting scheduling decisions, and a one-shot command is not one — it makes no scheduling decisions, and the writes it does make are ordinary transactions SQLite serialises against the server's own.

What they deliberately will **not** do is migrate a ledger another process is HOLDING. Run a newer billet's CLI against an older running control plane and it refuses rather than upgrading a schema that plane is mid-transaction against, and tells you which side to restart. A **stopped** deployment is the other case and it does migrate: whoever opens the ledger first creates or upgrades it, which is what lets `billet ca issue` work on a fresh install before any server exists — so running a newer binary's command against a stopped older deployment upgrades its schema, and that older server will then refuse to start. Upgrade the server binary at the same time. The reusable host role owns that transaction: before replacing desired-state files it preserves the installed configuration, units and binary, records the old identity, exact systemd enablement and active-service state in a durable manifest, and claims `/var/lib/billet/upgrades/active` with a no-replace hard link so concurrent controllers cannot own the same host. It proves only the configured Firecracker images eligible for this node, drains node then server, removes the installed executable, establishes the maintenance fence, commits a stopped-ledger snapshot, and only then installs and validates the candidate. The role accepts only distinct dedicated state directories below `/var/lib/billet`, refuses symlinks, and will not recursively change ownership through an arbitrary configured path. New and already-open operator handles remain excluded throughout backup, migration, probe startup, and possible restoration. Candidate units use an explicit quiescent upgrade mode that opens and validates the fenced ledger or initializes the provider but never polls GitHub, registers a node, dispatches, or accepts workload; steady-state units contain no maintenance bypass. After the probes remain stable, the role installs the steady units, persists desired enablement, flushes every mutated filesystem, records the durable decision, opens the fence, starts full services in server-then-node order, proves their stability, and only then removes the active pointer. If Ansible or the host stops while the pointer exists, the next converge inspects it before account, package, Firecracker, network, Ceph, or other desired-state work and then requires a fresh run. An uncommitted transaction proves both services terminal with zero main/control PIDs and conclusively empty or absent cgroups before touching the ledger, validates the completed snapshot before removing current state, restores the configuration, units, binary, exact enablement and ledger, durably records the rollback, and only then reopens administration and restarts the previously active services. A transaction already bearing either commit record retries only committed finalization, leaving the pointer in place until full services are stable, so an interruption after the fence opens can never cause later operator writes to be replaced by the old snapshot. If any prerequisite or inactivity evidence is inconclusive, the active pointer and recovery copy are left untouched for the operator. A command that needs to *write* while the plane happens to be mid-decision waits for it, and if it waits too long it stops and says so rather than hanging silently. Some commands commit more than one transaction — `nodes revoke` records each older certificate before withdrawing them — so it tells you that whatever it had already done stands, rather than pretending it was a no-op. A command that only reads never waits for the write lock at all, and no command re-verifies the whole ledger on the way in — that scan belongs to the control plane, which is about to schedule against it, and `billet check` asks for it explicitly. So `status`, `leases` and `nodes pending` answer immediately however busy or however old the deployment is.

| Action | Control-plane restart? |
|---|---|
| A registered machine reconnecting | **No** — it re-registers itself |
| Admitting a **new** machine | **No** — enroll it, approve the fingerprint, and it joins |
| Reclaiming stranded capacity | **No** — `leases release --force` works on a running deployment, which is the only place quarantine happens |
| Add or change a **tier** | **Yes** — tiers are read at startup, and each becomes one scale set |
| Change the `nodes:` policy block | **Yes** — it is snapshotted into the allocator at construction and enforced during placement |

The name in the certificate is the only thing that decides which node a request is from — a host holding a bundle can act as that node and as nothing else. The certificate also carries which **deployment** it belongs to, so a fresh host does not invent an identity the control plane would refuse.

**Certificates renew themselves** when less than a third of their life remains, over the wire, with the private key never leaving the node. A certificate that has already expired cannot renew — renewal is authenticated by the certificate being renewed — so that machine has to be re-enrolled. For a full-life certificate the window is months, and the usual way to miss it is a host that was powered off throughout. It is not the only way: the window is a third of the certificate's *own* life, so it shrinks as the authority approaches its expiry and starts capping what it issues, and a long control-plane outage or a renewal that keeps failing to install can carry a running node through expiry too.

**Taking one back:** `billet nodes revoke <node>` withdraws every credential that machine currently holds, and each is refused on the very next request it makes rather than at its expiry. Revoke the **node**, not a file — because a node renews itself, the bundle you issued names a serial it stopped presenting months ago, and taking that one back would report success and change nothing. A certificate issued in a later second is unaffected, so a rebuilt machine can keep its name — the cutoff is whole-second and resolves its own second toward refusing, so mint the replacement a second later rather than instantly. `billet ca revoke <node> --cert <path>` still withdraws one specific credential when that is what you mean.

One residual, because it is the kind of thing that should not be found out during an incident: everything issued since the credential ledger existed is revoked by **serial**, where no clock is involved, but a legacy certificate whose serial was never recorded is caught by the cutoff instead — and a cutoff is a comparison between two clocks. A certificate minted by an authority running ahead of the control plane can carry a date after the cutoff and survive it. If you cannot enumerate what a compromised host holds, or you do not trust the clocks, rotate the authority rather than relying on the cutoff.

**Replacing the authority** is an overlap rather than a switch, because a node trusts what it was given: `billet ca rotate` has the new authority issue node certificates while the old one still signs what the server presents and both stay trusted, nodes adopt the new one as they renew, and `billet ca retire` ends it once they have.

Loopback stays plain HTTP, because there is nothing between the two processes to authenticate
against. Anything else refuses to start without a certificate rather than serving unauthenticated on
a network, which is the failure that looks like it works.

Two things to know before relying on it. The **authority** is the cliff, not any single certificate: a leaf may not outlive the CA that signed it, so once the CA has less than a leaf's lifetime left, every certificate it issues is quietly shorter than the last. Renewals keep working and come round faster and faster, nothing errors, and then every node in the fleet expires on the day the authority does. `billet ca show` warns once that starts, because it is invisible otherwise, and `billet ca rotate` is the answer rather than waiting for it.

And a node's identity is its **name**, so two hosts configured with the same one are one host as far as the control plane is concerned. A per-process incarnation value is what routes new commands to the newest registration and stops a superseded process acting on work it was never given — but it does not tell a restart from a duplicate, and it deliberately lets a superseded process go on maintaining and reporting the work it already holds, because a draining process has to outlive its replacement. After a restart the plane still cannot say which of two machines sharing a name physically holds a given container. Give each host its own name.

**The runner path has run against a real organization.** Docker remains exercised by the end-to-end suite against a scripted Actions service, while Firecracker has completed a real private-repository workflow and the EC2 path has completed the same unchanged workflow after local capacity was withdrawn, as well as handling a live FIS Spot interruption. The remaining live proofs are cache reuse, failure recovery, power-cut safety and cross-node site behavior.

Everything below describes the intended design. Where a thing is not built, it says so.

## Quickstart

> **Use `billet init` for a Docker trial rather than copying `billet.example.yaml`.** The example describes the measured Firecracker deployment: it needs KVM, jailer, a guest bridge, scoped Ceph credentials, published guest images, and root. `billet init` instead writes a Docker config that runs on an ordinary development machine; Docker shares the host kernel and offers no block-device cache.
>
> Docker shares the host kernel and is for trials rather than for untrusted code.

```bash
billet init --org myorg --config ./billet.yaml     # writes a config sized to this machine
billet github-app create --org myorg --config ./billet.yaml   # creates the App, fills the block in
billet check --config ./billet.yaml                # validates config, key, state

billet server --config ./billet.yaml               # then, in two terminals:
billet node   --config ./billet.yaml               # the machine that runs the jobs
```

`billet init` measures this host and writes a ceiling below what it found, leaving room for the kernel, the container runtime and your shell. It picks a runner image that is actually pullable, points the state directories somewhere writable, and describes both roles in one file. Nothing in it has to be hand-edited: `github-app create --config` writes the App ids into the same file rather than printing a block to paste, and it will not overwrite a config you already have without `--force`.

**A single machine runs both roles**, as two processes reading that one file. They talk over the loopback address in `server.listen`, so nothing is exposed to the network and no certificates are involved: a control plane listening only on loopback serves plain HTTP, because there is nothing between two processes on one box to authenticate. Certificates start mattering when you add a second machine — `billet ca issue <node>` mints one, the new host's `node.name` comes from it, and the server then has to listen where that machine can reach it.

**And Docker has to be there.** `billet check` never touches it, but `billet node` calls `docker ps` before it takes any work, to re-adopt containers from a previous run. No CLI, no running daemon, or no permission on the socket, and it stops there.

`--config` is not optional here. billet deliberately does **not** read a
`billet.yaml` from the working directory — a server started from a directory
someone else can write to would otherwise adopt their config, which chooses the
state directory, the App key path and every tier's resources. Without the flag
it reads your user config directory (`billet check -h` prints the path).

Then, once the runner plane exists, in a workflow:

```yaml
jobs:
  build:
    runs-on: billet-4vcpu-ubuntu-2404
```

## Architecture

One binary, two roles (the Nomad/Consul model):

```
billet server   control plane — scale-set listeners, capacity allocator, scheduler, state
billet node     compute host  — runs a provider, launches instances, reports capacity
```

One machine runs both as two processes over loopback; there is no combined mode.

```
                 GitHub  ◄── outbound long-poll only, no inbound
                    ▲
              ┌─────┴──────┐
              │   server   │   SQLite state · global capacity allocator
              └─────┬──────┘
                    │  nodes dial OUT over mTLS
        ┌───────────┼───────────┐
        ▼           ▼           ▼
   ┌─────────┐ ┌─────────┐ ┌─────────┐
   │bare metal│ │Apple Si │ │   EC2   │
   │firecracker│ │  tart  │ │ per-job │
   └─────────┘ └─────────┘ └─────────┘
```

Runner tiers are **defined by you**, not chosen from a fixed catalog:

```yaml
tiers:
  - label: billet-8vcpu-ubuntu-2404
    provider: firecracker
    vcpu: 8
    memory: 32GiB
    disk: 160GiB
```

For Firecracker and EC2, `disk` is usable root-volume capacity rather than a scheduling annotation. Firecracker grows only the job's copy-on-write clone and expands ext4 on the host before boot; the immutable golden image stays small and shared, including images published before this behavior existed. A zero value keeps that image size as the backend default. Docker runs on the host filesystem and ignores this field.

## Security

Read this before pointing `billet` at anything.

**Do not use self-hosted runners with public repositories.** This is
[GitHub's own guidance](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access),
not ours. Fork pull requests do not receive your secrets, but they *do* get arbitrary code execution
on your hardware. `billet` isolates jobs in microVMs on bare metal, which helps — each job gets its own kernel — but **it does not make running untrusted code on your own machine safe, and billet will not pretend otherwise**. A microVM's boundary is the KERNEL, not the network it is attached to: a guest on your ordinary bridge reaches whatever that bridge reaches. So billet REFUSES fork pull-request work until `node.firecracker.untrusted_bridge` names a separate network for it. The runtime never rewrites host networking; the optional Ansible role creates separate bridges and blocks their access to the host, private networks, link-local/cloud metadata and CGNAT by default, with deployment-specific additions supplied as variables. If you do not use that role, the equivalent policy is yours to write before enabling untrusted work. The same rule governs the `ec2` provider, which rents you the boundary instead of owning it. The `docker` provider shares the host kernel and refuses untrusted work outright. Private repos with trusted contributors are the intended use case.

**Caches are a deliberate cross-job channel.** A job that writes a secret into a cached directory persists it for later jobs to read. Publication is an explicit fenced operation and the node discards every untrusted clone without failing its job, but untrusted work may still read the trusted baseline. Docker image stores are scoped by deployment, site and architecture; private images pulled by a trusted job are therefore readable by later jobs inside that deployment boundary. User sticky-disk keys are exact and may deliberately cross repositories, so prefix ordinary keys with `${{ github.repository }}`. Nothing prevents a trusted job from leaking into its own cache: **don't cache secrets.**

**GitHub App permissions.** `billet` requests exactly two:

| Permission | Level |
|---|---|
| Metadata | read |
| Organization self-hosted runners | read & write |

No repository *Contents* permission — `billet` cannot read your code. (It is not literally "no
access to anything", and any project claiming that is overselling; the App can manage runners on your
org, which is a real capability.)

**Transparent `actions/cache` interception is not implemented.** The proposed design terminates TLS because GitHub exposes no supported custom cache URL, and `ACTIONS_RESULTS_URL` carries artifact metadata as well as cache traffic. That puts the proxy in the release path even when only cache methods are handled locally. Config validation therefore refuses `intercept: true` instead of accepting a protection that does not exist. `.github/workflows/cache-conformance.yml` supplies the live two-runner save/restore and artifact passthrough harness with `actions/cache@v5`, `upload-artifact@v7`, and `download-artifact@v8`, on both host and container jobs. Issue [#29](https://github.com/junioryono/billet/issues/29) still requires that workflow to pass through the eventual proxy, deliberate fail-open fault injection, CA propagation into arbitrary job and action containers, per-VM identity, and a kill switch before interception can be enabled safely.

**EC2 cold-start measurement is built into images made by `billet ami build`.** Immediately before each `RunInstances` attempt billet puts its epoch in that attempt's user data. The image records when its runner entry point is ready, then GitHub's supported `ACTIONS_RUNNER_HOOK_JOB_STARTED` hook prints `billet timing: launch_to_job_start_ms=… launch_to_runner_ms=… runner_to_job_start_ms=…` before the first job step. Invalid or missing timing state exits successfully and prints nothing, so the probe cannot fail a workflow. Three cold runs reached the first job step in 47.609, 52.188 and 58.662 seconds; they spent 37.936–43.220 seconds reaching the runner and 9.673–15.442 seconds from runner readiness to the job. That is acceptable for fallback capacity and does not justify live, billable, unleased instances, so `warm_pool` remains refused. See [AWS acceptance](docs/aws-acceptance.md).

## Compatibility caveats

**There is no supported way to point `actions/cache` at your own server**, which is why the proposed transparent design requires interception rather than a custom URL. [actions/toolkit#1051](https://github.com/actions/toolkit/issues/1051) — "add support for non-GitHub-hosted caching for self-hosted runners" — has been open since **April 2022**, and the PR to allow a custom cache URL is still unmerged. Today billet leaves `actions/cache` on GitHub's service.

**The Actions Cache v2 protocol is reverse-engineered.** GitHub has never published the `.proto` files ([actions/toolkit#1931](https://github.com/actions/toolkit/issues/1931) has been open since January 2025), so every implementation — including the one billet will have — is derived from the generated TypeScript client and wire captures. **GitHub can change it without notice.** The plan is a conformance suite run against live GitHub on every image build to catch drift early, plus **failing open to a cache miss** on any error rather than failing your job — a cache miss is always better than a stall. The checked-in harness exercises current action and artifact versions directly against GitHub, but it has not run through an interception proxy because that proxy remains unimplemented.

**Apple Silicon support requires [Tart](https://tart.run), which is not open source.** Tart is
licensed FSL-1.1-ALv2; each release converts to Apache-2.0 after two years, and competing commercial
use is restricted. `billet` treats it as an optional external dependency you install yourself, the
same as Docker or Ceph. `billet` itself is Apache-2.0 throughout.

**A compute host needs Ceph, on the nodes' own NVMe.** A snapshot on one machine cannot be mounted on
another, so a cache kept in local storage is a cache that pins every repository to the host that
first built it. Ceph RBD gives the same snapshot-and-clone primitive from a pool any node at the site
can map, which is what makes a cache a property of a *place* rather than of a machine — and it is
what the commercial products run. The billet binary installs nothing; either run `cephadm bootstrap`, create two pools, run `ceph osd set-require-min-compat-client mimic`, and point `node.ceph` at them, or let the optional Ansible role perform those exact steps after you explicitly name the monitor address and every disk it may consume. That last command is not optional and `billet check` refuses a
cluster without it: `cephadm` leaves a cluster cloning the old way, where a snapshot must be protected
before it can be cloned and a protected snapshot with a live clone can be neither unprotected nor
removed — so a cache generation any running job holds would be undeletable. On a single
box it is honestly more moving parts than the ZFS pool it replaced ([#23](https://github.com/junioryono/billet/issues/23)),
and the reason to adopt it anyway is that retrofitting shared storage later means rewriting placement
at the same time. [`docs/adr-003-ceph-rbd.md`](docs/adr-003-ceph-rbd.md) is how the reference cluster
was built and what it measured — including the two things about Ubuntu 26.04 that break `cephadm
bootstrap`, and why clone v2 is a requirement rather than a preference.

## Roadmap

| Phase | Status |
|---|---|
| P0 — scaffolding, GitHub App onboarding, host prep | ✅ mostly |
| P1 — runner plane: scale sets, allocator, providers | ✅ listeners, allocator, the drain, and the Docker, EC2 and Firecracker providers |
| P2 — guest images, node split, user-defined tiers | 🚧 node split + mTLS done; the microVM and the guest image it boots are both done, and the image keeps itself current ([Guest images](#guest-images)). What is left is breadth: ours carries Docker and the runner against a hosted runner's ~50GB ([#66](https://github.com/junioryono/billet/issues/66)) |
| P3 — Ceph, the storage layer, sticky disks, trust classes | 🚧 The generation/lease/CAS layer, five Firecracker hot-attach slots and `actions/stickydisk` are implemented and locally tested; real-host warm-run and power-cut verification remains [#25](https://github.com/junioryono/billet/issues/25) [#26](https://github.com/junioryono/billet/issues/26) |
| P4 — colocated Actions cache | ⬜ [#29](https://github.com/junioryono/billet/issues/29) |
| P5 — Docker layer cache, registry mirrors, container baseline | 🚧 Persistent in-guest BuildKit state, per-mount ceilings, the architecture-scoped Docker image store and three-registry mirror plumbing are implemented and locally tested; real pilot timing remains [#27](https://github.com/junioryono/billet/issues/27) [#28](https://github.com/junioryono/billet/issues/28) |
| P6 — observability, SSH-into-a-job | ⬜ |
| P7 — Apple Silicon provider (macOS + Linux arm64) | ⬜ |
| P8 — EC2 provider, cloud-hosted control plane, provider failover | ✅ The provider, AMI builder, real GitHub job path, live Spot-warning handling, exact purchased-shape accounting and same-label local-to-cloud failover are proven ([#32](https://github.com/junioryono/billet/issues/32)) |
| P9 — per-node capacity, admission-time placement, addressed teardown. **A prerequisite of P8**, not a sequel: failover needs the decision made before the work is accepted | ✅ [#21](https://github.com/junioryono/billet/issues/21) [#30](https://github.com/junioryono/billet/issues/30) [#31](https://github.com/junioryono/billet/issues/31) |
| P10 — dashboard, signed releases, public launch | 🚧 releases and packages done; signing and the dashboard are not |
| P11 — AWS Terraform | — Deferred until billet has a versioned atomic configuration API and released users ask for it ([ADR-004](docs/adr-004-terraform-provider.md)) |

## Alternatives

Use one of these instead if it fits — most people should.

| | License | Runs on | Isolation | Notes |
|---|---|---|---|---|
| [actions-runner-controller](https://github.com/actions/actions-runner-controller) | Apache-2.0 | Kubernetes | container | GitHub's own. Mature and widely deployed. Needs a cluster; tracks no individual job and delegates scheduling to k8s; no cache or persistent build state. |
| [terraform-aws-github-runner](https://github.com/github-aws-runners/terraform-aws-github-runner) | MIT | AWS only | EC2 per job | Terraform + Lambda, webhook-driven. Well maintained. No cache service, no bare metal. |
| [GARM](https://github.com/cloudbase/garm) | Apache-2.0 | many clouds + LXD | varies | The closest existing OSS control plane, and a genuine multi-provider design. No colocated cache or build-state caching. |
| [Ubicloud](https://github.com/ubicloud/ubicloud) | **AGPL** | their cloud | microVM | The best open reference for how a commercial runner cloud is built. The licence makes adopting a piece of it hard. |
| [Actuated](https://actuated.com) | commercial | your hardware | Firecracker | Closest on isolation. Paid. |
| [Blacksmith](https://blacksmith.sh), [Namespace](https://namespace.so), [WarpBuild](https://warpbuild.com), [Depot](https://depot.dev), [BuildJet](https://buildjet.com) | commercial | their hardware | varies | Managed. If you do not want to run infrastructure, use one of these. |

**Where billet is different:** one `runs-on` label spanning bare metal, Apple Silicon and cloud with
failover between them, plus a colocated cache and persistent build state, in one Apache-2.0 binary
with no Kubernetes. Every piece of that exists somewhere in the table; the combination does not.

**Where it is worse, today:** all of them work and billet mostly does not.

## Prior art

`billet` is an open-source take on what [Blacksmith](https://blacksmith.sh) built, and it borrows
several of their published designs — persistent BuildKit state, snapshot-clone caches, transparent
cache interception. Their [engineering blog](https://www.blacksmith.sh/blog) is worth reading.
[Ubicloud](https://github.com/ubicloud/ubicloud) (AGPL) is the highest-quality open reference for how
a commercial runner cloud is actually built, and [GARM](https://github.com/cloudbase/garm) is the
closest existing OSS control plane.

## License

Apache-2.0. See [LICENSE](LICENSE).
