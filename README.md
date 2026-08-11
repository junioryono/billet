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

> The failover part is **half built**. A tier can now name several backends and be
> placed on any of them, so it is no longer pinned to one kind of machine. What is
> missing is the part that CHOOSES: nothing picks among live hosts yet, because a
> node binds itself ([#30](https://github.com/junioryono/billet/issues/30)) — and
> there is no EC2 provider for it to choose
> ([#32](https://github.com/junioryono/billet/issues/32)). See [Status](#status).

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
to GitHub over outbound long-poll, so it needs **no public ingress** — no open ports, no public IP,
no webhook endpoint, no tunnel.

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
```

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

**A compute host** — runs the containers. It needs a `node:` section naming the
control plane, and the certificate bundle `billet ca issue <name>` produced
there (see [Adding a second machine](#adding-a-second-machine)):

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
sudo install -m 0755 billet /usr/local/bin/billet   # wherever you put it
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

## Status

billet is pre-alpha. **A job runs end to end in a container**, and nothing above that line is
built. What works **today**:

| | |
|---|---|
| `billet github-app create` | Creates and installs the GitHub App via the manifest flow |
| `billet check` | Validates the config, the App private key, and the state database |
| `billet server --dry-run` | Connects to a real org, reconciles scale sets, polls — accepts nothing |
| `billet server` | The control plane on its own, serving the node wire. **Start the nodes first** — capacity is advertised from the budget without checking that any node exists, so a job assigned while none is registered is acquired, fails to launch, and is handed back. GitHub retries that about three times and then the job fails |
| `billet server --dev` | Control plane + node in one process: acquires jobs and runs them in containers |
| `billet node` | A separate compute host that dials the control plane and never listens |
| `billet ca issue <node>` | Mints the certificate a node authenticates with, for an operator to copy |
| `billet teardown` | Removes the scale sets billet created |
| Capacity ledger | Lease state machine, fencing epochs, placement enforcement, escrow before advertising |
| Docker provider | One container per job, JIT registration delivered off argv. **Trials only** — shares the host kernel, so it refuses anything not established as trusted |
| Crash recovery | A job running when the controller dies is adopted and left to finish, not killed; its capacity stays held |
| Graceful drain | SIGTERM stops it taking new work and waits for the jobs already running, so `systemctl restart` does not fail somebody's build. See [Updating](#updating) |
| Release pipeline | Tagged releases with checksums, `.deb`/`.rpm` with systemd units, and the install script — **built and never yet run: there are no tags, so no release exists to install.** Build from source until there is one |
| Multi-backend tiers | One label can name several providers and be placed on any of them. The preference ORDER is recorded but not yet acted on — see below |

**Not built:** Firecracker, Apple Silicon and EC2 providers; the cache; sticky disks; the scheduler
that would make provider preference and a cost policy mean something; observability; the dashboard.

**billet is a one-machine product today, and the reasons are specific rather than general.** Capacity
is a single deployment-wide budget rather than a figure per machine, so two hosts of different sizes
cannot be described ([#21](https://github.com/junioryono/billet/issues/21)); nothing chooses a node,
because a node binds itself ([#30](https://github.com/junioryono/billet/issues/30)); a destroy is
broadcast to every node instead of addressed to the one holding the job
([#31](https://github.com/junioryono/billet/issues/31)); and a cache lives on the machine that built
it ([#23](https://github.com/junioryono/billet/issues/23)). Each is invisible with one machine and
wrong the moment there are two. [#33](https://github.com/junioryono/billet/issues/33) tracks the
whole plan.

### Adding a second machine

A control plane bound to a network address requires client certificates, and mints its own authority
to issue them. There is no CA to run and nothing to install:

```bash
# on the control plane
billet ca issue mac-mini-1          # writes ./mac-mini-1-billet-tls/
scp -r mac-mini-1-billet-tls mac-mini-1:/etc/billet/tls

# in that host's billet.yaml
node:
  name: mac-mini-1
  server_addr: billet.example:7717
  tls:
    cert: /etc/billet/tls/node.crt
    key:  /etc/billet/tls/node.key
    ca:   /etc/billet/tls/ca.crt
```

The name in the certificate is the only thing that decides which node a request is from — a host
holding this bundle can act as `mac-mini-1` and as nothing else. The certificate also carries which
**deployment** it belongs to, so the copied bundle is all a fresh host needs; it does not invent an
identity the control plane would refuse.

Loopback stays plain HTTP, because there is nothing between the two processes to authenticate
against. Anything else refuses to start without a certificate rather than serving unauthenticated on
a network, which is the failure that looks like it works.

Two things to know before relying on it. A node certificate lasts a year and its expiry takes that
host out of the fleet with a handshake error, so the control plane warns for the last thirty days
while the node is still working — re-issue on that warning, not on the outage. And there is no
revocation: a compromised node means re-issuing the CA and every node certificate, which is a real
cost and an honest one at this size.

**Not yet run against a real organization.** The end-to-end path is exercised by a test suite that
drives the real control plane and a real container runtime against a scripted stand-in for GitHub's
Actions service. That catches protocol mistakes, but it is not the same as having run a workflow.

Everything below describes the intended design. Where a thing is not built, it says so.

## Quickstart

> Every command below works, with the caveat that the only compute backend is Docker, which is for
> trials rather than for untrusted code. `billet init` is not built — copy the example config.

```bash
billet github-app create --org myorg          # creates + installs the App
cp billet.example.yaml ./billet.yaml          # edit: org, tiers, node provider
billet check --config ./billet.yaml           # validates config, key, state
billet server --dry-run --config ./billet.yaml  # first contact: polls, accepts nothing
billet server --dev --config ./billet.yaml    # runs jobs
```

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
billet server        control plane — scale-set listeners, capacity allocator, scheduler, state
billet node          compute host  — runs a provider, launches instances, reports capacity
billet server --dev  both, on one box
```

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
on your hardware. `billet` is *designed* to isolate jobs in microVMs, which helps — but **today the
only provider is Docker, which shares the host kernel**, so what you actually get is container
isolation. Even once Firecracker lands it will not make running untrusted code on your own machine
safe. Private repos with trusted contributors are the intended use case.

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

**The cache will need Ceph, on the nodes' own NVMe.** A snapshot on one machine cannot be mounted on
another, so a cache kept in local storage is a cache that pins every repository to the host that
first built it. Ceph RBD gives the same snapshot-and-clone primitive from a pool any node can map,
which is what makes a cache a property of a *place* rather than of a machine — and it is what the
commercial products run. It replaces ZFS rather than sitting beside it
([#23](https://github.com/junioryono/billet/issues/23)); on a single box it is honestly more moving
parts than ZFS, and the reason to adopt it anyway is that retrofitting shared storage later means
rewriting placement at the same time.

## Roadmap

| Phase | Status |
|---|---|
| P0 — scaffolding, GitHub App onboarding, host prep | ✅ mostly |
| P1 — runner plane: scale sets, allocator, providers | 🚧 listeners, allocator, Docker and the drain done; Firecracker next ([#24](https://github.com/junioryono/billet/issues/24)) |
| P2 — guest images, node split, user-defined tiers | 🚧 node split + mTLS done; guest images need the machine ([#24](https://github.com/junioryono/billet/issues/24)) |
| P3 — Ceph, the storage layer, sticky disks, trust classes | ⬜ [#20](https://github.com/junioryono/billet/issues/20) [#23](https://github.com/junioryono/billet/issues/23) [#25](https://github.com/junioryono/billet/issues/25) [#26](https://github.com/junioryono/billet/issues/26) |
| P4 — colocated Actions cache | ⬜ [#29](https://github.com/junioryono/billet/issues/29) |
| P5 — Docker layer cache, registry mirrors, container baseline | ⬜ [#27](https://github.com/junioryono/billet/issues/27) [#28](https://github.com/junioryono/billet/issues/28) |
| P6 — observability, SSH-into-a-job | ⬜ |
| P7 — Apple Silicon provider (macOS + Linux arm64) | ⬜ |
| P8 — EC2 provider, cloud-hosted control plane, provider failover | ⬜ [#32](https://github.com/junioryono/billet/issues/32) |
| P9 — per-node capacity, real placement, addressed teardown. **A prerequisite of P8**, not a sequel: failover means nothing until something chooses | ⬜ [#21](https://github.com/junioryono/billet/issues/21) [#30](https://github.com/junioryono/billet/issues/30) [#31](https://github.com/junioryono/billet/issues/31) |
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
