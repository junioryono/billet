# fleet-ec2

Everything billet's EC2 **compute** side needs and nothing the control plane does: the node IAM role and instance profile carrying billet's own generated policy, the trusted-runner security group, the cache storage, and the spot interruption queue with its tag-scoped router. Consume it directly when you already have a control plane; the opinionated root composes it with `control-plane-ec2-sqlite` and attaches this child's instance profile to the controller for the co-located deployment.

The IAM policy is billet's **own** generator's output (`internal/awspolicy`, kept equal by the `internal/tfpolicy` drift test), feature-gated: compute-only, cache, cache+KMS renderings, plus the spot grant whose actions come from the committed `policy/spot-actions.json` — the same file the plan tests read, so the pin has no second copy to drift.

## Usage

```hcl
module "fleet" {
  source = "github.com/junioryono/billet//terraform/modules/billet/modules/fleet-ec2?ref=main"

  name         = "billet"
  vpc_id       = aws_vpc.mine.id
  enable_cache = true
}
```

**Pin it to a release.** `?ref=` names the version, and it is the same version as the binary and the collection beside it — `versions.tf`, `requirements.yml` and `billet_version` should all say `vX.Y.Z`. What you are reading on `main` says `?ref=main`, because documentation on `main` describes `main`; every release rewrites it to that release's tag, so the copy inside a release names the version you are installing.

## Create/adopt/always table

Every resource this child can own, mechanically complete — a resource absent here is a resource this child never touches.

| Resource | Created when | Notes |
|---|---|---|
| `aws_iam_role.node` | always | Trust policy rendered with `jsonencode`, partition-following |
| `aws_iam_role_policy.node` | always | The generated rendering, or `iam_policy_json` verbatim |
| `aws_iam_role_policy.pass_role` | `job_instance_profile_role_arn` set | `iam:PassRole` on exactly that ARN, to EC2 only |
| `aws_iam_instance_profile.node` | always | Attach to whatever instance runs billet |
| `aws_security_group.runner` + egress rule | always | Trusted work; an untrusted group is deliberately yours |
| `aws_s3_bucket.cache` + public-access block + SSE config | `enable_cache` | Bucket name from `cache_bucket` or `<name>-cache-<account>` |
| `aws_kms_key.cache` + alias | `enable_cache && enable_kms` | Per-deployment EBS snapshot boundary |
| `aws_sqs_queue.interruptions` | `enable_spot` | Basename must equal the spot node's `node.name` |
| `aws_iam_role_policy.spot` | `enable_spot` | Queue-scoped, actions from `policy/spot-actions.json` |
| `aws_iam_role.spot_router` + policy, `aws_lambda_function.spot_router`, `aws_cloudwatch_log_group.spot_router`, `aws_cloudwatch_event_rule.spot_interruption` + target, `aws_lambda_permission.spot_router` | `enable_spot` | The tag-scoped router; least-privilege IAM |
| `data.archive_file.spot_router` | `enable_spot` | Zips the committed Lambda source under the root's `.terraform` |

Adopted (never created): the VPC (`vpc_id` is a required input).

## Inputs of note

- `vpc_id` (required) — this child never creates a network.
- `iam_policy_json` — `billet init iam` output replaces the rendering entirely.
- `job_instance_profile_role_arn` — the exact role trusted JOB instances receive; grants `iam:PassRole` on it alone.
