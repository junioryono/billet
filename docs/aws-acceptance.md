# AWS acceptance

The EC2 runner path completed three cold, end-to-end GitHub Actions jobs from a private consumer repository on 2026-08-18. This document records the reusable Billet behavior without making the consumer part of Billet's configuration or product surface.

## Environment

- A Billet control plane and EC2 node ran as separate services against an isolated acceptance configuration and state directory.
- The node used an encrypted AMI produced by `billet ami build`; EBS encryption by default was enabled for the account.
- The AMI contained the GitHub Actions runner, Docker, git and the Billet runner bootstrap, but no Go toolchain.
- Each workflow used a dedicated EC2 instance and a JIT runner registration.

## Workflow coverage

The accepted workflow proved all of the conditions from issue #59 in one real GitHub job:

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

## What this does not prove

- Same-label failover from preferred local capacity to EC2 remains issue #32. The EC2 half is proven; the test still has to withdraw the local contribution and observe the same label complete in the cloud.
- The shape allowlist and resource ceilings are the enforceable cost policy. `billet check` reports a node's conservative compute-only peak, `billet status` aggregates registered EC2 nodes under the deployment ceiling, and AWS Budgets is the account-wide backstop rather than a stale copied price becoming a runtime admission gate.
- EBS/S3 cache reuse and failure recovery remain part of the caching-plane issues.
