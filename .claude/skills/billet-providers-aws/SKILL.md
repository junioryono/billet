---
name: billet-providers-aws
description: "Load when touching internal/provider/ec2 or internal/provider/codebuild, any aws* package (awscreds, awssig, awsjson, awspolicy, awsquota, awss3, awsssm, awssts), `billet ami`, `billet init iam`, `billet decommission`, or the IAM the terraform modules render. Every rule here contradicts something the API reference implies, and was measured against a real account."
---

# AWS providers: ec2 and codebuild

## What this area is

`internal/provider/ec2` launches one instance per job in one subnet with `RunInstances`, builds and verifies the AMI (`billet ami build|verify`), reads spot interruptions from a per-node SQS queue and reports quotas. `internal/provider/codebuild` starts one build per job on a dedicated tagged `NO_SOURCE` project, stages the runner registration in Parameter Store, and sweeps registrations a dead node left. Neither uses the AWS SDK: `awssig` signs SigV4, `awsjson` speaks JSON 1.1, `awscreds` resolves env or IMDSv2 credentials, `awspolicy` generates least-privilege IAM from action constants each package declares, `awsquota` reads Service Quotas, `awsssm` reads and writes Parameter Store, `awssts` answers which account a credential is in. An `ec2` or `codebuild` node is an orchestrator, a serial launch queue, with `node.max_vcpu`/`max_memory` as hard budgets. `docs/reference/decisions/adr-002-cloud-compute-backend.md` and `adr-007-codebuild-provider.md`, `docs/deploying/aws-codebuild.md`, and the acceptance record in `docs/reference/records/aws-acceptance.md` hold the measurements.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Shared by both backends: [references/shared.md](references/shared.md)

- **billet signs its own requests, pinned to AWS's own signer.**
- **A 404 from S3 is two different facts, and only `NoSuchKey` is absence.**
- **The credential chain lives in `internal/awscreds`, not in a backend.**
- **A fake proves the shape of a request, never the values a real service refuses.**

### ec2: [references/ec2.md](references/ec2.md)

- **`ClientToken` is durable.**
- **`TerminateInstances` returns when the request is accepted.**
- **The AMI is verified by booting it, and the tag is the promotion.**
- **User data is 16384 bytes, gzipped, and `packUserData` is the gate.**
- **Spot warnings are one queue per node.**
- **The builder's owner tag is the deployment's, and the policy's pattern comes from the same function that stamps it.**
- **`awspolicy` is one generator with three consumers and a measured boundary.**
- **`ec2:CreateVolume` from a snapshot authorises the snapshot as well**
- **`ec2:RunInstances` on `*` authorises every snapshot a block-device mapping names**

### codebuild: [references/codebuild.md](references/codebuild.md)

- **A build cannot be tagged, so ownership is a dedicated project plus markers.**
- **`ListBuildsForProject` has no status filter, keeps a year, returns 100 ids a page and errors if `sortOrder` is passed above 100 builds.**
- **`idempotencyToken` is valid for five minutes, so an ambiguous `StartBuild` is not retried.**
- **Two things made the backend unable to run a single job and died on the first real one.**
- **The registration goes through Parameter Store, and only the name is ever rendered.**
- **A staged registration has four removers, and only the control plane's sweep uses the ledger.**
- **Reserved capacity is not a per-job boundary; on-demand Linux is.**
- **A reserved fleet's claim is released by a proof.**
- **`terraform destroy` refuses under a running build, and it is the module that refuses.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A new AWS call.** Sign it with billet's own signer, take credentials from `internal/awscreds`, and prove the values a real service refuses against a real account: a fake proves only the shape of a request ([references/shared.md](references/shared.md)). Keep the IAM that `awspolicy` generates and the terraform modules render equal (`tfpolicy`).

## Where the tests are

- `internal/provider/ec2/*_test.go` (`sign_test.go`, `payload_test.go`, `userdata_test.go`, `authorize_test.go`, `build_freespace_test.go`, `toolcache_gate_test.go`, `sqs_test.go`, `reals3_test.go`, `live_test.go`).
- `internal/provider/codebuild/*_test.go` (`harness_test.go`, `api_test.go`, `sweep_test.go`, `buildspec_test.go`, `quota_test.go`, `realcodebuild_test.go`, `realwalk_test.go`).
- `internal/awssig`, `awsjson`, `awscreds`, `awspolicy`, `awsssm`, `awsquota`, `awssts` tests; `internal/tfpolicy` (drift), `internal/e2e/ec2_test.go`, `codebuild_test.go`, `holdergone_test.go`, `holdergone_live_test.go`; `internal/ops/images/ami_test.go`, `cmd/billet/initiam_test.go`, `initiamcodebuild_test.go`, `credentialsweep_test.go`; `internal/app/credentialsweep_test.go`.

## Related skills

`billet-capacity` (shapes, budgets, the fleet claim), `billet-security` (what a build can read), `billet-shell-gates` (the provisioning script and buildspec), `billet-storage-and-cache` (ebs-s3), `billet-infra-terraform-ansible` (the modules and IAM renderings).
