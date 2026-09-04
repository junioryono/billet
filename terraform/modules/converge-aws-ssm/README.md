# converge-aws-ssm

Reach Billet hosts from CI through AWS Systems Manager, so a converge needs no inbound port and no SSH key.

**This is optional.** Billet needs no inbound connectivity to run jobs — nodes always dial outbound to the control plane. This module is about *configuration management*: how the machine running `ansible-playbook` reaches the hosts it converges. See [Reaching your hosts to converge them](../../../docs/deploying/reaching-hosts.md) for the alternatives, including the one that needs no infrastructure at all.

## Read this before choosing this route

The `amazon.aws.aws_ssm` connection plugin **requires an S3 bucket**, and its own documentation is explicit that this is not optional: files transit S3 *even for modules that send no files*, because Ansible ships the module's own `.py` through it. The same documentation warns that secrets appearing in a task's arguments are written into those objects **in plaintext**, and recommends a bucket with versioning suspended.

**The Billet host role installs a GitHub App private key.** On this route, that key transits this bucket.

That is why this is a module rather than a checklist. It creates the bucket with **versioning suspended**, server-side encryption on, public access blocked, TLS required, and a short lifecycle expiry. A bucket built by hand with versioning left enabled keeps a copy of that key in version history after Ansible's own delete — invisible in an object listing that looks empty.

If that trade is not one you want to make, [Cloudflare Tunnel](../converge-cloudflare) carries no S3 and no plaintext-secret path.

## Usage

```hcl
module "converge" {
  source = "github.com/junioryono/billet//terraform/modules/converge-aws-ssm?ref=v0.6.0"

  name              = "billet-converge"
  github_repository = "your-org/your-infra-repo"
}
```

`github_repository` must name **one** repository, and naming one is not sufficient on its own. The trust policy matches the **exact** subject `repo:<repository>:ref:refs/heads/<github_branch>` with `StringEquals`, defaulting to `main` — so the converge workflow must run on that branch.

A wildcard such as `repo:owner/repo:*` would admit a pull-request job, whose subject is `repo:owner/repo:pull_request`. This role reads the bucket a GitHub App private key transits, so the subject has to be exact.

**An environment subject is not the fix**, though it looks like one. GitHub emits `repo:owner/repo:environment:NAME` whenever a job *references* an environment — whatever event triggered it — so a pull request declaring `environment: converge` matches it. Referencing an environment that does not exist also creates it, unprotected. A ref subject is the shape a pull-request job cannot mint.

If you want an environment's required reviewers as well, set `github_subject` to the environment form **and** configure that environment's protection rules, knowing it admits PR jobs that name it.

Repositories created after 2026-07-15, or opted into immutable subjects, carry ids (`repo:org@OWNER_ID/repo@REPO_ID:ref:refs/heads/main`) that the name-based default cannot authenticate. Set `github_subject` directly.

## Registering a host

Billet hosts are not EC2 instances, so each registers as a **hybrid activation** — off the path most SSM documentation describes. On the host:

```bash
sudo dnf install -y amazon-ssm-agent   # or the distribution's equivalent
sudo amazon-ssm-agent -register \
  -code "$ACTIVATION_CODE" -id "$ACTIVATION_ID" -region "$AWS_REGION"
sudo systemctl enable --now amazon-ssm-agent
```

The activation code and id are module outputs; the code is `sensitive` and lands in Terraform state, so treat the state file as holding a registration secret. It is single-purpose and expiring — it enrols a machine and grants nothing else.

The host also needs `curl`, which the plugin uses to move files to and from the presigned S3 URLs.

## Inventory

```ini
[billet_hosts]
host-1 ansible_aws_ssm_instance_id=mi-0123456789abcdef0

[billet_hosts:vars]
ansible_connection=amazon.aws.aws_ssm
ansible_aws_ssm_region=us-west-2
ansible_aws_ssm_bucket_name=<transfer_bucket output>
ansible_aws_ssm_bucket_sse_mode=aws:kms
ansible_aws_ssm_bucket_sse_kms_key_id=<transfer_bucket_kms_key output>
```

The bucket encrypts with a customer-managed key, so the two `sse` settings are required: without them the plugin writes with SSE-S3 and the bucket rejects it.

The controller needs the `amazon.aws` collection and AWS's `session-manager-plugin`.

## Outputs

| Output | |
|---|---|
| `activation_id`, `activation_code` | What a host registers with. The code is sensitive. |
| `transfer_bucket` | Set as `ansible_aws_ssm_bucket_name`. |
| `ci_role_arn` | The role a workflow on the configured branch assumes through OIDC. |
| `transfer_bucket_kms_key` | The CMK alias the bucket encrypts with. Set as `ansible_aws_ssm_bucket_sse_kms_key_id`. |

## What this does not do

It does not install or register the SSM agent on your hosts, and it does not write the workflow. It provisions the AWS side and encodes the settings that are dangerous to get wrong.
