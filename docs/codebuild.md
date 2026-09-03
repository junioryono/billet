# The CodeBuild backend

AWS-managed compute for a billet tier: Linux on demand, and Apple silicon on a reserved-capacity macOS fleet. It exists so one `runs-on` label can prefer a machine you own and fall back to AWS without anybody editing a workflow, and so a macOS label does not require you to allocate an EC2 Mac Dedicated Host, operate a macOS host, install Tart or build an 81 GB guest image.

The design record is [ADR-007](adr-007-codebuild-provider.md). This page is what you need in order to run it.

## What it cannot promise, up front

**Every CodeBuild-backed job has a hard 36-hour maximum.** That is CodeBuild's own build timeout ceiling and billet cannot lift it. A job that would exceed it belongs on owned EC2 or Mac capacity, where billet imposes no job limit at all. CodeBuild does **not** satisfy billet's unlimited-duration guarantee.

**There is a second ceiling: a queued build fails after at most 8 hours.** On a fleet at capacity with queue overflow, a job that waits longer than `queued_timeout_minutes` is failed by CodeBuild, not by billet.

Because both are outside billet's control, `node.codebuild.accept_external_build_ceiling` has **no default**. A config that does not set it is refused. That is deliberate: the alternative is finding out about the ceiling from a build that died at hour 36.

**Untrusted work is refused on reserved capacity, and admitted on on-demand Linux under a narrower door.** AWS documents reserved-capacity fleet instances as remaining alive between builds, shareable across projects, and as making cached data (source files, Docker layers, buildspec cache directories) reachable by other projects in the same account *by design*. A fork's pull request on such a fleet is arbitrary code on a machine that later runs somebody else's build, and no security group fixes that. So untrusted work is refused whenever `fleet_arn` is set, and macOS (reserved-only) can never run it.

On-demand Linux is different, and it was measured rather than assumed (`docs/aws-acceptance.md`, 2026-09-02): 31 on-demand builds ran on 31 distinct freshly-booted hosts with nothing surviving between them, so an on-demand build is a per-job machine the way an `ec2` instance is. Untrusted work is therefore admitted on an on-demand container tier **only when** `node.codebuild.untrusted_vpc_id`, `untrusted_subnets` and `untrusted_security_group_ids` name an isolated network — their absence is the refusal, the same rule as `node.ec2.untrusted_security_group_ids`. The network lives on the project (StartBuild has no VPC override), so billet verifies the project carries exactly that network before it starts a fork's build.

**Two things make this weaker than the `ec2` untrusted path, and both are worth understanding before you enable it.** A VPC-connected on-demand build's service role is reachable from inside the build, and a VPC build's service role must hold `ec2:CreateNetworkInterface`/`DeleteNetworkInterface` (scoped to the untrusted subnet and group) plus unscopable `ec2:Describe*` — so a fork gets a scoped-but-real EC2 network credential, where an `ec2` untrusted instance gets none. And that same role reads the JIT parameter path. So **untrusted CodeBuild work must run on its own dedicated node and project**, never one that also serves trusted tiers, or a fork could reach a trusted job's staged registration. Billet gates and verifies the network; it does not yet enforce that a trusted tier and an untrusted tier never share a CodeBuild node, so keep them on separate nodes yourself.

**Billet does not use CodeBuild's own GitHub Actions runner integration.** That feature is webhook-only and would take over job detection, runner registration and scheduling. Billet starts an ordinary project and runs GitHub's runner itself, so your workflows keep their existing labels and nothing in a repository names an AWS project. Do **not** add a `WORKFLOW_JOB_QUEUED` webhook to a billet-owned project: two schedulers acquiring one job produces duplicate runners.

## Shape of a deployment

A codebuild node is an **orchestrator**, exactly like an ec2 node. It runs the billet binary on some small machine, holds AWS credentials, and calls an API; the compute appears in a region. So:

- `node.max_vcpu` and `node.max_memory` are **required** and are hard resource budgets, not price budgets. Nothing is detected from the machine running the node process.
- `node.codebuild.compute_types` is an **ordered** list, each entry declaring what that compute type holds and what it costs. Placement charges the first entry that fits the tier and authorises every fallback against both the node and the deployment ceiling before the request reaches AWS. Do not declare a compute type you are unwilling to buy.
- **One billet node per project-and-fleet.** A reserved fleet's capacity is shared, so two nodes advertising the same fleet would each promise the whole of it. The control plane refuses a registration naming a fleet another live node already holds.
- `node.ceph` and `node.ebs_s3` are refused. A build cannot attach a block device, and its compute runs in a region that cannot reach your cluster — a storage block that looks configured and is inert reads as a working cache right up to the first job that expected one.

The order of a tier's `providers:` list is what makes home fill first:

```yaml
tiers:
  - label: linux-x64
    trust: trusted
    runner_group: platform
    workflows: ["ci.yml"]
    providers: [firecracker, codebuild]
    guest_os: linux
    vcpu: 4
    memory: 8GiB
    launch:
      firecracker:
        image: ubuntu-2404-x64@verified
      codebuild:
        image: aws/codebuild/amazonlinux-x86_64-standard:5.0
```

## macOS

macOS is `MAC_ARM` and reserved capacity only. Two machine types exist: Apple M2 with 24 GB and 8 vCPU, and Apple M2 with 32 GB and 12 vCPU.

| Compute | Regions |
|---|---|
| macOS Medium (M2, 24 GB, 8 vCPU) | us-east-1, us-east-2, us-west-2, ap-southeast-2, eu-central-1 |
| macOS Large (M2, 32 GB, 12 vCPU) | us-east-1, us-east-2, us-west-2, ap-southeast-2 |

**A reserved fleet is a standing cost, not a per-job one.** It carries an initial per-instance charge and bills for as long as it is provisioned, whether or not anything is building. Check the [CodeBuild pricing page](https://aws.amazon.com/codebuild/pricing/) before creating one, and delete a fleet you are not using.

**A fleet that is `ACTIVE` may have no Mac behind it yet, and a job dispatched to it then fails for a reason that is not billet's.** Measured 2026-09-02: a fresh `MAC_ARM` fleet reported `ACTIVE` nineteen seconds after `CreateFleet`, and then AWS reported `status.context: INSUFFICIENT_CAPACITY` ("We currently do not have sufficient capacity for the instance type you requested") for the best part of an hour. Every build dispatched to it sat `QUEUED`; GitHub cancelled the first assignment as unacquired after about five minutes and requeued the job, billet correctly stopped the build for the cancelled job and started one for the requeued job, and the cycle repeated until the queued ceiling failed the build. `billet check --provider codebuild` warns when a fleet carries a status context, so run it before enabling a macOS tier — and if it reports the fleet starved, the tier's jobs will fail until AWS finds capacity, whatever billet does. Warm a new fleet with one build before pointing a tier at it.

Apple's two-guests-per-host licence limit does **not** apply here, and billet knows that: the per-host macOS cap defaults to Apple's allowance only for a backend that runs work on the host itself. On a codebuild node the cap is the fleet capacity you declared. Set `nodes[].macos_vm_limit` if you want a tighter one.

## Concurrency quotas will bite first

**The default is 1.** CodeBuild's concurrently-running-builds quota is per environment type and compute type, and for most of them the default account limit is one; a Mac ARM fleet defaults to one concurrently running instance; an account gets ten fleets per region. So a fresh account cannot run two concurrent builds of one shape, however much capacity billet has escrowed — the second build queues, and queued builds fail after `queued_timeout_minutes`.

**A macOS fleet has no overflow, so `macos_vm_limit` must not exceed its capacity.** Measured 2026-09-02: `CreateFleet` with `overflow_behavior: ON_DEMAND` on `MAC_ARM` is refused (`Fleet on-demand overflow behavior is not supported for MAC_ARM`), so every build past the fleet's capacity queues for a Mac inside it. A tier declaring one more than the fleet holds did exactly what it declared — billet escrowed two jobs and started two builds — and the second sat `QUEUED` behind the busy Mac until GitHub withdrew its assignment at about five minutes, requeued it, and billet stopped and restarted the build; it finished 94 seconds after the first job ended, on the same warm Mac. `billet check --provider codebuild` now refuses a `nodes[].macos_vm_limit` above the fleet's base capacity, naming both numbers; set it to the number the fleet reports.

**Concurrency past one managed Mac comes from a Mac you own, in the same tier.** An account allows one `mac2-m2.metal` until AWS raises it, and a macOS fleet cannot overflow — but a macOS tier may list several backends, `providers: [tart, codebuild]`, with a `nodes[]` entry for each: the tart host gets Apple's two guests by default, the codebuild node declares the fleet's capacity, and the tier's `max_concurrent` defaults to what the two permit between them. Measured 2026-09-03 in `docs/aws-acceptance.md`: three macOS jobs through one `runs-on` ran at once, two on an owned M2 Max and one on the fleet's Mac, and when the codebuild node lost its credentials the same label kept running on the owned Mac alone. That tier is the one shape a macOS tier may leave unpinned.

**And the queue itself is capped at 30 builds for the whole account.** Measured 2026-09-02: past thirty queued builds, `StartBuild` is refused with `AccountLimitExceededException: Cannot have more than 30 builds in queue for the account`, and that limit is not one Service Quotas lists. It is account-wide, so every CodeBuild project you own shares it. billet treats the refusal as a conclusive launch failure — it tries the next declared compute type, then fails the lease — and GitHub requeues the job up to three times, so a burst of more than roughly *concurrency quota + 30* jobs aimed at CodeBuild turns the overflow into failed jobs rather than slow ones. Size `node.max_vcpu` and `node.max_memory` so the fleet can never escrow more builds than the account can queue.

Raise the quota in Service Quotas before a tier advertises capacity it cannot use. `billet check --provider codebuild` reports the two ceilings, the inventory window they imply, the trust refusal and, when it can reach AWS, the project, the fleet's configured capacity **and the concurrency quota for every compute type you declared** — against how many of that shape your budget could escrow, so an over-configured tier says so before it advertises. `billet status` reports the deployment's cost exposure from the shapes every registered node declared.

**It reads the limits by asking AWS what they are called, not by shipping their codes.** CodeBuild has one concurrency limit per environment and compute type, and billet does not own those identifiers — a stale one would read as "no limit", which is the direction that costs a build. Matching AWS's own names fails the other way: a rename makes billet report a limit it *could not find*, which is something you can act on.

**The account-wide queue depth is reported from a measurement rather than a read**, because Service Quotas does not list it. It is not compared to anything: thirty builds across every project in the account is not about one node's budget.

**A node's own IAM role deliberately does not grant `servicequotas`.** billet reads a quota only in this diagnostic and never at runtime — nothing in a launch, a teardown or a sweep consults one — and a permission granted for a diagnostic is one the machine holding your GitHub App private key carries forever. So run `billet check` under credentials that have `servicequotas:GetServiceQuota` and `servicequotas:ListServiceQuotas`; on the node itself the check will say it could not read them, and name the permission.

**Everything here is advisory and nothing gates.** A quota is raised by a support request rather than a config change, and it can move without billet hearing about it — so a stale or unreadable answer must never refuse a working deployment.

## What a stop, a drain and an upgrade do

They leave active builds **running**. This is the same contract as every other backend:

- `billet drain`, a control-plane restart, a node restart, a release rollout and a `terraform apply` never call `StopBuild`. A drain has no billet-imposed deadline, because elapsed time is not evidence that a job stopped making progress.
- A restart **re-adopts**: the build keeps running, its lease stays charged, and the node's inventory recovers it by exact deployment, project and lease identity. GitHub does not requeue a job whose runner vanished mid-execution, so tearing one down would be a deliberate job failure.
- The only routes to `StopBuild` are ordinary completed-job cleanup, an authoritative GitHub cancellation or terminal outcome, and `billet force-destroy`, which refuses unless admission is already sealed by an operator.
- A build CodeBuild timed out is reported **distinctly** from an ordinary failure, so a 36-hour cap is not filed as somebody's broken test.

**`terraform destroy` refuses while a build is running, and it is billet's module that refuses, not AWS.** Measured 2026-09-02: `DeleteProject` succeeds while a build is in its `BUILD` phase, the build runs on to completion, and `BatchGetBuilds` keeps answering for it — so a destroy would remove the node role, the build role and the log group under a live job and report success. The fleet-codebuild module therefore carries a destroy-time guard that walks the project's recent builds through the AWS CLI and aborts the destroy while any is not terminal; it needs the `aws` CLI on the operator's PATH and never translates a wait into a `StopBuild`. `BILLET_SKIP_ACTIVE_BUILD_GUARD=1` is the operator asserting the fleet was drained with `billet drain --wait`. See the module's README.

## Where the runner registration goes

Into an SSM Parameter Store SecureString, one parameter per build under a per-deployment path, handed to the build as a `PARAMETER_STORE`-typed environment variable. So the registration never appears in a command line, a buildspec, a log, the CodeBuild console, build metadata, or CloudTrail's request rendering — only the parameter's *name* does. The parameter is deleted once consumption is established.

Two IAM principals are involved and they are deliberately different:

- **The node role** — what billet uses. It may start, stop, describe and list builds on its own project, and write and delete parameters under its own path. It launches nothing else and can read no other deployment's path.
- **The build service role** — what the build itself runs as. It may read the parameter and write logs. It may not start builds.

Both are generated by `billet init iam` and by the Terraform module, from the same generator, so what billet's code does and what its policy grants cannot disagree.

`billet init iam` needs two things the config does not carry, because both land in an IAM `Resource` and a wildcard there is how a scoped-looking grant turns out not to be:

```bash
billet init iam --account $(aws sts get-caller-identity --query Account --output text)
billet init iam --account … --build-role     # the project's service role
```

Add `--kms-key-arn` when `node.codebuild.jit_kms_key_id` is a bare key id or an alias rather than a full ARN — an alias resolves to whichever key it points at today, so billet refuses to assemble an ARN from one:

```bash
billet init iam --account … \
  --kms-key-arn $(aws kms describe-key --key-id alias/billet-jit --query KeyMetadata.Arn --output text)
```

**The refusal you get without it prints the `describe-key` command and then tells you to add the flag to the invocation you ran, rather than printing a whole `billet init iam` line.** That is deliberate: billet cannot see your argv, so a reconstructed command has to guess at `--build-role` and `--config` — and guessing wrong on `--build-role` prints the *node's* policy, which carries `StartBuild`, `StopBuild` and `DeleteParameter`. Handing those to the role a workflow runs as is the one mistake this two-role split exists to prevent.

**The log group billet writes to is the one its policy names, on every launch.** If `node.codebuild.log_group` is set, that is the group; if it is not, billet pins CodeBuild's own default for the project (`/aws/codebuild/<project>`). It sends that as an override every time rather than leaving the project's own configuration to decide, because the build role's grant is scoped to a group ARN — so an adopted project carrying a custom group would otherwise get a policy for one group and write to another, and the role could not write its own logs. The grant is never `*`: that role runs the workflow, so `logs:CreateLogGroup` on every group would be a capability arbitrary job code holds.

**One thing the build role cannot be narrowed to, and it is worth knowing before you enable this backend.** CodeBuild resolves a `PARAMETER_STORE` environment variable using the *project's* service role, before the build exists — so there is no per-build identity to scope to, and the role must be able to read any registration the project's builds may reference. A job in this deployment that learns another concurrent job's parameter name could read that registration and register a runner as that lease. It is bounded by keeping trust classes on separate nodes: on a trusted node every job is one you trust, and those jobs already share a reserved fleet's cache by AWS's own design; on an untrusted on-demand node every job is an equally-untrusted fork under a single-use, unguessable lease name, reading a path that holds only other forks' registrations. What must not happen is a trusted tier and an untrusted tier sharing one CodeBuild node (and therefore one project, role and parameter path), which would let a fork reach a trusted job's registration — run them on separate nodes. Closing it entirely needs a per-build credential — a per-build role, or an intermediary that hands back only the caller's own parameter — which is not in this release.

## Staged registrations a dead node left behind

A registration is deleted by the node once its build is proved over. A node that dies between staging one and getting there leaks exactly one parameter, and nothing on the node can ever clean it up safely: from CodeBuild alone, "no build for this lease" and "the build has not appeared yet" look the same, and deleting on the second strands a live build that then registers nothing. So the **control plane** sweeps, because it holds the one thing that can authorise the delete: its ledger says the lease is terminal, and has been for longer than any build could still be running (CodeBuild's own 36-hour build ceiling plus its 8-hour queued ceiling, plus an hour). The parameter's own write time, by AWS's clock, must be older than that window too, so a billet clock that is wrong by hours cannot release a registration a queued build is still waiting to read. It runs on the reaper's 30-second tick, over every path any codebuild node has ever registered, decommissioned nodes included.

What it needs is a grant on the **controller's** role, not the node's and never the build's: `ssm:GetParametersByPath` and `ssm:DeleteParameter` on the path and its children, and nothing else. The controller lists names without decrypting anything and never reads a registration. One thing worth knowing about that grant, measured: under the account's default `aws/ssm` key it *would* return plaintext to a caller that asked for decryption, because that key authorises any principal in the account through Parameter Store. billet never asks, and its tests pin that; if you want the grant itself to be the boundary as well, encrypt registrations under a customer-managed key (`enable_kms` in the module), which the controller's grant cannot decrypt — measured: under one, the same role was refused `kms:Decrypt`, its delete still worked, and a page mixing a customer-key parameter with a default-key one failed whole rather than handing back the default one. The key covers the parameters under it and nothing beside them, so put the whole path under one. The Terraform module attaches it when you set `controller_role_name` (in the root module's co-located topology that is fleet-ec2's node role), exposes the same document as `controller_sweep_policy_json`, and `billet init iam --controller-sweep --account <id>` prints it from the node's config. Without the grant the sweep fails closed: nothing is deleted, and `billet status` shows the refusal.

`billet status` reports each path: how many registrations the sweep has removed in total and on its last pass, how many are waiting on a lease that is open or too recently closed, how many name a lease this ledger has never seen (kept, and worth a look — a ledger restored from an older backup looks like this), and how many under the path are not billet's at all. It also names a codebuild node that registered without its path, which happens on a node older than wire version 18; upgrade it and its path joins the sweep. A node that later changes `jit_parameter_path` stops its old path being swept — remove anything left under the old one by hand.

## What a reserved fleet actually costs

**Deleting a reserved fleet does not stop the billing.** `DeleteFleet` succeeds immediately and moves the fleet to `PENDING_DELETION`, and AWS's own response says why:

> Fleets are deleted after all instances have run for 24 hours. Fleets are available to build projects while they are pending deletion.

So **a reserved fleet is a 24-hour commitment**, not an hourly one. At the `arm.m2.medium` rate from AWS's pricing API — $0.02 per minute — a single-instance macOS fleet is **$28.80 minimum**, whether it runs one build or a thousand. Billet measured this by creating one to run two builds of under a minute each; that experiment cost $28.80.

**And the 24 hours is a QUOTA hold, not only a bill.** The default limit is **one** `mac2-m2.metal` instance per account, and a fleet in `PENDING_DELETION` still holds it — measured: creating a second macOS fleet while the first was pending returned `AccountLimitExceededException: The number of instances of type mac2-m2.metal exceeded limit 1`. So "delete it and make another" is impossible for a day. What saves you is the other half of AWS's own message: a pending-deletion fleet **still serves builds**, so the fleet you already have is the one to keep using.

Two consequences worth internalising before you enable a macOS tier:

- **There is no cheap way to "just try" reserved capacity.** Creating a fleet to see whether it works commits you to a day of it. Size the experiment accordingly, or start with on-demand Linux, which bills per build-minute with no fleet at all.
- **The commitment is per fleet, not per build**, so an idle fleet costs exactly what a busy one does. `billet check --provider codebuild` reports the fleet's configured capacity for this reason: on this backend the standing cost is the number to watch, not the per-job one.

Billet never creates or deletes a fleet. It is yours, the Terraform module makes it explicit, and nothing in a drain, an upgrade or a teardown will remove one.

## Getting started

The Terraform module `terraform/modules/billet/modules/fleet-codebuild` creates or adopts the project, an optional reserved fleet, both roles, the log group, the VPC configuration, the parameter path and an optional KMS key, and outputs the non-secret facts your `billet.yaml` needs. Linux on demand is the inexpensive default; macOS reserved capacity is an explicit opt-in. See its README.

Then generate the config. `billet init` writes a complete one, and every value it cannot know is a flag rather than a default:

```bash
billet init --provider codebuild \
  --org <your-org> \
  --runner-group <group> --workflow 'org/repo/.github/workflows/ci.yml@refs/heads/main' \
  --region us-west-2 \
  --codebuild-project billet-runners \
  --codebuild-environment LINUX_CONTAINER \
  --jit-parameter-path /billet/jit \
  --compute-type BUILD_GENERAL1_SMALL=2,3GiB,0.005 \
  --compute-type BUILD_GENERAL1_MEDIUM=4,7GiB,0.01 \
  --max-vcpu 16 --max-memory 32GiB \
  --privileged \
  --accept-external-build-ceiling
```

Five of those are refusals rather than conveniences, and each is a decision billet will not make for you:

- **`--runner-group` and `--workflow` are required.** Untrusted work is refused on this backend outright, so every tier is trusted — and a trusted pool is defined by a non-default runner group and an exact workflow allowlist. A generation without them would load, start, and then refuse its first fork pull request with no setting anywhere that would have made it work.
- **`--accept-external-build-ceiling` is required.** It changes nothing about how billet behaves, which is exactly why the generator will not write it for you: the whole point is that the 36-hour and 8-hour limits are read by a person before a tier advertises capacity.
- **`--compute-type` carries all three numbers.** There is no API that reports what `BUILD_GENERAL1_MEDIUM` holds, so billet is told — and a shape declared smaller than it is overcommits a budget nobody can see. They are ORDERED: placement charges the first that fits.
- **`--jit-parameter-path` is an IAM boundary**, not a naming preference. The node's policy is scoped to exactly this path, so a value billet guessed would be either unwritable or wider than the grant you reviewed.
- **`--privileged` is a real privilege grant.** Without it a job's first `docker build` or service container fails. billet does not turn it on for you.

A macOS generation needs two more, because a macOS tier has to pin the host its guest limit is enforced against and that limit is **not** Apple's per-machine allowance here:

```bash
billet init --provider codebuild ... \
  --codebuild-environment MAC_ARM \
  --codebuild-fleet-arn arn:aws:codebuild:us-west-2:…:fleet/macs \
  --codebuild-fleet-capacity 1 \
  --node-name cb-macs-1
```

The generation writes a `nodes:` policy raising that host's macOS limit to the fleet's capacity — otherwise the tier is judged against Apple's two-guest licence and refused at load for a reason that has nothing to do with CodeBuild. `aws codebuild batch-get-fleets --names <fleet>` reports the capacity.

Then:

```bash
billet github-app create --config <the file>   # creates the App and edits the config in place
billet check --provider codebuild   # ceilings, inventory window, project and fleet
billet ca issue <node>              # the node's certificate bundle
billet node                         # on the orchestrator machine
billet status                       # confirm the node registered and what it advertises
```
