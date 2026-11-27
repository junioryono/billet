# Security overview

This page is for deciding whether to run billet. It covers what billet can and cannot protect, what it is allowed to do on GitHub, what runs with privilege, how billet authenticates what it runs, and where each piece of data it handles lives. Each section links to the page that states it in full. To report a vulnerability, see the repository's `SECURITY.md`.

## The threat model in one place

billet runs CI jobs on machines you own. A job is arbitrary code, chosen by whoever can push to a repository the tier serves.

- **billet is not a sandbox unless the backend is a real boundary and the network is its own.** Each of these gives a job its own kernel and a machine no other job shares: a Firecracker microVM, a tart VM, an EC2 instance, and an on-demand CodeBuild container build. A Docker container shares the host's kernel, so on the Docker backend a job can affect the host it runs on. A CodeBuild reserved-capacity fleet keeps its machines alive between builds and shares cached data across them, and a non-container CodeBuild environment runs the job directly on such a machine. Neither is a boundary, so billet refuses untrusted work on them. [Trust and isolation](trust-and-isolation.md) says what each backend isolates and how a tier is marked trusted or untrusted.
- **An untrusted tier needs a network of its own.** By default, the host role's firewall on a Firecracker host denies guests the private address ranges. The host itself answers a guest on a short list of ports: DHCP, DNS, the cache endpoint, the site's registry mirrors, and, on the bridge, the image-verification port. The ranges and ports are role variables an operator can change. A Firecracker tier can also name an egress policy that narrows what its guests reach further. It is enforced on the host rather than inside the guest ([ADR-028](../reference/decisions/adr-028-host-enforced-egress.md), [Egress](../operating/egress.md)).
- **Caches are a deliberate channel between jobs.** What one job writes to a cache, a later job of the same scope reads. `tiers[].cache.publish` decides who may write ([ADR-013](../reference/decisions/adr-013-branch-protected-caches.md)):
  - `trusted-only`, the default on a trusted tier, publishes what any successful job on a trusted tier wrote;
  - `default-branch` publishes only what a job GitHub proves ran for the default branch wrote;
  - `off` publishes nothing.

  An untrusted pull request therefore cannot write what another job will read. Everyone who can run a job on a trusted tier can.
- **The guest is never believed.** Nothing a guest writes is quoted into a billet error, and nothing it reports about itself decides anything ([What runs as what](identity-and-security.md#what-runs-as-what)).

## What billet may do on GitHub

billet creates its GitHub App through the manifest flow, so every deployment holds the same permissions for its scope (`internal/github/manifest.go`):

| Target | Permissions requested |
|---|---|
| An organization | metadata: read; organization self-hosted runners: read and write |
| A repository | metadata: read; administration: read and write. This is the only permission GitHub offers for registering a repository's runners, and billet uses it for that alone. GitHub grants it far more: the repository's settings, collaborators and branch protection. |
| Either, by default | actions: read, to read a run's record (which job ran on which runner, and its steps). A cache publication's proof needs it, and so does an alert rule on a branch, a step or `stranded`. It also reaches the run's logs and artifacts. |
| A target with a tier that sets `retry.disrupted` or `retry.stranded` | actions: write instead. billet uses it to cancel a run and re-run its failed jobs, and GitHub grants the rest of what Actions write allows with it. |

billet never asks for contents, secrets, workflows, pull requests or checks, so the App cannot read a repository's code or its secrets through the API. That is a statement about the App, not about the machines:

- a job's logs and artifacts can contain anything the job printed or uploaded, and `actions: read` reaches them;
- the node's Git proxy fetches repository contents with the job's own GitHub credentials, for exactly the request the job made ([Build caches](../operating/build-caches.md));
- a job's secrets reach the runner, on hardware billet manages.

`billet check` verifies each App holds exactly what its config needs, in both directions: an App holding a permission billet never asked for is refused like one missing a permission it needs. The one exception is `actions: read`, which billet requests by default ([The GitHub App](identity-and-security.md#the-github-app)). A deployment can also hold no App key at all and ask a token broker for each token ([ADR-025](../reference/decisions/adr-025-installation-tokens-from-a-broker.md)).

## What runs with privilege

On Linux the control plane runs as the unprivileged `billet` account, with an empty capability set and a read-only view of the system. Those restrictions come from the packaged systemd unit, not from the binary. Three other services matter here:

- The packaged backup service runs under the same account and restrictions.
- `billet-upgrade` runs as root, because it replaces billet itself.
- `billet-images-refresh` runs as root, because it installs guest images and kernels a root node reads.

The node runs as root on Linux, because Firecracker needs TAP and block devices, cgroups and chroots. The jailer drops each microVM to its own uid before the microVM reads guest state.

On macOS the node and the control plane are launch agents in the operator's login session. They run as that user, without the systemd sandbox. [What runs as what](identity-and-security.md#what-runs-as-what) is the full table.

## How what billet runs is authenticated

- **Releases.** Each release's `release-manifest.json` is signed with Sigstore inside the release workflow. The automatic updater, `billet host-upgrade` and `billet rollout` verify the signature and the signing workflow's identity before they trust any digest the manifest names (`internal/releasesource`, [Upgrades](../operating/upgrades.md)). `billet release inspect` says which manifest produced the installed binary. There are two exceptions:
  - The first install through `install.sh` checks the archive against the release's checksum file but verifies no signature, since there is no billet yet to verify one with. The installed binary's provenance can be checked afterwards with `billet release inspect`.
  - `billet host-upgrade` and `billet rollout` accept `--skip-signature-verification`, an operator's explicit waiver for one run.
- **Guest images.** A guest image's manifest is signed the same way and verified before it is parsed. A Firecracker tier boots only a verified generation or one it pins exactly. `billet images pull` accepts the same waiver, `--skip-signature-verification`, for a directory an operator built and vouches for. The Docker backend runs whatever image reference its tier names, a mutable tag included, with no signature or digest check ([Guest images](../operating/guest-images.md)).
- **The node wire.** A control plane on a network address is its own certificate authority. Its fleet listener serves nothing to a caller without a certificate it issued. Certificates renew over the wire and are revoked by serial, and the authority rotates as an overlap ([The node wire](identity-and-security.md#the-node-wire), [CA rotation](../operating/ca-rotation.md)). Three paths differ:
  - `server.bootstrap_listen`, when set, is a second listener that takes callers without a certificate. It serves only the CA and the enrollment request.
  - A node joins with a bundle copied by hand, or by an enrollment an operator approves after comparing two fingerprints. A host the bare-metal pool ordered joins without a person when the source that placed it vouches for its key ([ADR-030](../reference/decisions/adr-030-rented-bare-metal-pool.md)).
  - A control plane and node on one machine talk over loopback in plain HTTP, with no certificates.

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
| The GitHub App private key | the control plane host, in a file only the service account reads; with `server.identity.backend: aws-ssm`, a Parameter Store parameter the controllers share instead; or, with a token broker, the broker's host alone | the control plane, or the broker; in Parameter Store, whatever IAM and KMS let read it | until an operator replaces it. The control plane's key is in every backup archive. With a broker, the archive holds no key, and the broker's own backup is the operator's to keep. |
| Installation tokens | the control plane's memory | the control plane | an hour, GitHub's own expiry |
| A runner's registration (JIT config) | handed to one guest at its launch; on CodeBuild, staged in Parameter Store under the node's path | that guest. On CodeBuild, also anything IAM and KMS let read that path: the project's service role can read every registration staged under it, which is why one CodeBuild node never serves trusted and untrusted tiers together, and with the default key the control plane's sweep could decrypt one, though it never does. | single use; a staged one is removed when its build ends, or by the control plane's sweep |
| The node-wire CA key, node certificates | the control plane's identity directory, and with `aws-ssm` a copy in Parameter Store for the controllers that share it; each node's state directory | the service account on each; in Parameter Store, whatever IAM and KMS let read it | until rotated or revoked; the CA is in every backup |
| The ledger: leases, capacity, node registrations, job history (repository, workflow, job name, branch, result, timings) | the control plane's database | the service account, and operators through `billet` commands | job history is kept for the deployment's life |
| A job's measured detail: CPU, memory, disk and network series, the destinations it reached | the ledger, when the node's `node.monitoring` collects them | as above | `server.job_detail_retention`, 72 hours by default |
| The guest's own report of itself, and the test results it collects | the ledger, when `node.monitoring.guest` is on | as above | the report with the detail above; test results for `server.test_results_retention`, 14 days by default, within `server.test_results_max_bytes`, 128 MiB by default |
| What a job's steps printed | on the node that ran the job, under its state directory, for a Firecracker tier that sets `tiers[].logs.keep`. The control plane's index ([ADR-031](../reference/decisions/adr-031-job-logs.md)) holds where each line is, not its text; `billet logs search` reads the text from the node. | the node's root, and operators through `billet logs search` | the node's `node.logs`: 14 days, 64 GiB in all and 64 MiB a job by default. The index has its own `server.logs`, 14 days and 32 GiB by default, and pruning it deletes no text on a node. |
| Cache content: the Actions cache, sticky disks, the Go, Bazel and npm caches | the site's storage | jobs of the same scope, by design ([ADR-013](../reference/decisions/adr-013-branch-protected-caches.md)) | until evicted for age or for the pool's room ([The Actions cache](../operating/actions-cache.md)) |
| Git mirrors | the node's state directory | a job, for what its own GitHub credentials may fetch | removed after seven days unused, or sooner, least recently used first, when their filesystem is 80% full |
| Registry mirrors | each Firecracker host's `/var/lib/billet-registry` | every guest on that host, trusted or not; they hold only public images | `billet_registry_mirror_ttl`, 168 hours by default |
| A cache bearer | its guest; and the node's `<state_dir>/cache-sessions`, a file only root reads, so a restarted node knows it | that guest, and the node | valid for the guest's lifetime; its file is removed when the session's cleanup ends, which can wait on storage after the guest is gone |
| Alert notices: the rule and condition, the job's repository, workflow, job name, branch and run link, and the tier that ran it | sent to each Slack channel or webhook a rule names ([Alerts](../operating/alerts.md)). A webhook receives the whole notice as JSON, which adds the result, node, lease, disruption, re-run decision, duration and firing time. Slack receives a sentence that adds the disruption and the duration, and leaves out the node, the lease, the re-run decision and a separate result. Each notice is also kept in the ledger's alert outbox, and so in backups. | whoever reads that channel or endpoint; in the ledger, as above | at the destination, as it keeps them; in the ledger, a delivered or failed notice for 8 days 6 hours, and one still pending until it settles |
| An alert destination's address | an environment variable or file the config names, read when the control plane starts and held in its memory | the control plane | as long as that file or variable; a changed or removed address takes effect at the control plane's next start. Never written to the ledger or a log. |
| Backups | the packaged backup directory, and an S3 bucket if one is configured ([Backup, restore and recover](../operating/backup-restore-recover.md)) | root and the service account; in the bucket, whatever its policy, IAM and key allow | in the packaged directory, the newest seven, and fewer when the disk needs room; an older one is removed only once the bucket lists it complete. A backup written anywhere outside the packaged directory is never pruned. In the bucket, as its lifecycle keeps them. |

billet reports nothing about its use to anyone. Beyond GitHub, it talks only to the services a deployment's configuration chooses. These are the ones that receive its data:

- a PostgreSQL ledger's server, which holds the whole ledger;
- alert destinations and the backup bucket;
- on AWS, the APIs of the services a deployment uses:
  - EC2 and CodeBuild receive launch requests, and Parameter Store receives runner registrations;
  - EBS and S3 receive cache content;
  - with `aws-ssm`, Parameter Store also receives the App key and the node-wire authority;
  - SQS, IAM, STS, Service Quotas and the Price List API receive questions about the deployment's own queue, instance profile, identity, quotas and instance prices;
- the site's Ceph cluster;
- a bare-metal pool's host provider, which receives the hosts it is asked to place;
- a token broker, which receives each token request;
- a smart plug `node.monitoring.meter` names, which is asked for the host's power.

A Firecracker host also passes each guest's DNS queries to the upstream resolvers the host role names (1.1.1.1 and 1.0.0.1 by default). The uplink shaper, where it runs, sends ICMP echoes with no payload of billet's to the addresses it probes (1.1.1.1, 8.8.8.8 and 9.9.9.9 by default).

billet fetches its releases, guest images, and the caches' upstream content.
