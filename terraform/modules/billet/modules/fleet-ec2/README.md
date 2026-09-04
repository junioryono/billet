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
| `aws_iam_role_policy.builder` | `builder` | What `billet ami build` needs, as its OWN document so the node's rendering is unchanged. Adds one S3 statement with `builder_payload_bucket` |
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
- `builder` — grant the node role what `billet ami build` performs: `ec2:CreateImage` on a builder-tagged instance and on the image and snapshots it makes, its own `TerminateInstances` for cleanup, `GetConsoleOutput` to read the verifier's report off the console, and the `CreateTags` that stamps a verified image's contract. **Off by default**, because it widens the identity every job's instance is launched by, and a deployment that builds its image elsewhere should not carry it. On, the build runs on the controller with its instance role instead of from a workstation holding an operator's own credentials.

  It is **additive**: the builder's launches ride the node policy's own `RunInstances`, which admits them because this module's rendering is presence-mode and `billet ami build` tags its instances with the same owner key. If you replace the rendering with a **value**-scoped `var.iam_policy_json` (`billet init iam --deployment <id>`), pass `--builder` to that command too, or the runtime statements will not admit the builder's own `billet-ami-build-*` tag.

  **The builder's own statements are scoped by prefix, not by deployment**, and that is a boundary worth knowing before you turn this on in a shared account. `billet ami build` stamps a per-build owner (`billet-ami-build-<image name>`) that carries no deployment id, so the builder's reach is gated by that prefix on the SOURCE INSTANCE, whatever mode the rest of the policy is in. (`BilletAMIBuilderImage` carries no condition at all, because the image and its snapshots do not exist yet at create time and a condition on them would deny the call; what bounds it is the instance statement in the same authorization.) Two billet deployments in one account that both carry this grant can therefore act on each other's **builder** instances (which exist only during a build); neither can touch the other's job instances, cache volumes or snapshots, which stay scoped by the owner tag's value. Closing it needs a deployment-scoped tag on the builder itself, which is [#56](https://github.com/junioryono/billet/issues/56).
- `builder_payload_bucket` — the bucket `billet ami build --payload-bucket` stages the shared installers in, needed once the pinned toolcache declaration outgrows EC2's 16384-byte user-data limit. Empty grants nothing on S3. The grant is scoped to the object names billet writes (`billet-payload-*`, at the bucket root, because the stager refuses a key containing a slash), so anything else in that bucket is out of reach. Requires `builder`.
