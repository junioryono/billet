# AWS acceptance

The EC2 runner path completed three cold, end-to-end GitHub Actions jobs from a private consumer repository on 2026-08-18. This document records the reusable Billet behavior without making the consumer part of Billet's configuration or product surface.

## Running one: `billet acceptance`

Everything below was measured by hand. **It is now a command**, and the reason to have made it one is not convenience: an acceptance run launches billable compute in a real account, and a hand-driven procedure that fails in the middle leaves it there. The teardown is what needed engineering, not the setup.

```bash
scripts/acceptance.sh --config /etc/billet/billet.yaml --account 810711872940
# or, once the deployment exists:
billet acceptance up    --config /etc/billet/billet.yaml --workspace /tmp/acc --account 810711872940
billet acceptance run   --workspace /tmp/acc --jobs 1
billet acceptance down  --workspace /tmp/acc
billet acceptance sweep --workspace /tmp/acc   # non-zero if anything remains
```

**`.github/workflows/acceptance.yml` runs it weekly** against the account named below, and on `workflow_dispatch` for a ref you choose. Never on `pull_request`: the job holds an OIDC role that can create EC2 instances and a GitHub App private key that can mint tokens for the whole organization, and a pull-request workflow from a fork runs code its author controls. Its concurrency group is never cancelled either — `cancel-in-progress` would kill a run *between* its launch and its teardown, which is exactly the state that leaves compute billing.

**`billet acceptance` does not dispatch a workflow.** `up` prints the tier labels it derived; start a run that names one. What billet owns is standing the deployment up, observing what happened, and destroying exactly what it made — deciding which workflow to run is the operator's, and a command that dispatched one would need write access to a repository for no reason. The workflow dispatches one, and the credential it uses is minted in the run from the App key the run already holds: the acceptance App is installed on the guide repository alone with `actions: write` there, the installation token is narrowed at minting to that repository and that permission, and the step after the dispatch revokes it and fails the job unless GitHub confirms the revocation; a job cancelled or a runner lost before that step runs leaves the token's own hour as the bound. No person's token is involved, which is the difference between a credential that expires on a date nobody watches and one that exists for seconds inside the job that needed it (measured 2026-09-04: the token the App mints carries exactly `actions: write` and `metadata: read` on that one repository, and `DELETE /installation/token` answers 204).

### What makes it isolated, and what it cannot isolate

**A fresh state directory mints its own `state.DeploymentID`**, and that identity already scopes every `List`, every `Destroy` and every cloud tag billet writes. So an acceptance run cannot discover or delete another deployment's compute *by construction* rather than by the command being careful. On top of that the command refuses three ways an operator could point it somewhere real:

- a workspace **inside** the base config's own state directory — there, the identity it minted and the teardown that follows would be the real deployment's;
- a workspace whose record and state directory name **different** deployments — two runs have used it, and a teardown scoped to either is wrong about the other's compute;
- an AWS credential in an account `--account` did not name. That check is three-valued: matched, differs, or **could not be established** — and the third refuses too, because "I could not find out" must never run the thing it stands in front of.

**Every tier label is prefixed** (`accept-` by default). A tier's label *is* its GitHub scale set, so this is what makes `billet acceptance down`'s `teardown --all` disjoint from the production deployment's scale sets rather than fatal to them.

**What it cannot isolate is the GitHub App and the organization**, and the command says so in the config it derives. There is one App per deployment in billet's model but one per org in practice, and minting a second per run is not something a harness should do — an App's private key is issued exactly once.

### The teardown, and why it is a trap

Seal → wait → GitHub → the cloud → **sweep**. The order is `billet local down`'s, for its reasons: sealing stops admission and says nothing about what is already running, so the wait is what covers that; the scale sets go before the compute, so no runner registration outlives the instance it named; and the sweep at the end turns "the commands ran" into "there is nothing left" by *asking the provider* rather than by assuming.

It runs on `EXIT`, `INT` and `TERM` rather than as the last line of the script, and `billet acceptance run` reaches it on a context **detached** from the caller's — because the likeliest way into the teardown is the operator's own Ctrl-C, and a teardown that inherited that cancellation would return immediately having destroyed nothing.

**Nothing here ever escalates to a kill.** `billet node`'s SIGTERM is a drain with no deadline; a harness that killed it would fail somebody's build to save itself a wait. What the timeouts bound is how long the command *watches*, and an overrun is reported rather than acted on.

### The sweep is a subcommand, not a grep

`billet acceptance sweep` asks the provider what still carries this deployment and exits non-zero if anything does. It is a command rather than a shell pipeline in the workflow because the first version of that step matched `billet decommission`'s *prose* for a phrase meaning "there is nothing here" — which makes a reworded diagnostic silently turn a red job green, and puts the one decision that has to be right where no test can reach it.

Its answer is **three-valued**: nothing remains, something remains, or billet could not ask. The third is its own non-zero, because a sweep that could not look establishes nothing in either direction. One match inside it is still on a sentence — `decommission` returns an unwrapped error — and that is fragile in the safe direction: a rewording stops being recognised as "something remains" and falls through to "could not establish", which is still red. A test asserts exactly that.

### The evidence

`evidence.json` is written **before** the teardown (which destroys what it is about) and **after** the services stop (so nothing it reads is moving). Every field is something billet observed:

- every job the ledger recorded, with **both** verdicts — GitHub's own reported result beside billet's own conclusion, because a run where those disagree is the finding and reporting one would hide it;
- what the ledger still holds, from `alloc.Quiescence`;
- and the **compute barrier**'s answer — each host's state as billet can actually establish it, which is the only thing in the document that can see compute whose lease has already gone. Its six states are reported as themselves rather than collapsed into two, so "could not be asked" never reads as either answer it is not.

### The lane, made real (2026-09-04)

Until 2026-09-04 the workflow above had never run: the role it assumes did not exist, nothing in the account trusted GitHub's OIDC issuer, and the repository held none of its secrets. The consumer's own Terraform root now creates the GitHub OIDC provider and the `billet-acceptance` role, trusting exactly this repository's `environment:acceptance` subject, so the workflow's job declares that environment and a scheduled run from `main` and a dispatched run from a branch are one principal. The role carries the node policy `billet init iam --account-wide` renders over the lane's base config (account-wide because `billet acceptance up` mints a fresh deployment identity every run), plus one delete for the teardown's cache purge, and an egress-only security group that doubles as the untrusted group. The App is a dedicated one, created with `billet github-app create` and installed on the organization for the guide repository alone, so the production controller's key never leaves the controller. The AMI the lane boots, `billet-acceptance-x64-20260904` (`ami-07219c3abc48abfc8`, contract 2), was built by `billet ami build` and boot-verified by `billet ami verify` after the build's own credentials expired between registering the image and verifying it; a build takes longer than an hour of SSO session on an 80 GiB builder disk.

**One guard the account states that the generator does not.** The account-wide policy grants `ec2:RunInstances` on `*`, and RunInstances authorises any snapshot a block-device mapping names; the same account keeps the production ledger volume's daily snapshots, a volume holding the deployment identity and the node-wire CA key. The role therefore carries an explicit deny on `ec2:RunInstances` for `arn:aws:ec2:*::snapshot/*` when `ec2:ResourceTag/sh.billet.owner` is null. Measured with `aws iam simulate-principal-policy` on 2026-09-04: `RunInstances` against a production ledger snapshot ARN is an explicit deny from that policy; the same with an owner tag in context is allowed; a plain instance launch is allowed. The generator is asked to carry the guard itself in issue #39, because the exposure is the same wherever a node role shares an account with anything that has snapshots.

The workflow first expected a secret only a person can mint, a fine-grained personal access token that dispatches the guide repository's workflow, and nobody had minted one. It now mints an installation token from the App key it already stages, as the paragraph on dispatch above says; the acceptance App was given `actions: write` on the guide repository for exactly that step, and nothing else about its permissions moved.

**The first two runs (2026-09-04).** Run `33838621001`, dispatched from `main` the moment the dispatch token landed, was refused at `Assume the acceptance role` with `Not authorized to perform sts:AssumeRoleWithWebIdentity`. CloudTrail in the account recorded the subject STS was shown: `repo:junioryono@19849174/billet@1355631884:environment:acceptance`, the id-qualified spelling, while the role trusted only the plain `repo:junioryono/billet:environment:acceptance`. The repository's OIDC customisation (`GET /repos/{owner}/{repo}/actions/oidc/customization/sub`) reports `use_immutable_subject: false`, so the plain spelling was the documented expectation and the qualified one is what GitHub issues; the rule is pinned to the measurement, and the role now names both spellings under `StringEquals`, each naming one repository and one environment. Nothing was created before the refusal, so the `always()` teardown and sweep were red only for want of a workspace. Run `33839266293`, dispatched after the trust policy was corrected, assumed the role, derived deployment `21859fc802fc7a8b1075cb63d486e68c`, minted its dispatch token and then failed at the dispatch: `gh workflow run` without `--ref` asks GraphQL for the repository's default branch, and `repository.defaultBranchRef` needs `contents: read`, which the token deliberately lacks (`Resource not accessible by integration`). The teardown and the sweep were green, so the run left nothing in the account, and the token was revoked before `run` would have started. The dispatch now reads the default branch from the REST repository object, which `metadata: read` covers (measured with a token of exactly those two permissions: 200, `main`), and posts to the dispatch endpoint directly.

## Environment

- A Billet control plane and EC2 node ran as separate services against an isolated acceptance configuration and state directory.
- The node used an encrypted AMI produced by `billet ami build`; EBS encryption by default was enabled for the account.
- **The account these measurements now run in is `810711872940`, us-west-2** — a dedicated CI account holding nothing else, created because the control plane stores a GitHub App private key and a node-wire CA, and because the EC2 fleet executes arbitrary pull-request code. Neither belongs beside anything that matters. Earlier acceptance work ran in a different account and left nothing behind there.
- That account is named here so contributors and automation target the same place rather than improvising, and it is the only consumer-specific fact in this document. It is not a credential; it is an identifier, and it is recorded deliberately rather than by accident.
- The AMI contained the GitHub Actions runner, Docker, git and the Billet runner bootstrap, but no Go toolchain.
- Each workflow used a dedicated EC2 instance and a JIT runner registration.

## Building the image the node launches

`billet ami build` needs three things named together, and the third is the one that gets missed:

```bash
billet ami build --region us-west-2 \
  --subnet         subnet-… \
  --security-group sg-… \
  --public-ip \
  --payload-bucket …
```

**`--public-ip` matters when the subnet has no NAT gateway**, which is the case here: the builder subnet's only default route is an internet gateway, and a route to an internet gateway is unusable without an address. That is what the flag's own help says, and it is the reason to pass it.

Whether a subnet's `MapPublicIpOnLaunch` can substitute for the flag is **not established**, and an earlier version of this section asserted that it cannot. That assertion was wrong to make: it was based on reading `PublicIpAddress: null` from *terminated* instances, where the field is always null regardless of what was assigned while running. CloudTrail shows every builder launch in this subnet requested `associatePublicIpAddress=true` explicitly, so no launch here has ever exercised the case the claim was about. Pass the flag; do not rely on the claim.

`--security-group` is required and billet deliberately does not fall back to the VPC's default group, which allows all traffic between its members.


## Workflow coverage

The accepted workflow proved all of the acceptance conditions in one real GitHub job:

- GitHub assigned the queued job to the Billet scale set and the runner consumed its JIT registration after privilege drop.
- Repository checkout succeeded over the instance's configured network path.
- A Docker image build succeeded.
- A service container started and was reachable from the job.
- `actions/setup-go` installed Go at runtime and the job used it successfully, proving the minimal image does not depend on a preinstalled toolchain.
- The workflow completed green.
- The EC2 instance entered termination and disappeared from live inventory; no acceptance instance remained afterward.

## Cold-start measurements

The three launch-to-job-start samples were 47.609 seconds, 52.188 seconds and 58.662 seconds. They recorded 37.936, 42.194 and 43.220 seconds from launch to runner readiness, followed by 9.673, 9.993 and 15.442 seconds from runner readiness to the first job step.

Those numbers put the current cold path under one minute. Billet's EC2 role is fallback capacity rather than the default place every job runs, so saving roughly forty seconds does not justify a standing pool of live, billable compute with no lease, unresolved trust reuse and a new teardown state. `warm_pool` therefore remains refused unless a materially different workload produces evidence that changes that trade.

## Spot interruption

The isolated acceptance configuration explicitly selected Spot for a fourth private-repository workflow, which stayed inside a five-minute observation step while AWS Fault Injection Service sent a two-minute `terminate` warning to its exact instance. EventBridge delivered the warning to that node's SQS queue. FIS completed the interruption action at 12:37:10 UTC; Billet observed the warning at 12:37:12 UTC, durably wrote `ec2 spot interruption: terminate`, and asked EC2 to terminate one second later. The lease stayed charged while the instance was `shutting-down`, then archived as failed at 12:38:41 UTC, 89 seconds after Billet observed the warning; the listener created a replacement capacity lease 13 seconds later without launching unrequested compute. GitHub marked the original run failed at 12:47:17 UTC; no replacement instance was launched. The acceptance configuration was then restored to on-demand, both acceptance services were healthy, and the temporary FIS template, IAM role, EventBridge rule and SQS queue were deleted. No acceptance instance remained.

## Same-label failover

The same unchanged workflow completed on the preferred local Firecracker provider, including service containers, a Docker build and run, repository checkout and runtime Go installation. After the local node contribution was stopped, that same label completed on an EC2 `c7i.xlarge`; Billet held its capacity through termination, left no instance behind, and resumed normal local service after the test. This closes the end-to-end acceptance gap for same-label failover.

## CodeBuild: what the API actually does

Seven questions the CodeBuild backend's design turns on, asked of real CodeBuild and real Parameter Store in `810711872940`/us-west-2 on **2026-08-31**, with a throwaway project (`billet-cb-measure`, `NO_SOURCE`, `LINUX_CONTAINER`, `BUILD_GENERAL1_SMALL`) and a service role holding nothing but what each question needed. These are observations, not pass/fail: three of them contradicted what the design had assumed, and each one changed code.

**The runner registration never leaves Parameter Store.** A build was started with `ACTIONS_RUNNER_INPUT_JITCONFIG` as a `PARAMETER_STORE`-typed override naming a SecureString. `BatchGetBuilds` reported `{"name":"ACTIONS_RUNNER_INPUT_JITCONFIG","value":"/billet/measure/jit/lease-aaaa","type":"PARAMETER_STORE"}` — the parameter's **name**, never its value — and the staged value appeared nowhere in the response, before or after completion. The build log did not contain it either, and the reason is worth keeping: CloudWatch logs each command's **source text**, not its expansion (`Running command echo "resolved-length=${#ACTIONS_RUNNER_INPUT_JITCONFIG}"`), so a buildspec that echoed the variable directly *would* leak it. billet's generated buildspec never names it. The build did resolve it: `resolved-length=48`, the exact byte length of the staged value.

**`ssm:GetParameters` alone is enough.** That role held only the plural action. The singular `ssm:GetParameter` was in the build-role grant on the reasoning that either spelling might be the one CodeBuild calls; it is not called, and it is no longer granted.

**A project-scoped IAM grant authorizes the build actions.** A role holding exactly `StartBuild`, `StopBuild`, `BatchGetBuilds`, `ListBuildsForProject` and `BatchGetProjects` on one project ARN — and nothing else — succeeded at all five, and was correctly refused `ListBuildsForProject` on a project the policy does not name. IAM's `simulate-custom-policy` agrees, and additionally refuses a `build/...`-scoped statement against a build ARN: that is not a resource type these actions accept. A review had argued the opposite, which would have meant every teardown and inventory failing after the first launch.

**The idempotency token behaves as documented, and only for five minutes.** Two identical `StartBuild` calls with one token returned the **same** build id. A third with the same token and a changed environment variable was refused `InvalidInputException: Request parameter mismatch for idempotency token <token>`. The five-minute validity is AWS's own documentation and was not waited out; what it means for billet is that an ambiguous `StartBuild` must not be retried at all, since the window is wall-clock and billet does not bound its own HTTP client's timeout.

**`StopBuild` against a build that is already over succeeds.** It returned `buildStatus: SUCCEEDED` and exit 0 for a completed build — not an error. Against an id the project never issued it returned `ResourceNotFoundException`. billet treats the second as neither an error nor proof, and confirms termination by polling either way.

**A timed-out build reports `FAILED`, not `TIMED_OUT`.** This is the one that mattered most and was the opposite of what the design assumed. A build with `timeoutInMinutesOverride: 5` whose `BUILD` phase slept 400s came back:

```
buildStatus                FAILED
phases[BUILD].phaseStatus  TIMED_OUT
phases[BUILD].contexts[0]  BUILD_TIMED_OUT: Build has timed out.
```

`TIMED_OUT` *is* a documented build status, so billet asks about both — but a predicate over `buildStatus` alone could never have fired for the case it exists for, and every build the service's own ceiling ended would have been filed as somebody's failing test.

**Reserved Parameter Store namespaces are case-insensitive.** `PutParameter` refused `/AWS/…` and `/Aws/…` with `AccessDeniedException: No access to reserved parameter name` and `/SSM/…` with `ValidationException: Parameter name: can't be prefixed with "ssm" (case-insensitive)` — AWS's own message states the rule. billet's path check was case-sensitive, so `/AWS/billet` loaded and then failed on every registration.

**`BatchGetBuilds` does preserve request order** — asked in reverse it answered in reverse — which is exactly why billet's dependence on that was invisible. AWS documents the call as answering *about* the ids it was given and says nothing about order, so billet imposes the order rather than observing it.

One incidental timing, from a `NO_SOURCE` `BUILD_GENERAL1_SMALL` build: submitted to complete in **7 seconds**, of which `PROVISIONING` was 4. `DOWNLOAD_SOURCE` runs and succeeds even with no source, which is what billet's consumption check keys on.

**And the one command a diagnostic prints was run.** `billet init iam` refuses a `jit_kms_key_id` that is an alias rather than an ARN, and prints the `aws kms describe-key` that resolves it. Executed as printed, AWS parsed every flag and looked the alias up in the configured region — the refusal names `arn:aws:kms:us-west-2:810711872940:alias/billet-jit`, so `--region` landed — and failed only because no such alias exists in the account. That is what the check was for: an earlier version of that command omitted `--region`, which would have resolved a same-named alias against whatever the operator's CLI defaulted to and handed back a *different key*, after which the policy applies cleanly and no build can decrypt its registration. A printed command is a claim, and this is the cheapest possible way to test one.

Everything created for these measurements was deleted afterwards.

### The provider itself, against the real API, under the policy billet tells you to attach

The nine answers above are API observations. This is the whole provider driven end to end against real CodeBuild on **2026-08-31**, and it lives in the repository as `internal/provider/codebuild/realcodebuild_test.go` rather than as a transcript — the same shape `realtart_test.go` and `realfirecracker_test.go` take one backend over, and for the same reason: a fake models this repository's *understanding* of an API, and three of the nine answers above contradicted that understanding.

It skips unless `BILLET_TEST_CODEBUILD_PROJECT` names a project and AWS credentials are in the environment, so it can never start billable compute by accident. Four cases pass: the whole lifecycle (`Launch` → `Find` → `List` → `Destroy`, with `Find` matching the build back by its lease marker alone, since a build cannot be tagged); the registration never leaving Parameter Store (`BatchGetBuilds` reports `type: PARAMETER_STORE` and the parameter's *name*); a `Destroy` that polls to a terminal state and then removes the staged registration; and an idempotent second `Destroy`.

**IT RUNS AS THE NODE ROLE, NOT AS AN ADMINISTRATOR, AND THAT IS THE POINT.** The policy is `billet init iam --account …` output, attached to a role assumed for the run. Running it as an administrator would prove the code works and say nothing about whether the grant billet tells operators to attach is sufficient — which is the half a review round argued was broken, and the half that fails on the first job of the day rather than in a test. It is sufficient: every call the provider makes is authorised by the project-scoped grant, including `StopBuild` and `BatchGetBuilds`.

Two things that only a real run produced:

- **The five-minute idempotency token deduplicates across PROCESSES.** The first version of that test used fixed lease ids, and a second run inside the window returned *the same build id as the previous run* — by then terminal, so `Running` was false and `List` correctly excluded it. Nothing was broken except the test's assumption that a name was free; lease ids are now random per run. It is live confirmation of the property the launch path's no-retry-on-ambiguity rule is written against.
- **An assertion that could only fail.** The test first checked the registration was gone by calling `deleteJITConfig` and expecting an error — but that call answers nil for an already-absent parameter, deliberately, because that idempotency is what lets settlement reap something teardown already removed. The check is now a no-clobber `putJITConfig`, whose *success* proves the name was free. That also keeps it inside the node's own grant, which carries no `ssm:GetParameter` by design: billet writes and deletes registrations and never reads them back.

### The reserved macOS fleet: not scrubbed, and a 24-hour minimum

A `MAC_ARM` reserved-capacity fleet (`BUILD_GENERAL1_MEDIUM`, base capacity 1) in us-west-2 on **2026-08-31**, created to answer the two questions ADR-007 left open.

**A RESERVED INSTANCE IS NOT SCRUBBED BETWEEN BUILDS, ASKED DIRECTLY.** Build one wrote `$HOME/billet-scrub-marker`; build two, a separate `StartBuild` on the same fleet, read it back:

```
build 1:  wrote to /Users/cbuser/billet-scrub-marker
build 2:  SURVIVED: billet-scrub-probe-1788221377
          whoami: cbuser  home=/Users/cbuser
```

Same user, same home directory, contents intact across builds. That is AWS's documented cross-build persistence confirmed by measurement rather than by quotation, and it is what billet's refusal of untrusted work on this backend rests on: no security group fixes a machine whose filesystem the next job inherits. It also means the on-demand case needs its own separate measurement — this says nothing about whether an **on-demand** fleet destroys its machine.

**WHAT AWS'S CURATED macOS IMAGE SHIPS** (`aws/codebuild/macos-arm-base:14`), which decides what a macOS tier's runner bootstrap can assume:

| | |
|---|---|
| Architecture | `arm64` |
| Xcode | **26.2**, build 17C52 |
| git | 2.52.0 |
| tar | `bsdtar` 3.5.3 (libarchive) — **not** GNU tar |
| Actions runner | **absent** — no `~/actions-runner`, no `Runner.Listener` on PATH |
| Build user | `cbuser`, `$HOME=/Users/cbuser` |

The runner's absence is what billet already assumed and now knows: the generated buildspec downloads and verifies the pinned `osx-arm64` tarball, which is why `internal/runnerrelease/pinned.txt` carries that platform's checksum. `bsdtar` rather than GNU tar is worth noting for anything that later reaches for GNU-only flags.

**AND THE 24 HOURS HOLDS THE QUOTA, NOT JUST THE BILL.** The default limit is one `mac2-m2.metal` per account and a `PENDING_DELETION` fleet still occupies it: creating a second macOS fleet while the first was pending was refused with `AccountLimitExceededException: The number of instances of type mac2-m2.metal exceeded limit 1`. A pending fleet does still serve builds, which is what made the macOS job run below possible without a second one — but an operator who deletes a macOS fleet expecting to recreate it is locked out for a day.

**AND DELETING THE FLEET DOES NOT STOP THE BILLING.** `DeleteFleet` returns success and moves the fleet to `PENDING_DELETION` with this message:

> *Fleets are deleted after all instances have run for 24 hours. Fleets are available to build projects while they are pending deletion.*

So a reserved fleet carries a **24-hour minimum**, whatever it is used for. At the `arm.m2.medium` rate from AWS's own pricing API — $0.02 per minute — that is **$28.80 for a fleet created to run two builds lasting under a minute each**. This measurement cost that. It is recorded here because it is the single most expensive thing an operator can get wrong about this backend: creating a macOS fleet "just to try it" is a $28.80 commitment, not an hourly one, and the same is true of every reserved fleet including Linux ones.

### A real GitHub Actions job on CodeBuild, and the two defects that only it could find

**2026-09-01, run `33456832871` in a private consumer repository.** A queued job on a `billet-4vcpu-codebuild` tier was acquired by the scale-set listener, escrowed against the ledger, dispatched to a `codebuild` node, launched as a real build, and completed green. Every step passed: `actions/checkout@v4` over the build's own network path, the runner's own identity, and an assertion that the job **cannot read its own registration**.

What ties the job to the build billet started is CodeBuild's own marker, printed from inside the job:

```
CODEBUILD_BUILD_ARN: arn:aws:codebuild:us-west-2:…:build/billet-accept-linux:71cc3815-2f93-4152-bf22-92c39c73c1e4
kernel:              Linux … 4.14.355-284.741.amzn2.x86_64 … x86_64 GNU/Linux
```

— the same build id the node logged at launch. The runner's own log closes it: `√ Connected to GitHub`, `Running job: prove`, `Job prove completed with result: Succeeded`, `√ Removed .credentials`.

**IT TOOK THREE ATTEMPTS, AND TWO OF THE FAILURES WERE REAL DEFECTS THAT NO TEST COULD HAVE CAUGHT.**

**A standard SSM parameter caps its value at 4096 characters, and a GitHub JIT runner configuration exceeds it.** Every launch died at staging with a bare `ValidationException` — before `StartBuild` was ever reached — so the backend could not run a single job. Measured directly afterwards: 4096 accepted, 4097 refused with `Standard tier parameters support a maximum parameter value of 4096 characters`. Nothing caught it because *every* test stages a short placeholder: the unit suite, the e2e stand-in and the live provider test all use a few dozen bytes, so the one thing that differs from production is exactly the thing that broke. `putJITConfig` now sets `Tier: Intelligent-Tiering`, which keeps the parameter standard while the value fits and promotes it only when it does not.

**A CodeBuild container runs as root, and GitHub's runner refuses to start there.** With the staging fixed, the build downloaded the runner, verified its checksum, and printed:

```
/opt/billet/actions-runner/actions-runner-linux-x64-2.336.0.tar.gz: OK
Must not run interactively with sudo
Exiting runner...
```

— after which **CodeBuild reported the build SUCCEEDED**, because the script exited zero. Compute that starts, registers nothing and reports success is precisely the failure the `ec2` boot script's own comment warns about, one backend over. The executed-buildspec suite could not see it and does not pretend to: it substitutes a stand-in runner, and a stand-in has no root guard. The buildspec now exports `RUNNER_ALLOW_RUNASROOT=1` in the phase that execs the runner, and a test asserts both that it is emitted and that it is in the *right phase* — CodeBuild runs each phase's commands in their own shell.

**Two things about the surrounding configuration are worth recording**, because both cost a cycle and neither is a billet defect:

- **A `codebuild` node must not declare a `site`.** A site's store is its cache authority (`ceph` or `ebs-s3`) and a CodeBuild build uses neither, so the node wire refuses the pairing. The refusal is correct; at the time it arrived at registration rather than at config load. Config load now refuses it too (`validateCodeBuildNode`), naming the reason, and the registration check stays because the control plane is the authority on what a site is.
- **A trusted pool needs a non-default runner group AND an exact workflow allowlist**, and billet validates its config against GitHub's *own* runner-group restriction rather than trusting it. That is the design working: an omitted `trust` defaults to `untrusted`, and this backend refuses untrusted work — which is how the first attempt failed, with the refusal naming reserved-capacity reuse exactly as intended. This organisation also has **two runner groups named `Default`** (its own, and an inherited enterprise one with zero selected repositories), so resolving a group by name is ambiguous there; the acceptance used a dedicated group.

**And the documented staged-credential residual is real.** The node was killed rather than drained at the end of the run, and three registrations were left in Parameter Store — exactly the leak ADR-007 records as bounded and not closed by a sweep. They were removed by hand.

### A real Xcode job on managed Apple silicon

**2026-09-01, run `33460796410`.** A `billet-macos-codebuild` tier — `MAC_ARM`, `BUILD_GENERAL1_MEDIUM`, pinned to a reserved fleet — took a queued job through the same path as the Linux one and completed green. From inside the job:

```
os:    macOS 26.2          arch: arm64          user: cbuser
CODEBUILD_BUILD_ARN: arn:aws:codebuild:us-west-2:…:build/billet-accept-macos:56e7f81c-…
Xcode 26.2 (build 17C52)
Apple Swift version 6.2.3, Target: arm64-apple-macosx26.0
./hello: Mach-O 64-bit executable arm64
built and ran on arm64
```

— the same build id the node logged launching. So the managed-macOS claim is now proven rather than implemented: a checkout, Xcode, and a Swift binary compiled *and executed* on Apple silicon billet neither owns nor administers, with the job unable to read its own runner registration.

**It took one more defect to get here, and it is the one only macOS could show.** `/opt/billet/actions-runner` was hardcoded as both the custom-image location and the download target. A Linux container runs as root so it worked; a `MAC_ARM` build runs as `cbuser` and died on `mkdir: /opt/billet: Permission denied`, then a curl that could not write, then a checksum of a file that did not exist. The download target is now under `$HOME` — the one path both environments agree on. The `test -x` gate added for the root guard is what made this a FAILED build rather than another silent success.

**Two operational notes from the same session.** A `macos_vm_limit` of 1 means a single stuck lease blocks the tier entirely, and a completion bound to a node incarnation that had been killed kept its capacity charged; the escape used here was a fresh ledger. That second note was read at the time as billet correctly refusing to free capacity it could not prove idle, and reproducing it against the real CodeBuild backend found it was a defect with two halves. The completion was bound to the process that launched the build; the plane refuses to hand its destroy to the replacement process (right — a replacement truthfully knows nothing), and the listener read that refusal as an ordinary failed destroy and kept RENEWING the lease while it retried. A lease that is renewed never expires, so it was never quarantined, so the replacement's inventory could never settle it, and every retry got the same answer for as long as the control plane ran; `billet leases` showed nothing held and `--force` refused the lease as busy. The listener now stops renewing a lease whose holder cannot be reached and keeps the obligation, the reaper quarantines it within a lease TTL, the replacement host's next inventory after the grace settles it, and the completion's retry corrects that provisional verdict to GitHub's own outcome. The issue's own hypothesis — a killed custody or teardown holder leaving its lease held forever — did not reproduce: nothing renews such a lease once its process dies, it is quarantined within a TTL, and `billet leases release --force` releases it from there; what that reproduction did find was a restart re-adopting a still-running teardown as custody, a transition the state machine refuses, so the adoption now keeps the phase it found — and, one layer down, that a failed launch's outcome lived only in the memory of the process that decided it, so the node now records why a hold will fail before it first reports it. `billet leases` names the node process holding each lease by its incarnation and says when a different process replaced it, and `billet status` lists running leases whose holder was replaced under `bound`. And each control-plane restart leaves a scale-set message session GitHub has to expire; billet says so and says queued jobs are not lost, but after several restarts the fastest recovery was `billet teardown --tier …` and letting billet recreate the scale set.

### The acceptance was RE-RUN against the buildspec that actually ships

**2026-09-01, run `33467578339`, green**, on build `billet-accept-linux:a945d271-cf45-4eba-bd50-7cbc51c44eab`.

The first two acceptance runs used the buildspec as of `320e3c4`. Three review rounds then changed the fetch path — the `&&` chain, the emptied download directory, the canonical-HOME guards, the `&&`-joined `test -x`, the guarded build phase — and the fetch path is exactly what a curated image exercises, so the earlier runs no longer said anything about what an operator would get. From inside the re-run:

```
os: Linux   arch: x86_64   user: root
CODEBUILD_BUILD_ARN: …:build/billet-accept-linux:a945d271-…
runner dir: /root/.billet/actions-runner
the registration is not in the job's environment
container ok
```

`actions/checkout@v4` and `docker run` both worked, the runner came from the new `$HOME` download directory rather than `/opt/billet`, and the job still cannot read its own registration. **The rule this re-run encodes: a live acceptance is evidence about the code that ran, and every later change to that code spends it.**

### Two operator traps the re-run walked into, neither a billet defect

**A `codebuild` TIER must not declare a `site` either, and the symptom was silence.** (Config load refuses this pairing now, in `validateSites`, naming the tier and the provider; what follows is what it looked like before.) The refusal an operator meets for `node.site` is loud and specific — registration fails naming the cache authority. A tier that declares `site:` while its codebuild node cannot has no eligible host, so placement matches nothing, the tier advertises **0**, the job queues, and `billet check` reports everything healthy. That is worse than the registration refusal it mirrors: `billet status` showed `0 available` with no line saying why.

**Two scale sets can carry the same label in different runner groups, and the stale one wins the queue.** A tier torn down without `billet teardown` leaves its scale set behind; billet says so at startup (*"a scale set billet created is no longer declared by any tier; it advertises nothing, so a job using that label queues rather than failing"*) and names the exact command. The trap is that `billet teardown` correctly refuses `--runner-group` naming a group the tier is not declared with — right, and it means the leftover cannot be removed with the config that replaced it. A unique label was the way through.

### The macOS half was re-run against the shipping buildspec too

**2026-09-01, run `33469472853`, green**, on build `billet-accept-macos:dcd2435e-…`, using the fleet that was already provisioned and pending deletion. From inside the job:

```
os:   macOS 26.2   arch: arm64   user: cbuser
HOME=/Users/cbuser   resolved=/Users/cbuser
the runner is under the resolved home
Xcode 26.2
Apple Swift version 6.2.3 (swiftlang-6.2.3.3.21 clang-1700.6.3.2)
./hello: Mach-O 64-bit executable arm64
built and ran on arm64
the registration is not in the job's environment
```

So both halves are now proven on the code that ships, not on the code that happened to be there when the first runs were made. The `HOME=…   resolved=…` line is deliberate: it is the new guard's input and output side by side on the platform whose home directory nothing else in this repository could vouch for.

**IT ALSO FOUND A DEFECT THAT ONLY A PENDING-DELETION FLEET COULD SHOW.** `billet check` refused the fleet as `PENDING_DELETION`, "so it cannot serve builds" — while quoting, in the same sentence, AWS's own status note saying *"Fleets are available to build projects while they are pending deletion."* The diagnostic contradicted itself and refused a deployment that works, which is the ADR-005 failure. It is now a warning that says the true thing: capacity works now and disappears when AWS finishes.

### The same-label fallback, which is the shape the whole feature exists for

**2026-09-01, run `33469948502`, green.** One tier, `providers: [docker, codebuild]`, `vcpu: 2`, and a docker node sized at exactly 2 vCPU so it holds one job. Two concurrent jobs, one unchanged `runs-on: billet-fallback`, neither naming a backend:

```
job 1 ran on HOME (no CODEBUILD_BUILD_ARN)     host: Linux aarch64
job 2 ran on CODEBUILD: …:build/billet-accept-linux:b5f79eb0-…   host: Linux x86_64
```

Home filled first and the overflow went to the cloud, decided at ESCROW by the tier's provider order rather than by which node polled first — and the two jobs even ran on different architectures without the workflow knowing. That is the claim this backend is about, and until this run it had been shown for `ec2` and not for this backend.

**Two things that setup taught, both correct billet behaviour.** A multi-provider tier is REFUSED unless every provider names its own `launch.<provider>.image`, because an image name belongs to the backend that boots it — there is no sensible default across a docker image and an AMI-shaped one. And billet takes a host-wide lock keyed on the deployment identity, so a second node of one deployment cannot start on the same machine: correct, since two billets sharing an identity manage the same containers and heartbeat each other's leases. In production these two nodes are two machines; this run simulated the second host by giving that process its own lock namespace, which is worth stating plainly because it is the one safety check the setup went around rather than satisfied.

### Every guard, executed on the builder's own OS

The buildspec suite runs the generated script under `/bin/sh` on whatever machine `go test` was invoked from. That is the right instrument for the script's shape and it cannot speak for the image AWS boots — and two of this file's defects were platform splits (`command -v` resolving differently under a privilege drop; `pwd -P` answering `/` under dash and `//` under macOS `/bin/sh`). So the real generated script was run on `amazonlinux:2023`, **with no `set -e`**, which is the condition the gates must hold under because CodeBuild runs the commands rather than a script billet controls.

| case | result |
|---|---|
| tarball not matching the pinned checksum | exit 1, mismatch reported, **nothing installed** |
| `HOME` unset | refused, naming HOME |
| `HOME` relative but naming an existing directory | refused as relative, nothing written under it |
| `HOME=/` and `HOME=//` | both refused as the root |
| `.billet` a symlink | refused, redirect target intact |
| correctly checksummed tarball | **exit 0, runner installed** |
| a file left by a previous build | gone from the directory the runner runs out of |

The last two matter as much as the refusals: a gate that rejects correct input is the failure ADR-005 names, and a reserved instance is measurably not scrubbed between builds. `TestDumpBuildspec` carries the command that reproduces this.

### The live provider test, re-run after the credential chain moved

**2026-09-02**, as `billet-accept-node-role` carrying exactly the policy `billet init iam --account …` emits from this branch, after the AWS credential chain moved into `internal/awscreds` and touched this provider's `api.go`. All four cases green — the whole lifecycle (`3349a76c-…`), the registration never echoed, a destroy that proves the build over, an idempotent second destroy — in 16.8 seconds. The grant billet tells operators to attach is still sufficient for every call the provider makes.

### The killed-holder scenario, re-run on real CodeBuild

**2026-09-03, project `billet-417-linux` (`LINUX_CONTAINER`, `BUILD_GENERAL1_SMALL`, on-demand, us-west-2), two real GitHub Actions jobs from a private consumer repository, real `billet server` and `billet node` processes on this laptop, the node killed with SIGKILL while each job ran.** The e2e reproduction in `internal/e2e/holdergone_test.go` runs against a fake AWS; this is the same scenario against the service, with the operator's view captured at each step. The job holds its runner for 150 seconds so there is time to kill the node underneath it. The binary was the tree of the commit that closes this round, built before its last two edits (a pooled runner's job start being recorded on the pool edge, and two lint fixes in tests), neither of which the scenario touches; the env-gated e2e test below ran on the final tree.

**Scenario A: the acceptance's shape — the node dies and stays dead until the job has finished.** Timestamps are UTC.

```
01:20:50  workflow_dispatch; billet escrows, the node launches build c0ef415f-… at 01:21:20
01:22:11  the runner registers and GitHub starts the job on it (lease d1177611…, process 237080c7…)
01:22:36  kill -9 the node
01:24:20  GitHub reports the job complete (succeeded); the build itself SUCCEEDED at 01:24:21
          the plane still lists the host, so the bound destroy is queued against it
01:26:16  the plane forgets the host (four 50-second poll windows of silence) and answers the queued
          destroy "went silent before taking this command"; the listener's retry lands at
01:26:49  "a finished job's compute is bound to a node process this control plane cannot reach; its
          lease is no longer renewed here and stays charged until whoever holds the compute renews it
          or its host proves the compute gone"
          billet status: capacity 2 of 32 vCPU, 1 open lease; held none;
                         cb-417 SAYS IT IS RUNNING 1 (this deployment cannot reach it)
01:28:35  the reaper quarantines the lease, one TTL (90s) after its last renewal at 01:26:42
          billet leases: d1177611…  quarantine  HOLDER process 237080c72b9c (host unreachable)
01:28:50  a replacement node starts (process 4b692017…) and registers with an empty inventory
          billet leases: d1177611…  quarantine  HOLDER process 237080c72b9c, REPLACED by 4b692017be41 <1m ago
01:29:42  the completion's retry, and again at 01:32:12: holder unavailable, nothing settled
01:33:42  the replacement's sweep, five minutes after the expiry: "freed capacity held for compute
          this host is no longer running leases=1"
          job_history: conclusion=done  result=succeeded  failure_reason=''  disruption=''
          billet status: held none; the tier advertises again
```

Three things this run says that the fake could not. **The order of the two exits from the loop is measured**: the plane forgot the dead host before the listener's first retry, so the destroy that had been queued against it was answered as never taken, and it was the RETRY that met the unreachable holder and parked — a control plane that reaches the unreachable state through a queued command and a staleness window rather than at once. **The quarantine arrived one TTL after the park**, on the reaper's clock and nothing else's. **And the settlement was `done` in one step**: the completion's result was in `job_history` before the destroy was ever dispatched, so the replacement's inventory settled the lease as a finished job rather than as a provisional failure for a later retry to correct. The retry still ran, found the lease terminal, and cleared its obligation.

**Scenario B: the node dies and is restarted while the job is still running.**

```
01:34:08  workflow_dispatch; build a8c36525-… launched at 01:34:12; job started 01:35:04 (lease 73381d22…)
01:35:45  kill -9 the node (process 4b692017…)
01:35:55  a replacement (process c186dba9…) registers: "adopted a running job from an earlier run;
          it will be left to finish and its capacity stays held" found=1 adopted=1
          billet leases: 73381d22…  custody  HOLDER process c186dba99607        (not REPLACED)
01:37:11  GitHub reports the job complete (succeeded)
01:37:12  the bound destroy answers holder unavailable — the plane's record still names the dead
          process — and the listener parks the obligation; in the same second the replacement's
          custody is told the job it adopted has finished, finds the build over, stops it
          ("released compute that was being held held_for=1m17s") and releases the lease
01:37:13  job_history: conclusion=done  result=succeeded  failure_reason=''  disruption=''
          billet status: held none, 15 available; billet leases: nothing held
```

So the replacement's own custody is what settles a build it can see, and it does so the moment the job ends; the parked completion finds the lease terminal on its next attempt and has nothing to correct. The `custody` holder is the LIVE process, which is why `billet leases` does not call it replaced.

**The env-gated e2e reproduction ran against the same project on the final tree**: `BILLET_TEST_CODEBUILD_PROJECT=billet-417-linux go test ./internal/e2e -run OnRealCodeBuild` — the real listener, plane, wire, node loop and provider over real CodeBuild and Parameter Store, with GitHub faked. It kills the node process, waits for the real build to end on its own — the runner refuses the fake GitHub's registration and exits ZERO (`Runner listener exit with terminated error, stop the service, no retry needed`), so CodeBuild reports the build SUCCEEDED eighteen seconds after StartBuild, the same shape as its root guard — registers a replacement, delivers the completion, and asserts the quarantine, the settlement from the replacement's real inventory after the grace, and the correction to GitHub's outcome.

**What this run does NOT cover.** The macOS fleet was not re-provisioned — a reserved `MAC_ARM` fleet is a 24-hour, $28.80 commitment, and nothing in the defect or the fix is specific to a backend. A draining superseded process (the old process alive and holding the build while a second registers under its name) is covered end to end against the fake AWS only (`TestASupersededProcessDrainsTheJobItHoldsWhileTheCompletionWaits`). A control-plane restart between the launch and the completion, which is what makes the plane's not-owned branch reachable, is covered by unit tests.

Everything created for this run was deleted afterwards: the project, its build role and log group, the staged registrations, the workflow and the runner group.

### DeleteProject does not stop a running build, and `terraform destroy` had to learn that

**2026-09-02, project `billet-measure-delete`, build `70bb5fa3-…`.** The fleet module's README had said a destroy attempted while builds are active "fails on the project — AWS refuses to delete a project with builds in progress". Nothing had measured it, and AWS's `DeleteProject` reference says only that a project's builds are not deleted with it. Asked directly, with a `BUILD_GENERAL1_SMALL` build whose buildspec slept 150 seconds:

```
01:37:14Z  phase=BUILD                          DeleteProject: SUCCEEDED (exit 0)
01:37:14Z  ListBuildsForProject on the name     ResourceNotFoundException: the provided project … does not exist
01:37:15Z  BatchGetBuilds on the build id       IN_PROGRESS  BUILD  billet-measure-delete
01:39:53Z  BatchGetBuilds on the build id       SUCCEEDED    COMPLETED
           phases: PROVISIONING 4s, BUILD 150s, every phase SUCCEEDED
```

So `DeleteProject` succeeds while a build is in its `BUILD` phase, the build runs to completion untouched, the listing by project name is gone at once, and the build stays reachable by id. A `terraform destroy` would therefore have removed the node role, the build role and the log group under a live job and reported a clean apply — and the "AWS refuses" sentence was the module's whole guard. It now carries its own: `terraform_data.active_build_guard` depends on everything a build needs and runs `scripts/refuse-active-builds.sh` at destroy time, which walks the project's builds through the operator's `aws` CLI and refuses while any is not terminal.

**The guard, proved against the real module in the CI account, same day.** The module was applied from the close-out branch as `billet-guard-test` (on-demand Linux, no KMS, 8 resources), a build sleeping 240 seconds was started, and `terraform destroy -auto-approve` was run twice:

```
01:52:21Z  build 0abe9bc9-… phase=BUILD; terraform destroy
           refuse-active-builds: project billet-guard-test still has a build that is not terminal: billet-guard-test:0abe9bc9-…(IN_PROGRESS)
           refuse-active-builds: drain first with `billet drain --wait` on the control plane, or set BILLET_SKIP_ACTIVE_BUILD_GUARD=1 …
           Error: local-exec provisioner error                    destroy exit=1
           still there: project, billet-guard-test-cb-node, billet-guard-test-cb-build, /aws/codebuild/billet-guard-test, build IN_PROGRESS
01:52:32Z  StopBuild -> STOPPED; terraform destroy
           refuse-active-builds: no build in project billet-guard-test started since 2026-08-31T04:52:37 UTC is still running (1 page(s) examined); destroy may proceed
           Destroy complete! Resources: 7 destroyed.               destroy exit=0
           remains: project not found, both roles NoSuchEntity, no log group
```

Two things the fake-CLI tests could not have found. **The CLI renders `startTime` in the machine's local zone** — `2026-09-01T18:37:02.895000-07:00` on this laptop — so the script's lexical comparison against a UTC cutoff was seven hours wrong until it pinned `TZ=UTC`, under which the same field reads `2026-09-02T01:37:02.895000+00:00`; a UTC server would have hidden that. And Terraform runs the provisioner from a COPY of the module under `.terraform/modules/`, as `/bin/sh -c <path>`, so the script's executable bit has to be committed or every operator meets "permission denied" at the one moment the guard matters.

The same script was then run in `ubuntu:24.04` (`/bin/sh` is dash, `date` is GNU coreutils 9.4) against a fake CLI, because the Go test had only exercised this Mac's BSD `date -r` and its `sh`: a running build refused with exit 1, a drained project passed, and a page entirely older than the window ended the walk without a second listing. Both `date` spellings are live, and neither machine is evidence about the other.

### A burst of StartBuild calls, and the third external ceiling nobody had named

**2026-09-02, project `billet-accept-linux`.** To give the inventory walk a busy history, a thousand `echo ok` builds (`BUILD_GENERAL1_SMALL`, 5-minute build and queued ceilings) were started 25 at a time from the AWS CLI. Of the first 558 attempts, 90 started. The rest:

```
220  AccountLimitExceededException: Cannot have more than 30 builds in queue for the account
 13  the same, after the CLI's own two retries
  1  ThrottlingException: Rate exceeded: StartBuild
234  the CLI exited non-zero and printed nothing (unexplained; 25 parallel invocations of an SSO profile)
```

**The account queues at most 30 builds, across every project it owns**, and Service Quotas lists no such quota (its CodeBuild list holds "Build projects" and the per-shape "Concurrently running builds" entries and nothing about a queue). billet's own launch path reads that refusal as a conclusive failure — it tries the next declared compute type and then fails the lease — and GitHub requeues the job up to three times, so a burst beyond *concurrency quota + 30* is failed jobs rather than slow ones. It is now `config.CodeBuildAccountQueuedBuilds`, printed by `billet check` beside the other two ceilings, and the operator guidance is to size `node.max_vcpu` so the node cannot escrow past it. Restarted eight at a time with a retry on that refusal, the same project accepted builds at roughly twenty a minute — the drain rate of a 30-wide queue against the Linux/Small concurrency quota of 30, with each build's own provisioning in front of it.

### The inventory walk against a busy project, measured

**2026-09-02, `billet-accept-linux` with 1,054 builds of history, all started inside the previous forty minutes** (the 90 that got through the burst above, 900 more started eight at a time with a retry on the queue limit over 651 seconds, and the live test's own). `Provider.List` was timed from `TestARealInventoryWalkOverABusyProject` (`internal/provider/codebuild/realwalk_test.go`, env-gated like the other live tests) through a counting transport, as the node role:

| window | ListBuildsForProject | BatchGetBuilds | wall time | throttled |
|---|---|---|---|---|
| 70 minutes (5-minute ceilings) | 11 | 11 | 6.5s | 0 |
| 53 hours (service maxima) | 11 | 11 | 6.2s | 0 |
| the same project at 323 builds, earlier | 5 | 5 | 2.9s | 0 |

So the arithmetic in ADR-007 holds: two requests per hundred builds inside the window, about 0.3s each, and nothing for history outside it — the two windows cost the same here only because every build was younger than the tighter one. A sweep over a thousand recent builds is six seconds, which is the number an operator running a busy fleet should expect per reconciliation pass, and the reason a tighter `build_timeout_minutes` is cheaper: it is the window, not the year of history, that is walked. Not re-measured after the builds aged past the 70-minute window, so "the walk stops at page one once the history is old" rests on the fake-API tests rather than on this project.

### A macOS fleet from cold: ACTIVE in nineteen seconds, and no Mac behind it

**2026-09-02, fleet `billet-accept-mac3` (`MAC_ARM`, `BUILD_GENERAL1_MEDIUM`, base capacity 1, us-west-2), created to measure cold provisioning and re-run the Xcode job.** The previous fleet had expired the night before (`DeleteFleet` 2026-09-01 00:11 UTC; gone by 01:30 UTC the next day, its ARN answering `fleetsNotFound`), so the `mac2-m2.metal` slot was free.

```
01:37:01Z  CreateFleet
01:37:20Z  status.statusCode = ACTIVE                       (19 seconds)
01:45:04Z  workflow_dispatch; billet launched build 9bafc694-… at 01:45:12
01:50:08Z  GitHub: completed job, result=canceled          (the assignment was never acquired)
01:50:09Z  billet: StopBuild 9bafc694 (QUEUED for 297s); GitHub requeued; build 6218bfe3-… started 01:50:13
01:55:10Z  the same again; build e7eda1a5-… started 01:55:13
02:04:00Z  status.context = INSUFFICIENT_CAPACITY  "We currently do not have sufficient capacity for the instance type you requested. Please try again later."
02:15:00Z  build e7eda1a5: buildStatus FAILED, phases[QUEUED].phaseStatus TIMED_OUT after 1178s
```

Four things, in order of how much they matter. **`ACTIVE` is the fleet OBJECT, not a machine**: the fleet was `ACTIVE` before any instance existed and stayed `ACTIVE` while AWS reported it had none to give, so a check that reads `statusCode` alone — which is what `billet check` did — prints "capacity 1" about a fleet that will fail every job. It now reads `status.context` too and warns. **GitHub's pickup deadline is shorter than a cold Mac**: each assignment was cancelled as unacquired after about five minutes, GitHub requeued the job, and billet did exactly the right thing each time — stopped the build for the job GitHub had withdrawn and started one for the requeued job — three builds for one job, none of them wrong. **The queued ceiling did not fire at fifteen minutes**: the build's `queuedTimeoutInMinutes` was 15 and its `QUEUED` phase lasted 1178 seconds before `TIMED_OUT`, so the ceiling is a floor on how long a queued build lives rather than the moment it dies, and a queued-timeout failure reports `buildStatus: FAILED` with the timeout in the phase — the same shape as a build timeout, which is why billet reads phases. **And whether the 24-hour minimum bills a fleet that never provisioned is not something this session could see**: Cost Explorer showed the previous fleet's charges as zero a day later too, so the ledger lags by more than a day and the $28.80 claim rests on AWS's pricing and its own deletion message rather than on a bill.

The control plane and node were restarted after the third build failed, the node freed the failed build's capacity on registration, GitHub reassigned the job, and a fourth build was launched at 02:23 UTC into the same starved fleet and failed the same way. After 66 minutes of `INSUFFICIENT_CAPACITY` the macOS half was moved to **us-east-1** — same control plane, same tier, same GitHub side; only the node's project, fleet, parameter path and log group are regional — and two things followed from that:

```
02:43:35Z  DeleteFleet on the starved us-west-2 fleet; gone within a minute (never held an instance, so no 24-hour hold)
02:44:13Z  CreateFleet in us-east-1; ACTIVE at +22s, and no status context this time
02:45:5xZ  the node's fleet change REFUSED: "1 lease(s) are still outstanding against" the old fleet
02:48:47Z  that lease (its build stopped and its project deleted) expired into quarantine; `billet leases release --force`
02:49:53Z  build 940ecadf-… launched; QUEUED 206s while the Mac provisioned; PROVISIONING 1s
02:53:44Z  run 33580611595 GREEN; billet stopped the build only after GitHub reported the job succeeded
```

**The reserved-fleet claim rule refused the move while a lease was outstanding**, exactly as ADR-007 says it should: a different fleet on the same node is a new pool of shared capacity, and releasing the old claim with a build still charged against it is the overcommit the rule exists to prevent. The lease's compute was provably gone (its build stopped, its project deleted), so the operator escape was `billet leases release --force` once the reaper had moved it to quarantine — the ordinary way through, and it took the control plane restart to get there because a running listener keeps renewing a lease its node can no longer settle. And **a fleet that never received an instance is deleted at once**: the 24-hour hold is on instances that ran, not on the fleet object.

From inside the green job, on a Mac billet neither owns nor administers, in a region the fleet was created in five minutes earlier:

```
os:   macOS 26.2   arch: arm64   user: cbuser
CODEBUILD_BUILD_ARN: arn:aws:codebuild:us-east-1:810711872940:build/billet-accept-macos3:940ecadf-…
HOME=/Users/cbuser   resolved=/Users/cbuser
the runner is under the resolved home
Xcode 26.2 · Apple Swift version 6.2.3
./hello: Mach-O 64-bit executable arm64 · built and ran on arm64
```

So cold provisioning, measured end to end in a region that had capacity: fleet `ACTIVE` in 22 seconds, the first build queued for 206 seconds while the Mac came up, and the job green three minutes fifty seconds after `StartBuild`. Whether a given region has a `mac2-m2.metal` to give is not something billet can know before asking; `billet check` now warns when the fleet says it does not.

### A macOS fleet at capacity: two jobs through one label, one Mac, and what the second one costs

**2026-09-02, project `billet-419-macos` in us-east-1, on the `billet-accept-mac3` fleet from the section above** — `MAC_ARM`, `BUILD_GENERAL1_MEDIUM`, base capacity 1, by then `PENDING_DELETION` and still serving builds, which is what made this measurable without a second $28.80 fleet. The open question is what a reserved macOS fleet does under CONCURRENT load, and the account cannot answer the whole of it: **a second `mac2-m2.metal` is still refused.**

```
06:25:55Z  CreateFleet base_capacity=2 (us-east-1)   AccountLimitExceededException: The number of instances of type mac2-m2.metal exceeded limit 1
06:25:57Z  UpdateFleet base_capacity=2 on the pending fleet   InvalidInputException: Fleet status is PENDING_DELETION. Cannot be updated.
06:25:58Z  CreateFleet base_capacity=2 (us-west-2)   the same AccountLimitExceededException
06:29:39Z  CreateFleet MAC_ARM overflow_behavior=ON_DEMAND   InvalidInputException: Fleet on-demand overflow behavior is not supported for MAC_ARM
```

The last line is the one that decides the rule below, and it was learned for free: CodeBuild validates the request BEFORE it counts instances, so an input error comes back on an account that could not create the fleet anyway. **A macOS fleet has no overflow to on-demand.** `QUEUE` is the only behaviour, so every build past the fleet's capacity waits for a Mac inside it, and nothing on the AWS side can absorb a burst.

**What one Mac does with two jobs.** A tier with `macos_vm_limit: 2` and `max_concurrent: 2` — deliberately one more than the fleet — took two concurrent Xcode jobs from one `workflow_dispatch` (run `33599278785`, a matrix of two slots, each holding the Mac busy for six minutes before compiling and running a Swift binary), after one job had warmed the fleet (run `33599064928`: `StartBuild` to job step in **eleven seconds**, because the reserved instance was already alive).

```
06:32:13Z  dispatch; both jobs queued at GitHub
06:32:22Z  billet: StartBuild b9147042 for job a          06:32:34Z  job a running on the Mac (12s)
06:32:35Z  billet: StartBuild 383ea7f4 for job b          QUEUED — the fleet's one Mac is busy
06:37:33Z  GitHub: job b's assignment cancelled as unacquired (298s); billet StopBuild 383ea7f4
06:37:40Z  billet: StartBuild ca4cf8bc for the requeued job b   QUEUED again
06:38:39Z  job a finished (green); 06:38:41Z billet StopBuild b9147042
06:39:25Z  job b running on the Mac — ca4cf8bc QUEUED 94s, the Mac reused 44s after job a's build stopped
06:45:30Z  job b finished (green); both jobs report hostName mac-mini.local — the same instance
```

Four things. **billet did exactly what the config declared**: it escrowed both jobs and started two builds, because the declared cap said the node could hold two. **The fleet cannot**, so the second build sat in `QUEUED` for the full length of GitHub's pickup window, GitHub withdrew the assignment at 298 seconds, billet stopped that build and started another for the requeued job — the same three-builds-for-one-job shape the cold-fleet section records, produced here by a busy Mac rather than a missing one. **It did finish**: the requeued build was queued only 94 seconds because job a ended inside its window, and job b was green 7m08s after dispatch against a 6m30s job — so with jobs shorter than about five minutes the excess is a delay, and with longer ones it is a delay plus a `StopBuild`/`StartBuild` per five minutes of waiting, each of the latter a slot in the account's 30-wide build queue. **And the Mac is reused warm between builds**, which is the reserved-fleet property the isolation section measures from the other side: 44 seconds from one build's stop to the next build's start, no provisioning.

So `billet check` now REFUSES a declared `macos_vm_limit` above the fleet's base capacity (`FleetReport.ConcurrencyProblems`), naming both numbers and the field, and downgrades it to a warning only when the fleet carries a scaling configuration whose maximum covers the number — a fleet that may scale is a fleet that queues while it does. It is a refusal rather than a warning because the excess is capacity advertised to GitHub that no Mac exists to run, MAC_ARM offers nothing to absorb it, and the fix is one number. The same session found two other things the check printed about this fleet that were false, both fixed: a fleet pending deletion reports `status.context = PENDING_DELETION`, and the capacity warning keyed on "any context at all" said "until an instance is provisioned every build queues" about a Mac that had just run a job; and the node-policy line said `macOS n/a (codebuild cannot run macOS guests)` beside the very node whose fleet ran them, because it asked `!= tart` rather than config's own allowlist — and, once that was fixed, printed `max 2 macOS (Apple default)` for a codebuild policy that declared no limit, attributing to Apple a number that governs no fleet AWS operates; it now says the limit is undeclared and what would require it. The judgement itself applies only to a `MAC_ARM` node with a policy that DECLARES the limit: a Linux fleet beside a legally declared but unused macOS limit, a node-only config, and a policy leaving the limit unset are all correct deployments, and a review round found the first version refusing the first of them. The same round found the opposite defect in config: a server-only file (no node block) with `nodes: [{name: cb}]` under a tier `provider: codebuild, node: cb` resolved to no provider at all, so an unset limit inherited Apple's two — advertising two jobs against a one-Mac fleet, the exact failure above. A macOS tier pinned to a node can only be placed on a node running one of its providers, so config now reads the provider from the pinned tiers when nothing else says, requires the limit when they are all remote, and asks for `nodes[].provider` when they disagree.

**Cost of this measurement: no new fleet.** Four builds ran on reserved capacity the account was already holding for its 24 hours; nothing here started an instance.

### The control plane's sweep of leaked registrations: three facts about Parameter Store, measured

Asked of real Parameter Store and IAM in `810711872940`/us-west-2 on **2026-09-02**, for the control plane's registration sweep, with throwaway SecureStrings under `/billet/measure418/jit` (removed afterwards; the path lists empty). Each one decided how the sweep is written.

**A listing without decryption never carries a registration.** `GetParametersByPath --no-with-decryption` over two SecureStrings returned each `Value` as 248 and 252 bytes beginning `AQICAHi` — the KMS ciphertext header — and neither equal to the plaintext that had been staged; the same call `--with-decryption` returned the plaintext. So a controller that asks for no decryption receives nothing it could use, and billet's sweep additionally decodes the response into a type with no `Value` field, so the ciphertext never lives past the response buffer.

**A pagination cursor is positional, and deleting while paging skips entries.** Three parameters, `MaxResults 1`: page one returned `billet-cafebabe` with a 720-character opaque token. `billet-cafebabe` was then deleted and page two fetched with that token — it returned `billet-x3`, and `billet-deadbeef`, which still existed (confirmed by a fresh full listing), was never returned. A sweep that deleted as it paged would therefore leave every parameter behind the one it removed unswept until a later pass, and report a clean pass in the meantime. billet collects the whole listing before it deletes anything.

**`GetParametersByPath` is authorised against the path itself, not its children — simulated AND run.** `iam:SimulateCustomPolicy` of the rendered controller grant: with resources `parameter/billet/measure418/jit` AND `parameter/billet/measure418/jit/*`, `ssm:GetParametersByPath` and `ssm:DeleteParameter` are **allowed** against both the path ARN and a child ARN while `ssm:GetParameter` and `ssm:PutParameter` are implicitly denied; with the children-only resource, `GetParametersByPath` against the path ARN is **implicitly denied**. Then the service half, under a throwaway role (`billet-measure418-controller`, trusting only the account, deleted afterwards) holding exactly the rendered grant as its one inline policy and assumed through a CLI profile: the listing **succeeded** and returned both names as ciphertext; `GetParameter` on a child was refused `AccessDeniedException … not authorized to perform: ssm:GetParameter`; `PutParameter` under the path was refused the same way; `DeleteParameter` on a child **succeeded** and the next listing no longer named it. The inline policy was then swapped for the children-only rendering, and the same listing was refused — `not authorized to perform: ssm:GetParametersByPath on resource: arn:aws:ssm:us-west-2:810711872940:parameter/billet/measure418/jit` — the service naming the PATH as the resource it authorised against. That is why the generator renders both resources, and why a hand-written grant naming only `<path>/*` looks scoped and lists nothing.

**AND THE ABSENCE OF A KMS GRANT IS NOT WHAT KEEPS THE CONTROLLER FROM READING A REGISTRATION.** Under that same role — `GetParametersByPath` and `DeleteParameter` on the path, no `kms:*` anywhere — the listing with `--with-decryption` returned both values as **plaintext**. The parameters were encrypted under the account's `aws/ssm` key, whose own key policy authorises any principal in the account that reaches it through Parameter Store, so the identity policy's silence about KMS bounded nothing. What keeps a registration out of the controller is billet's request stating `WithDecryption=false` and decoding no `Value` field, which the sweep's tests pin.

**A customer-managed key makes the grant decisive again, measured.** A fresh KMS key was created, a SecureString staged under it, and the same sweep-only role asked again: the listing without decryption returned the ciphertext as before; the listing with decryption was refused `AccessDeniedException … not authorized to perform: kms:Decrypt on resource: arn:aws:kms:us-west-2:…:key/9919f777-…`; `DeleteParameter` on that parameter **succeeded**, so the sweep needs no key to do its job. Then a second parameter under the account's default key was staged beside it and the decrypting listing was asked once more: it failed **whole**, with the same `kms:Decrypt` refusal, rather than returning the default-key value and omitting the other — so on a page that holds one customer-key parameter nothing decrypts, while a page holding only default-key parameters returns plaintext (the earlier measurement). The key's boundary therefore covers the parameters under it and nothing beside them, which is why `enable_kms` in the module encrypts the whole path under one key. The key was scheduled for deletion (seven days, the minimum), the role, its policy and every parameter were removed, and the path lists empty.

### An on-demand Linux fleet destroys its machine between builds — the on-demand measurement

**2026-09-02, project `billet-402-measure` (`NO_SOURCE`, `LINUX_CONTAINER`, `BUILD_GENERAL1_SMALL`, `privilegedMode=true`, no fleet — on-demand), and `billet-402-measure-cache` (the same with `cache: type: LOCAL`).** ADR-007 refuses untrusted work on this backend because a *reserved* instance is measurably not scrubbed between builds (`billet-scrub-marker`, above). The same question was asked of *on-demand* compute directly, because that is the property that would make an on-demand Linux build a real per-job boundary — the same argument that lets `ec2` accept untrusted work where `docker` must refuse it — and the rule is that a safety claim is pinned to measured behaviour, not to AWS's sentence that on-demand machines "are destroyed when the build finishes".

Every build ran a buildspec that recorded its host identity (`/proc/sys/kernel/random/boot_id`, `/proc/uptime`, `/etc/machine-id`), checked for every marker earlier builds had left (in `/tmp`, `$HOME`, under `/`, in a custom cache dir, as a Docker image, as a Docker volume, and — because `privilegedMode` gives the container the host's kernel — on the **host root disk**, which the build mounted read-write), and then left its own markers in all of those places before finishing.

The result, both directions, is that on-demand Linux gives a real per-job machine:

- **Three serial builds, three distinct `boot_id`s**, each host with an uptime of 3–5 minutes. No marker of any kind survived from one build to the next — not the container filesystem, not `$HOME`, not the host root disk (a build wrote `/hostroot/tmp/billet-402-<id>`; the next build found `none on host disk`).
- **Under load: 28 builds across two back-to-back waves of 14, 28 distinct `boot_id`s, zero shared hosts.** Wave 1 left a host-disk marker on all 14 of its hosts; wave 2 — 14 more builds fired immediately after — found none of them (`comm -12` of the two waves' host boot-ids was empty). Every one of the 28 reported `none in /tmp`, `none in HOME`, `none under /`, `none on host disk`, `no docker image`, `no docker volume`.
- **The `machine_id` is identical across every build** (`f7a4cd86…` in the container, `ec2ea0aa…` on the host disk) — the shared build AMI, *not* a shared host. `boot_id`, which changes on every kernel boot, is the host identity, and it was distinct for all 31 builds.
- **The one thing that did persist is the opt-in `cache: type: LOCAL`.** On the cache-enabled project a marker written under the `LOCAL_CUSTOM_CACHE` path came back on a later build (`SURVIVED cachedir`). That is documented, per-project cross-build persistence, controlled by the project's own configuration — billet's project has no cache, and never enables one.

So an on-demand Linux build is a per-job boundary the way an `ec2` instance is: a freshly-booted host, destroyed with the build, that a later build never inherits — the network is then the only remaining question, exactly as it is for `ec2`. IMDS was unreachable from the build container (`imds_instance=unreachable`), so a build could not read an instance role even if one existed. This is what lifts the outright refusal: `node.codebuild.untrusted_vpc_id`/`untrusted_subnets`/`untrusted_security_group_ids` name the isolated network, their absence stays the refusal, and untrusted work is admitted **only** for an on-demand container environment — never a `fleet_arn` (a `fleetOverride` discards the project's VPC and reserved capacity is shared between builds) and never `MAC_ARM` (reserved-only). Everything created for this measurement was deleted afterwards.
### Three macOS jobs at once, from a Mac you own and one AWS operates

**2026-09-03, one label, two backends.** The account still allows one `mac2-m2.metal` — asked again with the slot free this time, `CreateFleet` with `base_capacity: 2` answered `AccountLimitExceededException: The number of instances of type mac2-m2.metal exceeded limit 1` at 04:01Z — so macOS concurrency past one managed Mac has to come from somewhere else, and billet's answer is the Mac an operator already owns. A tier `providers: [tart, codebuild]`, `guest_os: macos`, with `nodes[]` declaring `mac-1` (tart, an M2 Max with 32GB, Apple's two guests) and `cb-mac` (codebuild, `macos_vm_limit: 1`, a `MAC_ARM` `BUILD_GENERAL1_MEDIUM` fleet of one in us-east-1), advertised three; a workflow dispatched three jobs through that one `runs-on` (`macos-fallback.yml`, each holding seven minutes after reporting where it landed). Run `33719311996`:

```
05:32:55Z  dispatched
05:33:45Z  mac-1: launched VM billet-e4ab7048…   05:33:53Z  slot 2 running on Manageds-Virtual-Machine.local (macOS 26.4, arm64)
05:34:56Z  mac-1: launched VM billet-4c42ba32…   05:35:02Z  slot 1 running on Manageds-Virtual-Machine.local
05:35:18Z  cb-mac: started build 7ada7c9a…        05:35:30Z  slot 3 running on ip-10-0-58-46.ec2.internal (macOS 26.2, arm64, CODEBUILD_BUILD_ARN set)
05:35:30Z – 05:40:53Z  all three running at once
05:42:38Z  run green, every slot succeeded
```

**What it took to be able to write that tier.** A macOS tier had to pin one node, on the reasoning that Apple's per-host limit could not be enforced without knowing the host — but placement counts macOS guests per host from the node's own policy whether or not the tier named it, so the pin was the load-time guard's convenience, not the enforcement point, and it made the one shape this measurement needs impossible to express. A macOS tier may now leave its node unnamed only when it lists several providers, and the guard then holds each backend's declared `nodes[]` policy to the pinned tier's rules: the fleet's node must declare its capacity (Apple's default is not a fleet's), every listed backend needs a declared host that permits macOS, and `max_concurrent` defaults to — and is bounded by — what those hosts permit between them (here 2 + 1). A reservation stays pinned. `billet check` prints the two policies side by side: `max 2 macOS (Apple default)` for the Mac and `max 1 macOS` for the fleet's node.

**Three things about how the three arrive, from this and the two runs before it.** The owned Mac launches its guests SERIALLY — a node executes one command at a time — and a macOS guest takes about 50–70 seconds from launch to the job's first step on this M2 Max (2m05s the first time after the 87GB image was pulled), so the second guest is up roughly two minutes after dispatch; a job shorter than that never sees three at once. Which backend takes the first job is not "home first" when both have room: in run `33716070359` the CodeBuild build started seven seconds after dispatch and took the first job at 49 seconds while the first guest was still booting, in run `33719311996` the two guests were up before the build was started — provider order decides placement at escrow when capacity is scarce, and with three slots free for three jobs nothing is scarce. And when the codebuild node's AWS credentials expired between runs (run `33717534803`: `ExpiredTokenException` at staging, so `could not launch the compute for a job`), the label did not fail — all three jobs ran on the owned Mac, one after the other once its two guests freed, and the plane declined the third assignment with `assigned a request with no escrow to back it` until a slot came back. The fleet was starved for the first 22 minutes of its life (`INSUFFICIENT_CAPACITY` from 04:13Z to 04:35Z; a second fleet in us-west-2 was starved for the same interval and deleted at once, at no cost, having never held an instance), and the run above was made once it had its Mac.

**Cost.** One reserved fleet for its 24-hour minimum ($28.80), created 04:13Z and put to `PENDING_DELETION` at 05:44Z; the Mac somebody owns, nothing.

### What the CodeBuild measurements do NOT cover

- **The inventory walk's cost is measured only for builds INSIDE the window.** A thousand recent builds cost 22 requests and 6.5 seconds; that the walk stops at page one once they age out of the window rests on the fake-API tests, not on this project.
- **A macOS fleet with MORE THAN ONE Mac is unmeasured, and is not going to be in this account.** `CreateFleet` with `base_capacity: 2` is refused at the account's limit of one `mac2-m2.metal` (re-asked 2026-09-03 with the slot free). Whether two Macs in one fleet run two builds at once, and how a second provisions beside a warm one, is not something billet decides to find out: macOS concurrency past one managed Mac comes from an owned Mac in the same tier (the section above), and a deployment that wants two managed Macs raises the limit with AWS. The cold path is measured once per region it was tried in; whether a region has a Mac to give is not knowable in advance, and both regions tried on 2026-09-03 were starved for the first twenty minutes.
- **An on-demand Linux build is now measured as a per-job machine** (the on-demand section above): 31 builds, 31 distinct host `boot_id`s, no marker surviving between builds even across the host root disk. What is still NOT measured is an on-demand build's egress reachability — the isolated-VPC half rests on the same `ec2` argument (a per-job machine plus a security group that reaches only what a stranger should) rather than on a second network measurement, and the untrusted acceptance job below is where that is exercised.

- **The 100-build `sortOrder` error was not exercised.** AWS documents `ListBuildsForProject` as erroring when `sortOrder` is passed and the project has more than 100 builds; this project had three, and it accepted the parameter. billet never sends it, so the documented behaviour is a reason for an absence rather than something to verify.

## What this does not prove

- The shape allowlist and resource ceilings are the enforceable cost policy. `billet check` reports a node's conservative compute-only peak, `billet status` aggregates registered EC2 nodes under the deployment ceiling, and AWS Budgets is the account-wide backstop rather than a stale copied price becoming a runtime admission gate.
- EBS/S3 cache reuse is measured in [`site-acceptance.md`](site-acceptance.md) (2026-09-03): a Docker image-store generation published from one instance came up warm on another, after the two defects that run found were fixed. Failure recovery of that store — a settlement interrupted mid-snapshot, a lost pointer write — remains unmeasured on real AWS.
