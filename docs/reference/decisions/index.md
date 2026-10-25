# Architecture decisions

Each record states the context, the decision, what was measured rather than reasoned about, and the alternatives that were rejected with the reason. They are cited by path from the code, so they stay where they are.

```{toctree}
:maxdepth: 1

adr-001-control-plane-hosting
adr-002-cloud-compute-backend
adr-003-ceph-rbd
adr-004-terraform-provider
adr-005-runner-image-parity
adr-006-rollouts
adr-007-codebuild-provider
adr-008-state-backends
adr-009-controller-election
adr-010-automatic-updates
adr-011-targets-and-repository-scope
adr-012-steady-advertisement
adr-013-branch-protected-caches
adr-014-guest-image-from-githubs-template
adr-015-typed-composition-root
adr-016-prometheus-metrics
adr-017-workspaces
adr-018-git-proxy-answers-as-github
adr-019-guest-image-corresponding-source
adr-020-npm-registry-interception
adr-021-fleet-envelope
```

| Record | Decides |
|---|---|
| [ADR-001](adr-001-control-plane-hosting.md) | where the control plane runs and what stores its state: one small EC2 instance with SQLite on EBS, recovered by EC2 auto-recovery rather than an Auto Scaling group; why "recovers in minutes" and not high availability |
| [ADR-002](adr-002-cloud-compute-backend.md) | which AWS service runs a job: EC2 instance per job, and why billet signs its own requests instead of taking the SDK |
| [ADR-003](adr-003-ceph-rbd.md) | why every cache is an RBD image in a Ceph cluster on the nodes' own NVMe, how the reference cluster was built, and what a real build costs on it |
| [ADR-004](adr-004-terraform-provider.md) | why there is no billet Terraform provider before billet has a configuration API |
| [ADR-005](adr-005-runner-image-parity.md) | why matching GitHub's hosted image is a rebuild from GitHub's own declaration, and what it costs |
| [ADR-006](adr-006-rollouts.md) | why upgrading a fleet is one durable decision resolved to a manifest digest, and why the host transaction's order is its safety content |
| [ADR-007](adr-007-codebuild-provider.md) | the CodeBuild backend, and why it does not use CodeBuild's own Actions runner integration |
| [ADR-008](adr-008-state-backends.md) | SQLite and PostgreSQL behind one contract, with one query set serving both engines |
| [ADR-009](adr-009-controller-election.md) | the controller election: a session advisory lock plus an epoch fence, and no failure detector |
| [ADR-010](adr-010-automatic-updates.md) | why a deployment updates itself by default, how the controller's own upgrade is carried out by a root timer on Linux and a launch agent on a Mac, what the transaction skips on a PostgreSQL ledger, and the release watermark that stops an unattended update going backwards |
| [ADR-011](adr-011-targets-and-repository-scope.md) | one control plane serving several GitHub targets, organizations and repositories, each with its own App; why a repository target is untrusted-only and holds the wider `administration: write` grant; what the Actions service answered at repository scope |
| [ADR-012](adr-012-steady-advertisement.md) | why every tier advertises what it could run rather than what it holds, why capacity is bought when a runner starts, and what the two shipped designs that reserved before advertising cost |
| [ADR-013](adr-013-branch-protected-caches.md) | how a pooled runner's caches publish: by the ref GitHub proves at the completion, in a namespace of the pool's static repository, with every cache on by default where a tier can have it |
| [ADR-014](adr-014-guest-image-from-githubs-template.md) | why the Firecracker guest image is GitHub's own runner-images build, run under systemd-nspawn on Ubuntu's cloud root filesystem, with every difference in one reviewed file |
| [ADR-015](adr-015-typed-composition-root.md) | why billet assembles itself in one typed composition root rather than a dependency-injection container, how the control plane's startup order became proof types, and the lint that keeps the roles built in one place |
| [ADR-016](adr-016-prometheus-metrics.md) | why billet serves Prometheus metrics through one package and only when asked, what the client costs in the binary, and why the profiler is loopback only |
| [ADR-017](adr-017-workspaces.md) | proposed: workspaces, long-lived environments forked from a warm Firecracker snapshot; why only an explicit request stops one, why nothing in a warmed stack runs until a fork is made unique, why a suspension is private and proved durable before its VM stops, why previews need no listener on a node and each workspace gets an origin of its own, and why repository access is a separate GitHub App that entitles no one by itself |
| [ADR-018](adr-018-git-proxy-answers-as-github.md) | why the Git proxy serves a job GitHub authorised any object its repository's mirror holds, as GitHub does; why anything the mirror cannot do goes to GitHub, a refresh belongs to no request, and the proxy reaches GitHub over HTTP/1.1 with every request body whole |
| [ADR-019](adr-019-guest-image-corresponding-source.md) | why every guest release carries the corresponding source of what it redistributes under a GNU license as its own assets, rather than a pointer to Launchpad or a written offer, where each source version is kept, and what that costs a release |
| [ADR-020](adr-020-npm-registry-interception.md) | why the node may serve the public npm registry to its guests through the Actions cache's interception, a second intercepted host; why only an anonymous read is ever cached, why one store per node serves every trust class, how a stalled fetch is retried and a stale document served, and why it starts off by default |
| [ADR-021](adr-021-fleet-envelope.md) | the fleet one control plane must schedule, 256 hosts and 4,096 open leases, and what each of its writes may take of the ledger's one writer slot; why a listener renews its leases in one commit a pass, why placement is counted a preference rank at a time, and why the commit's speed is the operator's |
