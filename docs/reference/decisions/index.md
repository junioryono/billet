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
