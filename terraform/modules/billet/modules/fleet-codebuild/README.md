# fleet-codebuild

The AWS side of a billet CodeBuild fleet: the project billet starts builds in, an optional reserved-capacity fleet, the two IAM roles, the log group, the Parameter Store path the single-use runner registration is staged under, and an optional KMS key.

Consume it when you already have a billet control plane and want AWS-managed compute — Linux on demand, or Apple silicon on a reserved macOS fleet — behind an existing `runs-on` label. The design record is [ADR-007](../../../../../docs/reference/decisions/adr-007-codebuild-provider.md); the operator's guide is [docs/deploying/aws-codebuild.md](../../../../../docs/deploying/aws-codebuild.md).

## Read this before enabling a tier

**Every job on this fleet inherits two ceilings billet cannot lift.** A build is capped at 36 hours, and a build still waiting for capacity is **failed** after at most 8. Both are CodeBuild's. That is why `node.codebuild.accept_external_build_ceiling` has no default and billet refuses a node that has not set it: the alternative is meeting the first one as a build that died at hour 36, and the second as an unexplained red build on a busy fleet. Work that can exceed either belongs on owned EC2 or Mac capacity.

**Untrusted work is refused on this backend, and no variable here changes that.** AWS documents a reserved-capacity instance as staying alive between builds, shareable across projects, and as making cached data — source files, Docker layers, buildspec cache directories — reachable by other projects in the same account, by design. A fork's pull request there is arbitrary code a later build inherits, and no security group fixes that. macOS is reserved-capacity only. Run untrusted tiers on `firecracker` or `ec2`.

**The default concurrency quota is 1.** CodeBuild's concurrently-running-builds quota is per environment type and compute type, and for most of them the account default is one; a Mac ARM fleet defaults to one concurrently running instance; an account gets ten fleets per region. Raise the quota in Service Quotas *before* a tier advertises capacity, or the second build queues — and a queued build fails. **The queue itself holds at most 30 builds for the whole account** (measured: `AccountLimitExceededException: Cannot have more than 30 builds in queue for the account`, and not a quota Service Quotas lists), so size the node's `max_vcpu`/`max_memory` so billet can never escrow more builds than concurrency plus thirty.

**The project must be billet's alone.** A CodeBuild build cannot be tagged: tags exist on projects and report groups, and `StartBuild` has no field that becomes one. So the project ARN is the entire IAM boundary between billet's builds and anybody else's, and billet's own inventory feeds a loop that **stops** builds. Adopting a project you also use for something else is how billet comes to stop somebody else's work.

**This module never creates a webhook.** CodeBuild's own GitHub Actions runner integration is webhook-driven, and a `WORKFLOW_JOB_QUEUED` webhook on a billet-owned project means two schedulers acquiring one job — which produces duplicate runners and reads as GitHub misbehaving. `billet check --provider codebuild` treats one as a fatal finding.

## What it creates, and what it adopts

| Resource | Created when | Adopted when |
|---|---|---|
| `aws_codebuild_project.this` | `project_name` unset | `project_name` set |
| `aws_codebuild_fleet.this` | `enable_fleet` and `fleet_arn` unset | `fleet_arn` set |
| `aws_cloudwatch_log_group.this` | `log_group_name` unset | `log_group_name` set |
| `aws_kms_key.registrations` + alias | `enable_kms` and `kms_key_arn` unset | `kms_key_arn` set |
| `aws_iam_role.node` + policy + instance profile | always | — |
| `aws_iam_role.build` | `project_name` unset | `existing_build_role_name` set (the adopted project's own role) |
| `aws_iam_role_policy.build` | always, on whichever of those roles the build runs as | — |
| `aws_iam_role.fleet` + policy | a created fleet with a VPC (`create_fleet_network_role`) | — |
| `terraform_data.active_build_guard` | always — the destroy-time guard, see [Tearing down](#tearing-down) | — |

The VPC, subnets and security groups are always yours: pass them in or leave the builds on CodeBuild's own network.

## Two roles, and why the split is the point

**The node role** is what billet assumes. It starts and stops builds in one project, reads them back, and stages and removes the single-use runner registration. It launches no EC2 instances, creates no fleet, and can neither create nor delete a project — Terraform owns the infrastructure and billet owns the jobs.

**The build role** is what CodeBuild runs the build *as*, which means every permission it holds is a permission the workflow holds. It reads the one parameter carrying its own registration and writes its own logs. It may not start a build — a role that could, running inside a build, is how a job launches runners billet never escrowed capacity for — and it may not delete a parameter, because that would let a job destroy another build's registration.

Both policies are billet's **own** generator's output: the module commits renderings of `internal/awspolicy` (kept equal by `internal/tfpolicy`'s drift test) and substitutes your account, region, project, fleet, parameter path, key and log group for the sentinels. What billet's code performs and what its policy grants cannot disagree if only one place decides. Pass `billet init iam` output as `var.iam_policy_json` to override the node's policy entirely.

**And a third grant, on the control plane.** A node that dies between staging a build's registration and removing it leaks exactly one parameter, and nothing on a node can ever authorise cleaning it up: from CodeBuild alone, "no build for this lease" and "the build has not appeared yet" are the same observation. The control plane holds the one thing that can, its ledger — the lease is terminal, and has been for longer than any build could still be running — so it lists the names under `parameter_path` and deletes the ones the ledger has proved dead. That needs `ssm:GetParametersByPath` and `ssm:DeleteParameter` on this path, on the role `billet server` runs under, and nothing else: no `GetParameter`, no `PutParameter`, no key, no CodeBuild action. Set `controller_role_name` and the module attaches it (in the root module's co-located topology that is fleet-ec2's node role); `controller_sweep_policy_json` carries the same document for a controller role terraform does not own, and `billet init iam --controller-sweep --account <id>` prints it from the node's config. `billet status` reports what the sweep has removed, per path.

## Where the runner registration goes

Into an SSM Parameter Store SecureString, one per build, under `parameter_path`, handed to the build as a `PARAMETER_STORE`-typed environment variable. So the registration never appears in a command line, a buildspec, a log, the CodeBuild console, build metadata, or CloudTrail's request rendering — only the parameter's *name* does.

`parameter_path` must be a literal, and the module refuses a wildcard: it lands in an IAM Resource ARN, and on a shared account the sibling paths a `*` admits are other deployments' runner registrations.

`enable_kms` is worth setting on a shared account. Without a customer-managed key, a SecureString is encrypted under the account's `aws/ssm` key, which authorizes any principal in the account that can reach Parameter Store — so a neighbouring deployment's role could read this one's staged registrations. With one, both roles' KMS grants are scoped to exactly that key **and** to Parameter Store by `kms:ViaService`.

## macOS

`environment_type = "MAC_ARM"` with `enable_fleet = true`. On-demand CodeBuild does not offer macOS at all, so the module refuses the pairing at **plan** time rather than letting AWS refuse it per job.

| Compute | Apple silicon | Regions |
|---|---|---|
| `BUILD_GENERAL1_MEDIUM` | M2, 24 GB, 8 vCPU | us-east-1, us-east-2, us-west-2, ap-southeast-2, eu-central-1 |
| `BUILD_GENERAL1_LARGE` | M2, 32 GB, 12 vCPU | us-east-1, us-east-2, us-west-2, ap-southeast-2 |

**A reserved fleet is a standing cost, not a per-job one.** It carries an initial per-instance charge and bills for as long as it is provisioned, whether or not anything is building. Check the [pricing page](https://aws.amazon.com/codebuild/pricing/) before creating one, and delete a fleet you are not using.

**`ACTIVE` does not mean a Mac exists yet.** A fresh fleet reports `ACTIVE` within seconds and may then carry `status.context: INSUFFICIENT_CAPACITY` for an hour or more while AWS looks for a `mac2-m2.metal` (measured 2026-09-02); every build dispatched to it queues until the queued timeout fails it, and GitHub gives up on the job after three requeues. Run `billet check --provider codebuild` after the apply — it warns on a fleet with a status context — and warm the fleet with one build before enabling a tier on it.

The module's `fleet_capacity` output is the number a macOS tier's `nodes[].macos_vm_limit` should be set to. billet refuses to assume Apple's per-host allowance of two applies to a fleet AWS operates under its own agreement, so it asks for the capacity — and this is the answer.

## Configuration handoff

The module writes no billet configuration. It **outputs** the non-secret facts the `junioryono.billet.host` Ansible role needs for `billet_config`:

| Output | billet.yaml key |
|---|---|
| `project_name` | `node.codebuild.project` |
| `fleet_arn` | `node.codebuild.fleet_arn` |
| `environment_type` | `node.codebuild.environment_type` |
| `parameter_path` | `node.codebuild.jit_parameter_path` |
| `kms_key_arn` | `node.codebuild.jit_kms_key_id` |
| `log_group_name` | `node.codebuild.log_group` |
| `node_instance_profile` | the orchestrator instance's profile — how billet reads AWS credentials from IMDS |

**A VPC configuration requires a fleet this module creates (`enable_fleet` with no `fleet_arn`), and both together are refused.** A fleet's network lives on the fleet; the module cannot edit one it does not own, and billet sends `fleetOverride` on every launch — which makes CodeBuild ignore the project's VPC configuration entirely. So passing `vpc_id`/`subnet_ids`/`security_group_ids` beside an adopted fleet used to apply cleanly and configure nothing, leaving builds on whatever network that fleet already had. Configure the network on the adopted fleet itself. A partial set is refused for the same reason, and a reserved fleet takes exactly one subnet.

**`node.codebuild.compute_types` is deliberately not an output.** billet ships no table of compute types, so a shape it may buy is declared along with what it holds — which keeps the fleet's cost surface in your own file, where a spending decision belongs. Declare the compute type this module was given, with its vCPU, memory and your audited hourly rate. Do not declare a shape you are unwilling to buy.

## Usage

```hcl
provider "aws" {
  region = "us-west-2"
}

# Linux on demand: the inexpensive default.
module "linux" {
  source = "github.com/junioryono/billet//terraform/modules/billet/modules/fleet-codebuild?ref=v0.9.0"
  name   = "billet-linux"
}

# macOS on reserved capacity: an explicit opt-in, and a standing cost.
module "macos" {
  source           = "github.com/junioryono/billet//terraform/modules/billet/modules/fleet-codebuild?ref=v0.9.0"
  name             = "billet-macos"
  environment_type = "MAC_ARM"
  compute_type     = "BUILD_GENERAL1_MEDIUM"
  enable_fleet     = true
  fleet_capacity   = 1
}
```

Then, on the orchestrator machine:

```bash
billet check --provider codebuild   # ceilings, inventory window, project and fleet
billet node
billet status                       # confirm what the node advertises
```

## Tearing down

**`terraform destroy` does not stop a running build, and must not be made to.** Terraform owns AWS infrastructure; billet owns jobs, and GitHub does not requeue a job whose runner vanished mid-execution. Drain first:

```bash
billet drain --wait      # stops admission and waits for running work, with no deadline
terraform destroy
```

**AWS will not refuse the destroy for you, so the module does.** An earlier version of this section claimed AWS refuses to delete a project with builds in progress. Measured 2026-09-02, it does not: `DeleteProject` succeeds while a build is in its `BUILD` phase, the build runs on to completion, and `BatchGetBuilds` keeps answering for it. What a destroy removes is the node role, the build role and the log group that build depends on — under a live job, reported as a clean apply.

So `terraform_data.active_build_guard` depends on all of those and runs `scripts/refuse-active-builds.sh` as a destroy-time provisioner. Terraform tears dependents down first, so the guard runs before anything a build needs is touched, and it refuses — aborting the plan with every resource intact — while the project has any build that is not terminal. It walks `ListBuildsForProject` newest first with the `aws` CLI on your PATH, using your credentials (which must be able to list and describe builds in the project), stopping once every build on a page started before CodeBuild's own ceilings could still have it running. A CLI error, an id CodeBuild does not know, a status the script has never heard of, or a listing that never reaches that edge all refuse: "could not tell" is never "nothing running".

The escape is an environment variable, and that is deliberate:

```bash
BILLET_SKIP_ACTIVE_BUILD_GUARD=1 terraform destroy   # asserts you drained; checks nothing
```

A destroy-time provisioner sees the values in state, not the ones on the command line, so a `-var` passed to `terraform destroy` would never reach it and a module variable would need its own apply first. The guard's window is CodeBuild's service maximum rather than your `build_timeout_minutes` because it is sound for any project, an adopted one included — a few extra pages on a rare operation. The guard is also replaced (which runs the provisioner, against the old project) when the project name, the region, `name`, `parameter_path` or `log_group_name` changes, or when a project or log group flips between created and adopted under the same name, so an apply that renames or hands over what builds depend on **fails at the guard** while a build is running.

**The guard is a snapshot, so the drain is not optional.** It walks the project's builds once; a control plane still admitting work can escrow a job and start a build between that walk and the destroy, and nothing a provisioner can ask AWS proves that admission is sealed. `billet drain --wait` seals it. The guard is what catches a destroy attempted without the drain, and its window covers whatever was already started.

**Removing the module block skips the guard, and that is Terraform's rule rather than this module's.** A destroy-time provisioner runs only while its configuration is still present; delete `module "linux" { … }` from your root and apply, and Terraform destroys everything it recorded with no provisioner left to run. To remove a fleet, drain it, run `terraform destroy -target=module.<name>` (the guard runs there), and only then delete the block. That is caught, not prevented: Terraform orders the old guard's destruction before the destruction or replacement of the resources it depends on, and promises nothing about in-place updates on the same apply, so a changed policy document may already have landed when the guard refuses. The guard is a gate on `destroy` and on replacement; reconfiguring what a live build depends on is what `billet drain --wait` is for. An apply that changes a timeout, a tag or an image never touches the guard.
