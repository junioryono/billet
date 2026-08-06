# billet

Self-hosted GitHub Actions runners on your own hardware, with a colocated cache.

A *billet* is a bar of metal prepared for forging — raw material shaped into something useful, which
is roughly what CI does to source code.

> **Status: pre-alpha.** Nothing here is production-ready yet. Do not point release or deploy
> pipelines at it. See [Roadmap](#roadmap) for what actually works today.

## What it is

`billet` runs your GitHub Actions jobs on machines you control — a server under your desk, a Mac
mini, or EC2 — with the accelerations that make self-hosting worth the trouble:

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

```bash
curl -fsSL https://billet.sh/install | sh     # not live yet — build from source for now
```

```bash
git clone https://github.com/junioryono/billet && cd billet && go build ./cmd/billet
```

## Quickstart

```bash
billet github-app create --org myorg   # creates + installs the GitHub App
billet init                            # pick a provider, define runner tiers
billet server --dev                    # control plane + node on one box
```

Then in a workflow:

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
on your hardware. `billet` isolates jobs in microVMs, which helps, but it does not make running
untrusted code on your own machine safe. Private repos with trusted contributors are the intended
use case.

**Caches are a deliberate cross-job channel.** A job that writes a secret into a cached directory
persists it for later jobs to read. Trust classes control *who may publish* a cache — only jobs from
`push`/`schedule`/`workflow_dispatch` on the default branch — but nothing prevents a trusted job from
leaking into its own cache. The rule is the same as with GitHub's and every other cache: **don't
cache secrets.**

**GitHub App permissions.** `billet` requests exactly two:

| Permission | Level |
|---|---|
| Metadata | read |
| Organization self-hosted runners | read & write |

No repository *Contents* permission — `billet` cannot read your code. (It is not literally "no
access to anything", and any project claiming that is overselling; the App can manage runners on your
org, which is a real capability.)

**The cache intercepts TLS.** To serve `actions/cache` locally without workflow changes, guest images
trust a CA generated for your deployment. That CA's private key is a real secret living on each node
— treat it like a signing key.

Interception is **opt-in per tier** (`intercept: true`) and defaults to off. It is a static tier
property, not a per-job decision: billet cannot tell from the label whether a given job will publish
a release artifact. **Define a separate tier without `intercept` for jobs that produce release
artifacts or hold deployment secrets, and point those jobs at it.** The reason is that
`ACTIONS_RESULTS_URL` carries artifact metadata as well as cache traffic, so anything in that path is
in your release path. Per-org and per-repo controls do not exist yet.

## Compatibility caveats

**The Actions Cache v2 protocol is reverse-engineered.** GitHub has never published the `.proto`
files ([actions/toolkit#1931](https://github.com/actions/toolkit/discussions/1890) has been open
since January 2025), so every implementation — including this one — is derived from the generated
TypeScript client and wire captures. **GitHub can change it without notice.** `billet` runs a
conformance suite against live GitHub on every image build to catch drift early, and **fails open to
a cache miss** on any error rather than failing your job. A cache miss is always better than a stall.

**Apple Silicon support requires [Tart](https://tart.run), which is not open source.** Tart is
licensed FSL-1.1-ALv2; each release converts to Apache-2.0 after two years, and competing commercial
use is restricted. `billet` treats it as an optional external dependency you install yourself, the
same as Docker or ZFS. `billet` itself is Apache-2.0 throughout.

## Roadmap

| Phase | Status |
|---|---|
| P0 — scaffolding, GitHub App onboarding, host prep | 🚧 in progress |
| P1 — runner plane: scale sets, allocator, Firecracker | ⬜ |
| P2 — guest images, node split, user-defined tiers | ⬜ |
| P3 — copy-on-write storage layer, trust classes | ⬜ |
| P4 — colocated Actions cache | ⬜ |
| P5 — Docker layer cache, registry mirrors, container baseline | ⬜ |
| P6 — observability, SSH-into-a-job | ⬜ |
| P7 — Apple Silicon provider (macOS + Linux arm64) | ⬜ |
| P8 — EC2 provider, WAN topology | ⬜ |
| P10 — dashboard, signed releases, public launch | ⬜ |
| P11 — AWS Terraform | ⬜ |

## Prior art

`billet` is an open-source take on what [Blacksmith](https://blacksmith.sh) built, and it borrows
several of their published designs — persistent BuildKit state, snapshot-clone caches, transparent
cache interception. Their [engineering blog](https://www.blacksmith.sh/blog) is worth reading.
[Ubicloud](https://github.com/ubicloud/ubicloud) (AGPL) is the highest-quality open reference for how
a commercial runner cloud is actually built, and [GARM](https://github.com/cloudbase/garm) is the
closest existing OSS control plane.

## License

Apache-2.0. See [LICENSE](LICENSE).
