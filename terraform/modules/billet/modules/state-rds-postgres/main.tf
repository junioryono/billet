# state-rds-postgres holds a billet control plane's LEDGER, and nothing else it
# owns.
#
# The PostgreSQL backend published the contract this module exists to satisfy: with
# `server.state.backend: postgres`, the capacity ledger, node registrations,
# custody and job history live in a database billet does not operate, and what
# stays on the machine is `identity_dir` — the deployment identity, the node-wire
# CA and its rotation state, the process lock and the maintenance fence. Those
# cannot follow the ledger here: a private key is not rows, and local process
# coordination has nothing to do with SQL.
#
# WHAT IT BUYS IS A REPLACEABLE CONTROLLER, and it is worth being precise about
# what it does not buy. Exactly ONE controller may make scheduling decisions
# either way — a database's ability to serialize writes is not proof that only
# one process is polling GitHub, which is why billet takes a claim row as well.
# See docs/deploying/postgres-and-active-passive.md.
#
# IT EXPOSES A NARROW CONNECTION OBJECT AND NOTHING ELSE: the endpoint, the port,
# the database, the username, and the ARNs of the secrets a controller may read.
# No instance handle, because a control-plane module has no business resizing
# somebody's database.
#
# ADOPTION IS A FIRST-CLASS SHAPE, not an afterthought: an operator with an
# existing RDS instance, an Aurora cluster or a database on a machine they run
# passes its endpoint and this module creates nothing at all. That is the same
# boundary the backup bucket already draws — granted, not configured.

data "aws_partition" "this" {}

data "aws_region" "this" {}

locals {
  create = var.endpoint == ""

  tags = merge(var.tags, {
    "sh.billet.module" = "terraform-aws-billet"
    "Name"             = var.name
  })

  # The resolved connection, from whichever side is real.
  endpoint = local.create ? aws_db_instance.ledger[0].address : var.endpoint
  port     = local.create ? aws_db_instance.ledger[0].port : var.port
  database = local.create ? aws_db_instance.ledger[0].db_name : var.database
  username = local.create ? aws_db_instance.ledger[0].username : var.username

  master_secret_arn = local.create ? aws_db_instance.ledger[0].master_user_secret[0].secret_arn : ""

  # Every secret a controller may need to read to build BILLET_STATE_DSN. The
  # LIST is what the policy is scoped to, so an empty one produces no statement
  # rather than a statement over nothing — which in IAM is not the same thing.
  readable_secret_arns = compact([local.master_secret_arn, var.dsn_secret_arn])
}

# THE SUBNET GROUP IS THE ONE PIECE AN ADOPTED DATABASE ALREADY HAS. Created only
# with the instance, because a group naming subnets somebody else's database does
# not sit in is an object nothing uses.
resource "aws_db_subnet_group" "ledger" {
  count = local.create ? 1 : 0

  name        = "${var.name}-ledger"
  description = "billet control-plane ledger"
  subnet_ids  = var.subnet_ids
  tags        = local.tags
}

# THE LEDGER'S OWN SECURITY GROUP, reachable only from the control plane.
#
# A DATABASE HOLDING A CAPACITY LEDGER IS NOT A SHARED SERVICE. Every row in it
# authorises compute, so the ingress is the controller's group and nothing else —
# not a CIDR, because a CIDR is a promise about addressing rather than about
# identity, and the whole point of this profile is that the controller is
# replaceable and therefore does not keep an address.
resource "aws_security_group" "ledger" {
  count = local.create ? 1 : 0

  name        = "${var.name}-ledger"
  description = "billet control-plane ledger: postgres from the control plane only"
  vpc_id      = var.vpc_id
  tags        = merge(local.tags, { "Name" = "${var.name}-ledger" })
}

# A MAP KEYED BY A NAME THE CALLER CHOSE, and both halves of that are forced.
#
# `for_each` needs its KEYS known at plan, and a security group Terraform is
# creating in the same apply has an id that is not — so `toset(<a list of group
# ids>)` fails the plan for exactly the composition this module exists to serve.
# A map fixes that because only the VALUES are unknown: the caller writes
# `{ controller = module.controller.security_group_id }` and the key is a literal.
#
# AND `count` WAS THE WRONG WAY OUT, which was the first shape here. Indexing by
# position means inserting a second client renumbers every rule after it, so
# Terraform destroys and recreates working ingress — an interruption to the
# controller's own connection to its ledger, caused by adding an unrelated
# reader. A caller-chosen key is stable across every edit that does not touch it.
resource "aws_vpc_security_group_ingress_rule" "ledger" {
  for_each = local.create ? var.client_security_groups : {}

  security_group_id            = aws_security_group.ledger[0].id
  description                  = "billet ${each.key}"
  referenced_security_group_id = each.value
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
}

resource "aws_db_instance" "ledger" {
  count = local.create ? 1 : 0

  identifier     = "${var.name}-ledger"
  engine         = "postgres"
  engine_version = var.engine_version
  instance_class = var.instance_class

  db_name  = var.database
  username = var.username
  # MANAGED BY AWS AND NEVER RENDERED. The password is generated and held in
  # Secrets Manager rather than passed through Terraform, so it is not in a plan,
  # a state file, or anybody's shell history. Generating one here with
  # random_password would put it in state, which is the single thing billet's
  # `dsn_env` indirection exists to avoid — so the DSN is assembled on the host
  # from this secret, and this module hands over its ARN and the grant for it.
  manage_master_user_password   = true
  master_user_secret_kms_key_id = var.kms_key_arn != "" ? var.kms_key_arn : null

  allocated_storage     = var.allocated_gib
  max_allocated_storage = var.max_allocated_gib > 0 ? var.max_allocated_gib : null
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = var.kms_key_arn != "" ? var.kms_key_arn : null

  db_subnet_group_name   = aws_db_subnet_group.ledger[0].name
  vpc_security_group_ids = [aws_security_group.ledger[0].id]
  publicly_accessible    = false

  multi_az = var.multi_az

  # THE LEDGER IS THE DEPLOYMENT'S CAPACITY RECORD, so retention is not a tuning
  # knob. A retention of zero disables automated backups entirely, which is the
  # state an operator discovers on the day they need one; the variable refuses it.
  backup_retention_period = var.backup_retention_days
  copy_tags_to_snapshot   = true

  # A FINAL SNAPSHOT IS TAKEN, and its name is deterministic so a destroy cannot
  # silently skip it. `skip_final_snapshot = true` is what most examples carry and
  # is how a ledger disappears during a `terraform destroy` somebody ran to clean
  # up something else — the same accident prevent_destroy guards on the SQLite
  # profile's ledger volume.
  skip_final_snapshot       = false
  final_snapshot_identifier = "${var.name}-ledger-final"

  # Minor versions are applied in the maintenance window rather than by a plan, so
  # an ordinary `terraform apply` does not restart the database a control plane is
  # mid-transaction against.
  auto_minor_version_upgrade  = true
  apply_immediately           = false
  allow_major_version_upgrade = false

  deletion_protection = var.deletion_protection

  # IAM AUTHENTICATION IS ON, AND BILLET ITSELF CANNOT USE IT. An IAM auth token
  # is valid for fifteen minutes and billet holds a long-lived connection pool, so
  # its own DSN carries the password either way. What this is for is the half of
  # the profile billet explicitly does NOT do: `billet local backup` refuses on a
  # PostgreSQL deployment, so pg_dump is the operator's, and this lets them reach
  # the ledger with their own IAM identity instead of a copy of the master
  # credential. Enabling it grants nobody anything on its own — a role still has
  # to be granted rds_iam inside the database.
  iam_database_authentication_enabled = true

  # PERFORMANCE INSIGHTS IS ON, at the free seven-day retention.
  #
  # MEASURED RATHER THAN READ, because the first version of this had it off with
  # a written justification that the default burstable class does not offer it.
  # `describe-orderable-db-instance-options --db-instance-class db.t4g.micro
  # --engine postgres` answers SupportsPerformanceInsights: true, and so does
  # SupportsIAMDatabaseAuthentication — so that justification was simply false,
  # and a suppression whose stated reason is untrue is worse than no suppression.
  performance_insights_enabled          = var.performance_insights
  performance_insights_retention_period = var.performance_insights ? 7 : null
  performance_insights_kms_key_id       = var.performance_insights && var.kms_key_arn != "" ? var.kms_key_arn : null

  # WHAT BILLET REQUIRES OF THE SERVER, asserted here rather than discovered at
  # startup. `synchronous_commit = off` lets PostgreSQL acknowledge a commit
  # before the record is on disk, so a crash can lose scheduling decisions billet
  # has already acted on — billet refuses to start against it, and this parameter
  # group is what stops a created database ever being in that state.
  parameter_group_name = aws_db_parameter_group.ledger[0].name

  tags = local.tags

  lifecycle {
    # A NEW MINOR VERSION MUST NOT PLAN A CHANGE. AWS applies them in the
    # maintenance window, so the value in state drifts from the configured one by
    # design; without this, every plan after a maintenance window proposes
    # downgrading a database that is fine.
    ignore_changes = [engine_version]
  }
}

resource "aws_db_parameter_group" "ledger" {
  count = local.create ? 1 : 0

  # name_prefix, BECAUSE create_before_destroy AND A FIXED NAME CANNOT BOTH HOLD.
  # A replacement builds the new group while the old one still exists, and RDS
  # refuses a duplicate name — so the pair as first written could never have
  # succeeded at the one moment it exists for. The family is derived from the
  # MAJOR version alone; a minor in engine_version must not produce
  # `postgres18.1`, which is not a family AWS has (measured:
  # describe-db-engine-versions --engine-version 18 answers postgres18).
  name_prefix = "${var.name}-ledger-"
  family      = "postgres${split(".", var.engine_version)[0]}"
  description = "billet control-plane ledger"

  parameter {
    # BILLET CHECKS THIS AT STARTUP AND REFUSES `off`. Pinning it here means a
    # created database can never be the thing that refusal is about.
    name  = "synchronous_commit"
    value = "on"
  }

  tags = local.tags

  lifecycle {
    create_before_destroy = true
  }
}
