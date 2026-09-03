# state-rds-postgres

The PostgreSQL ledger for a billet control plane — or the grant for one you already run.

With `server.state.backend: postgres`, billet's capacity ledger, node registrations, custody and job history live in a database billet does not operate. What stays on the controller is `server.identity_dir`: the deployment identity, the node-wire CA and its rotation state, the process lock and the maintenance fence. Those cannot follow the ledger here — a private key is not rows, and local process coordination has nothing to do with SQL. See [docs/state-backends.md](../../../../../docs/state-backends.md) for what the profile buys and what it does not.

**It is one controller either way.** A database's ability to serialize writes is not proof that only one process is polling GitHub, which is why billet takes a claim row in the ledger and fences a replaced controller by its epoch. This module provisions storage; it does not make billet highly available.

## Create or adopt

| Resource | Created when | Adopted when |
|---|---|---|
| `aws_db_instance.ledger` | `endpoint` unset | `endpoint` set (**nothing at all is created**) |
| `aws_db_subnet_group.ledger` | `endpoint` unset | — |
| `aws_db_parameter_group.ledger` | `endpoint` unset | — |
| `aws_security_group.ledger`, `aws_vpc_security_group_ingress_rule.ledger` | `endpoint` unset | — |

`client_security_groups` is a **map keyed by a name you choose**, not a list. The key becomes the rule's Terraform address, so adding a second client later does not renumber and recreate the controller's own ingress — and a map's keys are known at plan time even when a created group's id is not, which a set of ids would not be.

Adoption is a first-class shape, not a fallback: an existing RDS instance, an Aurora cluster writer or a database on a machine you run is **granted, not configured**, exactly as the backup bucket is one module over. With `endpoint` set this module creates no security group, no subnet group and no database, and your credential story stays yours.

## The password is AWS's, and that is the whole design

billet's `server.state.postgres.dsn_env` names an **environment variable** rather than carrying a connection string, because a DSN holds a password and a secret written into a config file ends up in a backup, a paste buffer and eventually a support thread. This module keeps that promise on the Terraform side by never learning the password at all:

- `manage_master_user_password = true`, so AWS generates and holds it in Secrets Manager. Generating one here with `random_password` and writing a secret version is the obvious shape and puts the password in the **state file**, which is the single thing the indirection exists to prevent.
- The module therefore **creates no DSN secret**. It hands over `master_secret_arn`, a `dsn_template` with the password left as a placeholder, and `secret_read_policy_json` — the exact IAM statement that reads them — and the connection string is assembled on the host.
- `dsn_secret_arn` adopts a secret you keep with the whole DSN in it, and includes it in that grant.

Assembling it once, on the controller. **The password must be percent-encoded**, and that is not a formality: this is a URI, and RDS generates passwords containing punctuation — a `#` truncates everything after it, a `%` starts an escape, and a `?` starts the query string, so a pasted password can silently change which database or options billet connects with, or fail to parse at all.

```bash
# ONE python3 invocation, so the password is never a shell variable and never
# reaches argv, /proc or a history file. urllib.parse.quote with safe='' is what
# encodes every URI-significant character rather than most of them.
aws secretsmanager get-secret-value --secret-id "$MASTER_SECRET_ARN" \
    --query SecretString --output text \
  | ENDPOINT="$ENDPOINT" PORT="$PORT" DATABASE="$DATABASE" python3 -c '
import json, os, sys, urllib.parse
d = json.load(sys.stdin)
q = lambda s: urllib.parse.quote(s, safe="")
print("BILLET_STATE_DSN=postgres://%s:%s@%s:%s/%s?sslmode=verify-full&sslrootcert=/etc/billet/rds-ca-bundle.pem"
      % (q(d["username"]), q(d["password"]),
         os.environ["ENDPOINT"], os.environ["PORT"], os.environ["DATABASE"]))' \
  | install -m 0640 -o root -g billet /dev/stdin /etc/billet/server.env
```

**And `sslrootcert` is not optional.** RDS server certificates are issued by self-signed per-region `Amazon RDS … Root CA` authorities — measured against the published bundle — which are in no operating system trust store, so `verify-full` with nothing to verify against fails on a stock host. Fetch the bundle from `ssl_bundle_url` and put it at `ssl_root_cert_path`:

```bash
curl -fsSL -o /etc/billet/rds-ca-bundle.pem \
  https://truststore.pki.rds.amazonaws.com/us-west-2/us-west-2-bundle.pem
```

The `junioryono.billet.host` role does both halves: `billet_state_ca_bundle_src` installs that file, and `billet_server_environment` writes `/etc/billet/server.env` and puts `EnvironmentFile=` in the unit it renders — never a systemd drop-in, because the transactional host upgrade refuses effective drop-ins it cannot replace and recover.

## What it pins, and why

- **`synchronous_commit = on`**, through a created parameter group. With it off PostgreSQL acknowledges a commit before the record is on disk, so a crash can lose scheduling decisions billet has already acted on — billet checks this at startup and refuses. Pinning it means a database this module made can never be the thing that refusal is about.
- **A final snapshot, under a deterministic name.** `skip_final_snapshot = true` is what most examples carry and is how a capacity ledger disappears during a `terraform destroy` somebody ran to clean up something else. It is the same accident `prevent_destroy` guards on the SQLite profile's ledger volume. The name is deterministic rather than timestamped so the plan stays deterministic, and the consequence is worth knowing: a **second** create-and-destroy cycle under the same `name` fails the destroy with `DBSnapshotAlreadyExists`. That is the safe direction — the refusal is loud and the previous snapshot survives — and the way through is to rename or delete that snapshot deliberately.
- **`iam_database_authentication_enabled`, Performance Insights and gp3 storage** are all measured against the API rather than assumed for the default `db.t4g.micro`: `describe-orderable-db-instance-options` reports all three supported, and the gp3 minimum of 20 GiB is where `allocated_gib` starts.
- **`deletion_protection` on, and `backup_retention_days` at least 1.** A retention of zero disables automated backups entirely, which is the state an operator discovers on the day they need one.
- **`ignore_changes = [engine_version]`.** AWS applies minor upgrades in the maintenance window, so the value in state drifts from the configured one by design; without this, every plan after a window proposes downgrading a database that is fine.
- **Ingress by security group, never by CIDR.** A database holding a capacity ledger is not a shared service — every row in it authorises compute — and the whole point of this profile is that the controller is replaceable, which means it does not keep an address.
- **IAM database authentication on.** billet itself cannot use it (a token lives fifteen minutes and billet holds a long-lived pool), but `billet local backup` **refuses** on a PostgreSQL deployment, so `pg_dump` is yours — and this lets you reach the ledger with your own IAM identity instead of a copy of the master credential. It grants nobody anything on its own; a role still has to be granted `rds_iam` inside the database.

## Backing it up is yours — both halves of it

The **ledger** is `pg_dump` or a provider snapshot. billet refuses to copy it deliberately: a half-measure copying rows out through billet's own connection would produce an archive that *looks* like a backup and is not. Automated backups are on here by default, and so is `deletion_protection`.

The **identity directory** is billet's. `billet local backup` writes an identity-only archive on a PostgreSQL deployment — the deployment identity, the node-wire CA and its rotation state, and the GitHub App private key — and records the ledger as external rather than pretending to have copied it. Restoring is the two halves paired: your dump first, then `billet local restore --external-ledger-attached`, which billet refuses without because it cannot see the database on the other end of the DSN. See [docs/state-backends.md](../../../../../docs/state-backends.md).

**Pair the two halves.** A ledger without its deployment identity is a fresh authority that cannot see the compute the old one launched; an identity without the CA cannot issue a node certificate. If you restore one, restore the other from the same moment.

## Usage

```hcl
module "ledger" {
  source = "github.com/junioryono/billet//terraform/modules/billet/modules/state-rds-postgres?ref=v0.5.0"

  name       = "billet"
  vpc_id     = var.vpc_id
  subnet_ids = var.private_subnet_ids # two zones; RDS requires it

  # Keyed by a name, so adding a second client never recreates this rule.
  client_security_groups = { controller = module.controller.security_group_id }
}
```

Adopting instead:

```hcl
module "ledger" {
  source = "github.com/junioryono/billet//terraform/modules/billet/modules/state-rds-postgres?ref=v0.5.0"

  name     = "billet"
  endpoint = "ledger.internal.example"
  database = "billet"
  username = "billet"
  # Optional: a secret you keep the whole DSN in, so the controller can read it.
  dsn_secret_arn = aws_secretsmanager_secret.billet_dsn.arn
}
```
