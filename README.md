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

> Failover is built the whole way down and has never been pointed at a real AWS account. A tier can name several backends, capacity is measured per machine, and the control plane picks the host when the job is admitted — in the tier's own order of preference, so `[firecracker, ec2]` means the box at home before the cloud. The ec2 backend now exists to BE that cloud: it is exercised against a fake EC2 API, and its request signing is checked byte-for-byte against Amazon's own signer, but nobody has yet watched a job run on an instance it started ([#32](https://github.com/junioryono/billet/issues/32)). See [Status](#status).

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

> **There is no release yet, so build from source.** The repository has no tags,
> which means the two paths below — the install script and the packages — have
> nothing to download and will fail. They are documented because the pipeline
> that produces them is built and tested; the first tag has simply not been cut.

```bash
git clone https://github.com/junioryono/billet && cd billet && go build ./cmd/billet
sudo mkdir -p /usr/local/bin && sudo install -m 0755 billet /usr/local/bin/billet
```

That second line is what makes the bare `billet` in every command below work; `go build` leaves the
binary in the current directory and installs nothing.

Once a release exists:

```bash
curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | sh
```

Downloads the latest release for your platform, verifies its checksum, and puts
the binary in `/usr/local/bin`. It does not create users, write config, or start
anything.

**For a machine that should run jobs across reboots, install the package
instead** — it ships the systemd units. Pick the file for your platform from the
[latest release](https://github.com/junioryono/billet/releases/latest):

```bash
sudo dpkg -i billet_*_linux_amd64.deb    # Debian / Ubuntu
sudo rpm -i  billet_*_linux_amd64.rpm    # Fedora / RHEL
```

The package installs the units and **does not enable or start them**. Installing
billet should not connect a machine to GitHub and begin accepting jobs; that is
your decision, and it cannot be made before `/etc/billet/billet.yaml` says
something true.

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

**If you built from source** — the only path that works until a release is cut:

```bash
git pull && go build ./cmd/billet
sudo mkdir -p /usr/local/bin && sudo install -m 0755 billet /usr/local/bin/billet
sudo systemctl restart billet-server                # if you wrote your own unit
```

The two paths below need a published release, and there is not one yet.

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

It resolves itself in the ordinary case: the host destroys the container and says so, or reports what it is actually running — every sweep, not only when it reconnects, because quarantine happens on the reaper's clock and a node that reconnects after an outage usually does so before the leases it was holding have expired. A quarantined lease missing from that report has no container by definition. The case that does not resolve is a machine that never returns — its capacity would be missing from the deployment permanently, so `billet leases quarantined` shows what is held and `billet leases release <lease> --force` hands it back. Force, because nothing has confirmed anything: you are asserting the compute is gone, and if you are wrong that slot is sold twice.

## Guest images

A Firecracker job boots a **golden image**: an Ubuntu 24.04 rootfs carrying Docker, the
GitHub Actions runner, and a small agent that reads the runner registration out of the
metadata service and starts the runner with it. It lives in Ceph as an RBD image with
immutable named snapshots called **generations**, and every job gets a copy-on-write
clone of one, discarded when the guest is.

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

If nothing has passed verification, `@verified` **refuses** rather than booting something
unproven.

### Why this is a schedule and not a chore

**GitHub stops sending jobs to a runner that is more than 30 days behind a release.** The
service refuses the message rather than asking the runner to update, so nothing on the
runner's side recovers: the fleet looks healthy and jobs simply queue.

An ordinary self-hosted runner updates itself in place, so a stale image would only be
slow. billet's cannot — a JIT configuration from GitHub's API carries
`DisableUpdate = True` (measured; there is no parameter to ask otherwise), so **the runner
baked into an image is the runner forever** and republishing is the only way past the
deadline.

So enable the timer. **It runs the build scripts from a checkout**, which the packages do
not install — the unit's `REPO` must point at one, and at a path that will still be there
next Sunday:

```bash
sudo git clone https://github.com/junioryono/billet /opt/billet   # what REPO points at
sudo cp deploy/billet-image-refresh.{service,timer} /etc/systemd/system/
sudoedit /etc/systemd/system/billet-image-refresh.service          # REPO, CONFIG, BILLET
sudo systemctl enable --now billet-image-refresh.timer             # the TIMER, not the service
```

Enabling the *service* instead of the timer would run a two-hour root build at every boot,
which is why it deliberately carries no `[Install]` section.

Weekly, against a 30-day deadline, because the run itself can fail — on a package mirror,
on a verification, on a full disk — and a monthly cadence leaves no room for the retry.

**Enable it on every node — that is the redundancy.** A timer on one machine stops when
that machine does, and GitHub's thirty days do not pause while it is down. With every node
carrying it, the rebuild happens as long as *any* node is up.

They cooperate rather than duplicate. Whichever starts second asks `billet images due`,
finds a recent generation, and stands down with exit 0 — a machine standing by has
succeeded, and a unit that reported failure every week on every node but one would teach
you to ignore it. If two genuinely overlap, an `rbd lock` taken in the cluster stops the
writes interleaving, and a run on one machine is kept single by a file lock.

### What it does, and what it deliberately does not

Each run rebuilds, publishes a new generation, and then **verifies it by booting it** —
the guest itself reports that it dropped to the unprivileged account, that the
registration arrived intact, that the runner binary executes, that Docker is up on
billet's kernel, and that a container ran. Nothing the host can check on its own is
enough: an image that boots perfectly and runs no job passes every host-side signal there
is.

It **does not promote**. Publishing is safe by construction — a generation is immutable
and nothing boots it until a tier names it — while promotion puts a new image in front of
every job at once. So a run leaves a verified generation and prints it, and pointing a
tier at it stays a decision you make:

```yaml
tiers:
  - label: billet-2vcpu
    image: ubuntu-2404-x64@g20260814143405
```

A failed verification therefore leaves the running fleet untouched by construction rather
than by care.

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

billet is pre-alpha. **A job runs end to end in a container**, and nothing above that line is
built. What works **today**:

| | |
|---|---|
| `billet github-app create` | Creates and installs the GitHub App via the manifest flow |
| `billet check` | Validates the config, the App private key, and the state database |
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
| `billet leases quarantined` | Capacity held for compute nobody has confirmed gone, and which host holds it |
| `billet leases release <lease> --force` | Hands that capacity back, for a machine that is never coming back |
| `billet ami build` | Builds the AWS machine image the `ec2` backend launches: the GitHub Actions runner, Docker, git. Deliberately minimal — `actions/setup-*` downloads toolchains at runtime and works, while a workflow assuming a preinstalled toolchain fails loudly. **An AMI id is region-scoped**, so a tier's `image:` only works in the region it was built for |
| `billet teardown` | Removes the scale sets billet created |
| Capacity ledger | Lease state machine, fencing epochs, placement enforcement, escrow before advertising |
| Docker provider | One container per job, JIT registration delivered off argv. **Trials only** — shares the host kernel, so it refuses anything not established as trusted |
| EC2 provider | One instance per job, in one subnet. The instance IS the isolation boundary, so unlike Docker it may run fork pull-request code — but only once `untrusted_security_group_ids` describes a network for it, because an instance isolates the kernel and not the VPC. A launch is idempotent by lease id, so an ambiguous retry cannot start two machines for one job. **Never yet run against a real account** — every test drives a fake EC2 API |
| Crash recovery | A job running when the controller dies is adopted and left to finish, not killed; its capacity stays held |
| Per-machine capacity | Each node reports what it contributes; a tier advertises the smaller of the deployment ceiling and what its machines can hold. A host nothing can reach stops backing advertisements |
| Placement | The control plane chooses the machine when the work is admitted, by provider preference, then packing (`placement: spread` to even the load instead), then name. Reserved floors are held against the machines that could keep them |
| Sites | A node says where it is; a tier may insist on a place. Carries identity today — the storage that will key off it is [#23](https://github.com/junioryono/billet/issues/23) |
| Graceful drain | SIGTERM stops it taking new work and waits for the jobs already running, so `systemctl restart` does not fail somebody's build. See [Updating](#updating) |
| Release pipeline | Tagged releases with checksums, `.deb`/`.rpm` with systemd units, and the install script — **built and never yet run: there are no tags, so no release exists to install.** Build from source until there is one |
| Multi-backend tiers | One label can name several providers, and the preference ORDER decides: the control plane picks the host when the job is admitted, walking the tier's list most-preferred-first. Both halves of the intended pair now exist — `[firecracker, ec2]` means the bare-metal box before the cloud — though nobody has watched a job fail over between them ([#32](https://github.com/junioryono/billet/issues/32)) |
| Firecracker microVMs | One job, one guest kernel, on bare metal. Under the jailer always: chrooted, dropped to its own unprivileged uid, in a cgroup, with a seccomp filter. The root disk is a copy-on-write RBD clone of a golden image, discarded with the guest, and the runner registration is delivered through the metadata service so it is never in argv and never on a disk. The guest image exists too: `scripts/build-guest-image.sh` builds and publishes it, and a guest boots, takes its registration and runs a container in about ten seconds. See [Guest images](#guest-images) |

**Not built:** the Apple Silicon provider; the cache; sticky disks; observability; the dashboard. The **Firecracker guest image** now exists and boots — what it is missing is breadth: it carries Docker and the runner, against the ~50GB of preinstalled software a GitHub-hosted runner has, and the sharpest consequence is that `actions/cache` silently produces caches that can never match one written on a hosted runner ([#66](https://github.com/junioryono/billet/issues/66)). A **cost policy** is closer to a consequence than a feature — an ec2 node declares `max_vcpu` and `max_memory`, and provider order decides that home fills first — but it is not yet a spending limit: the allocator charges a job the size its TIER asked for, while the backend buys the first declared shape that fits, so a 2-vCPU tier backed by an 8-vCPU shape can spend four times the declared budget ([#47](https://github.com/junioryono/billet/issues/47)). Nothing reacts to a price either ([#44](https://github.com/junioryono/billet/issues/44)), and nothing drains an instance AWS is about to reclaim ([#41](https://github.com/junioryono/billet/issues/41)).

**billet runs a fleet, with one thing still missing before it is worth having one.** Capacity is a
figure per machine, so hosts of different sizes can be described and a tier advertises only what its
machines can actually hold. The control plane chooses the host when the work is ADMITTED — which is
what finally makes `providers: [firecracker, ec2]` mean "the machine at home first, the cloud if you
must" — and a destroy goes to the machine holding the container rather than to everyone
([#21](https://github.com/junioryono/billet/issues/21),
[#30](https://github.com/junioryono/billet/issues/30),
[#31](https://github.com/junioryono/billet/issues/31)).

What is still true is that **a cache lives on the machine that built it**
([#23](https://github.com/junioryono/billet/issues/23)), so a second host is a second cold cache
until shared storage lands — and a job that fails over to the cloud runs cold by design, since
keeping it warm would mean shipping cache bytes over a WAN.

**A terminate request is not a stopped guest**, and that is the one known correctness gap in this backend ([#46](https://github.com/junioryono/billet/issues/46)). `TerminateInstances` returns when the request is accepted while the machine keeps running for a minute or two, and billet releases the lease on that success — so on a drain or a forced teardown a new job can start while the old guest is still finishing a deploy. Waiting inside the teardown is not the fix, because a node runs one command at a time and it would stall every launch behind it.

**One ec2 node is a serial launch queue**, because a node executes one command at a time. That is invisible for a backend where a node is one machine's worth of jobs and visible for one where a single node can stand for sixty — so a large cloud fleet wants several ec2 nodes, each registered separately with its own budget, rather than one with a large one.

**The cloud half is written and unproven.** `provider: ec2` launches one instance per job and the labels can already express the fallback, but the whole backend has only ever talked to a fake EC2 API. Until somebody stops the bare-metal host mid-workflow and watches the same `runs-on` label finish in a region, treat it as untested against the thing it is for ([#32](https://github.com/junioryono/billet/issues/32)). [#33](https://github.com/junioryono/billet/issues/33) tracks the whole plan.

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

**And the operator commands run against a live control plane**, which is when you actually need them. `billet nodes pending|approve|deny|revoke`, `ca token|issue|revoke|revocations`, `leases quarantined|release` and `check` reach the ledger without taking the exclusive lock the server holds: that lock exists to stop two control planes writing conflicting scheduling decisions, and a one-shot command is not one — it makes no scheduling decisions, and the writes it does make are ordinary transactions SQLite serialises against the server's own.

What they deliberately will **not** do is migrate a ledger another process is HOLDING. Run a newer billet's CLI against an older running control plane and it refuses rather than upgrading a schema that plane is mid-transaction against, and tells you which side to restart. A **stopped** deployment is the other case and it does migrate: whoever opens the ledger first creates or upgrades it, which is what lets `billet ca issue` work on a fresh install before any server exists — so running a newer binary's command against a stopped older deployment upgrades its schema, and that older server will then refuse to start. Upgrade the server binary at the same time. A command that needs to *write* while the plane happens to be mid-decision waits for it, and if it waits too long it stops and says so rather than hanging silently. Some commands commit more than one transaction — `nodes revoke` records each older certificate before withdrawing them — so it tells you that whatever it had already done stands, rather than pretending it was a no-op. A command that only reads never waits for the write lock at all, and no command re-verifies the whole ledger on the way in — that scan belongs to the control plane, which is about to schedule against it, and `billet check` asks for it explicitly. So `leases quarantined` and `nodes pending` answer immediately however busy or however old the deployment is.

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

**Not yet run against a real organization.** The end-to-end path is exercised by a test suite that
drives the real control plane and a real container runtime against a scripted stand-in for GitHub's
Actions service. That catches protocol mistakes, but it is not the same as having run a workflow.

Everything below describes the intended design. Where a thing is not built, it says so.

## Quickstart

> **Use `billet init` rather than copying `billet.example.yaml`.** The example describes the intended Firecracker deployment, and that provider is not built, so it does not run as shipped: the provider has to change in the node section and in every tier, the `ceph:` block has to be deleted (it is *refused* rather than ignored on a backend that cannot attach a block device, so a config that keeps it does not load), and each tier's `image:` has to become something pullable, because the image name is handed straight to the backend and `ubuntu-2404-x64` is a golden-image name. `billet init` writes a config that runs today.
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

## Security

Read this before pointing `billet` at anything.

**Do not use self-hosted runners with public repositories.** This is
[GitHub's own guidance](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access),
not ours. Fork pull requests do not receive your secrets, but they *do* get arbitrary code execution
on your hardware. `billet` isolates jobs in microVMs on bare metal, which helps — each job gets its own kernel — but **it does not make running untrusted code on your own machine safe, and billet will not pretend otherwise**. A microVM's boundary is the KERNEL, not the network it is attached to: a guest on your ordinary bridge reaches whatever that bridge reaches. So billet REFUSES fork pull-request work until `node.firecracker.untrusted_bridge` names a separate network for it, and what that network may reach is yours to write — billet programs no firewall. The same rule governs the `ec2` provider, which rents you the boundary instead of owning it. The `docker` provider shares the host kernel and refuses untrusted work outright. Private repos with trusted contributors are the intended use case.

**Caches are a deliberate cross-job channel.** A job that writes a secret into a cached directory
persists it for later jobs to read. Trust classes are *designed* to control who may publish a cache —
only jobs from `push`/`schedule`/`workflow_dispatch` on the default branch — but **they are not
implemented** (P3 below), and neither is the cache. Even once they are, nothing prevents a trusted job
from leaking into its own cache. The rule is the same as with GitHub's and every other cache:
**don't cache secrets.**

**GitHub App permissions.** `billet` requests exactly two:

| Permission | Level |
|---|---|
| Metadata | read |
| Organization self-hosted runners | read & write |

No repository *Contents* permission — `billet` cannot read your code. (It is not literally "no
access to anything", and any project claiming that is overselling; the App can manage runners on your
org, which is a real capability.)

**The cache intercepts TLS.** *(Design; the cache is not implemented — P4 below.)* To serve
`actions/cache` locally without workflow changes, guest images will trust a CA generated for your
deployment. That CA's private key is a real secret living on each node — treat it like a signing key.

Interception is **opt-in per tier** (`intercept: true`) and defaults to off. It is a static tier
property, not a per-job decision: billet cannot tell from the label whether a given job will publish
a release artifact. **Define a separate tier without `intercept` for jobs that produce release
artifacts or hold deployment secrets, and point those jobs at it.** The reason is that
`ACTIONS_RESULTS_URL` carries artifact metadata as well as cache traffic, so anything in that path is
in your release path. Per-org and per-repo controls do not exist yet.

## Compatibility caveats

**There is no supported way to point `actions/cache` at your own server**, which is why billet
intercepts it rather than asking you to swap the action out.
[actions/toolkit#1051](https://github.com/actions/toolkit/issues/1051) — "add support for
non-GitHub-hosted caching for self-hosted runners" — has been open since **April 2022**, and the PR
to allow a custom cache URL is still unmerged. Every self-hosted cache worth using does the same
thing for the same reason.

**The Actions Cache v2 protocol is reverse-engineered.** GitHub has never published the `.proto`
files ([actions/toolkit#1931](https://github.com/actions/toolkit/issues/1931) has been open
since January 2025), so every implementation — including the one billet will have — is derived from the generated
TypeScript client and wire captures. **GitHub can change it without notice.** The plan is a
conformance suite run against live GitHub on every image build to catch drift early, plus **failing
open to a cache miss** on any error rather than failing your job — a cache miss is always better than
a stall. Neither exists yet; the cache itself is unimplemented.

**Apple Silicon support requires [Tart](https://tart.run), which is not open source.** Tart is
licensed FSL-1.1-ALv2; each release converts to Apache-2.0 after two years, and competing commercial
use is restricted. `billet` treats it as an optional external dependency you install yourself, the
same as Docker or Ceph. `billet` itself is Apache-2.0 throughout.

**A compute host needs Ceph, on the nodes' own NVMe.** A snapshot on one machine cannot be mounted on
another, so a cache kept in local storage is a cache that pins every repository to the host that
first built it. Ceph RBD gives the same snapshot-and-clone primitive from a pool any node at the site
can map, which is what makes a cache a property of a *place* rather than of a machine — and it is
what the commercial products run. billet installs nothing: you run `cephadm bootstrap`, create two
pools, run `ceph osd set-require-min-compat-client mimic`, and point `node.ceph` at them, the same way
you install Docker or Tart yourself. That last command is not optional and `billet check` refuses a
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
| P3 — Ceph, the storage layer, sticky disks, trust classes | 🚧 Ceph replaces ZFS and the reference cluster is built ([#23](https://github.com/junioryono/billet/issues/23)); the storage layer is not ⬜ [#20](https://github.com/junioryono/billet/issues/20) [#25](https://github.com/junioryono/billet/issues/25) [#26](https://github.com/junioryono/billet/issues/26) |
| P4 — colocated Actions cache | ⬜ [#29](https://github.com/junioryono/billet/issues/29) |
| P5 — Docker layer cache, registry mirrors, container baseline | ⬜ [#27](https://github.com/junioryono/billet/issues/27) [#28](https://github.com/junioryono/billet/issues/28) |
| P6 — observability, SSH-into-a-job | ⬜ |
| P7 — Apple Silicon provider (macOS + Linux arm64) | ⬜ |
| P8 — EC2 provider, cloud-hosted control plane, provider failover | 🚧 the provider is built and has never run against a real account; nothing builds the AMI it needs ([#42](https://github.com/junioryono/billet/issues/42)) [#32](https://github.com/junioryono/billet/issues/32) |
| P9 — per-node capacity, admission-time placement, addressed teardown. **A prerequisite of P8**, not a sequel: failover needs the decision made before the work is accepted | ✅ [#21](https://github.com/junioryono/billet/issues/21) [#30](https://github.com/junioryono/billet/issues/30) [#31](https://github.com/junioryono/billet/issues/31) |
| P10 — dashboard, signed releases, public launch | 🚧 releases and packages done; signing and the dashboard are not |
| P11 — AWS Terraform | ⬜ |

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
