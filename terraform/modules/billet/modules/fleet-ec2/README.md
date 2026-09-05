# fleet-ec2

Everything billet's EC2 **compute** side needs and nothing the control plane does: the node IAM role and instance profile carrying billet's own generated policy, the trusted-runner security group, the cache storage, and the spot interruption queues with their tag-scoped router. Consume it directly when you already have a control plane; the opinionated root composes it with `control-plane-ec2-sqlite` and attaches this child's instance profile to the controller for the co-located deployment.

The IAM policy is billet's **own** generator's output (`internal/awspolicy`, kept equal by the `internal/tfpolicy` drift test), feature-gated: compute-only, cache, cache+KMS renderings, plus the spot grant whose actions come from the committed `policy/spot-actions.json` — the same file the plan tests read, so the pin has no second copy to drift.

## Usage

```hcl
module "fleet" {
  source = "github.com/junioryono/billet//terraform/modules/billet/modules/fleet-ec2?ref=v0.8.0"

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
| `aws_sqs_queue.interruptions` | `enable_spot` | The primary queue; its basename is the `spot_node_name` a spot node uses as `node.name` |
| `aws_sqs_queue.spot_nodes` | `spot_node_names` | One further queue per entry, named exactly it, with the primary's attributes |
| `aws_iam_role_policy.spot` | `enable_spot` | Scoped to every created queue, actions from `policy/spot-actions.json` |
| `aws_iam_role.spot_router` + policy, `aws_lambda_function.spot_router`, `aws_cloudwatch_log_group.spot_router`, `aws_cloudwatch_event_rule.spot_interruption` + target, `aws_lambda_permission.spot_router` | `enable_spot` | The tag-scoped router; least-privilege IAM. Granted, and told the names of, every created queue from one list, which is what lets it drop a foreign warning without dropping one its own grant merely failed to reach |
| `aws_cloudwatch_metric_alarm.spot_router_errors` | `enable_spot` | One `Errors` datapoint in one minute; no action until `spot_router_alarm_actions` names one |
| `data.archive_file.spot_router` | `enable_spot` | Zips the committed Lambda source under the root's `.terraform` |

Adopted (never created): the VPC (`vpc_id` is a required input).

## Inputs of note

- `vpc_id` (required) — this child never creates a network.
- `iam_policy_json` — `billet init iam` output replaces the rendering entirely.
- `job_instance_profile_role_arn` — the exact role trusted JOB instances receive; grants `iam:PassRole` on it alone.
- `builder` — grant the node role what `billet ami build` performs: `ec2:CreateImage` on a builder-tagged instance and on the image and snapshots it makes, its own `TerminateInstances` for cleanup, `GetConsoleOutput` to read the verifier's report off the console, and the `CreateTags` that stamps a verified image's contract. **Off by default**, because it widens the identity every job's instance is launched by, and a deployment that builds its image elsewhere should not carry it. On, the build runs on the controller with its instance role instead of from a workstation holding an operator's own credentials.

  It is **additive**: the builder's launches ride the node policy's own `RunInstances`, which admits them because this module's rendering is account-wide and `billet ami build` tags its instances with the same owner key. That is why it cannot be combined with an override, which is stated below.

  **The builder's statements are scoped to this deployment's builders**, in the same two modes the rest of the policy uses. `billet ami build` stamps `billet-ami-build-<deployment>-<image name>` when it knows the deployment (from `--deployment`, or from the identity in the config's state directory) and `billet-ami-build-<image name>` when it does not, and the statements match whichever of those the policy was generated for. So a value-scoped grant reaches this deployment's builds and no other's, and an account-wide one reaches every builder in the account — which is what that mode means everywhere else too.


  **`builder = true` is refused beside `iam_policy_json`.** This module's builder rendering is account-wide, because the module has no deployment id at apply time, and IAM unions allows — so attaching it beside a value-scoped override would hand the role account-wide reach over every deployment's builders and undo the isolation that override was chosen for. Generate the override with `billet init iam --deployment <id> --builder --payload-bucket <bucket>` instead, so one document carries the node grant, the builder grant and the payload statement, and leave `builder = false`. The payload bucket is not optional: `billet ami build` requires `--payload-bucket`, so an override without that statement fails at the staging step.

  Two details are worth knowing before turning this on in a shared account. `BilletAMIBuilderImage` carries no condition at all, because the image and its snapshots do not exist yet and cannot answer a resource-tag condition; the call is authorized only when the source-instance statement admits it too, so that statement is the gate. And a build run with **no** deployment identity available stamps the account-wide value, which a value-scoped policy will not admit — `billet ami build` says so on stderr rather than letting it fail at `RunInstances` with nothing naming the reason.
- `builder_payload_bucket` — the bucket `billet ami build --payload-bucket` stages the shared installers in, needed once the pinned toolcache declaration outgrows EC2's 16384-byte user-data limit. Empty grants nothing on S3. The grant is scoped to the object names billet writes (`billet-payload-*`, at the bucket root, because the stager refuses a key containing a slash), so anything else in that bucket is out of reach. Requires `builder`.
- `spot_node_names` — further spot nodes beside the one `enable_spot` creates. billet needs one interruption queue per spot node with the queue's basename equal to that node's `node.name`, so each entry creates a queue named exactly it (no prefix added; a queue name is account-wide per region in SQS), and the node role's consumer grant, the router's forwarding grant and the set of names the router is told it serves widen to every queue from this one list. That is deliberately one input rather than three: a queue granted by hand but never named to the router had its warnings dropped, not retried, while the grant propagated. `interruption_queue_urls` reports every node's queue keyed by its name. Names follow the intersection of billet's node-name rule and SQS's queue-name rule, so a dot is refused at plan rather than at apply; a duplicate and the primary queue's own name are refused too, and so is a seventeenth entry, because every queue's ARN is repeated in two inline policies and IAM caps a role's inline policies at 10,240 characters combined (more spot nodes than that are several `fleet-ec2` instances). That bound is this input's share of the quota, not the role's total: an `iam_policy_json` override is installed verbatim, and one already near the quota fails the primary spot grant with no further names at all. Requires `enable_spot`.
- `spot_router_alarm_actions` — where the router's error alarm sends. This module creates no SNS topic and will not invent one, so the default is an alarm with no action: still visible in the console and to `DescribeAlarms`, but nothing is pushed. A warning the router could not place is a failed invocation and a Lambda retry rather than a silent drop, and this alarm is the only thing that says so.
