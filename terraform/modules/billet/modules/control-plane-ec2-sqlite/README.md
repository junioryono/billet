# control-plane-ec2-sqlite

billet's control plane and nothing else, per [ADR-001](../../../../../docs/reference/decisions/adr-001-control-plane-hosting.md): a single small EC2 with SQLite on a **dedicated, retained** encrypted gp3 EBS volume, recovered by EC2 auto-recovery — not an ASG, which would launch a fresh instance that does not reattach the data volume. Consume it directly when you have your own compute IAM story; the opinionated root composes it with `fleet-ec2`, passing that child's instance profile so the co-located controller runs billet with the node role.

The fail-closed **mount** of the ledger volume is the configuration layer's job: set the `junioryono.billet.host` role's `billet_ledger_volume_id` to this child's `ledger_volume_id` output.

## Usage

```hcl
module "control_plane" {
  source = "github.com/junioryono/billet//terraform/modules/billet/modules/control-plane-ec2-sqlite?ref=v0.6.1"

  name              = "billet"
  vpc_id            = aws_vpc.mine.id
  vpc_cidr          = aws_vpc.mine.cidr_block
  subnet_id         = aws_subnet.mine.id
  availability_zone = aws_subnet.mine.availability_zone

  # YOUR assertion that the subnet is in that VPC, and it has no default on
  # purpose: this child cannot look the subnet up itself without deferring the
  # read and cascading unknowns through the instance.
  subnet_in_vpc_ok = true

  # DECLARE the controller's address rather than observe it. Every node's
  # server_addr, the Ansible inventory and whatever routes a node here all
  # repeat it; declared, an instance replacement keeps it. subnet_cidr is what
  # lets the plan refuse an address outside the subnet or one AWS reserves.
  private_ip  = "10.0.0.10"
  subnet_cidr = aws_subnet.mine.cidr_block
}
```

**Pin it to a release.** `?ref=` names the version, and it is the same version as the binary and the collection beside it — `versions.tf`, `requirements.yml` and `billet_version` should all say `vX.Y.Z`. What you are reading on `main` says `?ref=main`, because documentation on `main` describes `main`; every release rewrites it to that release's tag, so the copy inside a release names the version you are installing.

## Create/adopt/always table

Every resource this child can own, mechanically complete — a resource absent here is a resource this child never touches.

| Resource | Created when | Notes |
|---|---|---|
| `aws_instance.control_plane` | always | IMDSv2-only; ignores AMI drift; OS-only 16 GiB root |
| `aws_cloudwatch_metric_alarm.control_plane_recover` | always | `StatusCheckFailed_System` → `ec2:recover` |
| `aws_ebs_volume.ledger` + `aws_volume_attachment.ledger` | always | `prevent_destroy`; the SQLite ledger's home |
| `aws_security_group.control_plane` + node-wire/SSH ingress + egress | always | Node wire from `node_ingress_cidrs` (VPC CIDR default) |
| `aws_iam_role.this` + `aws_iam_instance_profile.this` | `create_instance_profile` | A bare EC2-trust identity with **no policies**, so a standalone controller has a profile to hang operator policies on |
| `data.aws_ami.ubuntu` | `ami` empty | Canonical Ubuntu 24.04 for `architecture` |
| `aws_s3_bucket.backups` + versioning, encryption, public-access block, lifecycle | `create_backup_bucket` | `prevent_destroy`; versioned, so billet's never-delete credential means something |
| `aws_iam_role_policy.backups` | a bucket is created **or** adopted | Attached to the own role, or to `instance_profile_role_name` when the profile was supplied. Get, Put and a prefix-scoped List. **No delete, and no compute** — it is billet's own generator's output, from `policy/backup-policy*.json` |

Adopted (never created): the VPC and subnet (`vpc_id`, `subnet_id`, `availability_zone`, `vpc_cidr` are required inputs), the instance profile when `create_instance_profile` is false, and the backup bucket when `backup_bucket` names one — this child grants access to an adopted bucket and configures nothing on it.

## Backups

A backup on the disk it protects is not one. `billet local backup` captures the SQLite ledger, the deployment identity, the GitHub App private key and the node-wire authority as one verified unit, and leaves it on the same volume as the deployment; ADR-001 has always said the copy belongs in S3 with a **rehearsed** restore. Set `create_backup_bucket = true`, then point the config at it:

```yaml
backup:
  s3:
    bucket: <the backup_bucket output>
    region: <this module's region>
    prefix: <the backup_prefix output>
```

Three properties are worth knowing before you rely on it. **billet never deletes** — `internal/archivestore` has no delete at all, and the grant this module attaches carries none, so the credential on the host that also holds the App key cannot destroy the copies whose whole purpose is surviving the loss of that host. **Retention is the bucket's**: `backup_retention_days` expires only NONCURRENT versions, because a rule that expired current objects would remove backups on a timer. And the controller **launches nothing**, so its policy grants no `ec2:` action — the generator's `NoCompute` is what says so, and `internal/tfpolicy`'s drift test pins the rendering this module reads.

## Inputs of note

- `create_instance_profile` — exactly one identity source: true creates the bare own identity (and refuses a supplied name, which would otherwise be silently ignored); set it false and supply `instance_profile_name` to attach an existing profile (the root passes `fleet-ec2`'s).
- `instance_profile_role_name` — the role behind a supplied profile, **required as soon as backups are enabled** beside `create_instance_profile = false`. The backup grant attaches to whichever role the controller actually runs with; the first version attached it only to the own role, so the co-located shape got a bucket, a lifecycle rule and no principal that may write to it — `billet local backup` failed at the upload and `billet check` reported the archive stale. The root passes `fleet-ec2`'s `node_role_name`; a direct consumer names their own. Refused beside the own profile, where it would be silently ignored.
- `subnet_in_vpc_ok` — the caller's explicit, required assertion that the subnet belongs to `vpc_id`; false fails the plan, because a launch cannot mix a subnet and security groups from different VPCs. The root resolves it from its adopted-subnet lookup.
- `private_ip` — the controller's private address, **declared rather than observed**. Left empty, AWS assigns one at launch and nothing pins it: the address is repeated in `server.listen` (which is also the certificate SAN of a concrete listener), in every node's `node.server_addr`, in the inventory's `ansible_host` and in whatever routes a node's path to it, and an instance replacement changes it silently, surfacing as a node timeout that names nothing. Declaring the address an **applied** instance already holds plans no change, because the state carries `private_ip`; any other value replaces the instance (the address is fixed at launch), which is a `draining` change — drain first. With `subnet_cidr` supplied the plan refuses an address outside the subnet or one of the five AWS reserves (the first four and the last); without it the check is skipped, for the same reason `subnet_in_vpc_ok` is an input rather than a lookup.
