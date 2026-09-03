# The PostgreSQL profile's plan tests, against both children directly. Same
# mocked provider as every other suite here: no credentials, nothing created.
#
# WHAT THESE ASSERT IS THE PROFILE'S PROMISES, not that the resources exist:
# an adopted endpoint creates nothing, a created one cannot lose its rows to a
# casual destroy, the credential never becomes a Terraform value, and the
# controller reaches its ledger by IDENTITY rather than by an address it will not
# keep — which is the whole reason a replaceable controller is possible at all.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }

  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_data "aws_ami" {
    defaults = {
      id = "ami-0123456789abcdef0"
    }
  }
}

# AN ADOPTED ENDPOINT CREATES NOTHING AT ALL.
#
# The sibling of `an_adopted_backup_bucket_is_granted_but_not_configured`: an
# operator who already runs PostgreSQL hands over its address and keeps their own
# credential story, and this module must not mint a database, a security group or
# a subnet group beside it. Asserted as counts of zero rather than by inspecting
# attributes, because "created nothing" is the claim.
run "an_adopted_endpoint_creates_nothing" {
  command = plan

  module {
    source = "./modules/state-rds-postgres"
  }

  variables {
    name     = "billet-test"
    endpoint = "ledger.internal.example"
    port     = 6432
    database = "billet_ledger"
    username = "billet_ro"
  }

  assert {
    condition     = length(aws_db_instance.ledger) == 0 && length(aws_db_subnet_group.ledger) == 0 && length(aws_security_group.ledger) == 0 && length(aws_db_parameter_group.ledger) == 0
    error_message = "an adopted endpoint must create nothing: the operator's database is granted, not configured"
  }

  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.ledger) == 0
    error_message = "an adopted endpoint's ingress is the operator's; this module must not add a rule to a group it does not own"
  }

  # The connection object reports what was adopted, verbatim — a normalised or
  # defaulted value here is a controller pointed somewhere the operator did not
  # name.
  assert {
    condition     = output.connection.endpoint == "ledger.internal.example" && output.connection.port == 6432 && output.connection.database == "billet_ledger" && output.connection.username == "billet_ro"
    error_message = "the connection object must report the adopted endpoint exactly as given"
  }

  assert {
    condition     = output.connection.adopted
    error_message = "adopted must say which side is real, so a caller need not infer it from which outputs are empty"
  }

  # NOTHING TO GRANT IS AN EMPTY DOCUMENT, NOT AN EMPTY RESOURCE LIST. An IAM
  # policy with `Resource: []` is invalid rather than narrow, and a control-plane
  # module attaching one would fail the apply after everything else succeeded.
  assert {
    condition     = output.secret_read_policy_json == "" && output.master_secret_arn == ""
    error_message = "with no created instance and no adopted DSN secret there is nothing to grant, and an empty policy document is how that is said"
  }

  # THE DSN IS REPORTED AS A TEMPLATE, so the shape is stated once here rather
  # than reconstructed by hand on every host — with the password left as a
  # placeholder, because a DSN this module could render is a password this module
  # would know. Asserted WHOLE, not by substring: a template that dropped the
  # database name or the port would still contain every fragment a `strcontains`
  # pair looks for.
  #
  # sslrootcert IS PART OF IT, and that is the assertion worth having. RDS
  # certificates are issued by self-signed per-region "Amazon RDS ... Root CA"
  # authorities — measured against the published bundle — which are in no
  # operating system trust store, so `sslmode=verify-full` with nothing to verify
  # against fails on every stock host. The first version of this output emitted
  # exactly that: a DSN that cannot connect.
  assert {
    condition     = output.dsn_template == "postgres://billet_ro:PASSWORD@ledger.internal.example:6432/billet_ledger?sslmode=verify-full&sslrootcert=/etc/billet/rds-ca-bundle.pem"
    error_message = "the DSN template must name this deployment's user, host, port and database, carry a placeholder rather than a credential, ask for a verified TLS connection, and say what to verify it against"
  }
}

# AN ADOPTED ENDPOINT WITH A DSN SECRET GETS EXACTLY THAT SECRET GRANTED.
run "an_adopted_dsn_secret_is_the_whole_grant" {
  command = plan

  module {
    source = "./modules/state-rds-postgres"
  }

  variables {
    name           = "billet-test"
    endpoint       = "ledger.internal.example"
    dsn_secret_arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:billet/dsn-AbCdEf"
  }

  assert {
    condition = jsondecode(output.secret_read_policy_json) == {
      Version = "2012-10-17"
      Statement = [{
        Sid      = "BilletReadLedgerCredential"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
        Resource = ["arn:aws:secretsmanager:us-east-1:123456789012:secret:billet/dsn-AbCdEf"]
      }]
    }
    error_message = "the grant must be exactly the one secret named, as a complete document"
  }

  assert {
    condition     = output.dsn_secret_arn == "arn:aws:secretsmanager:us-east-1:123456789012:secret:billet/dsn-AbCdEf"
    error_message = "the adopted DSN secret must pass through so a control-plane module can grant read on it"
  }
}

# AND A CUSTOMER KEY ADDS kms:Decrypt, because Secrets Manager decrypts on the
# caller's behalf: GetSecretValue against a secret encrypted with a
# customer-managed key ALSO requires kms:Decrypt on that key. A grant with the
# Secrets Manager actions alone returns AccessDenied — and only for the
# deployments that hardened their encryption, which is the worst set to break.
run "a_customer_key_is_granted_alongside_the_secret" {
  command = plan

  module {
    source = "./modules/state-rds-postgres"
  }

  variables {
    name           = "billet-test"
    endpoint       = "ledger.internal.example"
    dsn_secret_arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:billet/dsn-AbCdEf"
    kms_key_arn    = "arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555"
  }

  assert {
    condition = jsondecode(output.secret_read_policy_json) == {
      Version = "2012-10-17"
      Statement = [
        {
          Sid      = "BilletReadLedgerCredential"
          Effect   = "Allow"
          Action   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
          Resource = ["arn:aws:secretsmanager:us-east-1:123456789012:secret:billet/dsn-AbCdEf"]
        },
        {
          Sid      = "BilletDecryptLedgerCredential"
          Effect   = "Allow"
          Action   = ["kms:Decrypt"]
          Resource = ["arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555"]
          Condition = {
            StringEquals = {
              "kms:ViaService" = "secretsmanager.us-east-1.amazonaws.com"
            }
          }
        },
      ]
    }
    error_message = "with a customer-managed key the grant must also carry kms:Decrypt on it, scoped to Secrets Manager so the key cannot be called directly"
  }
}

# A CREATED LEDGER CANNOT LOSE ITS ROWS QUIETLY.
run "a_created_ledger_is_encrypted_private_and_hard_to_destroy" {
  command = plan

  module {
    source = "./modules/state-rds-postgres"
  }

  variables {
    name                   = "billet-test"
    vpc_id                 = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids             = ["subnet-0a0a0a0a0a0a0a0a0", "subnet-0b0b0b0b0b0b0b0b0"]
    client_security_groups = { controller = "sg-0c0c0c0c0c0c0c0c0" }
  }

  assert {
    condition     = aws_db_instance.ledger[0].storage_encrypted && !aws_db_instance.ledger[0].publicly_accessible
    error_message = "the ledger must be encrypted at rest and unreachable from the internet"
  }

  # A FINAL SNAPSHOT IS TAKEN, AND ITS NAME IS DETERMINISTIC. skip_final_snapshot
  # defaults to true in most examples and is how a capacity ledger disappears
  # during a destroy somebody ran to clean up something else.
  assert {
    condition     = !aws_db_instance.ledger[0].skip_final_snapshot && aws_db_instance.ledger[0].final_snapshot_identifier == "billet-test-ledger-final"
    error_message = "a destroy must take a final snapshot under a name that is not left to chance"
  }

  assert {
    condition     = aws_db_instance.ledger[0].deletion_protection
    error_message = "deletion protection is on by default for the reason prevent_destroy is on the SQLite ledger volume"
  }

  assert {
    condition     = aws_db_instance.ledger[0].backup_retention_period == 7
    error_message = "automated backups must be on by default; zero disables them entirely"
  }
}

# THE PASSWORD IS AWS'S, NOT TERRAFORM'S.
#
# This is the assertion the whole credential story rests on. Generating a
# password here — random_password plus a secret version — is the obvious shape
# and puts it in the state file, which is the single thing billet's `dsn_env`
# indirection exists to prevent. manage_master_user_password is what makes the
# module's promise structural rather than a convention.
run "the_ledger_password_is_never_a_terraform_value" {
  command = plan

  module {
    source = "./modules/state-rds-postgres"
  }

  variables {
    name                   = "billet-test"
    vpc_id                 = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids             = ["subnet-0a0a0a0a0a0a0a0a0", "subnet-0b0b0b0b0b0b0b0b0"]
    client_security_groups = { controller = "sg-0c0c0c0c0c0c0c0c0" }
  }

  assert {
    condition     = aws_db_instance.ledger[0].manage_master_user_password
    error_message = "the master password must be generated and held by AWS, never passed through Terraform"
  }

  assert {
    condition     = aws_db_instance.ledger[0].password == null
    error_message = "no password may be set here: one Terraform knows is one in the state file"
  }

}

# BILLET REFUSES synchronous_commit = off, SO A CREATED DATABASE CAN NEVER BE IN
# THAT STATE.
#
# With it off, PostgreSQL acknowledges a commit before the record is on disk, so
# a crash loses scheduling decisions billet has already acted on. billet checks
# it at startup; this is what keeps a database this module made from being the
# thing that refusal is about.
run "a_created_ledger_pins_what_billet_requires_of_the_server" {
  command = plan

  module {
    source = "./modules/state-rds-postgres"
  }

  variables {
    name                   = "billet-test"
    vpc_id                 = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids             = ["subnet-0a0a0a0a0a0a0a0a0", "subnet-0b0b0b0b0b0b0b0b0"]
    client_security_groups = { controller = "sg-0c0c0c0c0c0c0c0c0" }
    engine_version         = "18.1"
  }

  assert {
    condition     = one([for p in aws_db_parameter_group.ledger[0].parameter : p if p.name == "synchronous_commit"]).value == "on"
    error_message = "the created parameter group must pin synchronous_commit = on, which billet checks at startup and refuses without"
  }

  # The family follows the MAJOR version. A minor in engine_version must not
  # produce `postgres18.1`, which is not a family AWS has.
  assert {
    condition     = aws_db_parameter_group.ledger[0].family == "postgres18"
    error_message = "the parameter group family must be the major version alone"
  }
}

# THE LEDGER ADMITS THE CONTROLLER BY IDENTITY, NEVER BY ADDRESS.
#
# This is what makes a replaceable controller possible: a CIDR is a promise about
# addressing, and the whole point of this profile is that the machine holding the
# control plane can be thrown away — after which it does not have the address the
# rule named.
run "the_ledger_admits_the_controller_by_group_not_by_cidr" {
  command = plan

  module {
    source = "./modules/state-rds-postgres"
  }

  variables {
    name                   = "billet-test"
    vpc_id                 = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_ids             = ["subnet-0a0a0a0a0a0a0a0a0", "subnet-0b0b0b0b0b0b0b0b0"]
    client_security_groups = { controller = "sg-0c0c0c0c0c0c0c0c0" }
  }

  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.ledger) == 1
    error_message = "exactly one client group was named, so exactly one rule belongs here"
  }

  assert {
    condition     = aws_vpc_security_group_ingress_rule.ledger["controller"].referenced_security_group_id == "sg-0c0c0c0c0c0c0c0c0"
    error_message = "the ingress must reference the controller's security group"
  }

  assert {
    condition     = aws_vpc_security_group_ingress_rule.ledger["controller"].cidr_ipv4 == null
    error_message = "no CIDR may appear on the ledger's ingress: a database holding a capacity ledger is not a shared service"
  }

  assert {
    condition     = aws_vpc_security_group_ingress_rule.ledger["controller"].from_port == 5432 && aws_vpc_security_group_ingress_rule.ledger["controller"].to_port == 5432
    error_message = "the rule must open PostgreSQL and nothing else"
  }
}

# THE CONTROLLER ON THIS PROFILE HAS NO LEDGER VOLUME, AND THAT IS THE FEATURE.
#
# A volume pins the instance to an availability zone, which is exactly what makes
# the SQLite controller un-replaceable. Asserting its ABSENCE structurally is
# worth more than it looks: adding one back would read as a durability
# improvement and would quietly undo the reason this child exists.
run "the_postgres_controller_carries_no_ledger_volume" {
  command = plan

  module {
    source = "./modules/control-plane-postgres"
  }

  variables {
    name             = "billet-test"
    vpc_id           = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id        = "subnet-0e0e0e0e0e0e0e0e0"
    vpc_cidr         = "10.0.0.0/16"
    subnet_in_vpc_ok = true
  }

  assert {
    condition     = one(aws_instance.control_plane.root_block_device).volume_size == 20
    error_message = "the root volume carries the OS and the identity directory on this profile, and sizes from root_volume_gib"
  }

  assert {
    condition     = one(aws_instance.control_plane.root_block_device).encrypted
    error_message = "the root volume holds server.identity_dir — the node-wire CA's private key — so it must be encrypted"
  }

  # AUTO-RECOVERY, NOT AN ASG, and its action follows the PARTITION. This is the
  # same assertion control_plane.tftest.hcl makes about the SQLite child, made
  # again here because the two children are independent entry points and a rule
  # enforced at only one of them is an entry point that does not enforce it.
  assert {
    condition     = aws_cloudwatch_metric_alarm.control_plane_recover.alarm_actions == toset(["arn:aws:automate:us-east-1:ec2:recover"])
    error_message = "the recovery action must be exactly the built-in ec2:recover automation for this partition and region"
  }

  assert {
    condition     = aws_cloudwatch_metric_alarm.control_plane_recover.namespace == "AWS/EC2" && aws_cloudwatch_metric_alarm.control_plane_recover.metric_name == "StatusCheckFailed_System" && aws_cloudwatch_metric_alarm.control_plane_recover.statistic == "Maximum" && aws_cloudwatch_metric_alarm.control_plane_recover.comparison_operator == "GreaterThanOrEqualToThreshold" && aws_cloudwatch_metric_alarm.control_plane_recover.threshold == 1 && aws_cloudwatch_metric_alarm.control_plane_recover.period == 60 && aws_cloudwatch_metric_alarm.control_plane_recover.evaluation_periods == 2
    error_message = "the alarm must watch the SYSTEM status check — an instance status check is the guest's own health, which recovery cannot fix"
  }
}

# AND ITS RECOVERY ACTION FOLLOWS THE PARTITION.
#
# The one assertion here that can fail against a hard-coded `arn:aws:`, and the
# reason it is repeated for this child rather than inherited from the SQLite
# one's suite: they are two independent entry points, and a rule enforced at only
# one of two entry points is an entry point that does not enforce it. A
# commercial ARN in GovCloud creates an alarm that silently does nothing, which
# is worse than no alarm — it looks like recovery is configured.
run "the_postgres_controller_recovery_follows_the_partition" {
  command = plan

  module {
    source = "./modules/control-plane-postgres"
  }

  variables {
    name             = "billet-test"
    vpc_id           = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id        = "subnet-0e0e0e0e0e0e0e0e0"
    vpc_cidr         = "10.0.0.0/16"
    subnet_in_vpc_ok = true
  }

  override_data {
    target = data.aws_partition.this
    values = {
      partition  = "aws-us-gov"
      dns_suffix = "amazonaws.com"
    }
  }
  override_data {
    target = data.aws_region.this
    values = { region = "us-gov-west-1" }
  }

  assert {
    condition     = aws_cloudwatch_metric_alarm.control_plane_recover.alarm_actions == toset(["arn:aws-us-gov:automate:us-gov-west-1:ec2:recover"])
    error_message = "the recover action must follow BOTH the partition and the region; a commercial ARN in GovCloud creates an alarm that silently does nothing"
  }

  assert {
    condition = jsondecode(aws_iam_role.this[0].assume_role_policy) == {
      Version = "2012-10-17"
      Statement = [{
        Effect    = "Allow"
        Action    = "sts:AssumeRole"
        Principal = { Service = "ec2.amazonaws.com" }
      }]
    }
    error_message = "the own trust policy must name the PARTITION's EC2 service principal, taken from the partition's dns_suffix rather than written out"
  }
}

# THE STATE-SECRET GRANT IS OPT-IN AND EXACT.
run "the_state_secret_grant_is_attached_only_when_asked" {
  command = plan

  module {
    source = "./modules/control-plane-postgres"
  }

  variables {
    name             = "billet-test"
    vpc_id           = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id        = "subnet-0e0e0e0e0e0e0e0e0"
    vpc_cidr         = "10.0.0.0/16"
    subnet_in_vpc_ok = true
  }

  assert {
    condition     = length(aws_iam_role_policy.state_secrets) == 0
    error_message = "with no state secret to read, the controller must be granted nothing"
  }
}

run "the_state_secret_grant_is_the_document_it_was_given" {
  command = plan

  module {
    source = "./modules/control-plane-postgres"
  }

  variables {
    name                       = "billet-test"
    vpc_id                     = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id                  = "subnet-0e0e0e0e0e0e0e0e0"
    vpc_cidr                   = "10.0.0.0/16"
    subnet_in_vpc_ok           = true
    create_state_secret_policy = true
    state_secret_policy_json   = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Action\":[\"secretsmanager:GetSecretValue\"],\"Effect\":\"Allow\",\"Resource\":[\"arn:aws:secretsmanager:us-east-1:123456789012:secret:x-AbCdEf\"]}]}"
  }

  assert {
    condition     = length(aws_iam_role_policy.state_secrets) == 1
    error_message = "an explicit grant must actually be attached"
  }

  assert {
    condition = jsondecode(aws_iam_role_policy.state_secrets[0].policy) == {
      Version = "2012-10-17"
      Statement = [{
        Action   = ["secretsmanager:GetSecretValue"]
        Effect   = "Allow"
        Resource = ["arn:aws:secretsmanager:us-east-1:123456789012:secret:x-AbCdEf"]
      }]
    }
    error_message = "the attached policy must be the document the state module emitted, unaltered"
  }
}

# THE BACKUP GRANT IS THE SAME PRINCIPAL AS THE SQLITE PROFILE'S, and it must
# still carry no delete and no compute. The bytes are pinned by
# internal/tfpolicy's drift test; this proves the rendering the module actually
# attaches is the one that was pinned.
run "the_postgres_controller_backup_grant_carries_no_delete_and_no_compute" {
  command = plan

  module {
    source = "./modules/control-plane-postgres"
  }

  variables {
    name                 = "billet-test"
    vpc_id               = "vpc-0f0f0f0f0f0f0f0f0"
    subnet_id            = "subnet-0e0e0e0e0e0e0e0e0"
    vpc_cidr             = "10.0.0.0/16"
    subnet_in_vpc_ok     = true
    create_backup_bucket = true
  }

  assert {
    condition     = length(aws_iam_role_policy.backups) == 1
    error_message = "a created bucket must come with the controller's grant to write to it"
  }

  assert {
    condition     = length([for s in jsondecode(aws_iam_role_policy.backups[0].policy).Statement : s if length([for a in s.Action : a if strcontains(lower(a), "delete") || startswith(a, "ec2:")]) > 0]) == 0
    error_message = "the backup credential must hold no delete and no compute permission: it lives on the one host holding the App private key and the node-wire CA"
  }

  # The bucket outlives the stack. On this profile it is the ONLY copy of the
  # deployment identity, because there is no retained volume beside the instance.
  assert {
    condition     = aws_s3_bucket_versioning.backups[0].versioning_configuration[0].status == "Enabled"
    error_message = "versioning is what makes the no-delete grant mean something"
  }
}
