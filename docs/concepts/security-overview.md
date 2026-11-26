# Security overview

This page is for deciding whether to run billet: what it can and cannot protect, what it is allowed to do on GitHub, what runs with privilege, how what it runs is authenticated, and where every piece of data it handles lives. Each section links to the page that states it in full. To report a vulnerability, see the repository's `SECURITY.md`.

## The threat model in one place

billet runs other people's CI jobs on machines you own, and a job is arbitrary code chosen by whoever can push to a repository the tier serves.

- **billet is not a sandbox unless the backend is a real boundary and the network is its own.** A Firecracker microVM, a tart VM, an EC2 instance and a CodeBuild build each give a job its own kernel. A Docker container shares the host's, so on the Docker backend a job can affect the host it runs on. [Trust and isolation](trust-and-isolation.md) says what each backend isolates and how a tier is marked trusted or untrusted.
- **An untrusted tier needs a network of its own.** On a Firecracker host the host role denies guests the private address ranges, and the host itself answers a guest only for DHCP, DNS and the cache endpoint. A tier's egress policy can narrow what a guest reaches further, enforced on the host rather than inside the guest ([ADR-028](../reference/decisions/adr-028-host-enforced-egress.md)).
- **Caches are a deliberate channel between jobs.** What one job writes to a cache, another job of the same scope reads. Publication is gated by the ref GitHub proves the job ran for ([ADR-013](../reference/decisions/adr-013-branch-protected-caches.md)), so an untrusted pull request cannot write what a default-branch job will read.
- **The guest is never believed.** Nothing a guest writes is quoted into a billet error, and nothing it reports about itself decides anything ([What runs as what](identity-and-security.md#what-runs-as-what)).

## What billet may do on GitHub

billet creates its GitHub App through the manifest flow, so every deployment holds the same permissions for its scope (`internal/github/manifest.go`):

| Target | Permissions requested |
|---|---|
| An organization | metadata: read; organization self-hosted runners: read and write |
| A repository | metadata: read; administration: read and write, the only permission GitHub offers for registering a repository's runners, used for that alone |
| Either, by default | actions: read, to read a run's record (which job ran on which runner, its steps), which a cache publication's proof and an alert rule on a branch, a step or `stranded` need |
| A target with a tier that sets `retry.disrupted` or `retry.stranded` | actions: write instead, to cancel a run and re-run its failed jobs |

billet never asks for contents, secrets, workflows, pull requests or checks, so it cannot read your code or your secrets. `billet check` verifies each App holds exactly what its config needs, in both directions: an App holding a permission billet never asked for is refused like one missing a permission it needs. The one exception is `actions: read`, which billet requests by default ([The GitHub App](identity-and-security.md#the-github-app)). A deployment can also hold no App key at all and ask a token broker for each token ([ADR-025](../reference/decisions/adr-025-installation-tokens-from-a-broker.md)).

## What runs with privilege

The control plane runs as an unprivileged account with an empty capability set and a read-only system. The node runs as root on Linux, because Firecracker needs TAP and block devices, cgroups and chroots. Each microVM is dropped to its own uid by the jailer before it reads guest state. On macOS the node is a launch agent in the operator's login session. [What runs as what](identity-and-security.md#what-runs-as-what) is the full table.

## How what billet runs is authenticated

- **Releases.** Each release's `release-manifest.json` is signed with Sigstore inside the release workflow, and a deployment verifies the signature and the signing workflow's identity before it trusts any digest the manifest names (`internal/releasesource`, [Upgrades](../operating/upgrades.md)). `billet release inspect` says which manifest produced the installed binary.
- **Guest images.** A guest image's manifest is signed the same way and verified before it is parsed. A tier boots only a verified generation or one it pins exactly.
- **The node wire.** A control plane on a network address is its own certificate authority and serves nothing to a caller without a certificate it issued. A node joins either with a bundle copied by hand or by an enrollment an operator approves after comparing two fingerprints. Certificates renew over the wire, are revoked by serial, and the authority rotates as an overlap ([The node wire](identity-and-security.md#the-node-wire), [CA rotation](../operating/ca-rotation.md)).

## What data goes where

```text
GitHub ──scale-set messages, REST──▶ control plane ──ledger (SQLite or PostgreSQL)
   ▲                                     │  node wire (mutual TLS)
   │ one-use runner registration         ▼
   └──────────────── guest ◀──────── node ──▶ site storage (Ceph, EBS + S3)
                       │ caches, step logs, its own report
                       └──────────────▶ node ──▶ control plane
```

| Data | Where it lives | Who can read it | How long |
|---|---|---|---|
| The GitHub App private key | the control plane host, a file only the service account reads, or a token broker | the control plane | until an operator replaces it; it is in every backup archive |
| Installation tokens | the control plane's memory | the control plane | an hour, GitHub's own expiry |
| A runner's registration (JIT config) | handed to one guest at its launch; on CodeBuild, staged in Parameter Store under the build's name | that guest | single use; a staged one is removed when its build ends, or by the control plane's sweep |
| The node-wire CA key, node certificates | the control plane's identity directory; each node's state directory | the service account on each | until rotated or revoked; the CA is in every backup |
| The ledger: leases, capacity, node registrations, job history (repository, workflow, job name, branch, result, timings) | the control plane's database | the service account, and operators through `billet` commands | job history is kept for the deployment's life |
| A job's measured detail: CPU, memory, disk and network series, the destinations it reached, the guest's own report | the ledger | as above | `server.job_detail_retention`, 72 hours by default |
| A job's test results | the ledger | as above | `server.test_results_retention`, 14 days and 128 MiB by default |
| What a job's steps printed | the control plane's log index, a file a day under its identity directory ([ADR-031](../reference/decisions/adr-031-job-logs.md)) | as above, through `billet logs search` | `server.logs.retention`, 14 days and 32 GiB by default |
| Cache content: the Actions cache, sticky disks, the Go, Bazel and npm caches, the Git mirror, registry mirrors | the site's storage | jobs of the same scope, by design ([ADR-013](../reference/decisions/adr-013-branch-protected-caches.md)) | until evicted for age or for the pool's room ([The Actions cache](../operating/actions-cache.md)) |
| A cache bearer | one guest | that guest | the guest's lifetime |
| Alert notices: the rule and condition, the job's repository, workflow, job name, branch, run link and result, and the tier, node and lease that ran it | sent to each Slack channel or webhook a rule names ([Alerts](../operating/alerts.md)) | whoever reads that channel or endpoint | as that destination keeps them |
| An alert destination's address | an environment variable or file the config names, read for each send | the control plane | as long as that file or variable; never written to the ledger or a log |
| Backups | the archive directory, seven kept, and an S3 bucket if configured ([Backup, restore and recover](../operating/backup-restore-recover.md)) | root and the service account | as the archive directory and the bucket keep them |

Beyond GitHub, the only places billet sends anything are the alert destinations and the backup bucket an operator configures. It fetches its releases, guest images and the caches' upstream content, and reports nothing about its use to anyone.
