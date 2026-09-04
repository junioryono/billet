# AWS with EC2

One EC2 instance per job, launched by an `ec2` node that runs no jobs itself. The instance is the isolation boundary, so this backend may run fork pull requests once it has a network of its own. There is no provider-imposed job ceiling.

## What Terraform creates

The root module `terraform/modules/billet` composes an adopt-or-create network with two children:

| Module | Creates |
|---|---|
| `modules/control-plane-ec2-sqlite` | the controller instance, its security group, the EC2 auto-recovery alarm, and a retained encrypted gp3 ledger volume with `prevent_destroy` |
| `modules/fleet-ec2` | the node IAM role and instance profile rendered from billet's own policy generator, the trusted-runner security group, the cache bucket with an optional per-deployment KMS key, and the spot queue with the EventBridge-to-Lambda router |

Outputs are the non-secret facts your `billet.yaml` needs: `control_plane_private_ip` (declare it with the input of the same name, so an instance replacement cannot change the address every node dials), `subnet_id`, `availability_zone`, `runner_security_group_id`, `node_wire_address`, `bootstrap_wire_address`, `cache_bucket`, `cache_prefix`, `cache_kms_key_arn`, `interruption_queue_url`, `ledger_volume_id`, `spot_node_name`, and with `create_backup_bucket` the `backup_bucket` and `backup_prefix` that `backup.s3` names (the grant lands on the node role the co-located controller runs with). Examples live under `terraform/modules/billet/examples`.

```hcl
module "billet" {
  source = "github.com/junioryono/billet//terraform/modules/billet?ref=main"
  # see the module README for inputs
}
```

Pin the module to a release in your own configuration: the `?ref=` names the version, and it is the same version as the binary and the collection beside it. Documentation on `main` says `?ref=main` because it describes `main`; cutting a release rewrites every documented source to that release's tag, so the copy of this page inside a release names the version you are installing.

The module's IAM is exactly what `billet init iam` prints for the same config, kept equal by a drift test, so the module can never grant a permission billet would not ask for. `billet init iam --builder` adds what `billet ami build` needs (with `--payload-bucket` for the staged installers); `--controller-sweep` adds the CodeBuild registration sweep. The module's `builder` input attaches that same generated document as its own inline policy, so the build can run on the controller rather than from a workstation's credentials.

The node role may launch instances but may not launch one from a snapshot this deployment does not own, which is an explicit deny rather than a narrower grant: `ec2:RunInstances` authorises every snapshot a block-device mapping names, and the account a fleet runs in is usually also where the control plane's ledger snapshots live. billet's own launches name no snapshot, so nothing it does is refused by it. In account-wide mode the deny asks only that the snapshot carry billet's owner tag, so another billet deployment's snapshot in the same account is still reachable; a per-deployment KMS key is what closes that, the same as for the cache.

## The node

An `ec2` node is an orchestrator: it calls an API and the compute appears in a region. So `node.max_vcpu` and `node.max_memory` are required rather than detected, and they are hard resource budgets rather than price budgets. The node registers its ordered `instance_types`, each with `vcpu`, `memory` and an operator-audited `price_usd_per_hour`; placement charges the first shape that fits, and a shape AWS has none of (`InsufficientInstanceCapacity`) falls through to the next one you declared, after a synchronous refusal that launched nothing.

```yaml
node:
  provider: ec2
  site: us-west-2
  max_vcpu: 64
  max_memory: 256GiB
  ec2:
    region: us-west-2
    subnet_id: subnet-…
    security_group_ids: [sg-trusted]
    untrusted_security_group_ids: [sg-untrusted]   # absent: fork PRs refused
    instance_types:
      - {type: c7i.2xlarge, vcpu: 8, memory: 16GiB, price_usd_per_hour: 0.357}
      - {type: c7i.4xlarge, vcpu: 16, memory: 32GiB, price_usd_per_hour: 0.714}
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: my-billet-cache
```

`billet init --provider ec2` writes this block from flags. One `ec2` node is a serial launch queue (a node executes one command at a time), which is invisible when a node is one machine's worth of jobs and visible when it stands for sixty; run several `ec2` nodes, each registered with its own budget, if that binds.

## The image

A tier's `image:` (or `launch.ec2.image`) is an AMI id, and an AMI id is region-scoped.

```bash
billet ami build --config billet.yaml --region us-west-2 --subnet subnet-… --security-group sg-…
```

`ami build` reads the same pinned declaration as the Firecracker guest image and runs the same installers, so a workflow finds the declared apt set, the toolcache and the JDKs on either backend, on x64 and arm64 alike (`--arch arm64`). It then **boots the image it made** and asserts on the artifact rather than on the builder: free space, the Docker storage driver on a fresh boot, and that the runner and every declared toolcache line execute as the runner account under the job's own environment. Only then does it stamp the contract tag. An image billet has not booted carries no tag, and `billet check` reports it as needing a rebuild; `billet ami verify <ami>` stamps an existing image. Measured: provisioning under four minutes, snapshot to available eleven, a complete console report under five minutes after the verifier launched.

## Spot

Off by default. With `spot: true` the node needs `interruption_queue_url`, one queue per node whose basename equals the node's name: the router resolves the warned instance's `sh.billet.node` tag and targets only that node's queue, because a shared queue hides a message for the visibility timeout, which can consume the whole two-minute warning. A warning records the reclaim reason durably and starts teardown without waiting for lease expiry. GitHub does not requeue a job that already started, so an interruption fails the build; use Spot for explicitly retryable work. The router throws a warning away only on one of four proofs that it is not the router's to place: the instance is present and carries no `sh.billet.node` tag, EC2 says the instance is already gone, the tag cannot be a legal queue name, or the tag names a queue other than the one the router was told it serves *and* SQS answers `AccessDenied` or `AWS.SimpleQueueService.NonExistentQueue` for it (a queue the router is granted but was not told about forwards normally). Anything else it cannot complete — including `AccessDenied` for its own queue, which is equally what AWS answers while a grant is still propagating — is a failed invocation and a retry, reported by the module's `spot_router_errors` alarm. Point that alarm somewhere with the module's `spot_router_alarm_actions`, because a lost warning is otherwise a two-minute reclaim nobody hears about.

## What has run

Real private-repository jobs have launched on EC2, registered through JIT configuration, built Docker images, used a service container, installed Go at runtime, completed green and left no instance behind; the same unchanged workflow has completed first on preferred local capacity and then on EC2 after that capacity was withdrawn; a live FIS interruption exercised the spot path. Three cold launches reached the first job step in 47.6 to 58.7 seconds, which is acceptable for fallback and is why `warm_pool` stays refused ([AWS acceptance](../reference/records/aws-acceptance.md)).

## Cost

Declared prices report exposure; they do not gate admission. `billet check` reports one node's conservative peak; `billet status` reports the deployment-wide peak across registered cloud nodes under the shared ceiling, disconnected registrations included because their instances may still be billable. An AWS budget is the account-wide backstop. `billet decommission --yes` removes the instances and the EBS+S3 cache billet made outside Terraform; run it before `terraform destroy`.

## Everything in AWS

For an AWS-only deployment the controller is the same small instance, the node runs beside it, and there is no local host. [Hybrid](hybrid-owned-hardware.md) adds the owned hardware in front; [AWS with CodeBuild](aws-codebuild.md) is the managed alternative, including macOS.
