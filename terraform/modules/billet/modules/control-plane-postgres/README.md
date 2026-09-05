# control-plane-postgres

One billet control plane whose ledger is somewhere else.

It is [`control-plane-ec2-sqlite`](../control-plane-ec2-sqlite/README.md) with the ledger volume removed, and the removal is the point: with `server.state.backend: postgres` the capacity ledger, node registrations, custody and job history live in a database this module does not operate, so the instance carries no data that has to survive it and can be **rebuilt rather than recovered**. Pair it with [`state-rds-postgres`](../state-rds-postgres/README.md).

## One controller — not two, and not a failover

A capacity ledger with two writers admits the same slot twice and the machine that has to run both jobs finds out later. billet takes a **claim** row in the database before it polls GitHub, so a second controller is refused at startup naming the machine that holds it, and a replaced one is fenced by the claim's epoch on its next write.

That catches the accident. It is not high availability: nothing here decides that a controller is dead or promotes a standby by itself, so this module launches exactly one instance and says so rather than implying otherwise with a `count` variable.

## What still lives on the machine

`server.identity_dir` holds the deployment identity, the node-wire CA and its rotation state, the process lock and the maintenance fence. None of it moved, because a private key is not rows.

**There is deliberately no EBS volume for it.** A volume pins the instance to an availability zone, which is exactly what makes the SQLite controller un-replaceable — so the identity directory is on the encrypted root disk, which is deleted on termination. An off-site copy is therefore the *only* recovery path for it, not a nicety: a ledger without its identity is a fresh authority that cannot see the compute the old one launched, and the GitHub App private key is issued exactly once.

> **`billet local backup` writes an identity-only archive on this profile**, and taking one is not optional here. It deliberately does not copy the ledger — that is `pg_dump` or your provider's snapshot — but it does capture the deployment identity, the node-wire CA and its rotation state, and the GitHub App private key. **This module has no ledger volume by design and its root volume is `delete_on_termination`, so that archive is the only copy of the identity there is**: the App key is issued exactly once, and a CA no replacement controller has is a fleet whose certificates mean nothing. Set `create_backup_bucket` so the upload is automatic, and see [docs/deploying/postgres-and-active-passive.md](../../../../../docs/deploying/postgres-and-active-passive.md) for how the two halves are paired on the way back.

`aws_cloudwatch_metric_alarm.control_plane_recover` still matters for that reason: EC2 auto-recovery moves the **same** instance onto new hardware keeping its root volume, where an ASG's fresh launch would not (ADR-001).

## Create or adopt

| Resource | Created when | Adopted when |
|---|---|---|
| `aws_instance.control_plane`, `aws_security_group.control_plane`, its rules, `aws_cloudwatch_metric_alarm.control_plane_recover` | always | — |
| `aws_iam_role.this`, `aws_iam_instance_profile.this` | `create_instance_profile` | `instance_profile_name` set with the bool false |
| `aws_iam_role_policy.state_secrets` | `create_state_secret_policy` **and** this child owns the role | — |
| `aws_s3_bucket.backups` and its configuration | `create_backup_bucket` | `backup_bucket` set (granted, not configured) |
| `aws_iam_role_policy.backups` | a bucket is created **or** adopted, and this child owns the role | — |

The two `create_*` bools are explicit rather than inferred from whether a string is empty, because `count` cannot depend on a value known only at apply — a policy document naming a secret ARN that does not exist yet could otherwise never plan.

## Delivering `BILLET_STATE_DSN`

billet reads the connection string from the environment. `state-rds-postgres` hands over `secret_read_policy_json`; pass it here as `state_secret_policy_json` with `create_state_secret_policy = true`, and the host can read the ledger credential with its own instance identity and assemble the DSN at install time. Nothing about the password reaches a plan, a state file or cloud-init.

The `junioryono.billet.host` role writes `/etc/billet/server.env` from `billet_server_environment` and puts `EnvironmentFile=` in the unit it renders — never a drop-in, because the transactional host upgrade refuses effective drop-ins it cannot replace and recover.

## The backup grant is the SQLite profile's, byte for byte

Same principal, same rendering of `internal/awspolicy`, pinned by the same drift test in `internal/tfpolicy`: **read, write and list its own prefix, and never delete.** A backup credential that can destroy its own history is not an off-site copy, and there are no compute permissions in it at all — a control plane launches nothing.

The bytes are duplicated under `policy/` rather than read out of the sibling child, because a Terraform module cannot read a file from a sibling and reaching across with `${path.module}/../` would couple two children that are independently composable. The drift test is what keeps them equal.

## Usage

```hcl
module "controller" {
  source = "github.com/junioryono/billet//terraform/modules/billet/modules/control-plane-postgres?ref=v0.7.0"

  name             = "billet"
  vpc_id           = var.vpc_id
  subnet_id        = var.private_subnet_id
  vpc_cidr         = var.vpc_cidr
  subnet_in_vpc_ok = true

  create_backup_bucket = true

  create_state_secret_policy = true
  state_secret_policy_json   = module.ledger.secret_read_policy_json
}

module "ledger" {
  source = "github.com/junioryono/billet//terraform/modules/billet/modules/state-rds-postgres?ref=v0.7.0"

  name       = "billet"
  vpc_id     = var.vpc_id
  subnet_ids = var.private_subnet_ids

  client_security_groups = { controller = module.controller.security_group_id }
}
```

The two reference each other in one direction only — the ledger admits the controller's **security group**, and the controller is granted the ledger's **secret** — so there is no cycle. See `examples/postgres` for the whole composition against an adopted network.
